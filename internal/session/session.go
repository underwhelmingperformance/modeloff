// Package session provides the backend coordinator that ties together
// stores, the API client, and the protocol layer. It manages channels,
// model instances, and handles commands by updating state and emitting
// domain events.
package session

import (
	"context"
	"crypto/rand"
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
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
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

// listModelsState tracks whether the cached model catalogue reflects a
// successful `ListModels` round-trip. It lets `ensureStructuredOutputModel`
// distinguish "never attempted" (fall through to the lazy load) from
// "last attempt failed" (short-circuit with
// [modelclient.ErrModelListUnavailable]).
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

	baseContext func() context.Context
	user        *domain.Instance
	userModes   map[domain.ChannelName]domain.NickMode
	userMu      sync.Mutex
	apiKey      string
	smallModel  domain.ModelID
	factory     func(apiKey, baseURL string) (api.Client, error)
	now         func() time.Time

	// shuttingDown is closed by [Session.Shutdown] under `subsMu`
	// so [Session.ensureSubscription] sees the close and declines
	// to register a fresh subscription. The shape mirrors
	// [net/http.Server]'s `inShutdown` flag — new work is
	// rejected at the registration point, not documented away.
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

	// modelClientFactory is the per-instance lifecycle hook the
	// session calls into when an instance attaches to a channel
	// or is killed. Required at construction; see [New].
	modelClientFactory ModelClientFactory

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
// supplier shape mirrors [net/http.Server.BaseContext].
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
	factory ModelClientFactory,
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
		connectedC:          make(chan struct{}),
		persistenceFailures: persistenceFailures,
		tracerProvider:      otel.GetTracerProvider(),
		subscribers:         make(map[protocol.ClientID]*serverClient),
		clientHandles:       make(map[protocol.ClientID]*serverClient),
		shuttingDown:        make(chan struct{}),
		modelClientFactory:  factory,
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
	sess.userClient = newServerClient(sess, protocol.UserClientID, user)
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

// User returns the user-client subscription, created at session
// bootstrap and live for the whole session lifetime. The returned
// handle satisfies [protocol.Client]: it carries the user's
// identity ([protocol.UserClientID]) and grants
// [domain.ModeOperator].
func (s *Session) User() protocol.Client {
	return s.userClient
}

// lookupClientHandle returns the cached handle for `id` under the
// read lock, or nil if none has been allocated yet.
func (s *Session) lookupClientHandle(id protocol.ClientID) *serverClient {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()

	return s.clientHandles[id]
}

// Subscribe registers `c` with the session and returns the
// per-client [protocol.Subscription] handle the caller uses to
// read events and to release the subscription. The session creates
// (or reuses, on a repeat call for the same identity) an internal
// envelope keyed by `c.Identity()` that carries the canonical
// actor `*domain.Instance` (from `opts.Instance`) and the per-
// subscription mode set.
//
// Subscribe is idempotent: a repeat call for an already-registered
// identity returns the existing subscription and applies any new
// `InitialModes` to it. Returns an error if `opts.Instance` is nil
// or if [Session.Shutdown] has begun.
func (s *Session) Subscribe(c protocol.Client, opts protocol.SubscribeOptions) (protocol.Subscription, error) {
	if opts.Instance == nil {
		return nil, fmt.Errorf("session.Subscribe: opts.Instance is required")
	}

	id := c.Identity()
	if id == protocol.UserClientID {
		return nil, fmt.Errorf("session.Subscribe: user-client registration is not supported yet")
	}

	sc, ok := s.ensureSubscription(id, opts.Instance)
	if !ok {
		return nil, fmt.Errorf("session.Subscribe: session is shutting down")
	}

	for _, m := range opts.InitialModes {
		sc.setMode(m, true)
	}

	return sc, nil
}

// ensureSubscription allocates a subscription envelope for `id`
// if one does not already exist, returning the canonical handle
// and a flag reporting whether the registry is open. If
// [Session.Shutdown] has begun, registration is refused: an
// existing handle is still returned, but a fresh identity is not
// registered.
func (s *Session) ensureSubscription(id protocol.ClientID, inst *domain.Instance) (*serverClient, bool) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()

	if existing, ok := s.clientHandles[id]; ok {
		return existing, true
	}

	select {
	case <-s.shuttingDown:
		return nil, false
	default:
	}

	sc := newServerClient(s, id, inst)
	s.clientHandles[id] = sc
	s.subscribers[id] = sc

	return sc, true
}

