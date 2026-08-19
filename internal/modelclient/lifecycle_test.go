package modelclient

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/laney/modeloff/internal/api"
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

	mu          sync.Mutex
	sub         *fakeSubscription
	subscribes  int
	disconnects []protocol.ClientID
}

func newFakeSession() *fakeSession {
	return &fakeSession{sub: newFakeSubscription()}
}

func (f *fakeSession) Subscribe(protocol.Client, protocol.SubscribeOptions) (protocol.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.subscribes++

	return f.sub, nil
}

func (f *fakeSession) Handle(context.Context, protocol.Client, protocol.Command) (protocol.Response, error) {
	return protocol.Response{}, nil
}

func (f *fakeSession) Disconnect(_ context.Context, id protocol.ClientID, _ string) {
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

func (f *fakeSession) EventsBefore(context.Context, domain.ChannelName, *int64, int) ([]domain.StoredEvent, error) {
	return nil, nil
}

func (f *fakeSession) DMEventsBefore(context.Context, domain.InstanceID, domain.InstanceID, *int64, int) ([]domain.StoredEvent, error) {
	return nil, nil
}

func (f *fakeSession) InstanceRepliesBefore(context.Context, domain.InstanceID, *int64, int) ([]domain.StoredEvent, error) {
	if f.repliesGate != nil {
		<-f.repliesGate
	}

	return nil, nil
}

func (f *fakeSession) LoadChannelWindow(_ context.Context, name domain.ChannelName) (*domain.ChannelWindow, error) {
	return domain.NewChannelWindow(name, f.Now()), nil
}

func (f *fakeSession) Emit(context.Context, domain.ProtocolEvent) {}

func (f *fakeSession) ResolveInstanceByID(context.Context, domain.InstanceID) (*domain.Instance, error) {
	return nil, nil
}

func (f *fakeSession) LookupClient(protocol.ClientID) protocol.Client { return nil }

func (f *fakeSession) TracerProvider() trace.TracerProvider { return noop.NewTracerProvider() }

func (f *fakeSession) GetWindow(_ context.Context, name domain.ChannelName) (domain.Window, error) {
	return domain.NewChannelWindow(name, f.Now()), nil
}

func (f *fakeSession) ResolveNick(context.Context, domain.Nick) (*domain.Instance, error) {
	return nil, nil
}

func (f *fakeSession) Now() time.Time { return time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC) }

type fakeSubscription struct {
	events chan protocol.Delivery
	done   chan struct{}
	once   sync.Once
}

func newFakeSubscription() *fakeSubscription {
	return &fakeSubscription{
		events: make(chan protocol.Delivery, 64),
		done:   make(chan struct{}),
	}
}

func (s *fakeSubscription) Events() <-chan protocol.Delivery { return s.events }
func (s *fakeSubscription) Done() <-chan struct{}            { return s.done }
func (s *fakeSubscription) Unsubscribe()                     { s.once.Do(func() { close(s.done) }) }

func newTestModelClient(sess Session) *ModelClient {
	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)

	return New(inst, sess, func() api.Client { return nil }, nil, nil, nil, context.Background, nil)
}

// TestModelClient_Detach_waits_for_an_in_flight_Attach pins the
// atomicity an attach owes a detach. An attach that published its
// subscription, dropped the lock and only then registered its
// dispatch goroutine left a window over the history load: a `Detach`
// landing in it found an empty wait group, returned as though the
// goroutine had been joined, and the goroutine started afterwards.
//
// The fixture parks the attach inside its history load and asserts
// the detach cannot finish while it is parked.
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

		require.NoError(t, <-attached)
		<-detached

		// The connection is over, so it cannot be taken up again.
		require.ErrorIs(t, mc.Attach(t.Context()), ErrReleased)
	})
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

		require.NoError(t, mc.Attach(t.Context()))

		sess.sub.events <- protocol.Delivery{Event: domain.Message{
			Target:     "#general",
			From:       "alice",
			InstanceID: "inst-alice",
			Body:       "hi",
			At:         sess.Now(),
		}}

		synctest.Wait()
		mc.Wait()

		require.Equal(t, []protocol.ClientID{"inst-botty"}, sess.disconnected())
	})
}
