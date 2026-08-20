// Package modelclient holds the model-client implementation of
// [protocol.Client]. A model-client represents a single LLM
// instance participating in the session: it attaches itself to the
// session via [Session.Subscribe], holds the resulting
// [protocol.Subscription], drives its own dispatch goroutine, and
// acts as the actor for any commands the LLM issues during a
// dispatch turn.
package modelclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
)

// Session is the dependency surface a [ModelClient] needs from the
// session. The concrete `*session.Session` satisfies it implicitly.
// It embeds [SessionAPI] so the tool registry's [ToolContext.Session]
// can be populated from the same handle the dispatch loop holds.
type Session interface {
	SessionAPI

	// Subscribe registers the client with the session and returns
	// the per-client delivery handle.
	Subscribe(c protocol.Client, opts protocol.SubscribeOptions) (protocol.Subscription, error)

	// Handle is the wire dispatcher's entry point.
	Handle(ctx context.Context, c protocol.Client, cmd protocol.Command) (protocol.Response, error)

	// Disconnect ends the named client's connection server-side: the
	// QUIT carrying `reason` is broadcast to the channels it was in,
	// its subscription is reaped and its model-client released. It
	// is how the server closes a connection it can no longer serve,
	// leaving nothing half-present behind it.
	Disconnect(ctx context.Context, id protocol.ClientID, reason string)

	// EventsBefore returns up to `n` channel events strictly
	// before `before` (most recent if nil), in chronological
	// order. Used at attach-time history seeding.
	EventsBefore(ctx context.Context, ch domain.ChannelName, before *int64, n int) ([]domain.StoredEvent, error)

	// DMEventsBefore returns up to `n` DM events strictly before
	// `before` between `self` and `peer`. Used at lazy DM history
	// seeding.
	DMEventsBefore(ctx context.Context, self, peer domain.InstanceID, before *int64, n int) ([]domain.StoredEvent, error)

	// InstanceRepliesBefore returns up to `n` of the instance's own
	// point-to-point replies (WHOIS, LIST) strictly before `before`,
	// in chronological order. These are the instance's private memory
	// of replies it received, merged into its prompt transcript.
	InstanceRepliesBefore(ctx context.Context, id domain.InstanceID, before *int64, n int) ([]domain.StoredEvent, error)

	// LoadChannelWindow loads the addressable `*ChannelWindow` row
	// the prompt-assembly and instance-resolution paths use.
	LoadChannelWindow(ctx context.Context, name domain.ChannelName) (*domain.ChannelWindow, error)

	// Emit fans out a [domain.ProtocolEvent] on the per-subscription
	// bus.
	Emit(ctx context.Context, evt domain.ProtocolEvent)

	// ResolveInstanceByID returns the canonical `*domain.Instance`
	// for the given id.
	ResolveInstanceByID(ctx context.Context, id domain.InstanceID) (*domain.Instance, error)

	// LookupClient returns the registered [protocol.Client] for
	// the given identity, or nil if none is registered.
	LookupClient(id protocol.ClientID) protocol.Client

	// ClientCaps reports the capabilities granted to a registered
	// subscription. [ModelClient.Caps] delegates to it, so the tool
	// registry is filtered by the modes the session holds.
	ClientCaps(id protocol.ClientID) command.CapabilityHolder

	// TracerProvider returns the OTel tracer provider used for
	// modelclient-side spans.
	TracerProvider() trace.TracerProvider
}

