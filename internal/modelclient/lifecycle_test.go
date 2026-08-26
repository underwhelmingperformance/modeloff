package modelclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// fakeSession is the smallest [Session] a dispatch goroutine needs to
// start, run and stop. Every read answers empty and every emit is
// discarded: the lifecycle tests are about when the goroutine exists,
// not what it says.
type fakeSession struct {
	// repliesGate, when non-nil, parks the attach-time reply load
	// until it is closed, so a test can hold an attach open and act
	// while it is in flight.
	repliesGate chan struct{}

	// dmThreads is the persisted DM history, keyed by the peer the
	// thread is with. `instances` answers id lookups. Both are set at
	// construction by the test that needs them and read-only after.
	dmThreads map[domain.InstanceID][]domain.StoredEvent
	instances map[domain.InstanceID]*domain.Instance
	windows   map[domain.ChannelName]*domain.ChannelWindow

	// handleFn, when non-nil, answers a dispatched command, so a test
	// can drive the reply events a real dispatcher would return.
	handleFn func(protocol.Command) protocol.Response

	// caps, when non-nil, is what [fakeSession.ClientCaps] reports for
	// every identity, so a test can grant the client under test a
	// capability the tool filter reads.
	caps command.CapabilityHolder

	mu          sync.Mutex
	sub         *fakeSubscription
	actor       *domain.Instance
	subscribes  int
	disconnects []protocol.ClientID
	dmReads     []dmRead
	emitted     []domain.ProtocolEvent
}

// dmRead records one subscription scrollback read, so a test can pin
// which DM thread the seed requested.
type dmRead struct {
	self domain.InstanceID
	peer domain.InstanceID
}

func newFakeSession() *fakeSession {
	f := &fakeSession{}
	f.sub = newFakeSubscription(func(_ context.Context, target protocol.WindowTarget, _ int) ([]protocol.ScrollbackEntry, error) {
		direct, ok := protocol.DirectWindowPeer(target)
		if !ok {
			return nil, nil
		}
		peer := direct

		f.mu.Lock()
		f.dmReads = append(f.dmReads, dmRead{self: "inst-botty", peer: peer})
		stored := append([]domain.StoredEvent(nil), f.dmThreads[peer]...)
		f.mu.Unlock()

		entries := make([]protocol.ScrollbackEntry, 0, len(stored))
		for _, event := range stored {
			entries = append(entries, protocol.ScrollbackEntry{Event: event.Event})
		}

		return entries, nil
	})
	f.sub.replies = func(context.Context, protocol.WindowTarget, int) ([]protocol.ReplyEntry, error) {
		if f.repliesGate != nil {
			<-f.repliesGate
		}

		return nil, nil
	}

	return f
}

func (f *fakeSession) Subscribe(_ context.Context, client protocol.Client, _ protocol.SubscribeOptions) (protocol.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	model := client.(*ModelClient)
	f.actor = model.instance
	f.sub.nick = model.instance.Nick()
	f.subscribes++

	return f.sub, nil
}

func (f *fakeSession) Handle(_ context.Context, _ protocol.Client, cmd protocol.Command) (protocol.Response, error) {
	if f.handleFn == nil {
		return protocol.Response{}, nil
	}

	return f.handleFn(cmd), nil
}

func (f *fakeSession) DisconnectDeadClient(_ context.Context, id protocol.ClientID, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.disconnects = append(f.disconnects, id)
}

func (f *fakeSession) disconnected() []protocol.ClientID {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]protocol.ClientID(nil), f.disconnects...)
}

func (f *fakeSession) subscribeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.subscribes
}

// dmReadsSoFar returns the DM thread reads the fake has answered.
func (f *fakeSession) dmReadsSoFar() []dmRead {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]dmRead(nil), f.dmReads...)
}

func (f *fakeSession) LoadChannelWindow(_ context.Context, name domain.ChannelName) (*domain.ChannelWindow, error) {
	if window, ok := f.windows[name]; ok {
		return window, nil
	}

	window := domain.NewChannelWindow(name, f.Now())
	if f.actor != nil {
		window.Members.Add(f.actor)
		f.actor.JoinChannel(name, f.Now())
	}

	return window, nil
}