// reapClient removes a model-client from the subscriber set and
// closes the subscription's `Done` channel. The user-client is
// never reaped — its lifetime equals the session. Idempotent
// across concurrent callers via `unsubOnce` on the envelope. The
// modelclient owning the subscription is responsible for joining
// its own dispatch goroutine via [modelclient.ModelClient.Detach].
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

	if !ok {
		return
	}

	client.unsubOnce.Do(func() {
		close(client.done)
	})
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
	pe = anonymiseIfNeeded(ctx, s, pe)

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
		case <-sub.done:
			// Subscription was reaped between the snapshot and the
			// send. The recipient is gone; drop the delivery.
		case <-ctx.Done():
			return
		}
	}
}

// anonymiseIfNeeded rewrites a chat-traffic event's `From` field
// to `"anonymous"` when the target channel carries `+a`. Returns
// the event unchanged when the channel is not anonymous or when
// the event is not chat traffic.
func anonymiseIfNeeded(ctx context.Context, s *Session, pe domain.ProtocolEvent) domain.ProtocolEvent {
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
// sees the actor's full channel list — the chat-screen needs the
// complete picture to route the line into every open window where
// the actor was a known member. Window-scoped events pass
// `actorChannels == nil` and receive a nil result.
func intersectActorTargets(sub *serverClient, actorChannels []domain.ChannelName) []domain.ChannelName {
	if len(actorChannels) == 0 {
		return nil
	}

	if sub.id == protocol.UserClientID {
		// The chat-screen is the operator window for every channel;
		// expose the full actor list for routing.
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

// Shutdown closes the session's shutdown gate so that any further
// [Session.Subscribe] call declines to register a fresh
// subscription. The shape mirrors [net/http.Server.Shutdown]: new
// work is refused at the registration point, and dispatch
// goroutines belong to the model-clients holding subscriptions —
// they exit when their lifetime ctx (derived from the
// `baseContext` supplier passed to [New]) is cancelled.
//
// Shutdown returns `ctx.Err()` if `ctx` is cancelled before the
// gate close completes; otherwise nil. Safe to call more than
// once via `sync.Once` on the gate.
func (s *Session) Shutdown(ctx context.Context) (retErr error) {
	_, span := s.startSpan(ctx, "session.shutdown",
		attribute.String(observability.AttrOperation, "session.shutdown"),
	)
	defer endSpan(span, &retErr, "")

	s.subsMu.Lock()
	s.shuttingDownOnce.Do(func() { close(s.shuttingDown) })
	s.subsMu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	return nil
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

// ModelClientFactory constructs and tears down the per-instance
// model-client backing each LLM participant. The session pairs an
// `Attach` call with each instance-attach (JOIN / ADDMODEL /
// INVITE) and a `Detach` call with each model-actor reap (QUIT /
// KILL) so the model-client's dispatch goroutine joins
// deterministically. The interface lives in the session package
// so the session does not depend on `internal/modelclient`.
//
// `Attach` receives the owning [Session] as a parameter so the
// factory does not hold a back-reference; the factory can be
// constructed before the session it serves and passed to [New] in
// the same expression.
type ModelClientFactory interface {
	// Attach constructs (or returns the existing handle for) the
	// model-client backing `inst` and attaches it to `sess` via
	// [Session.Subscribe]. Idempotent on a repeat call for the
	// same identity. A nil client return is permitted and means
	// no model-client is attached for this instance — a degraded
	// mode where channel state is exercised without driving
	// dispatch.
	Attach(ctx context.Context, sess *Session, inst *domain.Instance) (protocol.Client, error)

	// Detach releases the model-client for `id`. Idempotent on
	// an unknown id.
	Detach(id protocol.ClientID)
}

// HasAPIKey reports whether the session has an active API key.
func (s *Session) HasAPIKey() bool {
	return strings.TrimSpace(s.apiKey) != ""
}

// Now returns the session's current time, using the configured
// clock. Tests override the clock to freeze time.
func (s *Session) Now() time.Time {
	return s.now()
}

// TracerProvider returns the OTel tracer provider the session
// records spans on.
func (s *Session) TracerProvider() trace.TracerProvider {
	return s.tracerProvider
}

// LoadChannelWindow reads an addressable `#`-channel as its
// typed `*ChannelWindow`. See [Session.loadChannelWindow] for
// behavioural details.
func (s *Session) LoadChannelWindow(ctx context.Context, name domain.ChannelName) (*domain.ChannelWindow, error) {
	return s.loadChannelWindow(ctx, name)
}

// Emit fans out a [domain.ProtocolEvent] on the per-subscription
// bus.
func (s *Session) Emit(ctx context.Context, evt domain.ProtocolEvent) {
	s.emit(ctx, evt)
}

// AppendEvent persists a channel event, narrating any persistence
// failure to the session's logger.
func (s *Session) AppendEvent(ctx context.Context, ch domain.ChannelName, event domain.PersistableEvent) {
	s.appendEvent(ctx, ch, event)
}

// ResolveInstanceByID returns the canonical `*domain.Instance`
// for the given id.
func (s *Session) ResolveInstanceByID(ctx context.Context, id domain.InstanceID) (*domain.Instance, error) {
	return s.store.GetInstanceByID(ctx, id)
}

// DMEventsBefore returns up to `n` DM events strictly before
// `before` between `self` and `peer`.
func (s *Session) DMEventsBefore(ctx context.Context, self, peer domain.InstanceID, before *int64, n int) ([]domain.StoredEvent, error) {
	return s.store.DMEventsBefore(ctx, self, peer, before, n)
}

// LookupClient returns the registered [protocol.Client] handle
// for the given identity, or nil if no subscription is registered.
// Returns the bare subscription envelope for the user-client and
// for any model-client whose subscription has been registered via
// [Session.Subscribe].
func (s *Session) LookupClient(id protocol.ClientID) protocol.Client {
	sc := s.lookupClientHandle(id)
	if sc == nil {
		return nil
	}

	return sc
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
		build: func() broadcastEvent {
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
func (s *Session) DirectoryChannels(ctx context.Context) (_ []domain.ChannelDirectoryEntry, retErr error) {
	ctx, span := s.startSpan(ctx, "session.directory_channels",
		attribute.String(observability.AttrOperation, "session.directory_channels"),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

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

	span.SetAttributes(attribute.Int("directory.entry_count", len(entries)))

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

	if _, err := s.modelClientFactory.Attach(ctx, s, inst); err != nil {
		slog.Default().WarnContext(ctx, "attach model client",
			"component", "session",
			"instance_id", inst.ID(),
			"channel", ch,
			"error", err,
		)
	}

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
func (s *Session) MarkRead(ctx context.Context, ch domain.ChannelName) (retErr error) {
	ctx, span := s.startSpan(ctx, "session.mark_read",
		attribute.String(observability.AttrOperation, "session.mark_read"),
		attribute.String(observability.AttrChannel, string(ch)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

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
func (s *Session) UnreadCount(ctx context.Context, ch domain.ChannelName) (count int, retErr error) {
	ctx, span := s.startSpan(ctx, "session.unread_count",
		attribute.String(observability.AttrOperation, "session.unread_count"),
		attribute.String(observability.AttrChannel, string(ch)),
	)
	defer func() {
		span.SetAttributes(attribute.Int("unread.count", count))
		endSpan(span, &retErr, observability.ErrorKindStore)
	}()

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
		setSpanError(span, modelclient.ErrNoAPIKey, observability.ErrorKindValidation)
		return nil, modelclient.ErrNoAPIKey
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

// EventsAfter returns the channel's events whose timestamp is at or
// after the given cutoff, in chronological order. The status pane
// uses this to render the per-session view of the status channel
// without showing previous sessions' entries.
func (s *Session) EventsAfter(ctx context.Context, ch domain.ChannelName, after time.Time) (_ []domain.StoredEvent, retErr error) {
	ctx, span := s.startSpan(ctx, "session.events_after",
		attribute.String(observability.AttrOperation, "session.events_after"),
		attribute.String(observability.AttrChannel, string(ch)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

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

// broadcastEvent is the intersection type carried through the
// session's persist-then-emit helpers: every value is both a
// channel event the store accepts and a protocol event the
// per-client fan-out delivers. The two sealed sums overlap
// entirely on the concrete types the session emits (every
// `domain.PersistableEvent` implementer is also a
// `domain.ProtocolEvent`), and this combined interface makes that
// invariant explicit in the helper signatures.
type broadcastEvent interface {
	domain.PersistableEvent
	domain.ProtocolEvent
}

// emit hands a protocol event to the subscriber-registry fan-out.
// The context is threaded through to preserve OTel trace parenting
// and to honour cancellation during fan-out.
func (s *Session) emit(ctx context.Context, evt domain.ProtocolEvent) {
	s.fanOutProtocol(ctx, evt)
}

// persistAndEmit appends `evt` to the channel event log and emits
// it on the protocol bus, in that order. Persistence completing
// before emission is a session-wide invariant: any consumer that
// learns about an event must always be able to find the same
// event in the store. The same value flows to both destinations
// — the live `*Instance` and `Actor` fields are `json:"-"` so the
// persisted shape is the snapshot, while live consumers see the
// populated handle.
func (s *Session) persistAndEmit(ctx context.Context, ch domain.ChannelName, evt broadcastEvent) {
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
//   - `storeOnly` persists without emission; used by the user's
//     `Quit` since the app is exiting.
//   - `afterEach` runs per channel after the state update for
//     caller-specific side effects (e.g. `forgetUserMode`).
type actorEventConfig struct {
	storeOnly bool
	mutate    func(*domain.ChannelWindow)
	build     func() broadcastEvent
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

// classifyEnsureModelError maps the errors produced by
// `ensureStructuredOutputModel` to the appropriate observability error
// kind. The cached short-circuit sentinels (`modelclient.ErrModelListUnavailable`,
// `modelclient.ErrNoAPIKey`) reflect session-layer state that forbade the call
// before any upstream attempt. `domain.UnsupportedModelError` reflects
// a user-supplied model ID the catalogue does not include — fixable
// by the user, not infrastructure. Everything else is wrapped around
// a real upstream attempt and stays as `ErrorKindDispatch`.
func classifyEnsureModelError(err error) string {
	if errors.Is(err, modelclient.ErrModelListUnavailable) || errors.Is(err, modelclient.ErrNoAPIKey) {
		return observability.ErrorKindClientState
	}

	if _, ok := errors.AsType[domain.UnsupportedModelError](err); ok {
		return observability.ErrorKindValidation
	}

	return observability.ErrorKindDispatch
}

// EnsureStructuredOutputModel validates that the given model
// supports structured outputs, lazy-loading the OpenRouter
// catalogue if needed. Cached short-circuits return the typed
// [modelclient.ErrModelListUnavailable] and [modelclient.ErrNoAPIKey] sentinels.
func (s *Session) EnsureStructuredOutputModel(ctx context.Context, modelID domain.ModelID) error {
	return s.ensureStructuredOutputModel(ctx, modelID)
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

		return modelclient.ErrModelListUnavailable
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