// ModelClient is the [protocol.Client] backing a single LLM
// instance. Construct one per instance and call [ModelClient.Attach]
// to register it with a session; call [ModelClient.Release] to end
// its connection and [ModelClient.Wait] to join the dispatch
// goroutine afterwards.
//
// Ending a connection is two phases because the model can end its
// own: a `quit` tool call runs on the dispatch goroutine, so the
// QUIT handler reaches this client from inside the very goroutine a
// join would wait for. `Release` is the phase that is safe there;
// `Wait` is the phase that is not, and belongs to whoever owns the
// client's lifetime.
type ModelClient struct {
	instance     *domain.Instance
	sess         Session
	apiFn        func() api.Client
	memStore     memory.Store
	tools        *ToolRegistry
	ensure       EnsureStructuredOutputModel
	contextLenFn func(domain.ModelID) int
	pacer        *Pacer

	baseContext func() context.Context

	hist *history

	// mu guards the subscription handle and the released flag.
	mu       sync.Mutex
	sub      protocol.Subscription
	cancel   context.CancelFunc
	released bool
	wg       sync.WaitGroup
}

// New returns an unattached `ModelClient` for `inst`. The client is
// inert until [ModelClient.Attach] runs.
//
// `apiFn` is consulted once per dispatch turn to obtain the current
// [api.Client], so a manager-driven `SetAPIKey` rebuild propagates
// to the next turn without reattach. A nil return from `apiFn` is
// the same signal as "no API key configured" — the dispatch turn
// short-circuits to silence.
//
// `baseContext` supplies the long-lived context the dispatch
// goroutine derives its lifetime from; cancelling it (and calling
// [ModelClient.Detach]) is how the goroutine is woken at shutdown.
//
// `contextLenFn` reports the live catalogue-cached context length
// for a model id. It is consulted at the top of every dispatch burst
// (see [ModelClient.runDispatchLoop]), so the transcript token
// budget stays current with whatever the catalogue holds across the
// client's whole lifetime. A zero return — including from a nil
// `contextLenFn` — disables the budget for that burst, leaving
// [modelHistorySize]'s event-count ring as the only bound.
//
// `pacer` adds a typing delay before each chat-tool emit so bots
// don't fire at machine speed; a nil `pacer` disables pacing.
func New(
	inst *domain.Instance,
	sess Session,
	apiFn func() api.Client,
	memStore memory.Store,
	tools *ToolRegistry,
	ensure EnsureStructuredOutputModel,
	contextLenFn func(domain.ModelID) int,
	baseContext func() context.Context,
	pacer *Pacer,
) *ModelClient {
	if ensure == nil {
		ensure = noEnsure
	}
	if contextLenFn == nil {
		contextLenFn = noContextLen
	}
	return &ModelClient{
		instance:     inst,
		sess:         sess,
		apiFn:        apiFn,
		memStore:     memStore,
		tools:        tools,
		ensure:       ensure,
		contextLenFn: contextLenFn,
		pacer:        pacer,
		baseContext:  baseContext,
		hist:         newHistory(),
	}
}

// noContextLen is the permissive default consulted when a
// [ModelClient] is constructed without a real catalogue lookup.
// Reporting 0 (unknown) for every model id disables the transcript
// token budget, leaving [modelHistorySize]'s event-count ring as the
// only bound — the same behaviour the codebase had before the budget
// existed.
func noContextLen(domain.ModelID) int { return 0 }

// Instance returns the canonical actor handle.
func (mc *ModelClient) Instance() *domain.Instance { return mc.instance }

// Identity reports the client's stable id, equal to the instance's
// id by construction.
func (mc *ModelClient) Identity() protocol.ClientID {
	return protocol.ClientID(mc.instance.ID())
}