type fakeModelDispatch struct {
	session *fakeSession
}

func (d fakeModelDispatch) Done(_ context.Context, event domain.ModelDispatchDone) {
	d.session.recordModelEvent(event)
}

func (f *fakeSession) BeginModelDispatch(
	_ context.Context,
	_ protocol.WindowGuard,
	_ protocol.WindowTarget,
	event domain.ModelDispatchStarted,
) protocol.ModelDispatch {
	f.recordModelEvent(event)

	return fakeModelDispatch{session: f}
}

func (f *fakeSession) EmitModelFailure(
	_ context.Context,
	_ protocol.WindowTarget,
	event domain.ModelUnavailableError,
) {
	f.recordModelEvent(event)
}

func (f *fakeSession) recordModelEvent(evt domain.ModelClientEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.emitted = append(f.emitted, evt)
}

// emittedEvents returns what the client has put on the bus so far.
func (f *fakeSession) emittedEvents() []domain.ProtocolEvent {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]domain.ProtocolEvent(nil), f.emitted...)
}

func (f *fakeSession) ResolveInstanceByID(_ context.Context, id domain.InstanceID) (domain.Nick, error) {
	inst, ok := f.instances[id]
	if !ok {
		return "", fmt.Errorf("no such instance %q", id)
	}

	return inst.Nick(), nil
}

// ClientCaps answers with whatever the test put in `caps`. The zero
// value is nil, which reads as the no-capabilities holder a model
// subscribed with no modes gets from the real session.
func (f *fakeSession) ClientCaps(protocol.ClientID) command.CapabilityHolder {
	if f.caps == nil {
		return command.NoCapabilities()
	}

	return f.caps
}

func (f *fakeSession) TracerProvider() trace.TracerProvider { return noop.NewTracerProvider() }

func (f *fakeSession) GetWindow(_ context.Context, name domain.ChannelName) (domain.Window, error) {
	return domain.NewChannelWindow(name, f.Now()), nil
}

func (f *fakeSession) ResolveNick(_ context.Context, nick domain.Nick) (domain.InstanceID, domain.Nick, error) {
	for id, inst := range f.instances {
		if domain.EqualNick(inst.Nick(), nick) {
			return id, inst.Nick(), nil
		}
	}

	return "", "", fmt.Errorf("no such nick %q", nick)
}

func (f *fakeSession) Now() time.Time { return time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC) }

type fakeSubscription struct {
	events     chan protocol.Delivery
	done       chan struct{}
	once       sync.Once
	activated  bool
	nick       domain.Nick
	scrollback func(context.Context, protocol.WindowTarget, int) ([]protocol.ScrollbackEntry, error)
	replies    func(context.Context, protocol.WindowTarget, int) ([]protocol.ReplyEntry, error)
}

func newFakeSubscription(scrollback func(context.Context, protocol.WindowTarget, int) ([]protocol.ScrollbackEntry, error)) *fakeSubscription {
	return &fakeSubscription{
		events:     make(chan protocol.Delivery, 64),
		done:       make(chan struct{}),
		scrollback: scrollback,
	}
}

func (s *fakeSubscription) Events() <-chan protocol.Delivery { return s.events }
func (s *fakeSubscription) Nick() domain.Nick                { return s.nick }
func (s *fakeSubscription) Done() <-chan struct{}            { return s.done }
func (s *fakeSubscription) Activate()                        { s.activated = true }
func (s *fakeSubscription) Scrollback(ctx context.Context, window protocol.WindowTarget, limit int) ([]protocol.ScrollbackEntry, error) {
	if s.scrollback == nil {
		return nil, nil
	}

	return s.scrollback(ctx, window, limit)
}
func (s *fakeSubscription) Replies(ctx context.Context, window protocol.WindowTarget, limit int) ([]protocol.ReplyEntry, error) {
	if s.replies == nil {
		return nil, nil
	}

	return s.replies(ctx, window, limit)
}
func (*fakeSubscription) DirectoryChannels(context.Context) ([]domain.ChannelDirectoryEntry, error) {
	return nil, nil
}
func (s *fakeSubscription) GuardWindow(_ context.Context, target protocol.WindowTarget) (protocol.WindowGuard, error) {
	var window protocol.WindowContext
	if channel, ok := protocol.ChannelWindowName(target); ok {
		window = testChannelContext(domain.NewChannelWindow(channel, time.Time{}))
	}
	if peer, ok := protocol.DirectWindowPeer(target); ok {
		window = testDirectContext(peer)
	}

	return validWindowGuard{window: window}, nil
}
func (s *fakeSubscription) GuardInvitation(_ context.Context, channel domain.ChannelName) (protocol.WindowGuard, error) {
	return validWindowGuard{window: testChannelContext(domain.NewChannelWindow(channel, time.Time{}))}, nil
}
func (s *fakeSubscription) Unsubscribe() { s.once.Do(func() { close(s.done) }) }

