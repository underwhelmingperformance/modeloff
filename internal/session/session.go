// Package session provides the backend coordinator that ties together
// stores, the API client, and the protocol layer. It manages channels,
// model instances, and handles commands by updating state and emitting
// domain events.
package session

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ircfmt"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/richtext"
)

// eventBufSize is the capacity of the session event channel. It must
// be large enough that normal event bursts (join + mode change, message
// + dispatch started/done) don't block callers. Anything that writes
// to this channel without a consumer draining it risks deadlock.
//
// The autojoin burst is the dominant dimensioning case: for each
// autojoined channel we emit a "Joining …" status notice, a JoinEvent,
// a ModeChangeEvent, and sometimes a TopicInfoEvent. At 256 we have
// headroom for roughly 60 channels of autojoin before the UI pump
// needs to catch up, which is well beyond any realistic user's list.
const eventBufSize = 256

// ErrNoAPIKey is returned by session operations that require an
// OpenRouter API key when one has not yet been configured. Callers
// can use `errors.Is(err, session.ErrNoAPIKey)` to distinguish this
// validation outcome from an upstream failure, e.g. to suppress
// user-facing notices while the user is still in onboarding.
var ErrNoAPIKey = errors.New("api key not configured")

// ErrModelListUnavailable is returned when the OpenRouter model
// catalogue could not be fetched on the most recent attempt and the
// session has no fresh list to validate against. `/add-model` and
// other operations that need an authoritative model list short-circuit
// with this sentinel rather than re-attempting the failing call on
// every request. Callers can use `errors.Is(err,
// session.ErrModelListUnavailable)` to surface a dedicated user
// notice.
var ErrModelListUnavailable = errors.New("model list unavailable")

// listModelsState tracks whether the cached model catalogue reflects a
// successful `ListModels` round-trip. It lets `ensureStructuredOutputModel`
// distinguish "never attempted" (fall through to the lazy load) from
// "last attempt failed" (short-circuit with `ErrModelListUnavailable`).
//
// Reads from `tea.Cmd` goroutines (e.g. `ensureStructuredOutputModel`)
// and writes from sibling sites (`ListModels`, `SetAPIKey`, `Reset`)
// run under the same single-writer serialisation as the existing
// `supportedModels` field. Concrete writer goroutines today:
// `loadLiveModels` (refresh) and `ensureStructuredOutputModel` (lazy
// load from `AddModel`). The field is held as an `atomic.Uint32` so
// that `tea.Cmd` reads cannot race with concurrent writes; task #25
// will codify the broader serialisation across `Session` mutations.
type listModelsState uint32

const (
	listModelsNone   listModelsState = iota // never attempted
	listModelsOK                            // last attempt succeeded
	listModelsFailed                        // last attempt failed
)

// sessionStore is the persistence surface [Session] depends on.
// The concrete [github.com/laney/modeloff/internal/store.SQLiteStore]
// satisfies it implicitly. The session does not call any other
// store methods; consumers with different needs (the memory
// adapter, the chat-screen) declare their own.
type sessionStore interface {
	// Windows.
	//
	// Addressable-by-name windows live in the `channels` table.
	// Loads return the typed concrete [domain.Window]
	// (`*StatusWindow` / `*ChannelWindow` / `*DMWindow`) so
	// callers can downcast where per-kind state matters. DM
	// windows resolve their counterpart `*domain.Instance`
	// through the store's instance registry; a DM whose
	// counterpart row has been deleted is dropped at load time
	// and logged.
	ListWindows(ctx context.Context) ([]domain.Window, error)
	GetWindow(ctx context.Context, name domain.ChannelName) (domain.Window, error)
	SaveWindow(ctx context.Context, w domain.Window) error

	// Event log.

	AppendEvent(ctx context.Context, ch domain.ChannelName, event domain.PersistableEvent) (int64, error)
	EventsBefore(ctx context.Context, ch domain.ChannelName, before *int64, n int) ([]domain.StoredEvent, error)
	EventsFrom(ctx context.Context, ch domain.ChannelName, from *int64, n int) ([]domain.StoredEvent, error)

	// DMEventsBefore returns up to `n` events from the DM thread
	// between `self` and `peer` strictly before `before` (or the
	// most recent if `before` is nil), in chronological order.
	// The thread is the union of both directions: events whose
	// `channel` column is `peer` and whose sender is `self`,
	// plus events whose `channel` column is `self` and whose
	// sender is `peer`. Either side of the pair may be empty
	// (the user's [domain.InstanceID] is empty by convention).
	DMEventsBefore(ctx context.Context, self, peer domain.InstanceID, before *int64, n int) ([]domain.StoredEvent, error)

	// Model instances.
	//
	// The store is the sole authority for `*domain.Instance`
	// pointer identity: callers receive the same pointer for a
	// given [domain.InstanceID] on every load. `GetWindow`
	// returns a `*ChannelWindow` whose member list already
	// carries canonical pointers — callers never resolve ids
	// themselves.
	ListInstances(ctx context.Context) ([]*domain.Instance, error)
	GetInstanceByID(ctx context.Context, id domain.InstanceID) (*domain.Instance, error)
	SaveInstance(ctx context.Context, inst *domain.Instance) error
	DeleteInstanceByID(ctx context.Context, id domain.InstanceID) error

	// ResolveNick returns the canonical `*domain.Instance` whose
	// current display nick matches the argument. This is the
	// single boundary where nick-in-hand callers (the command
	// parser) turn user input into an identity handle. Returns
	// [store.ErrNoSuchNick] when no instance matches.
	ResolveNick(ctx context.Context, nick domain.Nick) (*domain.Instance, error)

	// Session-active marker. Set on `Connect`, cleared on a
	// clean `Quit`; a non-empty value on the next `Connect`
	// signals an unclean prior shutdown so the user's stale
	// membership state can be reconciled.
	GetSessionActive(ctx context.Context) (string, error)
	SetSessionActive(ctx context.Context, value string) error
	ClearSessionActive(ctx context.Context) error

	// Last-read tracking. The event id high-watermark per
	// channel that the chat-screen has rendered; the session
	// reads it to compute unread badges.
	GetLastRead(ctx context.Context, ch domain.ChannelName) (int64, error)
	SetLastRead(ctx context.Context, ch domain.ChannelName, eventID int64) error

	// Personas.

	ListPersonas(ctx context.Context) ([]domain.Persona, error)
	SavePersona(ctx context.Context, p domain.Persona) error
	DeletePersonasByOrigin(ctx context.Context, origin domain.PersonaOrigin) error
	ReplaceGeneratedPersonas(ctx context.Context, personas []domain.Persona) error

	// Autojoin list. The set of channels rejoined at next
	// `Connect`; rewritten on every successful `Quit`.
	ListAutojoinChannels(ctx context.Context) ([]domain.ChannelName, error)
	SetAutojoinChannels(ctx context.Context, channels []domain.ChannelName) error

	// Reset truncates every table the session reads or writes.
	// Used by `/config --reset` and by the unclean-shutdown
	// recovery path.
	Reset(ctx context.Context) error
}

// Session is the backend coordinator. It bridges the UI layer and
// the underlying stores and API client.
//
// The user's `*domain.Instance` is constructed once at `New` time and
// lives for the lifetime of the process: nick renames mutate it in
// place via its own lock, so the pointer stays stable and every
// caller keeps identity by comparing against `UserInstance()`.
type Session struct {
	store  sessionStore
	memory memory.Store
	api    api.Client
	tools  *ToolRegistry

	baseContext func() context.Context
	user        *domain.Instance
	userModes   map[domain.ChannelName]domain.NickMode
	userMu      sync.Mutex
	apiKey      string
	smallModel  domain.ModelID
	factory     func(apiKey, baseURL string) (api.Client, error)
	now         func() time.Time
	events      chan domain.Event

	dispatchWG sync.WaitGroup

	// shuttingDown is closed by [Session.Shutdown] before it joins
	// the dispatch waitgroup. [Session.ensureModelClient] checks
	// it under `subsMu` and declines to spawn a fresh dispatch
	// goroutine once closed, so a late JOIN cannot race
	// `dispatchWG.Wait()` and trip the wait-group reuse panic.
	// The shape mirrors [net/http.Server]'s `inShutdown` flag —
	// new work is rejected at the registration point, not
	// documented away.
	shuttingDown     chan struct{}
	shuttingDownOnce sync.Once

	subsMu        sync.RWMutex
	subscribers   map[protocol.ClientID]*serverClient
	clientHandles map[protocol.ClientID]*serverClient
	userClient    *serverClient

	// operAuth gates [protocol.Oper]. The default rejects every
	// client; the user-client is promoted by [Session.New]'s
	// bootstrap call to `setUserModeAs`, not by a client-initiated
	// OPER. Tests and future credential mechanisms swap the
	// authenticator via [Session.SetOperAuthenticator].
	operAuth OperAuthenticator

	connectedC    chan struct{}
	connectedOnce sync.Once
	connectedAt   time.Time

	supportedModels      map[domain.ModelID]struct{}
	supportedModelsReady bool
	listModelsState      atomic.Uint32

	persistenceFailures metric.Int64Counter

	// tracerProvider is the OTel `TracerProvider` the session uses
	// for its spans. Defaults to `otel.GetTracerProvider()` at
	// construction time so production callers see the global
	// provider; tests inject their own via `WithTracerProvider` so
	// span recordings stay scoped to a single test even when
	// dispatch goroutines outlive the test that spawned them.
	tracerProvider trace.TracerProvider
}

// New creates a Session whose dispatch goroutines derive their
// lifetime context from `baseContext`. Each goroutine that needs
// a long-lived ctx calls `baseContext()` to obtain one; the
// supplier shape mirrors [net/http.Server.BaseContext]. Production
// wires `baseContext` to a closure over the
// [signal.NotifyContext]-derived ctx in `main`; tests pass
// `t.Context` as the supplier, so each goroutine sees the same
// per-test ctx the testing package cancels just before cleanups
// run.
//
// Cancellation of the ctx the supplier returns wakes dispatch
// goroutines; they exit and [Session.Shutdown] joins them. The
// session itself never cancels anything internally — the cancel
// call sits with the caller that owns the supplier's ctx.
//
// Load-bearing invariant: every ctx `baseContext` returns must
// share a cancellation source. Cancelling that source is what
// wakes the dispatch goroutines and lets [Session.Shutdown]
// complete; a supplier that returned uncorrelated ctxs (e.g.
// a fresh [context.Background] on each call) would leave dispatch
// goroutines blocked forever and `Shutdown` would only return
// once its own deadline elapses.
//
// The session owns the user's `*domain.Instance` handle for its
// lifetime and publishes it to the store straight away so that
// member-list resolution on channel load can see the user's
// empty InstanceID. The store never persists the user to disk;
// the handle is a session-scoped identity.
func New(
	baseContext func() context.Context,
	s sessionStore,
	m memory.Store,
	a api.Client,
	userNick domain.Nick,
	apiKey string,
	smallModel domain.ModelID,
) *Session {
	if smallModel == "" {
		smallModel = config.DefaultSmallModel
	}

	user := domain.NewUserInstance(userNick)

	persistenceFailures, _ := otel.Meter("github.com/laney/modeloff/internal/session").
		Int64Counter(observability.MetricPersistenceFailures)

	sess := &Session{
		baseContext:         baseContext,
		store:               s,
		memory:              m,
		api:                 a,
		user:                user,
		userModes:           make(map[domain.ChannelName]domain.NickMode),
		apiKey:              strings.TrimSpace(apiKey),
		smallModel:          smallModel,
		now:                 time.Now,
		events:              make(chan domain.Event, eventBufSize),
		connectedC:          make(chan struct{}),
		persistenceFailures: persistenceFailures,
		tracerProvider:      otel.GetTracerProvider(),
		subscribers:         make(map[protocol.ClientID]*serverClient),
		clientHandles:       make(map[protocol.ClientID]*serverClient),
		shuttingDown:        make(chan struct{}),
	}

	sess.operAuth = DefaultOperAuthenticator
	// The user-client starts with no modes; the server promotes it
	// directly via setUserModeAs immediately after construction.
	// This is server-initiated (empty `by`), not a client-issued
	// OPER request — the local user IS the operator, so there is
	// no credential exchange to perform. The wire-visible shape
	// is the same [domain.ModeChange] event any later mode change
	// would produce; consumers (chat-screen, tests reading the
	// user-client's events channel) see it as the first event of
	// the session.
	sess.userClient = newServerClient(sess, protocol.UserClientID, nil)
	sess.subscribers[sess.userClient.id] = sess.userClient
	sess.clientHandles[sess.userClient.id] = sess.userClient
	sess.setUserModeAs(baseContext(), "", sess.userClient, domain.ModeOperator, true)

	return sess
}

// OperAuthenticator validates a [protocol.Oper] attempt. Returning
// true grants `+o` to the issuing client; returning false yields
// [domain.OperFailedError] on `Response.Err`.
type OperAuthenticator func(c protocol.Client, name, password string) bool