// Send routes `cmd` through the session's dispatcher with this
// client as the issuing actor and files the dispatcher's synchronous
// reply events into the model's local memory:
//
//   - [domain.Message] events go to the rolling buffer for the window
//     [domain.Message.RoutingKey] places them in, which is the same
//     buffer the incoming half of a DM lands in; the
//     originator-suppression rule (RFC 2812 §3.3.1) keeps them off
//     the bus, so this is the only path that feeds the model its own
//     chat traffic.
//   - the model's own point-to-point replies ([domain.Whois],
//     [domain.ListReply], and the [domain.SystemNotice] a refused
//     INVITE answers with) go to the private replies ring. These are
//     exactly the [domain.IssuerReply] events the dispatcher persists
//     to the instance-reply log, so the local ring stays in step with
//     the log it loads at attach: a model meets its own refusal on
//     the turn that caused it, not only after a reattach. The wire-
//     terminator [domain.ListEnd] carries no transcript line and the
//     dispatcher does not persist it, so it is not filed.
func (mc *ModelClient) Send(ctx context.Context, cmd protocol.Command) (protocol.Response, error) {
	resp, err := mc.sess.Handle(ctx, mc, cmd)
	if err != nil || resp.Err != nil {
		return resp, err
	}

	selfID := mc.instance.ID()

	for _, evt := range resp.Events {
		switch e := evt.(type) {
		case domain.Message:
			key, ok := e.RoutingKey(selfID)
			if !ok {
				continue
			}

			mc.hist.append(ctx, mc.sess, selfID, domain.StoredEvent{Event: e}, key)
		case domain.Whois, domain.ListReply, domain.SystemNotice:
			mc.hist.appendReply(domain.StoredEvent{Event: e.(domain.PersistableEvent)})
		}
	}

	return resp, nil
}

// Events returns the per-subscription delivery stream, or nil if
// the client has not been attached.
func (mc *ModelClient) Events() <-chan protocol.Delivery {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	if mc.sub == nil {
		return nil
	}

	return mc.sub.Events()
}

// Caps reports the capabilities the session has granted this
// client's subscription, which is what the chatcmd grammar's
// `caps:` filter hides operator-gated tools by. A model is
// subscribed with no modes, so today it holds none. The answer comes
// from the session on every question, so a client the server elevates
// is offered what it may actually use.
func (mc *ModelClient) Caps() command.CapabilityHolder {
	return protocol.LiveCaps(mc.sess, mc.Identity())
}

// ErrReleased is returned by [ModelClient.Attach] for a client whose
// connection has already ended. A released client is spent: QUIT and
// KILL end a client for good, and the instance behind it is deleted,
// so a fresh connection means a fresh `ModelClient`.
var ErrReleased = errors.New("modelclient: client has been released")

// Attach registers the client with its session, loads its local
// memory (the join-scoped per-channel transcript and its own private
// replies) from the persisted logs, and starts the dispatch
// goroutine. Returns the registration error from [Session.Subscribe];
// the client remains inert on failure.
//
// Attach is idempotent: a repeat call on an already-attached
// client returns nil. It returns [ErrReleased] once the client's
// connection has ended.
func (mc *ModelClient) Attach(ctx context.Context) error {
	mc.mu.Lock()

	if mc.released {
		mc.mu.Unlock()
		return fmt.Errorf("attach model client %q: %w", mc.instance.ID(), ErrReleased)
	}

	if mc.sub != nil {
		mc.mu.Unlock()
		return nil
	}

	sub, err := mc.sess.Subscribe(mc, protocol.SubscribeOptions{Instance: mc.instance})
	if err != nil {
		mc.mu.Unlock()
		return fmt.Errorf("attach model client %q: %w", mc.instance.ID(), err)
	}

	loopCtx, cancel := context.WithCancel(mc.baseContext())

	mc.sub = sub
	mc.cancel = cancel

	// The dispatch goroutine joins the wait group before the lock is
	// released, so a `Release` landing during the history load below
	// is followed by a `Wait` that joins this goroutine. Registering
	// it after the load would leave that window with an empty group,
	// and a `Wait` in it would report a join that had not happened.
	//
	// It then waits for the load, because a loaded event must never
	// reach the model as a trigger: the loop reads its first
	// delivery only once the history it is prompted from is in
	// place. The close is deferred so the gate opens however this
	// call returns — a load that panics releases the goroutine on
	// the way out, where leaving it parked would hang every later
	// `Wait`, shutdown's included.
	loaded := make(chan struct{})
	defer close(loaded)

	mc.wg.Go(func() {
		<-loaded

		mc.runDispatchLoop(loopCtx, sub)
	})

	mc.mu.Unlock()

	mc.loadHistory(ctx)

	return nil
}