type validWindowGuard struct {
	window protocol.WindowContext
	err    error
}

func (validWindowGuard) Valid(context.Context) bool { return true }
func (validWindowGuard) RunWithAuthority(_ context.Context, operation func() error) error {
	return operation()
}
func (g validWindowGuard) Context(context.Context) (protocol.WindowContext, error) {
	return g.window, g.err
}
func (validWindowGuard) Send(
	ctx context.Context,
	client protocol.Client,
	cmd protocol.Command,
) (protocol.Response, error) {
	return client.Send(ctx, cmd)
}

type testWindowContext struct {
	target       protocol.WindowTarget
	channelState *protocol.ChannelState
	topic        *domain.TopicInfo
}

func (c testWindowContext) Target() protocol.WindowTarget { return c.target }
func (c testWindowContext) ChannelState() (protocol.ChannelState, bool) {
	if c.channelState == nil {
		return protocol.ChannelState{}, false
	}

	return *c.channelState, true
}
func (c testWindowContext) Topic() (domain.TopicInfo, bool) {
	if c.topic == nil {
		return domain.TopicInfo{}, false
	}

	return *c.topic, true
}

func testChannelContext(window *domain.ChannelWindow) protocol.WindowContext {
	context := testWindowContext{target: protocol.ChannelWindowTarget(window.Name())}
	if window.Topic != "" {
		setter := window.TopicSetBy
		if window.Modes.Anonymous {
			setter = domain.AnonymousNick
		}

		context.topic = &domain.TopicInfo{
			Target: window.Name(), Topic: window.Topic,
			TopicSetBy: setter, TopicSetAt: window.TopicSetAt,
		}
	}

	return context
}

func testDirectContext(peer domain.InstanceID) protocol.WindowContext {
	return testWindowContext{target: protocol.DirectWindowTarget(peer)}
}

func newTestModelClient(sess Session) *ModelClient {
	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)

	mc := New(Config{
		Instance:        inst,
		Attachment:      protocol.NewAttachment(),
		Session:         sess,
		APIClient:       func() api.Client { return nil },
		LifetimeContext: context.Background,
	})
	if fake, ok := sess.(*fakeSession); ok {
		mc.hist.bind(fake.sub)
	}

	return mc
}

// TestModelClient_Detach_waits_for_an_in_flight_Attach pins the
// atomicity an attach owes a detach. An attach that published its
// subscription, dropped the lock and only then registered its
// dispatch goroutine left a window over the history load: a `Detach`
// landing in it found an empty wait group, returned as though the
// goroutine had been joined, and the goroutine started afterwards.
//
// The fixture parks the attach inside its history load and asserts
// the detach cannot finish while it is parked. Once released, the
// attach reports that it did not establish a live connection.
func TestModelClient_Detach_waits_for_an_in_flight_Attach(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		sess.repliesGate = make(chan struct{})

		mc := newTestModelClient(sess)

		attached := make(chan error, 1)
		go func() { attached <- mc.Attach(t.Context()) }()

		// Let the attach reach its history load and park there.
		synctest.Wait()

		detached := make(chan struct{})
		go func() {
			defer close(detached)
			mc.Detach()
		}()

		// Everything that can run has run, so the detach has got as
		// far as it is going to while the attach is in flight.
		synctest.Wait()

		select {
		case <-detached:
			t.Fatal("Detach returned while the attach was still in flight")
		default:
		}

		close(sess.repliesGate)
		synctest.Wait()

		require.ErrorIs(t, <-attached, ErrReleased)
		<-detached
		require.False(t, sess.sub.activated)

		// The connection is over, so it cannot be taken up again.
		require.ErrorIs(t, mc.Attach(t.Context()), ErrReleased)
	})
}