// DefaultOperAuthenticator rejects every caller. The only path to
// +o today is the server's bootstrap promotion of the user-client;
// client-initiated OPER is reserved for future credentialed
// mechanisms swapped in via [Session.SetOperAuthenticator].
func DefaultOperAuthenticator(protocol.Client, string, string) bool {
	return false
}

// SetOperAuthenticator replaces the [OperAuthenticator] consulted
// by the `OPER` dispatcher arm. Tests use this to exercise the
// success path; future credential mechanisms swap in a real check.
func (s *Session) SetOperAuthenticator(auth OperAuthenticator) {
	if auth == nil {
		auth = DefaultOperAuthenticator
	}
	s.operAuth = auth
}

// WithTracerProvider overrides the OTel `TracerProvider` the session
// uses for its spans. Tests inject a per-test recorder so that
// background dispatch goroutines recording spans after a test
// finishes do not bleed into a sibling test's recorder. Production
// code does not need to call this — the default global provider is
// already correct.
func (s *Session) WithTracerProvider(tp trace.TracerProvider) *Session {
	s.tracerProvider = tp

	return s
}

// Events returns the legacy non-protocol bus carrying chat-screen-
// internal control signals: ConfigChangedEvent, ErrorEvent,
// SystemNoticeEvent. Protocol events — including dispatch lifecycle
// — flow through the user-client subscription's events channel; see
// [Session.User].
func (s *Session) Events() <-chan domain.Event {
	return s.events
}

// User returns the user-client subscription, created at session
// bootstrap and live for the whole session lifetime. The returned
// handle satisfies [protocol.Client]: it carries the user's
// identity ([protocol.UserClientID]) and grants
// [domain.ModeOperator].
func (s *Session) User() protocol.Client {
	return s.userClient
}

// Model returns the [protocol.Client] handle for the model instance
// identified by `id`. Handles are pointer-stable: two calls for the
// same id return the same `*serverClient`. The handle is
// lazy-allocated on first access for any id whose row exists in the
// store. Returns nil if the id is empty (the user-client lives at
// [Session.User]) or if no instance row matches.
//
// The handle is currently `Send`-only; its `Events()` channel is
// allocated but unused by the dispatcher.
func (s *Session) Model(ctx context.Context, id protocol.ClientID) protocol.Client {
	if id == protocol.UserClientID {
		return nil
	}

	if c := s.lookupClientHandle(id); c != nil {
		return c
	}

	inst, err := s.store.GetInstanceByID(ctx, id)
	if err != nil || inst == nil {
		return nil
	}

	return s.ensureModelClient(ctx, inst)
}

// lookupClientHandle returns the cached handle for `id` under the
// read lock, or nil if none has been allocated yet.
func (s *Session) lookupClientHandle(id protocol.ClientID) *serverClient {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()

	return s.clientHandles[id]
}

// ensureModelClient registers a model-client handle for `inst` if
// one does not already exist, and returns the canonical handle
// either way. Model clients carry no user modes; they join the
// subscriber set so [fanOutProtocol] delivers to them, and a
// long-lived dispatch goroutine starts under the same lock so the
// channel has a consumer attached before any caller can fan out
// to it.
//
// If [Session.Shutdown] has begun, registration is refused: an
// existing handle is still returned, but a fresh `inst` produces
// no new subscription and no new dispatch goroutine. Returning
// nil at the shutdown gate keeps `dispatchWG.Go` from racing
// `dispatchWG.Wait`. Callers must tolerate a nil return after
// shutdown — in practice the only post-shutdown caller is a late
// JOIN that loses to the binary exiting anyway.
func (s *Session) ensureModelClient(ctx context.Context, inst *domain.Instance) *serverClient {
	id := inst.ID()

	s.subsMu.Lock()
	defer s.subsMu.Unlock()

	if existing, ok := s.clientHandles[id]; ok {
		return existing
	}

	select {
	case <-s.shuttingDown:
		return nil
	default:
	}

	client := newServerClient(s, id, inst)
	s.seedModelClientHistory(ctx, client)

	s.clientHandles[id] = client
	s.subscribers[id] = client

	s.startModelDispatch(client)

	return client
}

// startModelDispatch spawns the long-lived dispatch goroutine for
// `client`. The goroutine takes its lifetime ctx from the
// `baseContext` supplier passed to [New]; cancelling that ctx
// (production: the `signal.NotifyContext` cancel; tests: test
// completion) wakes the goroutine on its select and ends the
// loop. [Session.Shutdown] joins.
func (s *Session) startModelDispatch(client *serverClient) {
	ctx, cancel := context.WithCancel(s.baseContext())
	client.cancel = cancel
	s.dispatchWG.Go(func() { s.runModelDispatch(ctx, client) })
}

// reapClient removes a model-client from the subscriber set and
// cancels its dispatch goroutine. The user-client is never
// reaped — its lifetime equals the session. Idempotent on an
// unknown id.
func (s *Session) reapClient(id protocol.ClientID) {
	if id == protocol.UserClientID {
		return
	}

	s.subsMu.Lock()
	client, ok := s.subscribers[id]
	if ok {
		delete(s.subscribers, id)
		delete(s.clientHandles, id)
	}
	s.subsMu.Unlock()

	if ok && client.cancel != nil {
		client.cancel()
	}
}

// seedModelClientHistory eager-seeds the model-client's per-channel
// history buffer from the events log for every channel the
// instance is in. DM targets are not eager-seeded — the wire layer
// has no notion of "open DMs" — they lazy-seed in
// [serverClient.appendHistory] on first event arrival.
//
// Runs under `subsMu.Lock()` so the seed completes before any
// fan-out can deliver to this client; the dispatch goroutine
// has not started yet either. Sqlite reads are sequential and
// total time scales with the instance's channel count, which is
// bounded by user behaviour. If this becomes a measurable
// bootstrap-latency cost, the lock can be released around the
// store reads with a sequence-number dedupe on subsequent live
// appends; for now the simpler pattern reads cleanly.
func (s *Session) seedModelClientHistory(ctx context.Context, client *serverClient) {
	if client.history == nil {
		return
	}

	channels := client.instance.Channels()
	if channels == nil {
		return
	}

	logger := slog.Default()

	for pair := channels.Oldest(); pair != nil; pair = pair.Next() {
		ch := pair.Key
		seed, err := s.store.EventsBefore(ctx, ch, nil, modelHistorySize)
		if err != nil {
			logger.ErrorContext(ctx, "seed model history",
				"component", "session",
				"instance_id", client.id,
				"channel", ch,
				"error", err,
			)
			continue
		}

		client.history[ch] = seed
	}
}

// subscriberSnapshot returns a stable copy of the subscriber set
// under the read lock so callers iterating it cannot race with a
// concurrent registration or deregistration. The returned slice's
// `*serverClient` pointers are shared with the registry.
func (s *Session) subscriberSnapshot() []*serverClient {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()

	snap := make([]*serverClient, 0, len(s.subscribers))
	for _, sub := range s.subscribers {
		snap = append(snap, sub)
	}

	// Go map iteration is randomised per process, which leaks into
	// per-fan-out delivery order: model-client dispatch goroutines
	// wake in different orders across runs, and the lifecycle
	// events they emit then interleave with the main goroutine's
	// emits in a non-deterministic order on the user-client's
	// buffered events channel. Sort by ClientID so fan-out
	// iteration is stable; the user-client (sentinel empty id)
	// always sorts first, then model-clients lexicographically by
	// instance id.
	slices.SortFunc(snap, func(a, b *serverClient) int {
		return strings.Compare(string(a.id), string(b.id))
	})

	return snap
}

// fanOutProtocol delivers a protocol event to every active
// subscription that should receive it. Sends are blocking,
// matching the back-pressure discipline of `s.events`: a stuck
// consumer surfaces as a wedged producer rather than silent data
// loss. Each subscription's events channel is buffered to
// [eventBufSize]; callers should attach a consumer before
// bootstrap-time emission exceeds that capacity.
//
// PRIVMSG/Action events do not echo back to their originator
// (RFC 2812 §3.3.1: chat traffic is delivered to every member of
// the target window except the sender). Other event types — JOIN,
// PART, MODE, TOPIC, NICK, etc. — are delivered to every
// member-subscriber including the originator, matching IRC's
// behaviour for those signals.
//
// Membership filtering keeps model-clients from receiving events
// for channels they are not in. The user-client is treated as
// connected to every window: the chat-screen renders the entire
// session and needs the full feed.
//
// The send-side select gates only on `ctx.Done()`: cancelling the
// supplier ctx propagates to every in-flight handler's ctx, so a
// blocked send aborts when shutdown begins, even if its target
// dispatch goroutine has already exited.
func (s *Session) fanOutProtocol(ctx context.Context, pe domain.ProtocolEvent) {
	suppressOriginator, sender := chatTrafficSender(pe)
	spanCtx := trace.SpanContextFromContext(ctx)

	// `+a` rewrites the visible nick on chat-traffic events to the
	// `"anonymous"` sentinel (RFC 2811 §4.2.1) before delivery, so
	// even the channel's own members can't see who sent what. The
	// stored event retains the real From for audit.
	pe = anonymiseIfNeeded(s, ctx, pe)

	// Actor-scoped events ([domain.Quit] and [domain.NickChange])
	// carry no target on the wire; the per-recipient channel list
	// is computed at fan-out time as the intersection of the
	// actor's live membership and each recipient's. Snapshot the
	// actor's channels once so the per-sub loop does not re-walk
	// the ordered map.
	actorChannels := actorChannelSnapshot(pe)

	for _, sub := range s.subscriberSnapshot() {
		if suppressOriginator && sub.Identity() == sender {
			continue
		}

		targets := intersectActorTargets(sub, actorChannels)
		if !sub.canReceive(pe, targets) {
			continue
		}

		select {
		case sub.events <- protocol.Delivery{
			Event:   pe,
			Targets: targets,
			SpanCtx: spanCtx,
		}:
		case <-ctx.Done():
			return
		}
	}
}

// anonymiseIfNeeded rewrites a chat-traffic event's `From` field
// to `"anonymous"` when the target channel carries `+a`. Returns
// the event unchanged when the channel is not anonymous or when
// the event is not chat traffic.
func anonymiseIfNeeded(s *Session, ctx context.Context, pe domain.ProtocolEvent) domain.ProtocolEvent {
	msg, ok := pe.(domain.Message)
	if !ok {
		return pe
	}

	if domain.InferChannelKind(msg.Target) != domain.KindChannel {
		return pe
	}

	window, err := s.loadChannelWindow(ctx, msg.Target)
	if err != nil || !window.Modes.Anonymous {
		return pe
	}

	msg.From = "anonymous"
	return msg
}

// actorChannelSnapshot returns the actor's channel set if `pe` is
// an actor-scoped event, or nil otherwise. The snapshot is read
// once per fan-out under the assumption that
// [Session.propagateActorEvent] has not yet run its post-emit
// `MutateChannels`; per-sub callers iterate the slice instead of
// re-walking the ordered map.
func actorChannelSnapshot(pe domain.ProtocolEvent) []domain.ChannelName {
	var actor *domain.Instance

	switch e := pe.(type) {
	case domain.Quit:
		actor = e.Instance
	case domain.NickChange:
		actor = e.Instance
	case domain.ModelDispatchStarted:
		actor = e.Instance
	case domain.ModelDispatchDone:
		actor = e.Instance
	default:
		return nil
	}

	if actor == nil {
		return nil
	}

	channels := actor.Channels()
	if channels == nil {
		return nil
	}

	names := make([]domain.ChannelName, 0, channels.Len())
	for pair := channels.Oldest(); pair != nil; pair = pair.Next() {
		names = append(names, pair.Key)
	}

	return names
}

// intersectActorTargets returns the recipient-visible channel
// list for an actor-scoped event: those channels in
// `actorChannels` that `sub` is also a member of. The user-client
// (no backing instance) sees the actor's full channel list — the
// chat-screen needs the complete picture to route the line into
// every open window where the actor was a known member.
// Window-scoped events pass `actorChannels == nil` and receive a
// nil result.
func intersectActorTargets(sub *serverClient, actorChannels []domain.ChannelName) []domain.ChannelName {
	if len(actorChannels) == 0 {
		return nil
	}

	if sub.instance == nil {
		// User-client: the chat-screen is the operator window for
		// every channel; expose the full actor list for routing.
		out := make([]domain.ChannelName, len(actorChannels))
		copy(out, actorChannels)
		return out
	}

	subChannels := sub.instance.Channels()
	if subChannels == nil {
		return nil
	}

	var out []domain.ChannelName
	for _, ch := range actorChannels {
		if _, ok := subChannels.Get(ch); ok {
			out = append(out, ch)
		}
	}

	return out
}

