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
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

// Session is the dependency surface a [ModelClient] needs from the
// session. The concrete `*session.Session` satisfies it implicitly.
// It embeds [SessionAPI] so the tool registry's [ToolContext.Session]
// can be populated from the same handle the dispatch loop holds.
type Session interface {
	SessionAPI

	// Subscribe registers the client with the session and returns
	// the per-client delivery handle.
	Subscribe(ctx context.Context, c protocol.Client, opts protocol.SubscribeOptions) (protocol.Subscription, error)

	// Handle is the wire dispatcher's entry point.
	Handle(ctx context.Context, c protocol.Client, cmd protocol.Command) (protocol.Response, error)

	// Disconnect ends the named client's connection server-side: the
	// QUIT carrying `reason` is broadcast to the channels it was in,
	// its subscription is reaped and its model-client released. It
	// is how the server closes a connection it can no longer serve,
	// leaving nothing half-present behind it.
	DisconnectDeadClient(ctx context.Context, id protocol.ClientID, reason string)

	// BeginModelDispatch reports the start of a turn and captures the
	// recipients that must receive its completion.
	BeginModelDispatch(
		ctx context.Context,
		guard protocol.WindowGuard,
		window protocol.WindowTarget,
		event domain.ModelDispatchStarted,
	) protocol.ModelDispatch

	// EmitModelFailure reports a failed turn to the operator bus.
	EmitModelFailure(
		ctx context.Context,
		window protocol.WindowTarget,
		event domain.ModelUnavailableError,
	)

	// ClientCaps reports the capabilities granted to a registered
	// subscription. [ModelClient.Caps] delegates to it, so the tool
	// registry is filtered by the modes the session holds.
	ClientCaps(id protocol.ClientID) command.CapabilityHolder

	// ResolveInstanceByID returns the connected client's current nick.
	// Provider projection uses it to render a stable DM target as IRC
	// presentation state without storing that state in the window.
	ResolveInstanceByID(ctx context.Context, id domain.InstanceID) (domain.Nick, error)

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
	attachment   *protocol.Attachment
	sess         Session
	apiFn        func() api.Client
	memStore     memory.Store
	tools        *ToolRegistry
	ensure       EnsureStructuredOutputModel
	contextLenFn func(domain.ModelID) int
	pacer        *Pacer
	journal      TurnJournal
	contexts     ContextStore
	reflections  ReflectionInbox
	now          func() time.Time

	dispatchContext context.Context
	journalQueue    *journalQueue

	hist *history

	// retry is how long a turn lost to a transient upstream failure
	// waits before its single re-dispatch, and redispatch is how that
	// turn reaches the dispatch loop's select. The channel is
	// unbuffered: a scheduler that finds the loop mid-turn waits, and
	// gives up if the client is torn down first. Whether the loop
	// still wants the batch it hands back is decided there, against
	// the loop's own [redispatchSet].
	retry      retryPolicy
	redispatch chan *turnBatch

	// mu guards the subscription handle, lifetime cancellation, active
	// turn cancellation and the released flag.
	mu         sync.Mutex
	sub        protocol.Subscription
	cancel     context.CancelFunc
	activeTurn *activeTurn
	released   bool
	wg         sync.WaitGroup
}

type activeTurn struct {
	window domain.ChannelName
	cancel context.CancelFunc
}

// TurnJournal is the actor-bound model-turn surface used by a
// ModelClient.
type TurnJournal interface {
	BeginModelTurn(
		ctx context.Context,
		guard protocol.WindowGuard,
		turn store.ModelTurn,
		input store.ModelTurnEntry,
	) (store.ModelTurnRecorder, error)
}

// ContextStore is the actor-bound summary surface used by a
// ModelClient.
type ContextStore interface {
	ContextSummaries(
		ctx context.Context,
		guard protocol.WindowGuard,
	) ([]store.ContextSummary, error)
	CommitContextSummary(
		ctx context.Context,
		guard protocol.WindowGuard,
		update store.ContextSummaryUpdate,
	) (store.ContextSummary, error)
}

// ReflectionInbox is the durable candidate stream used by persona reflection.
type ReflectionInbox interface {
	AppendReflectionEvents(
		ctx context.Context,
		instanceID domain.InstanceID,
		candidates []store.ReflectionEventCandidate,
		createdAt time.Time,
	) error
}