// TestModelClient_Release_during_channel_history_load covers teardown
// after Attach publishes its subscription but before it has read every
// joined channel. Release clears the mutable client field, while the
// attach still owns the subscription returned by Subscribe.
func TestModelClient_Release_during_channel_history_load(t *testing.T) {
	sess := newFakeSession()
	firstRead := make(chan struct{})
	continueRead := make(chan struct{})

	var mu sync.Mutex
	var windows []domain.ChannelName
	sess.sub.scrollback = func(_ context.Context, target protocol.WindowTarget, _ int) ([]protocol.ScrollbackEntry, error) {
		window := protocol.WindowKey(target)
		mu.Lock()
		windows = append(windows, window)
		readCount := len(windows)
		mu.Unlock()

		if readCount == 1 {
			close(firstRead)
			<-continueRead
		}

		return nil, nil
	}

	mc := newTestModelClient(sess)
	mc.instance.JoinChannel("#one", sess.Now())
	mc.instance.JoinChannel("#two", sess.Now())

	attached := make(chan error, 1)
	go func() { attached <- mc.Attach(t.Context()) }()

	<-firstRead
	mc.Release()
	close(continueRead)

	require.ErrorIs(t, <-attached, ErrReleased)
	mc.Wait()
	require.False(t, sess.sub.activated)

	mu.Lock()
	gotWindows := append([]domain.ChannelName(nil), windows...)
	mu.Unlock()
	require.Equal(t, []domain.ChannelName{"#one", "#two"}, gotWindows)
}

func TestModelClient_Attach_refuses_incomplete_channel_history(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		failScrollback     bool
		failReplyRead      int
		wantScrollbackRead int
		wantReplyReads     int
	}{
		{
			name: "channel scrollback fails", failScrollback: true,
			wantScrollbackRead: 1,
		},
		{
			name: "channel replies fail", failReplyRead: 1,
			wantScrollbackRead: 1, wantReplyReads: 1,
		},
		{
			name: "global replies fail", failReplyRead: 2,
			wantScrollbackRead: 1, wantReplyReads: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sentinel := errors.New("history unavailable")
			sess := newFakeSession()
			scrollbackReads := 0
			replyReads := 0
			sess.sub.scrollback = func(
				context.Context,
				protocol.WindowTarget,
				int,
			) ([]protocol.ScrollbackEntry, error) {
				scrollbackReads++
				if tc.failScrollback {
					return nil, sentinel
				}

				return nil, nil
			}
			sess.sub.replies = func(context.Context, protocol.WindowTarget, int) ([]protocol.ReplyEntry, error) {
				replyReads++
				if replyReads == tc.failReplyRead {
					return nil, sentinel
				}

				return nil, nil
			}
			mc := newTestModelClient(sess)
			mc.instance.JoinChannel("#dev", sess.Now())
			t.Cleanup(mc.Detach)

			err := mc.Attach(t.Context())
			closed := false
			select {
			case <-sess.sub.Done():
				closed = true
			default:
			}

			type assertionSnapshot struct {
				HistoryError   bool
				Activated      bool
				Closed         bool
				ScrollbackRead int
				ReplyReads     int
			}

			require.Equal(t, assertionSnapshot{
				HistoryError:   true,
				Closed:         true,
				ScrollbackRead: tc.wantScrollbackRead,
				ReplyReads:     tc.wantReplyReads,
			}, assertionSnapshot{
				HistoryError:   errors.Is(err, sentinel),
				Activated:      sess.sub.activated,
				Closed:         closed,
				ScrollbackRead: scrollbackReads,
				ReplyReads:     replyReads,
			})
		})
	}
}

// TestModelClient_Attach_is_idempotent_and_final covers the two
// answers a repeat attach can give: nil while the client is
// connected, [ErrReleased] once it is not.
func TestModelClient_Attach_is_idempotent_and_final(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		mc := newTestModelClient(sess)

		require.NoError(t, mc.Attach(t.Context()))
		require.NoError(t, mc.Attach(t.Context()))
		require.Equal(t, 1, sess.subscribeCount())

		mc.Detach()

		require.ErrorIs(t, mc.Attach(t.Context()), ErrReleased)
		require.Equal(t, 1, sess.subscribeCount())
	})
}