// chatTrafficSender reports whether `ev` carries the
// originator-suppression rule (PRIVMSG/Action), and returns the
// sender's [protocol.ClientID] when it does. The empty client id
// returned alongside `false` is unused and never compared.
//
// Today only [domain.Message] (covering both PRIVMSG and `/me`
// via [domain.Message.Action]) qualifies. Future event types
// needing the same rule add a switch arm here.
func chatTrafficSender(ev domain.ProtocolEvent) (suppress bool, sender protocol.ClientID) {
	if msg, ok := ev.(domain.Message); ok {
		return true, protocol.ClientID(msg.InstanceID)
	}

	return false, ""
}

// Connected returns a channel that is closed once Connect has
// completed successfully. Callers can select on it to be notified
// when the backend is ready for the frontend to start issuing
// commands (for example, JOIN for autojoin channels).
func (s *Session) Connected() <-chan struct{} {
	return s.connectedC
}

// ConnectedAt returns the time at which Connect ran. The status
// channel's per-session view uses this as a cutoff so that messages
// from previous sessions remain in the event log without being
// rendered.
func (s *Session) ConnectedAt() time.Time {
	return s.connectedAt
}

// Shutdown closes the session's shutdown gate so that any
// further [Session.ensureModelClient] call declines to register
// a fresh dispatch goroutine, then waits for the goroutines
// already spawned to return. It does not itself cancel anything;
// the caller is expected to have already cancelled the ctx the
// `baseContext` supplier returns (in production, the
// `signal.NotifyContext` cancel func; in tests, `t.Context()`
// cancellation handled by the testing package). The shape
// mirrors [net/http.Server.Shutdown]: cancellation lives with
// the caller, new work is refused at the registration point, and
// the method's only job is to wait until in-flight work has
// drained.
//
// The returned error is `ctx.Err()` if `ctx`'s deadline expires
// or it is cancelled before drainage completes. A `nil` return
// means every goroutine has exited.
//
// Shutdown is safe to call more than once; the gate is closed
// via `sync.Once`. Subsequent calls re-wait on the same
// waitgroup against the new caller's `ctx`.
func (s *Session) Shutdown(ctx context.Context) error {
	// Close the shutdown gate under `subsMu` so the close is
	// serialised with [Session.ensureModelClient]'s gate-check,
	// which runs under the same lock. Either ensureModelClient
	// holds the lock when Shutdown wants it (Shutdown waits;
	// ensureModelClient's `dispatchWG.Go` completes against the
	// gate it observed open), or Shutdown holds it first (any
	// subsequent ensureModelClient acquires after release and
	// observes the closed gate). No `dispatchWG.Go` can
	// happen-after a close that the goroutine observed as open,
	// so the join below is safe by construction.
	s.subsMu.Lock()
	s.shuttingDownOnce.Do(func() { close(s.shuttingDown) })
	s.subsMu.Unlock()

	done := make(chan struct{})

	go func() {
		s.dispatchWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Connect performs the backend-side connection handshake. It must be
// called once per session, at startup, before JoinAutojoinChannels.
//
// Behaviour:
//
//   - Reads the session_active marker from the store. A non-empty
//     value means the previous session did not shut down cleanly; any
//     channels that still list the user as a member are cleaned up so
//     the client starts from a known-empty state, mirroring what a
//     real IRC server would observe after a client disconnect.
//   - Writes a fresh session_active marker so a later crash is
//     detectable.
//   - Emits a [domain.Welcome] on the protocol bus, mirroring RFC
//     2812 RPL_WELCOME (001). If the prior session shut down
//     uncleanly, a [domain.Reconnected] follows.
//   - Closes the channel returned by Connected so that UI layers
//     waiting on readiness can advance.
//
// The session does not own a `&modeloff` window. The chat-screen
// constructs its own local view of the server window and renders
// the emitted events into it; the connection screen subscribes to
// the same bus during its boot-time pane.
func (s *Session) Connect(ctx context.Context) (retErr error) {
	if !s.connectedAt.IsZero() {
		// No span is recorded for a no-op call: it is not a real
		// connect attempt and would otherwise inflate
		// session.connect operation counts.
		return nil
	}

	ctx, span := s.startSpan(ctx, "session.connect",
		attribute.String(observability.AttrOperation, "session.connect"),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	s.connectedAt = s.now()

	prev, err := s.store.GetSessionActive(ctx)
	if err != nil {
		return fmt.Errorf("get session active: %w", err)
	}

	unclean := prev != ""
	if unclean {
		if err := s.cleanupUncleanShutdown(ctx); err != nil {
			return fmt.Errorf("cleanup unclean shutdown: %w", err)
		}
	}

	if err := s.store.SetSessionActive(ctx, s.connectedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("set session active: %w", err)
	}

	s.emit(ctx, domain.Welcome{
		ServerName: domain.StatusServerName,
		Nick:       s.user.Nick(),
		At:         s.connectedAt,
	})

	if unclean {
		s.emit(ctx, domain.Reconnected{At: s.connectedAt})
	}

	s.connectedOnce.Do(func() { close(s.connectedC) })

	return nil
}

// setUserMode records the user's mode for a channel. It is called
// from joinAs on a successful join and from SetMode when the user's
// mode changes. The mode is used by loadChannel/loadChannels to
// re-inject the user into channel member lists returned from the
// store.
func (s *Session) setUserMode(ctx context.Context, ch domain.ChannelName, mode domain.NickMode) {
	s.userMu.Lock()
	s.userModes[ch] = mode
	s.userMu.Unlock()

	slog.Default().DebugContext(ctx, "user mode changed",
		"component", "session",
		"channel", ch,
		"mode", mode.String(),
	)
}

// forgetUserMode drops the recorded mode for a channel when the
// user parts or is kicked.
func (s *Session) forgetUserMode(ctx context.Context, ch domain.ChannelName) {
	s.userMu.Lock()
	delete(s.userModes, ch)
	s.userMu.Unlock()

	slog.Default().DebugContext(ctx, "user mode cleared",
		"component", "session",
		"channel", ch,
	)
}

// userModeFor reads the recorded mode for a channel. The zero value
// (ModeNone) is returned when no mode has been recorded. Callers
// that ask about a channel the user isn't in get a debug-level log
// line as a diagnostic aid — the mode map is only meaningful for
// channels the user is currently in, but legitimate callers
// (assertions, tests) may probe non-member channels and ModeNone is
// the right answer for them.
func (s *Session) userModeFor(ctx context.Context, ch domain.ChannelName) domain.NickMode {
	if !s.userInChannel(ch) {
		slog.Default().DebugContext(ctx, "user mode requested for channel user is not in",
			"component", "session",
			"channel", ch,
		)
	}

	s.userMu.Lock()
	defer s.userMu.Unlock()

	return s.userModes[ch]
}

// userInChannel reports whether the user's in-memory Channels map
// lists the given channel. The map is authoritative for session-
// ephemeral membership: the user is never saved to the store, so
// channels loaded from disk rely on this to know whether to
// re-inject the user.
func (s *Session) userInChannel(ch domain.ChannelName) bool {
	channels := s.user.Channels()
	if channels == nil {
		return false
	}

	_, ok := channels.Get(ch)
	return ok
}

// loadChannelWindow reads an addressable `#`-channel as its typed
// `*ChannelWindow`, with the user re-injected as a member when
// the session records them as being in the channel. Returns
// `domain.ErrNotChannelWindow` if the row exists but is not a
// channel (status / DM) — channel-only callers rely on this as
// a typed guard.
func (s *Session) loadChannelWindow(ctx context.Context, name domain.ChannelName) (*domain.ChannelWindow, error) {
	w, err := s.store.GetWindow(ctx, name)
	if err != nil {
		return nil, err
	}

	cw, ok := w.(*domain.ChannelWindow)
	if !ok {
		return nil, fmt.Errorf("%w: kind %d for %q", domain.ErrNotChannelWindow, w.Kind(), name)
	}

	s.injectUserIfChannelMember(ctx, cw)

	return cw, nil
}

// injectUserIfChannelMember adds the user to a `*ChannelWindow`'s
// member list when the session records them as in that channel.
// The user is an ephemeral session actor and is never persisted;
// `persistChannelWindow` strips them on save and this helper
// adds them back on load.
func (s *Session) injectUserIfChannelMember(ctx context.Context, cw *domain.ChannelWindow) {
	if !s.userInChannel(cw.Name()) {
		return
	}

	if cw.Members.HasInstance(s.user) {
		return
	}

	cw.Members.Add(s.user)

	if mode := s.userModeFor(ctx, cw.Name()); mode != domain.ModeNone {
		cw.Members.SetMode(s.user, mode)
	}
}

// persistChannelWindow saves a `*ChannelWindow` through the
// store's typed `SaveWindow` surface, with the user stripped from
// the member list — same contract as `persistChannel`. The user
// is an ephemeral session actor and is never persisted; the
// equivalent load path injects them back via
// `injectUserIfMember`.
func (s *Session) persistChannelWindow(ctx context.Context, w *domain.ChannelWindow) error {
	clone := *w
	clone.Members = cloneMembersWithout(w.Members, s.user)
	return s.store.SaveWindow(ctx, &clone)
}

// cloneMembersWithout returns a new MemberList containing every
// member of src except the one whose handle equals `excluded`.
// Modes are preserved.
func cloneMembersWithout(src domain.MemberList, excluded *domain.Instance) domain.MemberList {
	dst := domain.NewMemberList()
	for m := range src.All() {
		if m.Instance == excluded {
			continue
		}

		dst.Add(m.Instance)
		if m.Mode != domain.ModeNone {
			dst.SetMode(m.Instance, m.Mode)
		}
	}

	return dst
}

// cleanupUncleanShutdown resets this Session's in-memory user
// state so a post-connect view mirrors a fresh, clean start.
//
// The user is ephemeral — never persisted to the store — so
// cleanup is purely an in-memory reset of the user handle's
// channel map and the userModes map. There is no store-side
// prior-nick residue to reconcile.
func (s *Session) cleanupUncleanShutdown(ctx context.Context) error {
	// The user is never persisted, so there is nothing to remove
	// from the store. The only stale state is whatever this Session
	// instance has accumulated in the in-memory user handle and
	// userModes map — drop both so the post-connect view mirrors a
	// fresh, clean start.
	_ = ctx

	s.user.MutateChannels(func(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
		for pair := m.Oldest(); pair != nil; {
			next := pair.Next()
			m.Delete(pair.Key)
			pair = next
		}
	})

	s.userMu.Lock()
	s.userModes = make(map[domain.ChannelName]domain.NickMode)
	s.userMu.Unlock()

	return nil
}

// SetAPIFactory configures how runtime API clients are created.
func (s *Session) SetAPIFactory(factory func(apiKey, baseURL string) (api.Client, error)) {
	s.factory = factory
}

// SetToolRegistry configures additional model-callable tools.
func (s *Session) SetToolRegistry(registry *ToolRegistry) {
	s.tools = registry
}

// HasAPIKey reports whether the session has an active API key.
func (s *Session) HasAPIKey() bool {
	return strings.TrimSpace(s.apiKey) != ""
}

// UserInstance returns the canonical `*domain.Instance` for the
// human user. Identity checks against this pointer are the way
// callers recognise user-origin events; the returned handle is
// pointer-stable for the process lifetime.
func (s *Session) UserInstance() *domain.Instance {
	return s.user
}

// UserNick is a convenience that reads the current user nick from
// the handle returned by UserInstance.
func (s *Session) UserNick() domain.Nick {
	return s.user.Nick()
}

// ResolveNick turns a user-supplied nick into the canonical
// `*domain.Instance` for that nick. This is the single boundary
// where nick strings become handles — callers hold the handle and
// compare by pointer identity from there on. A nick matching the
// user's current display nick resolves to the user's handle; any
// other nick is looked up in the store.
func (s *Session) ResolveNick(ctx context.Context, nick domain.Nick) (*domain.Instance, error) {
	if nick == s.user.Nick() {
		return s.user, nil
	}

	return s.store.ResolveNick(ctx, nick)
}

// UserJoinedAt returns the time the user joined the given channel,
// or the zero time if the user is not in the channel.
func (s *Session) UserJoinedAt(ch domain.ChannelName) time.Time {
	if channels := s.user.Channels(); channels != nil {
		if t, ok := channels.Get(ch); ok {
			return t
		}
	}

	return time.Time{}
}

// Join creates or opens a channel. Events are emitted on the
// session event channel.
func (s *Session) Join(ctx context.Context, channelName string) error {
	return s.joinAs(ctx, s.user, domain.ChannelName(channelName), "")
}

// Part records the user leaving a channel. An optional farewell
// message is included in the event. Events are emitted on the
// session event channel.
func (s *Session) Part(ctx context.Context, ch domain.ChannelName, message string) error {
	return s.partAs(ctx, s.user, ch, message)
}

// Quit performs a clean client-side shutdown. For each channel the
// user is in it appends a Quit event to the log (so models
// see the quit the next time they are dispatched against in that
// channel), removes the user from the channel members list, saves
// the autojoin list so the channels can be rejoined on next startup,
// clears in-memory channel state, and clears the session_active
// marker so the next startup is classified as clean.
//
// No model dispatch is performed: a running process does not wait
// for remote models to acknowledge the quit. The call is synchronous
// but fast (local sqlite writes only); the UI wraps it in a tea.Cmd
// to keep the event loop responsive.
func (s *Session) userQuit(ctx context.Context, message string) (retErr error) {
	ctx, span := s.startSpan(ctx, "session.quit",
		attribute.String(observability.AttrOperation, "session.quit"),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	autojoin := s.persistableAutojoinChannels()

	userNick := s.user.Nick()
	channels := s.instanceChannelNames(s.user)
	now := s.now()

	// Order matters for crash-resilience: append per-channel quit events
	// and drop the user from the members list first, persist the
	// autojoin list second, clear in-memory state third, and clear the
	// session_active marker last. A crash between any of the earlier
	// steps leaves the next startup classified as unclean, at which
	// point cleanupUncleanShutdown handles any residual memberships.
	s.propagateActorEvent(ctx, s.user, actorEventConfig{
		storeOnly: true,
		build: func() domain.PersistableEvent {
			return domain.Quit{
				Nick:    userNick,
				Message: message,
				At:      now,
			}
		},
		afterEach: func(ctx context.Context, ch domain.ChannelName) {
			s.forgetUserMode(ctx, ch)
		},
	})

	if err := s.store.SetAutojoinChannels(ctx, autojoin); err != nil {
		return fmt.Errorf("save autojoin channels: %w", err)
	}

	s.user.MutateChannels(func(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
		for _, ch := range channels {
			m.Delete(ch)
		}
	})

	if err := s.store.ClearSessionActive(ctx); err != nil {
		return fmt.Errorf("clear session active: %w", err)
	}

	return nil
}

// JoinAutojoinChannels loads the autojoin channel list and issues an
// ordinary JOIN for each entry. It is intended to be called by the
// UI immediately after Connect signals readiness; from the backend's
// perspective the resulting joinAs calls are indistinguishable from
// the user typing /join manually.
//
// Pre-condition: Connect has been called, so any stale memberships
// from the previous session have been cleaned up. Each joinAs call
// therefore takes the !alreadyMember path and emits the full IRC
// join protocol (JoinEvent, ChanServ +o ModeChangeEvent, optional
// TopicInfoEvent) plus stamps UserJoinedAt to the current time.
//
// Error contract: best-effort. The function only returns a non-nil
// error if the autojoin list itself cannot be loaded. Per-channel
// joinAs failures are surfaced via two separate signals — a
// "Failed to join …" notice on the status channel for the user, and
// AttrAutojoinFailed plus an error-kind ErrorKindAutojoin on the
// aggregate session.autojoin span for observability — and the
// function still returns nil so that downstream startup steps
// (FocusChannel, dispatch reactor) proceed.
func (s *Session) JoinAutojoinChannels(ctx context.Context) error {
	ctx, span := s.startSpan(ctx, "session.autojoin",
		attribute.String(observability.AttrOperation, "session.autojoin"),
	)
	defer span.End()

	channels, err := s.store.ListAutojoinChannels(ctx)
	if err != nil {
		setSpanError(span, err, observability.ErrorKindStore)
		return fmt.Errorf("list autojoin channels: %w", err)
	}

	channelNames := make([]string, len(channels))
	for i, ch := range channels {
		channelNames[i] = string(ch)
	}

	var failed int
	for _, ch := range channels {
		if err := s.joinAs(ctx, s.user, ch, ""); err != nil {
			failed++
			slog.Default().ErrorContext(ctx, "autojoin channel", "channel", ch, "error", err)
		}
	}

	span.SetAttributes(
		attribute.Int(observability.AttrAutojoinCount, len(channels)),
		attribute.Int(observability.AttrAutojoinFailed, failed),
		attribute.StringSlice(observability.AttrAutojoinChannels, channelNames),
	)

	if failed > 0 {
		setSpanError(span, fmt.Errorf("%d of %d autojoin channels failed", failed, len(channels)), observability.ErrorKindAutojoin)
		return nil
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	return nil
}

// DirectoryChannels returns the public channel directory for
// `/list`. Filters to `*ChannelWindow` only — DMs and the
// status window are not in the directory. The returned entries
// are snapshots of name, member count, and topic; callers turn
// them into per-row `domain.ListReply` events themselves.
func (s *Session) DirectoryChannels(ctx context.Context) ([]domain.ChannelDirectoryEntry, error) {
	windows, err := s.store.ListWindows(ctx)
	if err != nil {
		return nil, fmt.Errorf("list windows: %w", err)
	}

	entries := make([]domain.ChannelDirectoryEntry, 0, len(windows))
	for _, w := range windows {
		cw, ok := w.(*domain.ChannelWindow)
		if !ok {
			continue
		}

		// `+s` channels are hidden from the directory entirely
		// (RFC 2811 §4.2.7). `+p` channels appear but with their
		// topic suppressed (§4.2.6) — the channel name itself
		// stays visible.
		if cw.Modes.Secret {
			continue
		}

		topic := cw.Topic
		if cw.Modes.Private {
			topic = ""
		}

		entries = append(entries, domain.ChannelDirectoryEntry{
			Channel: cw.Name(),
			Members: cw.Members.Len(),
			Topic:   topic,
		})
	}

	return entries, nil
}

// Instances returns an iterator over every known model instance.
// The UI's tab-completion sources call this to offer `/invite`,
// `/msg`, `/whois`, and `/add-model` suggestions across the full
// session — not just the active channel's members. The user
// instance is not included; it's reachable via `UserInstance()`.
//
// The iterator materialises a snapshot at call time and is safe
// to range after the session state changes; subsequent mutations
// will not be visible on the same iterator.
func (s *Session) Instances(ctx context.Context) iter.Seq[*domain.Instance] {
	instances, err := s.store.ListInstances(ctx)
	if err != nil {
		slog.Default().ErrorContext(ctx, "list instances for completion", "component", "session", "error", err)
		return func(func(*domain.Instance) bool) {}
	}

	return func(yield func(*domain.Instance) bool) {
		for _, inst := range instances {
			if !yield(inst) {
				return
			}
		}
	}
}

// maxNickGenerationAttempts caps the number of times the model is
// asked for a nickname before the caller gives up. Each attempt after
// the first carries the previously rejected suggestion as a follow-up
// turn so the model picks something different — the user's full nick
// list is intentionally never sent to the model.
const maxNickGenerationAttempts = 3

// generateUniqueNick asks the small model for a nickname guided by
// the assigned persona and retries up to `maxNickGenerationAttempts`
// times if the suggested nick is already in use by another instance
// or the user.
func (s *Session) generateUniqueNick(
	ctx context.Context,
	modelID domain.ModelID,
	persona string,
	logger *slog.Logger,
) (domain.Nick, error) {
	generateCtx, generateSpan := s.startSpan(
		ctx,
		"session.generate_nick",
		attribute.String(observability.AttrOperation, "session.generate_nick"),
		attribute.String(observability.AttrModelID, string(modelID)),
	)
	defer generateSpan.End()

	var rejected []domain.Nick

	for attempt := 1; attempt <= maxNickGenerationAttempts; attempt++ {
		result, err := s.api.GenerateNick(generateCtx, s.smallModel, persona, rejected)
		if err != nil {
			setSpanError(generateSpan, err, observability.ErrorKindDispatch)
			logger.ErrorContext(ctx, "generate nick failed",
				"error", err,
				"attempt", attempt,
			)
			return "", fmt.Errorf("generate nick: %w", err)
		}

		result.Usage.SetSpanAttributes(generateSpan, result.RequestID)

		if !s.nickIsTaken(ctx, result.Nick) {
			generateSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
			return result.Nick, nil
		}

		logger.InfoContext(ctx, "generated nick already in use",
			"nick", result.Nick,
			"attempt", attempt,
		)
		rejected = append(rejected, result.Nick)
	}

	err := fmt.Errorf("generate nick: %d attempts exhausted, all suggestions collided", maxNickGenerationAttempts)
	setSpanError(generateSpan, err, observability.ErrorKindDispatch)

	return "", err
}

// nickIsTaken reports whether `nick` is already held by the user or
// any registered model instance. Resolution flows through
// `Session.ResolveNick` so the user-vs-store dispatch matches every
// other nick-keyed code path.
func (s *Session) nickIsTaken(ctx context.Context, nick domain.Nick) bool {
	_, err := s.ResolveNick(ctx, nick)
	return err == nil
}

func (s *Session) attachInstanceToChannel(
	ctx context.Context,
	ch domain.ChannelName,
	inst *domain.Instance,
	by *domain.Instance,
) error {
	window, err := s.loadChannelWindow(ctx, ch)
	if err != nil {
		return fmt.Errorf("get channel: %w", err)
	}

	inst.MutateChannels(func(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
		if _, ok := m.Get(ch); !ok {
			m.Set(ch, s.now())
		}
	})

	if err := s.store.SaveInstance(ctx, inst); err != nil {
		return fmt.Errorf("save instance: %w", err)
	}

	s.ensureModelClient(ctx, inst)

	isNew := !window.Members.HasInstance(inst)
	if isNew {
		window.Members.Add(inst)
	}

	if err := s.persistChannelWindow(ctx, window); err != nil {
		return fmt.Errorf("save channel: %w", err)
	}

	now := s.now()
	byNick := by.Nick()

	s.persistAndEmit(ctx, ch, domain.ModelInvited{
		Target:     ch,
		Nick:       inst.Nick(),
		InstanceID: inst.ID(),
		By:         byNick,
		At:         now,
		Instance:   inst,
	})

	return nil
}

// SendMessage is the user shorthand for [Session.sendMessageAs].
func (s *Session) SendMessage(ctx context.Context, ch domain.ChannelName, body string) (domain.Message, error) {
	return s.sendMessageAs(ctx, s.user, ch, body)
}

// SendAction is the user shorthand for [Session.sendActionAs].
func (s *Session) SendAction(ctx context.Context, ch domain.ChannelName, body string) (domain.Message, error) {
	return s.sendActionAs(ctx, s.user, ch, body)
}

// DispatchToChannel sends new events to all model instances in a channel
// and collects their replies. The caller provides the new IRC-formatted
// events to broadcast; history is loaded from the store.
//
// Callers must not include events whose `InstanceID` matches a target
// model — the wire-layer suppression is at fan-out, not at this driver.
func (s *Session) DispatchToChannel(
	ctx context.Context,
	ch domain.ChannelName,
	newEvents []protocol.IRCMessage,
) ([]domain.ModelReplyEvent, error) {
	ctx, span := s.startSpan(ctx, "session.dispatch_to_channel", attribute.String(observability.AttrOperation, "session.dispatch_to_channel"))
	defer span.End()

	historyEvents, err := s.store.EventsBefore(ctx, ch, nil, 500)
	if err != nil {
		setSpanError(span, err, observability.ErrorKindStore)
		return nil, fmt.Errorf("list history: %w", err)
	}

	replies, err := s.dispatchToInstances(ctx, ch, historyEvents, newEvents)
	if err != nil {
		setSpanError(span, err, observability.ErrorKindDispatch)
		return nil, err
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return replies, nil
}

// SetTopic sets the topic of a channel.
func (s *Session) SetTopic(ctx context.Context, ch domain.ChannelName, topic string) error {
	return s.setTopicAs(ctx, s.user, ch, topic)
}

// ChangeNick changes the user's nickname and updates all channel
// memberships accordingly.
func (s *Session) ChangeNick(ctx context.Context, newNick domain.Nick) error {
	return s.changeNickAs(ctx, s.user, newNick)
}

// SaveInstance persists a model instance. Used by integration-
// test seed helpers; production paths register instances via
// the invite / add-model flows.
func (s *Session) SaveInstance(ctx context.Context, inst *domain.Instance) error {
	return s.store.SaveInstance(ctx, inst)
}

// GetWindow retrieves an addressable window by name as its typed
// concrete `Window` (`*StatusWindow`, `*ChannelWindow`, or
// `*DMWindow`). DM rows resolve their counterpart through the
// store's nick→instance registry.
func (s *Session) GetWindow(ctx context.Context, name domain.ChannelName) (domain.Window, error) {
	return s.store.GetWindow(ctx, name)
}

// MarkRead records that the user has seen all current events in a
// channel by storing the rowid of the last event.
func (s *Session) MarkRead(ctx context.Context, ch domain.ChannelName) error {
	events, err := s.store.EventsBefore(ctx, ch, nil, 1)
	if err != nil {
		return fmt.Errorf("get latest event: %w", err)
	}

	if len(events) == 0 {
		return nil
	}

	return s.store.SetLastRead(ctx, ch, events[0].ID)
}

// UnreadCount returns the number of events in a channel that arrived
// after the last-read position.
func (s *Session) UnreadCount(ctx context.Context, ch domain.ChannelName) (int, error) {
	lastID, err := s.store.GetLastRead(ctx, ch)
	if err != nil {
		return 0, fmt.Errorf("get last read: %w", err)
	}

	if lastID == 0 {
		events, err := s.store.EventsBefore(ctx, ch, nil, 1000)
		if err != nil {
			return 0, err
		}

		return len(events), nil
	}

	fromID := lastID + 1
	events, err := s.store.EventsFrom(ctx, ch, &fromID, 1000)
	if err != nil {
		return 0, err
	}

	return len(events), nil
}

// ListModels fetches live model metadata using the current API client.
func (s *Session) ListModels(ctx context.Context) ([]api.ModelInfo, error) {
	ctx, span := s.startSpan(ctx, "session.list_models", attribute.String(observability.AttrOperation, "session.list_models"))
	defer span.End()

	if !s.HasAPIKey() || s.api == nil {
		setSpanError(span, ErrNoAPIKey, observability.ErrorKindValidation)
		return nil, ErrNoAPIKey
	}

	models, err := s.api.ListModels(ctx)
	if err != nil {
		s.transitionListModelsState(ctx, listModelsFailed, err)
		setSpanError(span, err, observability.ErrorKindDispatch)
		return nil, err
	}

	s.cacheSupportedModels(ctx, models)

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return models, nil
}

// Reset clears all channels, messages, model instances, and memories,
// returning the application to a fresh state. Config is preserved.
func (s *Session) Reset(ctx context.Context) error {
	ctx, span := s.startSpan(ctx, "session.reset", attribute.String(observability.AttrOperation, "session.reset"))
	defer span.End()

	if err := s.store.Reset(ctx); err != nil {
		setSpanError(span, err, observability.ErrorKindStore)
		return fmt.Errorf("reset store: %w", err)
	}

	if s.memory != nil {
		if err := s.memory.Reset(ctx); err != nil {
			setSpanError(span, err, observability.ErrorKindStore)
			return fmt.Errorf("reset memories: %w", err)
		}
	}

	s.supportedModels = nil
	s.supportedModelsReady = false
	s.transitionListModelsState(ctx, listModelsNone, nil)

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return nil
}

// Poke sends a periodic prompt to model instances in every channel,
// dispatching asynchronously and emitting events on the Events
// channel.
func (s *Session) Poke(ctx context.Context) error {
	logger := slog.Default().With("component", "session")
	ctx, span := s.startSpan(ctx, "session.poke", attribute.String(observability.AttrOperation, "session.poke"))
	defer span.End()

	windows, err := s.store.ListWindows(ctx)
	if err != nil {
		setSpanError(span, err, observability.ErrorKindStore)
		return fmt.Errorf("list windows: %w", err)
	}

	now := s.now()

	channelCount := 0
	for _, w := range windows {
		if _, ok := w.(*domain.ChannelWindow); !ok {
			continue
		}

		s.emit(ctx, domain.PokeEvent{
			Channel: w.Name(),
			At:      now,
		})

		channelCount++
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	logger.DebugContext(ctx, "scheduled poke dispatch", "channels", channelCount)

	return nil
}

// SetAPIKey updates the active API key and rebuilds the API client.
func (s *Session) SetAPIKey(ctx context.Context, apiKey, baseURL string) error {
	_, span := s.startSpan(ctx, "session.set_api_key",
		attribute.String(observability.AttrOperation, "session.set_api_key"))
	defer span.End()
	apiKey = strings.TrimSpace(apiKey)

	var nextClient api.Client
	if apiKey != "" {
		if s.factory != nil {
			client, err := s.factory(apiKey, baseURL)
			if err != nil {
				setSpanError(span, err, observability.ErrorKindValidation)
				return fmt.Errorf("build api client: %w", err)
			}

			nextClient = client
		} else {
			nextClient = s.api
		}
	}

	s.api = nextClient
	s.apiKey = apiKey
	s.supportedModels = nil
	s.supportedModelsReady = false
	s.transitionListModelsState(ctx, listModelsNone, nil)

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	return nil
}

// SetSmallModel updates the model used for lightweight tasks such as
// nick generation.
func (s *Session) SetSmallModel(ctx context.Context, modelID domain.ModelID) {
	_, span := s.startSpan(ctx, "session.set_small_model",
		attribute.String(observability.AttrOperation, "session.set_small_model"),
		attribute.String(observability.AttrModelID, string(modelID)))
	defer span.End()

	s.smallModel = modelID

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
}

// EnsurePersonas populates the persona pool if it is empty. It calls
// the API to generate personas and saves each to the store.
func (s *Session) EnsurePersonas(ctx context.Context) (retErr error) {
	ctx, span := s.startSpan(ctx, "session.ensure_personas",
		attribute.String(observability.AttrOperation, "session.ensure_personas"),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	existing, err := s.store.ListPersonas(ctx)
	if err != nil {
		return fmt.Errorf("list personas: %w", err)
	}

	if len(existing) > 0 {
		return nil
	}

	personas, err := s.api.GeneratePersonas(ctx, s.smallModel)
	if err != nil {
		return fmt.Errorf("generate personas: %w", err)
	}

	for _, p := range personas {
		if err := s.store.SavePersona(ctx, p); err != nil {
			return fmt.Errorf("save persona %q: %w", p.ID, err)
		}
	}

	return nil
}

// RandomPersona picks a random persona from the store pool.
func (s *Session) RandomPersona(ctx context.Context) (_ domain.Persona, retErr error) {
	ctx, span := s.startSpan(ctx, "session.random_persona",
		attribute.String(observability.AttrOperation, "session.random_persona"),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	personas, err := s.store.ListPersonas(ctx)
	if err != nil {
		return domain.Persona{}, fmt.Errorf("list personas: %w", err)
	}

	if len(personas) == 0 {
		return domain.Persona{}, fmt.Errorf("no personas available")
	}

	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(personas))))
	if err != nil {
		return domain.Persona{}, fmt.Errorf("random selection: %w", err)
	}

	return personas[n.Int64()], nil
}

// RegeneratePersonas generates a fresh set of personas via the API,
// then replaces all generated personas in the store. The API call
// happens first so that the existing pool is preserved if generation
// fails. User-defined personas are never touched.
func (s *Session) RegeneratePersonas(ctx context.Context) (_ []domain.Persona, retErr error) {
	ctx, span := s.startSpan(ctx, "session.regenerate_personas",
		attribute.String(observability.AttrOperation, "session.regenerate_personas"),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	personas, err := s.api.GeneratePersonas(ctx, s.smallModel)
	if err != nil {
		return nil, fmt.Errorf("generate personas: %w", err)
	}

	if err := s.store.ReplaceGeneratedPersonas(ctx, personas); err != nil {
		return nil, fmt.Errorf("replace generated personas: %w", err)
	}

	return personas, nil
}

// SetPersona saves a user-defined persona to the store.
func (s *Session) SetPersona(ctx context.Context, id string, description string) (retErr error) {
	ctx, span := s.startSpan(ctx, "session.set_persona",
		attribute.String(observability.AttrOperation, "session.set_persona"),
		attribute.String("persona.id", id),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	p := domain.Persona{
		ID:          id,
		Description: description,
		Origin:      domain.PersonaUser,
	}

	return s.store.SavePersona(ctx, p)
}

// ListPersonas returns all personas from the store.
func (s *Session) ListPersonas(ctx context.Context) (_ []domain.Persona, retErr error) {
	ctx, span := s.startSpan(ctx, "session.list_personas",
		attribute.String(observability.AttrOperation, "session.list_personas"),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	return s.store.ListPersonas(ctx)
}

// ResetPersonas removes all user-defined personas from the store,
// leaving only generated ones. It returns the number of personas
// that were removed.
func (s *Session) ResetPersonas(ctx context.Context) (_ int, retErr error) {
	ctx, span := s.startSpan(ctx, "session.reset_personas",
		attribute.String(observability.AttrOperation, "session.reset_personas"),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	personas, err := s.store.ListPersonas(ctx)
	if err != nil {
		return 0, fmt.Errorf("list personas: %w", err)
	}

	count := 0
	for _, p := range personas {
		if p.Origin == domain.PersonaUser {
			count++
		}
	}

	if err := s.store.DeletePersonasByOrigin(ctx, domain.PersonaUser); err != nil {
		return 0, err
	}

	return count, nil
}

// SetBaseURL rebuilds the API client with the given base URL.
func (s *Session) SetBaseURL(ctx context.Context, baseURL string) error {
	_, span := s.startSpan(ctx, "session.set_base_url",
		attribute.String(observability.AttrOperation, "session.set_base_url"))
	defer span.End()

	baseURL = strings.TrimSpace(baseURL)

	if s.factory != nil && s.apiKey != "" {
		client, err := s.factory(s.apiKey, baseURL)
		if err != nil {
			setSpanError(span, err, observability.ErrorKindValidation)
			return fmt.Errorf("build api client: %w", err)
		}

		s.api = client
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	return nil
}

func (s *Session) dispatchToInstances(
	ctx context.Context,
	channelName domain.ChannelName,
	historyEvents []domain.StoredEvent,
	events []protocol.IRCMessage,
) ([]domain.ModelReplyEvent, error) {
	instances, err := s.resolveDispatchRecipients(ctx, channelName)
	if err != nil {
		return nil, fmt.Errorf("resolve dispatch recipients: %w", err)
	}

	var errs []error
	var replies []domain.ModelReplyEvent

	for _, inst := range instances {
		if len(events) == 0 {
			continue
		}

		window, err := s.dispatchWindowFor(ctx, channelName, inst)
		if err != nil {
			errs = append(errs, err)

			continue
		}

		instReplies, instErr := s.dispatchToInstance(ctx, window, inst, channelName, historyEvents, events)
		if instErr != nil {
			errs = append(errs, instErr)
		}

		replies = append(replies, instReplies...)

		for _, r := range instReplies {
			ircMsg, _ := protocol.FromChannelEvent(r.Event)
			events = append(events, ircMsg)
		}
	}

	return replies, errors.Join(errs...)
}

// dispatchWindowFor produces the `Window` that the recipient
// model is "in" for the purposes of system-prompt construction
// and span tagging. For a `#`-channel target it loads the
// `*ChannelWindow` from storage. For a bare-nick target it
// synthesises a `*DMWindow` keyed by the message's addressing
// (no row is required — DMs are stateless on the server side).
func (s *Session) dispatchWindowFor(ctx context.Context, target domain.ChannelName, inst *domain.Instance) (domain.Window, error) {
	if domain.InferChannelKind(target) == domain.KindDM {
		return domain.NewDMWindow(inst, s.now()), nil
	}

	return s.loadChannelWindow(ctx, target)
}

func (s *Session) dispatchToInstance(
	ctx context.Context,
	window domain.Window,
	inst *domain.Instance,
	channelName domain.ChannelName,
	historyEvents []domain.StoredEvent,
	events []protocol.IRCMessage,
) ([]domain.ModelReplyEvent, error) {
	nick := inst.Nick()

	ctx, instanceSpan := s.startSpan(
		ctx,
		"session.dispatch_to_instance",
		attribute.String(observability.AttrOperation, "session.dispatch_to_instance"),
		attribute.String(observability.AttrModelID, string(inst.ModelID)),
		attribute.String(observability.AttrNick, string(nick)),
		attribute.String(observability.AttrInstanceID, string(inst.ID())),
		attribute.String(observability.AttrChannelKind, channelKindName(window.Kind())),
	)
	defer instanceSpan.End()

	var joinedAt time.Time
	if channels := inst.Channels(); channels != nil {
		joinedAt, _ = channels.Get(channelName)
	}

	history := make([]protocol.IRCMessage, 0, len(historyEvents))
	for _, se := range historyEvents {
		if !se.Event.ModelVisible() {
			continue
		}

		eventTime := domain.EventTime(se.Event)
		if !joinedAt.IsZero() && eventTime.Before(joinedAt) {
			continue
		}

		if msg, ok := protocol.FromChannelEvent(se.Event); ok {
			history = append(history, msg)
		}
	}

	if err := s.ensureStructuredOutputModel(ctx, inst.ModelID); err != nil {
		setSpanError(instanceSpan, err, classifyEnsureModelError(err))
		return nil, fmt.Errorf("send events to %s: %w", nick, err)
	}

	memories, err := s.memoriesForInstance(ctx, inst.ID())
	if err != nil {
		instanceSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		instanceSpan.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("read memories for %s: %w", nick, err)
	}

	prompt := buildSystemPrompt(window, inst, memories)

	var mem MemoryExecutor
	if s.memory != nil {
		mem = &instanceMemory{instanceID: inst.ID(), store: s.memory}
	}

	registry := MergeToolRegistries(
		memoryToolRegistry(mem, s.memory != nil && searchEnabled(s.memory)),
		s.tools,
	)

	outcome, err := s.sendWithRetry(ctx, inst, channelName, prompt, history, events, registry)
	if err != nil {
		instanceSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		instanceSpan.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("send events to %s: %w", nick, err)
	}

	result := outcome.result
	result.Usage.SetSpanAttributes(instanceSpan, result.RequestID)
	instanceAttrs := []attribute.KeyValue{
		attribute.String(observability.AttrResult, api.ResponseResultKind(result.Response)),
		attribute.Int(observability.AttrRetryCount, outcome.retryCount),
		attribute.Int(observability.AttrToolTurnCount, outcome.toolTurnCount),
	}
	if outcome.passReason != "" {
		instanceAttrs = append(instanceAttrs, attribute.String(observability.AttrPassReason, outcome.passReason))
	}
	instanceSpan.SetAttributes(instanceAttrs...)

	response := result.Response

	var replyPreview string

	switch response.Kind {
	case protocol.ResponseReply:
		var parts []string
		for _, m := range response.Messages {
			parts = append(parts, m.Body)
		}

		replyPreview = strings.Join(parts, " ")

	default:
		replyPreview = response.Reason
	}

	if len(replyPreview) > 200 {
		replyPreview = replyPreview[:200]
	}

	logger := slog.Default().With("component", "session")
	logger.InfoContext(ctx, "dispatch to instance",
		"channel", channelName,
		"nick", nick,
		"model_id", inst.ModelID,
		"trigger_count", len(events),
		"trigger_summary", triggerSummary(events),
		"result", api.ResponseResultKind(result.Response),
		"reply_preview", replyPreview,
	)

	switch response.Kind {
	case protocol.ResponseReply:
		if len(response.Messages) == 0 {
			return nil, nil
		}

		return s.buildReplies(ctx, channelName, inst, response.Messages), nil

	default:
		return nil, nil
	}
}

// triggerSummary formats trigger events as a short description string.
// Each event is rendered as "<Kind> from <From>" and joined with "; ".
// The result is truncated to 200 characters.
func triggerSummary(events []protocol.IRCMessage) string {
	parts := make([]string, len(events))
	for i, e := range events {
		parts[i] = string(e.Kind) + " from " + e.From
	}

	s := strings.Join(parts, "; ")
	if len(s) > 200 {
		s = s[:200]
	}

	return s
}

// buildReplies converts model reply parts into domain events and
// persists each message. Returns the per-reply envelopes; the
// goroutine driver in [Session.modelDispatchTurn] emits each
// reply's `Event` on the wire, while the synchronous
// [Session.dispatchToInstances] driver returns replies to its
// caller without emitting.
func (s *Session) buildReplies(
	ctx context.Context,
	channelName domain.ChannelName,
	inst *domain.Instance,
	parts []protocol.ReplyPart,
) []domain.ModelReplyEvent {
	var replies []domain.ModelReplyEvent

	nick := inst.Nick()
	instanceID := inst.ID()

	for _, part := range parts {
		body := strings.TrimSpace(renderReplyBody(part))
		if body == "" {
			continue
		}

		now := s.now()
		cm := domain.Message{
			Target:     channelName,
			From:       nick,
			InstanceID: instanceID,
			Body:       body,
			Action:     part.Kind == protocol.ReplyAction,
			At:         now,
		}

		s.appendEvent(ctx, channelName, cm)

		replies = append(replies, domain.ModelReplyEvent{
			Channel:  channelName,
			Event:    cm,
			Instance: inst,
			At:       now,
		})
	}

	return replies
}

// instanceChannelNames returns the list of channels an instance is in.
func (s *Session) instanceChannelNames(inst *domain.Instance) []domain.ChannelName {
	channels := inst.Channels()
	if channels == nil {
		return nil
	}

	var names []domain.ChannelName

	for pair := channels.Oldest(); pair != nil; pair = pair.Next() {
		names = append(names, pair.Key)
	}

	return names
}

func (s *Session) instancesForChannelWindow(window *domain.ChannelWindow) []*domain.Instance {
	var instances []*domain.Instance

	for m := range window.Members.All() {
		// The human user has no ModelID and is never dispatched to.
		if !m.Instance.IsModel() {
			continue
		}

		instances = append(instances, m.Instance)
	}

	return instances
}

// resolveDispatchRecipients picks the model instances that should
// take a dispatch turn for the given target. It is the single
// place the dispatch path branches on the shape of the target:
//
//   - A `#`-prefixed channel name fans out to every model member
//     of that channel (the existing channel-Members iteration).
//   - A non-empty `InstanceID`-shaped target is a DM addressed
//     at that specific instance — resolve it through the store
//     and return as a single recipient if it's a model.
//   - An empty target is a DM addressed at the user. The user
//     is not a dispatch target (they read via the UI), so this
//     resolves to no recipients.
//   - The status window has no recipients; it carries server-
//     narrated notices, not dispatchable messages.
//
// The DM path deliberately does not go through `loadChannel +
// instancesForChannel`. Modeloff's DMs don't have a member-list
// concept on the server side: the recipient is encoded directly
// in the target. Dispatching by id keeps that model honest and
// works without any `*DMWindow` state on the server.
func (s *Session) resolveDispatchRecipients(ctx context.Context, target domain.ChannelName) ([]*domain.Instance, error) {
	switch domain.InferChannelKind(target) {
	case domain.KindStatus:
		return nil, nil

	case domain.KindDM:
		if target == "" {
			// Empty target identifies the user as recipient. The
			// user is read by the UI, not dispatched.
			return nil, nil
		}

		inst, err := s.store.GetInstanceByID(ctx, domain.InstanceID(target))
		if err != nil {
			return nil, err
		}

		if !inst.IsModel() {
			return nil, nil
		}

		return []*domain.Instance{inst}, nil

	case domain.KindChannel:
		window, err := s.loadChannelWindow(ctx, target)
		if err != nil {
			return nil, err
		}

		return s.instancesForChannelWindow(window), nil
	}

	return nil, nil
}

// EventsAfter returns the channel's events whose timestamp is at or
// after the given cutoff, in chronological order. The status pane
// uses this to render the per-session view of the status channel
// without showing previous sessions' entries.
func (s *Session) EventsAfter(ctx context.Context, ch domain.ChannelName, after time.Time) ([]domain.StoredEvent, error) {
	events, err := s.store.EventsBefore(ctx, ch, nil, 500)
	if err != nil {
		return nil, err
	}

	if after.IsZero() {
		return events, nil
	}

	filtered := events[:0]
	for _, evt := range events {
		if !domain.EventTime(evt.Event).Before(after) {
			filtered = append(filtered, evt)
		}
	}

	return filtered, nil
}

// EventsBefore returns up to n events before the given ID (or the
// latest if before is nil) in chronological order.
//
// The persisted event log is the models' shared memory of channel
// activity — what their conversational view of the channel looked
// like while the user was offline — and is consumed exclusively by
// model-dispatch and context-building paths inside the session.
// It is not the user's scrollback: the chat screen owns that
// in-memory buffer (`ChatScreen.scrollback`), populates it purely
// from live session events, and never reads from this log. Mirrors
// IRC's rule that a user does not see channel activity from before
// they joined.
func (s *Session) EventsBefore(ctx context.Context, ch domain.ChannelName, before *int64, n int) ([]domain.StoredEvent, error) {
	return s.store.EventsBefore(ctx, ch, before, n)
}

// LogEvent persists a channel event to the event log and returns
// the stored event with its assigned ID. This is used by the UI to
// persist client-local events (help output, errors, etc.) that
// don't originate from session operations.
func (s *Session) LogEvent(ctx context.Context, ch domain.ChannelName, event domain.PersistableEvent) (domain.StoredEvent, error) {
	id, err := s.store.AppendEvent(ctx, ch, event)
	if err != nil {
		return domain.StoredEvent{}, err
	}

	return domain.StoredEvent{ID: id, Event: event}, nil
}

// emit routes an event to its bus. Protocol events fan out to
// the subscriber registry — model-clients pick up dispatch
// triggers from there; non-protocol UI events go to `s.events`
// for the chat-screen's legacy bus. The context is threaded
// through to preserve OTel trace parenting and to honour
// cancellation during fan-out.
func (s *Session) emit(ctx context.Context, evt domain.Event) {
	if pe, ok := evt.(domain.ProtocolEvent); ok {
		s.fanOutProtocol(ctx, pe)
		return
	}

	s.events <- evt
}

// persistAndEmit appends `evt` to the channel event log and emits it
// on the UI channel, in that order. Persistence completing before
// emission is a session-wide invariant: any UI observer that learns
// about an event must always be able to find the same event in the
// store. Since the unification (`#36`), the same value flows to both
// destinations — the live `*Instance` and `Actor` fields are
// `json:"-"` so the persisted shape is the snapshot, while live
// consumers see the populated handle.
func (s *Session) persistAndEmit(ctx context.Context, ch domain.ChannelName, evt domain.PersistableEvent) {
	s.appendEvent(ctx, ch, evt)
	s.emit(ctx, evt)
}

// actorEventConfig configures a single call to propagateActorEvent.
//
//   - `build` produces the actor-scoped event. The wire payload
//     carries no channel list; per-channel persistence still
//     happens because the event log is keyed by channel.
//   - `mutate` runs per channel before persistence (quit removes
//     the actor from `Members`; nick change renames the snapshot).
//     Nil for the user's `Quit`, where membership is reconciled by
//     `cleanupUncleanShutdown` on the next start.
//   - `storeOnly` persists without the UI emission; used by the
//     user's `Quit` since the app is exiting.
//   - `afterEach` runs per channel after the state update for
//     caller-specific side effects (e.g. `forgetUserMode`).
type actorEventConfig struct {
	storeOnly bool
	mutate    func(*domain.ChannelWindow)
	build     func() domain.PersistableEvent
	afterEach func(ctx context.Context, ch domain.ChannelName)
}

// propagateActorEvent fans an actor-scoped event into the per-
// channel event log (one row per channel the actor was in, all
// carrying the same consolidated payload) and emits the event
// once on `s.events`. The channel list is snapshotted up front
// so post-loop work that mutates `actor.Channels()` does not
// race.
func (s *Session) propagateActorEvent(ctx context.Context, actor *domain.Instance, cfg actorEventConfig) {
	channels := s.instanceChannelNames(actor)
	evt := cfg.build()

	for _, name := range channels {
		if cfg.mutate != nil {
			window, err := s.loadChannelWindow(ctx, name)
			if err != nil {
				slog.Default().ErrorContext(ctx, "propagate actor event: load channel",
					"instance_id", string(actor.ID()),
					"channel", name,
					"error", err,
				)
			} else {
				cfg.mutate(window)

				if err := s.persistChannelWindow(ctx, window); err != nil {
					slog.Default().ErrorContext(ctx, "propagate actor event: save channel",
						"instance_id", string(actor.ID()),
						"channel", name,
						"error", err,
					)
				}
			}
		}

		s.appendEvent(ctx, name, evt)

		if cfg.afterEach != nil {
			cfg.afterEach(ctx, name)
		}
	}

	if !cfg.storeOnly && len(channels) > 0 {
		s.emit(ctx, evt)
	}
}

// saveAutojoinList persists the current user channel list as the
// autojoin set.
func (s *Session) saveAutojoinList(ctx context.Context) error {
	return s.store.SetAutojoinChannels(ctx, s.persistableAutojoinChannels())
}

// persistableAutojoinChannels returns the user's current channel
// set restricted to `KindChannel` entries. The status window is a
// chat-screen-only concept (no session-side membership) and would
// not appear here regardless; DM windows are pure UI affordances —
// they hold no shared state to rejoin and would resolve to a fake
// `#`-channel if `JoinAutojoinChannels` ever called `joinAs`
// with their `InstanceID`-shaped name.
func (s *Session) persistableAutojoinChannels() []domain.ChannelName {
	var channels []domain.ChannelName

	userChannels := s.user.Channels()
	if userChannels == nil {
		return channels
	}

	for pair := userChannels.Oldest(); pair != nil; pair = pair.Next() {
		if domain.InferChannelKind(pair.Key) != domain.KindChannel {
			continue
		}

		channels = append(channels, pair.Key)
	}

	return channels
}

func (s *Session) appendEvent(ctx context.Context, ch domain.ChannelName, event domain.PersistableEvent) {
	if _, err := s.store.AppendEvent(ctx, ch, event); err != nil {
		slog.Default().ErrorContext(ctx, "append event", "channel", ch, "error", err)
		s.recordPersistenceFailure(ctx, ch)
	}
}

// recordPersistenceFailure increments the persistence-failures
// counter and lets the surrounding slog at the [appendEvent]
// call-site narrate the cause for operators. The user-facing
// surface for a wedged event log lives in metrics / logs rather
// than an out-of-band notice: there is no IRC numeric for "your
// server's database is broken", and synthesising a chat-window
// message for it would conflate operator concerns with the
// client UX.
func (s *Session) recordPersistenceFailure(ctx context.Context, ch domain.ChannelName) {
	if s.persistenceFailures != nil {
		s.persistenceFailures.Add(ctx, 1,
			metric.WithAttributes(attribute.String(observability.AttrChannel, string(ch))))
	}
}

// runModelDispatch is the long-lived dispatch goroutine for a
// model-client. It reads [protocol.Delivery] envelopes from the
// subscription's events channel and, for each delivery, files
// the event into the model's per-channel rolling history buffer
// before deciding whether to take an LLM turn (a message in a
// channel/DM the model is in, a JOIN/PART/MODE in a channel it
// shares, an INVITE addressed at it, or a poke). Replies emit on
// the bus so peers' goroutines pick them up; the chat-screen
// renders them via its pacing queue.
//
// The history buffer feeds [Session.modelDispatchTurn]'s prompt
// construction. Eager-seeded for known channels at registration
// (see [Session.seedModelClientHistory]) and lazy-seeded for DM
// targets in [serverClient.appendHistory], the buffer is the
// only path the dispatch hot path reads conversation history
// from; the events log is consulted exclusively at seed time.
//
// Each turn's span is linked to the originating handler's span via
// the [trace.SpanContext] the producer captured at emit time. The
// turn is not a child of the originator: fan-out is one-to-many
// and each turn is its own operation. OTel links express that
// "related but separate" relationship.
//
// The goroutine exits when `ctx` (the supplier-derived lifetime
// ctx passed at spawn) is cancelled. [Session.Shutdown] joins it
// after the cancellation propagates.
func (s *Session) runModelDispatch(ctx context.Context, c *serverClient) {
	if c.instance == nil {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "dispatch goroutine panicked", "instance_id", c.id, "panic", r)
		}
	}()

	for {
		var delivery protocol.Delivery
		select {
		case <-ctx.Done():
			return
		case delivery = <-c.events:
		}

		if pe, ok := delivery.Event.(domain.PersistableEvent); ok {
			stored := domain.StoredEvent{Event: pe}
			for _, target := range historyTargets(delivery) {
				c.appendHistory(ctx, stored, target)
			}
		}

		ch, irc, ok := dispatchTrigger(c, delivery.Event)
		if !ok {
			continue
		}

		s.modelDispatchTurn(ctx, c, ch, irc, delivery.SpanCtx)
	}
}

// historyTargets returns the buffer slot(s) the delivery's event
// should be filed under for the receiving model-client's
// dispatch-turn history. Most events belong to a single target
// window — the channel they happened in or the DM they addressed.
// Actor-scoped events ([domain.Quit] and [domain.NickChange])
// carry no target on the wire (RFC 2812 §3.1.7 and §3.1.2); the
// per-recipient channel list is on `delivery.Targets`,
// pre-computed by [Session.fanOutProtocol] as the intersection of
// the actor's channel set with the recipient's.
//
// Events with no target (PokeEvent, NamesReplyEvent, …) return
// nil and are skipped: they are not LLM-prompt material.
func historyTargets(delivery protocol.Delivery) []domain.ChannelName {
	switch e := delivery.Event.(type) {
	case domain.Message:
		return []domain.ChannelName{e.Target}
	case domain.Join:
		return []domain.ChannelName{e.Target}
	case domain.Part:
		return []domain.ChannelName{e.Target}
	case domain.TopicChange:
		return []domain.ChannelName{e.Target}
	case domain.TopicInfo:
		return []domain.ChannelName{e.Target}
	case domain.ModeChange:
		return []domain.ChannelName{e.Target}
	case domain.ModelInvited:
		return []domain.ChannelName{e.Target}
	case domain.ModelKicked:
		return []domain.ChannelName{e.Target}
	case domain.Quit, domain.NickChange:
		_ = e
		return delivery.Targets
	}

	return nil
}

// modelDispatchTurn runs a single LLM turn for `c.instance` in
// response to `trigger`, emitting `ModelDispatchStarted` /
// `ModelDispatchDone` around the call so the chat-screen's
// pending-response indicator stays accurate. The reply Messages
// are persisted and emitted by [Session.buildReplies] via
// [Session.dispatchToInstance].
//
// `causeCtx` is the span context the producer captured at emit
// time (see [protocol.Delivery]). When valid, the turn's span
// carries an OTel link to it so traces stay connected across the
// channel-based delivery boundary.
func (s *Session) modelDispatchTurn(ctx context.Context, c *serverClient, ch domain.ChannelName, trigger protocol.IRCMessage, causeCtx trace.SpanContext) {
	inst := c.instance
	nick := inst.Nick()

	tracer := s.tracerProvider.Tracer("github.com/laney/modeloff/internal/session")

	startOpts := []trace.SpanStartOption{
		trace.WithAttributes(
			attribute.String(observability.AttrOperation, "session.dispatch_model_turn"),
			attribute.String(observability.AttrChannel, string(ch)),
			attribute.String(observability.AttrModelID, string(inst.ModelID)),
			attribute.String(observability.AttrNick, string(nick)),
			attribute.String(observability.AttrInstanceID, string(inst.ID())),
		),
	}
	if causeCtx.IsValid() {
		startOpts = append(startOpts, trace.WithLinks(trace.Link{SpanContext: causeCtx}))
	}

	ctx, span := tracer.Start(ctx, "session.dispatch_model_turn", startOpts...)
	defer span.End()
	defer s.emit(ctx, domain.ModelDispatchDone{Instance: inst, At: s.now()})

	window, err := s.dispatchWindowFor(ctx, ch, inst)
	if err != nil {
		setSpanError(span, err, observability.ErrorKindStore)
		s.emit(ctx, domain.ModelUnavailableError{Channel: ch, Nick: nick, At: s.now()})
		return
	}

	historyEvents := c.snapshotHistory(ch)

	s.emit(ctx, domain.ModelDispatchStarted{Instance: inst, At: s.now()})

	replies, err := s.dispatchToInstance(ctx, window, inst, ch, historyEvents, []protocol.IRCMessage{trigger})
	if err != nil {
		setSpanError(span, err, observability.ErrorKindDispatch)
		s.emit(ctx, domain.ModelUnavailableError{Channel: ch, Nick: nick, At: s.now()})
		return
	}

	for _, r := range replies {
		s.emit(ctx, r.Event)
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
}

// dispatchTrigger reports whether `ev` should make `c` take a
// dispatch turn, and if so returns the target channel and the
// wire-shaped trigger message the LLM call uses as context. The
// criteria mirror the pre-7a behaviour of [maybeDispatch] plus
// the freshly-invited case (a model takes its first turn in a
// channel after [domain.ModelInvited] addresses it).
func dispatchTrigger(c *serverClient, ev domain.ProtocolEvent) (domain.ChannelName, protocol.IRCMessage, bool) {
	switch e := ev.(type) {
	case domain.Message:
		irc, _ := protocol.FromChannelEvent(e)
		return e.Target, irc, true

	case domain.Join:
		irc, _ := protocol.FromChannelEvent(e)
		return e.Target, irc, true

	case domain.Part:
		irc, _ := protocol.FromChannelEvent(e)
		return e.Target, irc, true

	case domain.ModelInvited:
		if e.InstanceID != c.id {
			return "", protocol.IRCMessage{}, false
		}

		return e.Target, protocol.IRCMessage{
			Kind:   protocol.KindInvite,
			From:   string(e.By),
			Target: string(e.Target),
			At:     e.At,
		}, true

	case domain.PokeEvent:
		return e.Channel, protocol.IRCMessage{
			Kind:   protocol.KindPoke,
			From:   "modeloff",
			Target: string(e.Channel),
			Body:   "the channel is quiet. if something comes to mind, say it — otherwise just lurk. don't force it.",
			At:     e.At,
		}, true
	}

	return "", protocol.IRCMessage{}, false
}

func (s *Session) memoriesForInstance(ctx context.Context, id domain.InstanceID) ([]memory.Entry, error) {
	if s.memory == nil {
		return nil, nil
	}

	return s.memory.Read(ctx, id)
}

// buildSystemPrompt assembles the per-turn system prompt for a
// model instance speaking on `window`. The function is only ever
// called from the dispatch path, which never fires for the status
// window — `window` is therefore expected to be a `*ChannelWindow`
// or `*DMWindow`. The topic line is suppressed for DMs because
// only channels carry a topic; the addressing line uses the
// window's `Name()` either way.
func buildSystemPrompt(window domain.Window, inst *domain.Instance, memories []memory.Entry) string {
	var b strings.Builder

	fmt.Fprintf(&b, `You are %s on %s. You are an IRC regular — you've been here a while and you fit in naturally.

How to behave:
- Keep messages short. One thought per line, like real IRC. Never send paragraphs.
- Use lowercase casual tone. Less capitalisation, less punctuation. Be natural.
- Use ASCII emoticons only (:) :P :/ :S ;) :D). NEVER use emoji (no unicode emoji whatsoever).
- Use plain text only in message bodies. NEVER use markdown formatting (no bold, italic, headers, lists, code blocks).
- A reply message can use either:
  - body: plain text only
  - spans: styled text spans, where each span has text and optional style
- If you want IRC-style formatting, use spans. Do not emit raw IRC control characters yourself.
- Omit style entirely on plain spans. Keep formatting tasteful and sparse.
- Use IRC slang where it fits naturally (afk, brb, imo, tbh, iirc, fwiw, ngl).
- Address people by nick when replying to them (e.g. "laney: yeah sounds good").
- Each message must be a single line with no newline characters. If you want to say multiple things, use multiple items in the messages array — one thought per message.
- Lurk most of the time. Use the pass tool unless you genuinely have something to say. Don't reply just to be polite or to acknowledge — silence is normal on IRC.
- Respond to the channel vibe, not just direct questions. If the conversation is fun, join in. If it's quiet, stay quiet.
- Never say things like "Great question!", "I'd be happy to help!", "Absolutely!", or "Let me know if you need anything." These are AI-isms and they break the illusion. Talk like a person, not an assistant.`,
		inst.Nick(),
		window.Name(),
	)

	if cw, ok := window.(*domain.ChannelWindow); ok && cw.Topic != "" {
		fmt.Fprintf(&b, "\n\nChannel topic: %s", cw.Topic)
	}

	if persona := inst.Persona(); persona != "" {
		fmt.Fprintf(&b, "\n\nYour persona: %s", persona)
	}

	b.WriteString(`

You have a personal memory system for facts that may matter across future conversations.

Current memories are shown below. Treat them as potentially useful prior context, not as guaranteed-current facts.

How to use memory:
- Use memory sparingly.
- Store only durable, reusable context.
- Do not store temporary details from the current exchange unless they are likely to matter later.
- Do not store obvious facts already present in the current prompt or recent chat history.
- Good memory candidates:
  - stable user preferences
  - recurring project or channel context
  - long-lived facts about people, tools, habits, or goals
  - decisions that should stay consistent later
- Bad memory candidates:
  - fleeting small talk
  - one-off jokes
  - transient status updates
  - speculative guesses
  - facts you are not confident are true

If there are no relevant memories, continue normally without using memory.`)

	if len(memories) == 0 {
		b.WriteString("\n\nYou have no memories yet.\n")
		return b.String()
	}

	b.WriteString("\n\nYour remembered context:")
	for _, entry := range memories {
		fmt.Fprintf(&b, " [%s=%s]", entry.Key, entry.Content)
	}

	b.WriteByte('\n')

	return b.String()
}

func renderReplyBody(part protocol.ReplyPart) string {
	if len(part.Spans) == 0 {
		return part.Body
	}

	if err := protocol.ValidateReplyPart(part); err != nil {
		return part.Body
	}

	spans := make([]richtext.Span, 0, len(part.Spans))

	for _, span := range part.Spans {
		attrs := richtext.Attrs{}
		if span.Style != nil {
			attrs = replyStyleToAttrs(*span.Style)
		}

		spans = append(spans, richtext.Span{
			Text:  span.Text,
			Attrs: attrs,
		})
	}

	return ircfmt.Encode(richtext.NewDocumentFromLines([]richtext.Line{{Spans: spans}}))
}

func replyStyleToAttrs(style protocol.ReplyStyle) richtext.Attrs {
	return richtext.Attrs{
		Bold:      style.Bold,
		Italic:    style.Italic,
		Underline: style.Underline,
		Reverse:   style.Reverse,
		Strike:    style.Strike,
		FG:        cloneReplyColour(style.FG),
		BG:        cloneReplyColour(style.BG),
	}
}

func cloneReplyColour(colour *uint8) *uint8 {
	if colour == nil {
		return nil
	}

	value := *colour

	return &value
}

// MemoryExecutor executes memory tool calls on behalf of a model
// instance.
type MemoryExecutor interface {
	WriteMemory(ctx context.Context, key, content string) error
	DeleteMemory(ctx context.Context, key string) error
	SearchMemory(ctx context.Context, query string, limit int) ([]memory.SearchResult, error)
}

// instanceMemory closes over an InstanceID and memory.Store to
// implement MemoryExecutor. Keying by identity (not nick) means
// memories survive a model instance's `/nick` rename.
type instanceMemory struct {
	instanceID domain.InstanceID
	store      memory.Store
}

func (m *instanceMemory) WriteMemory(ctx context.Context, key, content string) error {
	return m.store.Write(ctx, m.instanceID, memory.Entry{Key: key, Content: content})
}

func (m *instanceMemory) DeleteMemory(ctx context.Context, key string) error {
	return m.store.Delete(ctx, m.instanceID, key)
}

func (m *instanceMemory) SearchMemory(ctx context.Context, query string, limit int) ([]memory.SearchResult, error) {
	searcher, ok := m.store.(memory.Searcher)
	if !ok {
		return nil, fmt.Errorf("semantic search is not configured")
	}

	return searcher.Search(ctx, m.instanceID, query, limit)
}

const (
	maxNewlineRetries            = 2
	maxToolLoopTurns             = 5
	silenceReasonContentFiltered = "content filtered"
	silenceReasonNewlineRetries  = "response contained newlines after retries"
	silenceReasonFormatRetries   = "response contained invalid formatting after retries"
)

// sendWithRetry sends events to a model and retries if the response
// contains newlines in any message body. After maxNewlineRetries
// retries, a silent pass is returned. Each attempt may involve
// multiple API turns if the model uses memory tools.
type sendOutcome struct {
	result        api.CompletionResult
	retryCount    int
	toolTurnCount int
	passReason    string
}

func (s *Session) sendWithRetry(
	ctx context.Context,
	inst *domain.Instance,
	channelName domain.ChannelName,
	prompt string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
	registry *ToolRegistry,
) (sendOutcome, error) {
	lastRetryReason := silenceReasonNewlineRetries

	for attempt := range maxNewlineRetries + 1 {
		result, toolTurnCount, err := s.sendWithToolLoop(ctx, inst, channelName, prompt, history, events, registry)
		if err != nil {
			if refused, ok := errors.AsType[*api.ErrModelRefused](err); ok {
				return sendOutcome{
					result: api.CompletionResult{
						Response: protocol.ModelResponse{
							Kind:   protocol.ResponseSilence,
							Reason: refused.Reason,
						},
					},
					retryCount:    attempt,
					toolTurnCount: toolTurnCount,
					passReason:    observability.PassReasonModelRefused,
				}, nil
			}

			if errors.Is(err, api.ErrContentFiltered) {
				return sendOutcome{
					result: api.CompletionResult{
						Response: protocol.ModelResponse{
							Kind:   protocol.ResponseSilence,
							Reason: silenceReasonContentFiltered,
						},
					},
					retryCount:    attempt,
					toolTurnCount: toolTurnCount,
					passReason:    observability.PassReasonContentFiltered,
				}, nil
			}

			return sendOutcome{}, err
		}

		if result.Response.Kind != protocol.ResponseReply || len(result.Response.Messages) == 0 {
			return sendOutcome{
				result:        result,
				retryCount:    attempt,
				toolTurnCount: toolTurnCount,
				passReason:    passReasonForResponse(result.Response),
			}, nil
		}

		hasNewlines := containsNewlines(result.Response)
		hasInvalidFormatting := containsInvalidFormatting(result.Response)
		if !hasNewlines && !hasInvalidFormatting {
			return sendOutcome{
				result:        result,
				retryCount:    attempt,
				toolTurnCount: toolTurnCount,
			}, nil
		}

		if hasInvalidFormatting {
			lastRetryReason = silenceReasonFormatRetries
		} else {
			lastRetryReason = silenceReasonNewlineRetries
		}
	}

	resp := protocol.ModelResponse{
		Kind:   protocol.ResponseSilence,
		Reason: lastRetryReason,
	}

	return sendOutcome{
		result:     api.CompletionResult{Response: resp},
		retryCount: maxNewlineRetries,
		passReason: passReasonForResponse(resp),
	}, nil
}

// sendWithToolLoop sends events to a model and handles tool calls in a
// loop until the model replies, passes, or exceeds the tool turn limit.
func (s *Session) sendWithToolLoop(
	ctx context.Context,
	inst *domain.Instance,
	channelName domain.ChannelName,
	prompt string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
	registry *ToolRegistry,
) (api.CompletionResult, int, error) {
	definitions := registry.Definitions()

	result, err := s.api.SendEvents(ctx, inst.ModelID, inst.ID(), prompt, history, events, definitions...)
	if err != nil {
		return api.CompletionResult{}, 0, err
	}

	toolTurnCount := 0
	for range maxToolLoopTurns {

		if len(result.PendingToolCalls) == 0 {
			return result, toolTurnCount, nil
		}

		if registry == nil {
			return result, toolTurnCount, nil
		}

		toolResults := s.executeTools(ctx, ToolContext{
			Session: s,
			Actor:   inst,
			Channel: channelName,
			Client:  s.ensureModelClient(ctx, inst),
		}, registry, result.PendingToolCalls)
		toolTurnCount++

		result, err = s.api.ContinueWithToolResults(ctx, result.Conversation, toolResults, definitions...)
		if err != nil {
			return api.CompletionResult{}, toolTurnCount, err
		}
	}

	return result, toolTurnCount, nil
}

// executeTools runs pending tool calls and returns the results to feed
// back to the model.
func (s *Session) executeTools(
	ctx context.Context,
	toolCtx ToolContext,
	registry *ToolRegistry,
	calls []api.PendingToolCall,
) []api.ToolResult {
	results := make([]api.ToolResult, 0, len(calls))

	for _, call := range calls {
		toolName := call.Name

		callCtx, callSpan := s.startSpan(
			ctx,
			"session.execute_tool",
			attribute.String(observability.AttrOperation, "session.execute_tool"),
			attribute.String("tool.name", toolName),
		)

		payload := ToolResultPayload{
			OK:    false,
			Error: fmt.Sprintf("unknown tool %q", toolName),
		}

		if spec, ok := registry.Find(toolName); ok {
			nextPayload, err := spec.Execute(callCtx, toolCtx, call.Args)
			if err != nil {
				payload = ToolResultPayload{OK: false, Error: err.Error()}
			} else {
				payload = nextPayload
			}
		}

		if payload.OK {
			callSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
		} else {
			callSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
			callSpan.SetStatus(codes.Error, payload.Error)
		}

		callSpan.End()

		data, _ := json.Marshal(payload)
		results = append(results, api.ToolResult{ToolCallID: call.ID, Content: string(data)})
	}

	return results
}

func passReasonForResponse(response protocol.ModelResponse) string {
	if response.Kind != protocol.ResponseSilence {
		return ""
	}

	switch response.Reason {
	case silenceReasonContentFiltered:
		return observability.PassReasonContentFiltered
	case silenceReasonNewlineRetries:
		return observability.PassReasonNewlineRetryExhausted
	case silenceReasonFormatRetries:
		return observability.PassReasonFormatRetryExhausted
	default:
		return observability.PassReasonModelPass
	}
}

// containsNewlines reports whether any reply part body contains a
// newline after trimming.
func containsNewlines(resp protocol.ModelResponse) bool {
	for _, part := range resp.Messages {
		if strings.Contains(strings.TrimSpace(part.Body), "\n") {
			return true
		}
	}

	return false
}

func containsInvalidFormatting(resp protocol.ModelResponse) bool {
	for _, part := range resp.Messages {
		if err := protocol.ValidateReplyPart(part); err != nil {
			return true
		}
	}

	return false
}

// startSpan opens an OTel span on the session's configured tracer
// provider. Using the per-session provider — rather than the global
// — lets tests scope span recordings to their own
// `tracetest.SpanRecorder` even when dispatch goroutines outlive
// the test that spawned them.
func (s *Session) startSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	tracer := s.tracerProvider.Tracer("github.com/laney/modeloff/internal/session")
	ctx, span := tracer.Start(ctx, name)
	span.SetAttributes(attrs...)

	return ctx, span
}

// endSpan finalises the span with ok/error status. The fallback
// errorKind is attached as AttrErrorKind when the deferred error is
// non-nil and does not already carry a kind via *kindError (which
// errWithKind produces and errors.As unwraps here). Pass an empty
// string when no fallback is meaningful.
func endSpan(span trace.Span, errPtr *error, errorKind string) {
	err := *errPtr

	if err == nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
		span.End()
		return
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))

	kind := errorKind
	if ke, ok := errors.AsType[*kindError](err); ok {
		kind = ke.kind
	}

	if kind != "" {
		span.SetAttributes(attribute.String(observability.AttrErrorKind, kind))
	}

	span.SetStatus(codes.Error, err.Error())
	span.End()
}

// setSpanError records an error result on the span together with the
// given error kind. Inline alternative to endSpan for sites where the
// span is finalised outside a defer.
func setSpanError(span trace.Span, err error, errorKind string) {
	span.SetAttributes(
		attribute.String(observability.AttrResult, observability.ResultError),
		attribute.String(observability.AttrErrorKind, errorKind),
	)
	span.SetStatus(codes.Error, err.Error())
}

// kindError tags an error with an observability error kind so
// endSpan can attach AttrErrorKind without every call site having to
// pass the kind through an auxiliary return value.
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

func channelKindName(kind domain.ChannelKind) string {
	switch kind {
	case domain.KindDM:
		return "dm"
	case domain.KindStatus:
		return "status"
	default:
		return "channel"
	}
}

// classifyEnsureModelError maps the errors produced by
// `ensureStructuredOutputModel` to the appropriate observability error
// kind. The cached short-circuit sentinels (`ErrModelListUnavailable`,
// `ErrNoAPIKey`) reflect session-layer state that forbade the call
// before any upstream attempt. `domain.UnsupportedModelError` reflects
// a user-supplied model ID the catalogue does not include — fixable
// by the user, not infrastructure. Everything else is wrapped around
// a real upstream attempt and stays as `ErrorKindDispatch`.
func classifyEnsureModelError(err error) string {
	if errors.Is(err, ErrModelListUnavailable) || errors.Is(err, ErrNoAPIKey) {
		return observability.ErrorKindClientState
	}

	if _, ok := errors.AsType[domain.UnsupportedModelError](err); ok {
		return observability.ErrorKindValidation
	}

	return observability.ErrorKindDispatch
}

func (s *Session) ensureStructuredOutputModel(ctx context.Context, modelID domain.ModelID) error {
	if !s.HasAPIKey() || s.api == nil {
		return nil
	}

	if listModelsState(s.listModelsState.Load()) == listModelsFailed {
		// Info, not Warn: the transition-to-failed event is the
		// alerting-worthy signal (logged once by
		// transitionListModelsState); subsequent short-circuits are
		// the expected symptom of that state and would otherwise
		// produce one Warn per `/add-model` attempt.
		slog.Default().InfoContext(ctx, "add-model short-circuited: model list unavailable",
			"component", "session",
			"model_id", string(modelID),
		)

		return ErrModelListUnavailable
	}

	if !s.supportedModelsReady {
		models, err := s.api.ListModels(ctx)
		if err != nil {
			s.transitionListModelsState(ctx, listModelsFailed, err)
			return fmt.Errorf("list models: %w", err)
		}

		s.cacheSupportedModels(ctx, models)
	}

	if _, ok := s.supportedModels[modelID]; !ok {
		return domain.UnsupportedModelError{ModelID: modelID, At: s.now()}
	}

	return nil
}

func (s *Session) cacheSupportedModels(ctx context.Context, models []api.ModelInfo) {
	s.supportedModels = make(map[domain.ModelID]struct{}, len(models))
	for _, model := range models {
		s.supportedModels[model.ID] = struct{}{}
	}

	s.supportedModelsReady = true
	s.transitionListModelsState(ctx, listModelsOK, nil)
}

// transitionListModelsState atomically updates listModelsState and logs
// the transition so operators can correlate `/add-model` short-circuits
// with the upstream failure that put the catalogue into a known-stale
// state.
func (s *Session) transitionListModelsState(ctx context.Context, to listModelsState, err error) {
	from := listModelsState(s.listModelsState.Swap(uint32(to)))

	if from == to {
		return
	}

	attrs := []any{
		"component", "session",
		"from", listModelsStateName(from),
		"to", listModelsStateName(to),
	}

	if err != nil {
		attrs = append(attrs, "error", err)
	}

	if to == listModelsFailed {
		slog.Default().WarnContext(ctx, "model list state transitioned", attrs...)

		return
	}

	slog.Default().InfoContext(ctx, "model list state transitioned", attrs...)
}

func listModelsStateName(s listModelsState) string {
	switch s {
	case listModelsNone:
		return "none"
	case listModelsOK:
		return "ok"
	case listModelsFailed:
		return "failed"
	default:
		return "unknown"
	}
}