// Release ends the client's connection: it cancels the dispatch
// goroutine's context and unsubscribes from the session. It does not
// wait for the goroutine to finish, so it is safe from any goroutine
// — including the dispatch goroutine itself, which is where a model
// that calls the `quit` tool issues its own QUIT from. Idempotent on
// an already-released or never-attached client.
func (mc *ModelClient) Release() {
	mc.mu.Lock()
	sub := mc.sub
	cancel := mc.cancel
	mc.sub = nil
	mc.cancel = nil
	mc.released = true
	mc.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	if sub != nil {
		sub.Unsubscribe()
	}
}

// Wait blocks until the dispatch goroutine has exited. Call it only
// from a goroutine that owns the client's lifetime — shutdown, or a
// test's cleanup. Calling it from the dispatch goroutine would be
// that goroutine waiting on itself.
func (mc *ModelClient) Wait() {
	mc.wg.Wait()
}

// Detach releases the connection and joins the dispatch goroutine.
// It carries `Wait`'s restriction: never call it from the dispatch
// path.
func (mc *ModelClient) Detach() {
	mc.Release()
	mc.Wait()
}

// loadHistory loads both of the model's local memories at attach: the
// per-channel shared transcript and the model's own private replies.
//
// Each channel buffer is join-scoped: only events at or after the
// instance's recorded join time are kept, and a channel with a
// zero/unknown join time loads nothing. Reaction to history is
// avoided purely by order of operations — this load runs before the
// dispatch loop starts, so loaded events are never delivered as
// triggers. DM targets are not loaded here; they lazy-seed in
// [history.append] on first event arrival.
func (mc *ModelClient) loadHistory(ctx context.Context) {
	logger := slog.Default()

	if channels := mc.instance.Channels(); channels != nil {
		for pair := channels.Oldest(); pair != nil; pair = pair.Next() {
			ch, joinedAt := pair.Key, pair.Value
			if joinedAt.IsZero() {
				continue
			}

			// The channel log records activity in arrival order, so
			// this single bounded read returns the n most-recent
			// rows. Command replies route to the per-instance reply
			// log and notices render transiently, so the rows kept at
			// or after the join time are the model's join-scoped view.
			seed, err := mc.sess.EventsBefore(ctx, ch, nil, modelHistorySize)
			if err != nil {
				logger.ErrorContext(ctx, "load model channel history",
					"component", "modelclient",
					"instance_id", mc.instance.ID(),
					"channel", ch,
					"error", err,
				)
				continue
			}

			kept := seed[:0:0]
			for _, se := range seed {
				if domain.EventTime(se.Event).Before(joinedAt) {
					continue
				}
				kept = append(kept, se)
			}

			mc.hist.seedChannel(ch, kept)
		}
	}

	replies, err := mc.sess.InstanceRepliesBefore(ctx, mc.instance.ID(), nil, modelHistorySize)
	if err != nil {
		logger.ErrorContext(ctx, "load model replies",
			"component", "modelclient",
			"instance_id", mc.instance.ID(),
			"error", err,
		)
		return
	}

	mc.hist.seedReplies(replies)
}

// inSpan brackets fn with a span and result-recording on the
// session's tracer provider. The fallback error kind is
// [observability.ErrorKindStore] — most modelclient operations are
// persistence-backed. Sites that need to override (downstream
// dispatch failures, ensure-model classification) wrap their
// returned error with [observability.ErrWithKind], which the
// classifier here unwraps.
func (mc *ModelClient) inSpan(
	ctx context.Context,
	op string,
	attrs []attribute.KeyValue,
	fn func(ctx context.Context, span trace.Span) error,
) error {
	return observability.SpanRunner{
		Tracer:         mc.sess.TracerProvider().Tracer("github.com/laney/modeloff/internal/modelclient"),
		DefaultErrKind: observability.ErrorKindStore,
		ClassifyError:  observability.ErrorKindOf,
	}.Run(ctx, op, attrs, fn)
}