func TestModelClient_InterruptTurn_keeps_the_event_loop_alive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		firstStarted := make(chan struct{})
		firstCancelled := make(chan struct{})
		secondCompleted := make(chan struct{})
		calls := 0
		fake := &apitest.Fake{
			SendEventsFn: func(
				ctx context.Context,
				_ domain.ModelID,
				_ domain.InstanceID,
				_ api.SystemPrompt,
				_ []protocol.IRCMessage,
				_ []protocol.IRCMessage,
			) (api.CompletionResult, error) {
				calls++
				if calls == 1 {
					close(firstStarted)
					<-ctx.Done()
					close(firstCancelled)

					return api.CompletionResult{}, ctx.Err()
				}

				close(secondCompleted)
				return api.CompletionResult{}, nil
			},
		}

		mc := newTestModelClient(sess)
		mc.apiFn = func() api.Client { return fake }
		require.NoError(t, mc.Attach(t.Context()))
		defer mc.Detach()

		sess.sub.events <- protocol.Delivery{Event: domain.Message{
			Source: domain.ClientSource("inst-alice", "alice"),
			Target: "#general", Body: "first", At: sess.Now(),
		}}
		<-firstStarted

		mc.InterruptTurn()
		<-firstCancelled

		sess.sub.events <- protocol.Delivery{Event: domain.Message{
			Source: domain.ClientSource("inst-alice", "alice"),
			Target: "#general", Body: "second", At: sess.Now(),
		}}
		<-secondCompleted

		mc.Detach()
	})
}

func TestModelClient_InterruptWindow_cancels_only_the_matching_provider_call(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		generalStarted := make(chan struct{})
		generalCancelled := make(chan error, 1)
		otherCompleted := make(chan struct{})
		fake := &apitest.Fake{
			SendEventsFn: func(
				ctx context.Context,
				_ domain.ModelID,
				_ domain.InstanceID,
				_ api.SystemPrompt,
				_ []protocol.IRCMessage,
				events []protocol.IRCMessage,
			) (api.CompletionResult, error) {
				switch events[len(events)-1].Target {
				case "#general":
					close(generalStarted)
					<-ctx.Done()
					generalCancelled <- ctx.Err()

					return api.CompletionResult{}, ctx.Err()
				case "#other":
					close(otherCompleted)

					return api.CompletionResult{}, nil
				default:
					return api.CompletionResult{}, fmt.Errorf("unexpected target %q", events[len(events)-1].Target)
				}
			},
		}

		mc := newTestModelClient(sess)
		mc.apiFn = func() api.Client { return fake }
		require.NoError(t, mc.Attach(t.Context()))

		sess.sub.events <- protocol.Delivery{Event: domain.Message{
			Source: domain.ClientSource("inst-alice", "alice"),
			Target: "#general", Body: "first", At: sess.Now(),
		}}
		<-generalStarted

		mc.InterruptWindow("#other")
		synctest.Wait()
		select {
		case err := <-generalCancelled:
			t.Fatalf("another window cancelled #general: %v", err)
		default:
		}

		mc.InterruptWindow("#general")
		require.ErrorIs(t, <-generalCancelled, context.Canceled)

		sess.sub.events <- protocol.Delivery{Event: domain.Message{
			Source: domain.ClientSource("inst-alice", "alice"),
			Target: "#other", Body: "second", At: sess.Now(),
		}}
		<-otherCompleted
		synctest.Wait()
		mc.Detach()
		require.Equal(t, []domain.ProtocolEvent{
			domain.ModelDispatchStarted{Source: domain.ClientSource(mc.instance.ID(), mc.instance.Nick()), At: sess.Now()},
			domain.ModelDispatchDone{Source: domain.ClientSource(mc.instance.ID(), mc.instance.Nick()), At: sess.Now()},
			domain.ModelDispatchStarted{Source: domain.ClientSource(mc.instance.ID(), mc.instance.Nick()), At: sess.Now()},
			domain.ModelDispatchDone{Source: domain.ClientSource(mc.instance.ID(), mc.instance.Nick()), At: sess.Now()},
		}, sess.emittedEvents())
	})
}

