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

	// EventsBefore returns up to `n` channel events strictly
	// before `before` (most recent if nil), in chronological
	// order. Used at attach-time history seeding.
	EventsBefore(ctx context.Context, ch domain.ChannelName, before *int64, n int) ([]domain.StoredEvent, error)

	// DMEventsBefore returns up to `n` DM events strictly before
	// `before` between `self` and `peer`. Used at lazy DM history
	// seeding.
	DMEventsBefore(ctx context.Context, self, peer domain.InstanceID, before *int64, n int) ([]domain.StoredEvent, error)

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

	// TracerProvider returns the OTel tracer provider used for
	// modelclient-side spans.
	TracerProvider() trace.TracerProvider
}

// ModelClient is the [protocol.Client] backing a single LLM
// instance. Construct one per instance and call [ModelClient.Attach]
// to register it with a session; call [ModelClient.Detach] to
// release the subscription and join the dispatch goroutine.
type ModelClient struct {
	instance *domain.Instance
	sess     Session
	apiFn    func() api.Client
	memStore memory.Store
	tools    *ToolRegistry
	ensure   EnsureStructuredOutputModel
	pacer    *Pacer

	baseContext func() context.Context

	hist *history

	mu     sync.Mutex
	sub    protocol.Subscription
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
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
// `pacer` adds a typing delay before each chat-tool emit so bots
// don't fire at machine speed; a nil `pacer` disables pacing.
func New(
	inst *domain.Instance,
	sess Session,
	apiFn func() api.Client,
	memStore memory.Store,
	tools *ToolRegistry,
	ensure EnsureStructuredOutputModel,
	baseContext func() context.Context,
	pacer *Pacer,
) *ModelClient {
	if ensure == nil {
		ensure = noEnsure
	}
	return &ModelClient{
		instance:    inst,
		sess:        sess,
		apiFn:       apiFn,
		memStore:    memStore,
		tools:       tools,
		ensure:      ensure,
		pacer:       pacer,
		baseContext: baseContext,
		hist:        newHistory(),
	}
}

// Instance returns the canonical actor handle.
func (mc *ModelClient) Instance() *domain.Instance { return mc.instance }

// Identity reports the client's stable id, equal to the instance's
// id by construction.
func (mc *ModelClient) Identity() protocol.ClientID {
	return protocol.ClientID(mc.instance.ID())
}

// Send routes `cmd` through the session's dispatcher with this
// client as the issuing actor. Successful [domain.Message] events
// in `Response.Events` are filed into the model's rolling history
// buffer; the originator-suppression rule (RFC 2812 §3.3.1) keeps
// them off the bus.
func (mc *ModelClient) Send(ctx context.Context, cmd protocol.Command) (protocol.Response, error) {
	resp, err := mc.sess.Handle(ctx, mc, cmd)
	if err != nil || resp.Err != nil {
		return resp, err
	}

	for _, evt := range resp.Events {
		msg, ok := evt.(domain.Message)
		if !ok {
			continue
		}

		mc.hist.append(ctx, mc.sess, mc.instance.ID(), domain.StoredEvent{Event: msg}, msg.Target)
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

// Caps exposes a static capability holder reporting no
// capabilities. The chatcmd grammar's `caps:` filter therefore
// hides operator-gated tools from model invocations.
func (mc *ModelClient) Caps() command.CapabilityHolder { return modelCaps{} }

// Attach registers the client with its session, seeds per-channel
// history from the event log for every channel the instance is in,
// and starts the dispatch goroutine. Returns the registration
// error from [Session.Subscribe]; the client remains inert on
// failure.
//
// Attach is idempotent: a repeat call on an already-attached
// client returns nil.
func (mc *ModelClient) Attach(ctx context.Context) error {
	mc.mu.Lock()
	if mc.sub != nil {
		mc.mu.Unlock()
		return nil
	}

	sub, err := mc.sess.Subscribe(mc, protocol.SubscribeOptions{Instance: mc.instance})
	if err != nil {
		mc.mu.Unlock()
		return fmt.Errorf("attach model client %q: %w", mc.instance.ID(), err)
	}

	mc.sub = sub

	loopCtx, cancel := context.WithCancel(mc.baseContext())
	mc.ctx = loopCtx
	mc.cancel = cancel
	mc.mu.Unlock()

	mc.seedHistory(ctx)

	mc.wg.Go(func() {
		mc.runDispatchLoop(loopCtx, sub)
	})

	return nil
}

// Detach releases the subscription, cancels the dispatch
// goroutine's context, and joins it. Idempotent on an already-
// detached or never-attached client.
func (mc *ModelClient) Detach() {
	mc.mu.Lock()
	sub := mc.sub
	cancel := mc.cancel
	mc.sub = nil
	mc.cancel = nil
	mc.ctx = nil
	mc.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	if sub != nil {
		sub.Unsubscribe()
	}

	mc.wg.Wait()
}

// seedHistory eager-seeds the per-channel history buffer from the
// event log for every channel the instance is currently in. DM
// targets are not eager-seeded — they lazy-seed in [history.append]
// on first event arrival.
func (mc *ModelClient) seedHistory(ctx context.Context) {
	channels := mc.instance.Channels()
	if channels == nil {
		return
	}

	logger := slog.Default()

	for pair := channels.Oldest(); pair != nil; pair = pair.Next() {
		ch := pair.Key
		seed, err := mc.sess.EventsBefore(ctx, ch, nil, modelHistorySize)
		if err != nil {
			logger.ErrorContext(ctx, "seed model history",
				"component", "modelclient",
				"instance_id", mc.instance.ID(),
				"channel", ch,
				"error", err,
			)
			continue
		}

		mc.hist.seedChannel(ch, seed)
	}
}

// modelCaps is the no-capabilities holder returned by
// [ModelClient.Caps].
type modelCaps struct{}

func (modelCaps) Has(_ command.Capability) bool { return false }

// inSpan brackets fn with a span and result-recording on the
// session's tracer provider. The fallback error kind is
// [observability.ErrorKindStore] — most modelclient operations are
// persistence-backed. Sites that need to override (downstream
// dispatch failures, ensure-model classification) wrap their
// returned error with [errWithKind], which the classifier here
// unwraps.
func (mc *ModelClient) inSpan(
	ctx context.Context,
	op string,
	attrs []attribute.KeyValue,
	fn func(ctx context.Context, span trace.Span) error,
) error {
	return observability.SpanRunner{
		Tracer:         mc.sess.TracerProvider().Tracer("github.com/laney/modeloff/internal/modelclient"),
		DefaultErrKind: observability.ErrorKindStore,
		ClassifyError:  classifyModelclientError,
	}.Run(ctx, op, attrs, fn)
}

func classifyModelclientError(err error) string {
	if ke, ok := errors.AsType[*kindError](err); ok {
		return ke.kind
	}
	return ""
}

// kindError tags an error with an observability error kind so the
// span classifier can attach `AttrErrorKind` without every call site
// having to pass the kind through an auxiliary return value.
type kindError struct {
	kind string
	err  error
}

func (e *kindError) Error() string { return e.err.Error() }
func (e *kindError) Unwrap() error { return e.err }

// errWithKind annotates err with the given observability error kind.
// Returns nil when err is nil so it can wrap the tail of a return.
func errWithKind(err error, kind string) error {
	if err == nil {
		return nil
	}
	return &kindError{kind: kind, err: err}
}