// Config contains the lifetime dependencies of a [ModelClient].
type Config struct {
	Instance        *domain.Instance
	Attachment      *protocol.Attachment
	Session         Session
	APIClient       func() api.Client
	Memory          memory.Store
	Tools           *ToolRegistry
	EnsureModel     EnsureStructuredOutputModel
	ContextLen      func(domain.ModelID) int
	LifetimeContext func() context.Context
	JournalContext  context.Context
	Pacer           *Pacer
	Journal         TurnJournal
	Contexts        ContextStore
	Reflections     ReflectionInbox
	Now             func() time.Time
}

// New returns an unattached `ModelClient` for cfg.Instance. The client is
// inert until [ModelClient.Attach] runs.
//
// Config.APIClient is consulted once per dispatch turn to obtain the current
// [api.Client], so a manager-driven `SetAPIKey` rebuild propagates
// to the next turn without reattach. A nil return means
// no API key is configured: the turn ends without calling upstream,
// raising a [domain.ModelUnavailableError] so the user is told why
// their models have gone quiet.
//
// Config.LifetimeContext supplies the long-lived context the dispatch
// goroutine derives its lifetime from; cancelling it (and calling
// [ModelClient.Detach]) is how the goroutine is woken at shutdown.
//
// Config.JournalContext bounds evidence writes separately from dispatch.
// A manager keeps it alive while cancelled turns unwind, then cancels it
// before the store can close.
//
// Config.ContextLen reports the live catalogue-cached context length
// for a model id. Each turn consults it after refreshing the model
// catalogue, so the request budget uses the current provider value.
// A zero return disables the budget for that turn, leaving
// [modelHistorySize]'s event-count ring as the only bound, and a nil
// function returns zero for every model.
//
// Config.Pacer adds a typing delay before each chat-tool emit so bots
// don't fire at machine speed; a nil value disables pacing.
func New(cfg Config) *ModelClient {
	if cfg.EnsureModel == nil {
		cfg.EnsureModel = noEnsure
	}
	if cfg.ContextLen == nil {
		cfg.ContextLen = noContextLen
	}
	dispatchContext := context.Background()
	if cfg.LifetimeContext != nil {
		if lifetimeContext := cfg.LifetimeContext(); lifetimeContext != nil {
			dispatchContext = lifetimeContext
		}
	}
	if cfg.JournalContext == nil {
		cfg.JournalContext = dispatchContext
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	mc := &ModelClient{
		instance:        cfg.Instance,
		attachment:      cfg.Attachment,
		sess:            cfg.Session,
		apiFn:           cfg.APIClient,
		memStore:        cfg.Memory,
		tools:           cfg.Tools,
		ensure:          cfg.EnsureModel,
		contextLenFn:    cfg.ContextLen,
		pacer:           cfg.Pacer,
		journal:         cfg.Journal,
		contexts:        cfg.Contexts,
		reflections:     cfg.Reflections,
		now:             cfg.Now,
		dispatchContext: dispatchContext,
		journalQueue:    newJournalQueue(cfg.JournalContext),
		hist:            newHistory(),
		retry:           defaultRetryPolicy(),
		redispatch:      make(chan *turnBatch),
	}
	mc.hist.observeSeed = mc.recordReflectionEntries

	return mc
}

// noContextLen is the permissive default consulted when a
// [ModelClient] is constructed without a real catalogue lookup.
// Reporting 0 (unknown) for every model id disables the transcript
// token budget, leaving [modelHistorySize]'s event-count ring as the
// only bound — the same behaviour the codebase had before the budget
// existed.
func noContextLen(domain.ModelID) int { return 0 }

// Identity reports the client's stable id, equal to the instance's
// id by construction.
func (mc *ModelClient) Identity() protocol.ClientID {
	return protocol.ClientID(mc.instance.ID())
}

// Send routes `cmd` through the session's dispatcher with this
// client as the issuing actor. It files the model's synchronous
// point-to-point replies ([domain.Whois], [domain.ListReply],
// [domain.TopicInfo], and the [domain.SystemNotice] a refused INVITE
// answers with) in the private replies ring. These are exactly the
// [domain.IssuerReply] events the dispatcher persists to the
// instance-reply log. The local ring stays in step with the log it
// loads at attach. A model therefore meets its own refusal during the
// turn that caused it and after a reattach. The wire terminator
// [domain.ListEnd] carries no transcript line and the dispatcher does
// not persist it, so it is not filed.
func (mc *ModelClient) Send(ctx context.Context, cmd protocol.Command) (protocol.Response, error) {
	resp, err := mc.sess.Handle(ctx, mc, cmd)
	if err != nil {
		return resp, err
	}

	for _, evt := range resp.Events {
		switch e := evt.(type) {
		case domain.Whois, domain.ListReply, domain.TopicInfo, domain.Inviting, domain.SystemNotice:
			mc.hist.appendReply(protocol.CommandWindow(cmd), domain.StoredEvent{Event: e.(domain.PersistableEvent)})
		}
	}

	return resp, nil
}

// nick returns the nick the server currently holds for this client.
// A NICK rename is server state, and the subscription is where the
// client reads it back. A released client has no subscription left
// to ask and answers with the nick it attached under.
func (mc *ModelClient) nick() domain.Nick {
	mc.mu.Lock()
	sub := mc.sub
	mc.mu.Unlock()

	if sub == nil {
		return mc.instance.Nick()
	}

	return sub.Nick()
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
var ErrReleased = fmt.Errorf(
	"modelclient: client has been released: %w",
	protocol.ErrSubscriptionClosed,
)

// Attach registers the client with its session, loads its local
// memory (the join-scoped per-channel transcript and its own private
// replies) from the persisted logs, and starts the dispatch
// goroutine. Returns the registration error from [Session.Subscribe],
// a history-load error, or [ErrReleased] if teardown wins while
// history is loading. The client remains inert on failure.
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

	sub, err := mc.sess.Subscribe(ctx, mc, protocol.SubscribeOptions{
		Attachment:    mc.attachment,
		ReplayHistory: true,
	})
	if err != nil {
		mc.mu.Unlock()
		return fmt.Errorf("attach model client %q: %w", mc.instance.ID(), err)
	}

	loopCtx, cancel := context.WithCancel(mc.dispatchContext)

	mc.sub = sub
	mc.cancel = cancel
	mc.hist.bind(sub)

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
	var loadGate sync.Once
	finishLoad := func() {
		loadGate.Do(func() { close(loaded) })
	}
	defer finishLoad()

	mc.wg.Go(func() {
		<-loaded

		mc.runDispatchLoop(loopCtx, sub)
	})

	mc.mu.Unlock()

	historyErr := mc.loadHistory(ctx, sub)

	mc.mu.Lock()
	if mc.released || mc.sub != sub {
		mc.mu.Unlock()
		return fmt.Errorf("attach model client %q: %w", mc.instance.ID(), ErrReleased)
	}
	if historyErr != nil {
		mc.sub = nil
		mc.cancel = nil
		mc.released = true
		mc.mu.Unlock()

		cancel()
		sub.Unsubscribe()
		finishLoad()
		mc.Wait()

		return fmt.Errorf("attach model client %q: load history: %w", mc.instance.ID(), historyErr)
	}

	sub.Activate()
	mc.mu.Unlock()

	mc.mu.Lock()
	released := mc.released
	mc.mu.Unlock()
	if released {
		return fmt.Errorf("attach model client %q: %w", mc.instance.ID(), ErrReleased)
	}

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
	turn := mc.activeTurn
	mc.sub = nil
	mc.cancel = nil
	mc.activeTurn = nil
	mc.released = true
	mc.mu.Unlock()

	if turn != nil {
		turn.cancel()
	}
	if cancel != nil {
		cancel()
	}

	if sub != nil {
		sub.Unsubscribe()
	}
}

// InterruptTurn cancels the current upstream turn without ending the
// dispatch loop. Connection teardown uses it after revoking command
// authority so the loop can resume consuming its accepted terminal
// delivery prefix before [ModelClient.Release] closes the subscription.
func (mc *ModelClient) InterruptTurn() {
	mc.mu.Lock()
	turn := mc.activeTurn
	mc.mu.Unlock()

	if turn != nil {
		turn.cancel()
	}
}

// InterruptWindow cancels an upstream turn only when it belongs to
// `window`. The dispatch loop remains attached and can continue with
// traffic from the model's other windows.
func (mc *ModelClient) InterruptWindow(window domain.ChannelName) {
	mc.mu.Lock()
	turn := mc.activeTurn
	mc.mu.Unlock()

	if turn != nil && turn.window == window {
		turn.cancel()
	}
}

// Wait blocks until the dispatch goroutine has exited and every
// journal entry that goroutine produced has been written. Call it
// only from a goroutine that owns the client's lifetime: shutdown, or
// a test's cleanup. Calling it from the dispatch goroutine would be
// that goroutine waiting on itself.
//
// The dispatch goroutine is the journal queue's only producer, which
// is why the queue is closed after that goroutine is joined and not
// by [ModelClient.Release]: a turn unwinding after its release still
// records how it ended.
func (mc *ModelClient) Wait() {
	mc.wg.Wait()
	mc.journalQueue.stop()
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
// Each channel buffer comes from the actor-bound subscription. The
// session checks current membership and starts the result at the
// latest matching JOIN row. Reaction to history is avoided purely by
// order of operations: this load runs before the dispatch loop starts,
// so loaded events are never delivered as triggers. DM targets are not
// loaded here; they lazy-seed in [history.snapshot] on the first turn.
func (mc *ModelClient) loadHistory(ctx context.Context, sub protocol.Subscription) error {
	var reflectionSeed []protocol.ScrollbackEntry
	if channels := mc.instance.Channels(); channels != nil {
		for pair := channels.Oldest(); pair != nil; pair = pair.Next() {
			ch := pair.Key

			seed, err := sub.Scrollback(ctx, protocol.ChannelWindowTarget(ch), modelHistorySize)
			if err != nil {
				return fmt.Errorf("load channel %q scrollback: %w", ch, err)
			}

			replies, err := sub.Replies(ctx, protocol.ChannelWindowTarget(ch), modelHistorySize)
			if err != nil {
				return fmt.Errorf("load channel %q replies: %w", ch, err)
			}

			reflectionSeed = append(reflectionSeed, seed...)
			mc.hist.seedChannel(ch, storedScrollback(seed))
			mc.hist.seedReplies(protocol.ChannelWindowTarget(ch), replies)
		}
	}

	replies, err := sub.Replies(ctx, nil, modelHistorySize)
	if err != nil {
		return fmt.Errorf("load model replies: %w", err)
	}

	mc.hist.seedReplies(nil, replies)
	slices.SortStableFunc(reflectionSeed, func(a, b protocol.ScrollbackEntry) int {
		return domain.EventTime(a.Event).Compare(domain.EventTime(b.Event))
	})
	mc.recordReflectionEntries(ctx, reflectionSeed)

	return nil
}

func storedScrollback(entries []protocol.ScrollbackEntry) []domain.StoredEvent {
	stored := make([]domain.StoredEvent, 0, len(entries))
	for _, entry := range entries {
		stored = append(stored, domain.StoredEvent{Event: entry.Event})
	}

	return stored
}

func (mc *ModelClient) recordReflectionEntries(
	ctx context.Context,
	entries []protocol.ScrollbackEntry,
) {
	if mc.reflections == nil {
		return
	}

	candidates := make([]store.ReflectionEventCandidate, 0, len(entries))
	for _, entry := range entries {
		candidate, ok := mc.reflectionCandidate(entry.Event, entry.History)
		if ok {
			candidates = append(candidates, candidate)
		}
	}
	mc.appendReflectionCandidates(ctx, candidates)
}

func (mc *ModelClient) recordReflectionDeliveries(
	ctx context.Context,
	deliveries []protocol.Delivery,
) {
	if mc.reflections == nil {
		return
	}

	var candidates []store.ReflectionEventCandidate
	for _, delivery := range deliveries {
		event, ok := delivery.Event.(domain.PersistableEvent)
		if !ok {
			continue
		}
		for _, history := range delivery.History {
			candidate, ok := mc.reflectionCandidate(event, history)
			if ok {
				candidates = append(candidates, candidate)
			}
		}
	}
	mc.appendReflectionCandidates(ctx, candidates)
}

func (mc *ModelClient) reflectionCandidate(
	event domain.PersistableEvent,
	history protocol.HistoryRef,
) (store.ReflectionEventCandidate, bool) {
	if history.ID == 0 || history.Window == nil {
		return store.ReflectionEventCandidate{}, false
	}
	message, ok := protocol.FromChannelEvent(event)
	if !ok {
		return store.ReflectionEventCandidate{}, false
	}
	activity, substantive := event.(domain.Message)
	substantive = substantive && !activity.AuthoredBy(mc.instance.ID())

	return store.ReflectionEventCandidate{
		Source: history, Message: message, Substantive: substantive,
	}, true
}

func (mc *ModelClient) appendReflectionCandidates(
	ctx context.Context,
	candidates []store.ReflectionEventCandidate,
) {
	if len(candidates) == 0 {
		return
	}
	if err := mc.reflections.AppendReflectionEvents(
		ctx, mc.instance.ID(), candidates, mc.now(),
	); err != nil {
		slog.Default().ErrorContext(ctx, "append reflection events", "error", err)
	}
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