type blockingContinuationWaiter struct {
	started   chan struct{}
	cancelled chan error
}

func (w *blockingContinuationWaiter) Wait(ctx context.Context, _ time.Duration) error {
	close(w.started)
	<-ctx.Done()
	w.cancelled <- ctx.Err()

	return ctx.Err()
}

func TestModelClient_InterruptWindow_cancels_a_matching_continuation_wait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var effects []string
		tools := NewToolRegistry(ToolSpec{
			Definition: api.ToolDefinition{Name: "effect"},
			Execute: func(_ context.Context, _ ToolContext, _ json.RawMessage) (ToolResultPayload, error) {
				effects = append(effects, "effect applied")

				return ToolResultPayload{OK: true}, nil
			},
		})
		otherCompleted := make(chan struct{})
		upstream := &apitest.Fake{
			SendEventsFn: func(
				_ context.Context,
				_ domain.ModelID,
				_ domain.InstanceID,
				_ api.SystemPrompt,
				_ []protocol.IRCMessage,
				events []protocol.IRCMessage,
			) (api.CompletionResult, error) {
				if events[len(events)-1].Target == "#other" {
					close(otherCompleted)

					return api.CompletionResult{}, nil
				}

				return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{{
					ID: "effect-1", Name: "effect", Args: json.RawMessage(`{}`),
				}}}, nil
			},
			ContinueWithToolResultsFn: func(
				context.Context,
				*api.Conversation,
				[]api.ToolResult,
			) (api.CompletionResult, error) {
				return api.CompletionResult{}, context.DeadlineExceeded
			},
		}
		waiter := &blockingContinuationWaiter{
			started: make(chan struct{}), cancelled: make(chan error, 1),
		}
		sess := newFakeSession()
		mc := New(Config{
			Instance:        domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil),
			Attachment:      protocol.NewAttachment(),
			Session:         sess,
			APIClient:       func() api.Client { return upstream },
			Tools:           tools,
			LifetimeContext: context.Background,
		})
		mc.hist.bind(sess.sub)
		mc.retry = retryPolicy{Delay: time.Hour, Waiter: waiter}
		require.NoError(t, mc.Attach(t.Context()))
		defer mc.Detach()

		sess.sub.events <- protocol.Delivery{Event: domain.Message{
			Source: domain.ClientSource("inst-alice", "alice"),
			Target: "#general", Body: "first", At: sess.Now(),
		}}
		<-waiter.started

		mc.InterruptWindow("#general")
		require.ErrorIs(t, <-waiter.cancelled, context.Canceled)

		sess.sub.events <- protocol.Delivery{Event: domain.Message{
			Source: domain.ClientSource("inst-alice", "alice"),
			Target: "#other", Body: "second", At: sess.Now(),
		}}
		<-otherCompleted
		synctest.Wait()
		mc.Detach()
		require.Equal(t, []string{"effect applied"}, effects)
	})
}

// TestModelClient_dispatch_panic_disconnects_the_client pins the
// teardown a dead dispatch goroutine gets. Left registered, the
// subscription would go on collecting deliveries nobody reads and
// the instance would stay a member of channels it can no longer
// answer in; the disconnect is what puts the QUIT in the channel.
func TestModelClient_dispatch_panic_disconnects_the_client(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		mc := newTestModelClient(sess)
		mc.apiFn = func() api.Client { panic("upstream exploded") }
		window := domain.NewChannelWindow("#general", sess.Now())
		window.Members.Add(mc.instance)
		mc.instance.JoinChannel("#general", sess.Now())
		sess.windows = map[domain.ChannelName]*domain.ChannelWindow{"#general": window}

		require.NoError(t, mc.Attach(t.Context()))

		sess.sub.events <- protocol.Delivery{Event: domain.Message{
			Source: domain.ClientSource("inst-alice", "alice"), Target: "#general",
			Body: "hi", At: sess.Now(),
		}}

		synctest.Wait()
		mc.Wait()

		require.Equal(t, []protocol.ClientID{"inst-botty"}, sess.disconnected())
	})
}
