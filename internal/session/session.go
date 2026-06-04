// Package session provides the backend coordinator that ties together
// stores, the API client, and the protocol layer. It manages channels,
// model instances, and handles commands by updating state and emitting
// domain events.
package session

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/domain"
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

// Store is the persistence surface [Session] depends on.
// The concrete [github.com/laney/modeloff/internal/store.SQLiteStore]
// satisfies it implicitly. The session does not call any other
// store methods; consumers with different needs (the memory
// adapter, the chat-screen) declare their own.
type Store interface {
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
	DeleteWindow(ctx context.Context, name domain.ChannelName) error

	// Event log.

	AppendEvent(ctx context.Context, ch domain.ChannelName, event domain.ChannelActivity) (int64, error)
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

	// AppendInstanceReply records a point-to-point reply (WHOIS,
	// LIST) in the instance's private reply log; InstanceRepliesBefore
	// reads it back in chronological order. This is an instance's own
	// memory of replies it received, replayed only into its own
	// prompt — never the shared channel log.
	AppendInstanceReply(ctx context.Context, id domain.InstanceID, event domain.IssuerReply) (int64, error)
	InstanceRepliesBefore(ctx context.Context, id domain.InstanceID, before *int64, n int) ([]domain.StoredEvent, error)

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

	// Autojoin list. The set of channels rejoined at next
	// `Connect`; rewritten on every successful `Quit`.
	ListAutojoinChannels(ctx context.Context) ([]domain.ChannelName, error)
	SetAutojoinChannels(ctx context.Context, channels []domain.ChannelName) error
}

// Session is the backend coordinator. It bridges the UI layer and
// the underlying stores and API client.
//
// The user's `*domain.Instance` is owned by an external
// `*userclient.UserClient` and reaches the session through the
// registered subscription envelope; the session reads it through
// [Session.userInstance] when channel-state code needs the user
// handle (member-list injection, autojoin persistence, mode
// bookkeeping).
type Session struct {
	store Store

	baseContext func() context.Context
	userModes   map[domain.ChannelName]domain.NickMode
	userMu      sync.Mutex
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

	// operAuth gates [protocol.Oper]. The default rejects every
	// client; the user-client requests `+o` via
	// [protocol.SubscribeOptions.InitialModes] at attach time, so
	// the authenticator is consulted only for future
	// credentialled OPER promotions. Tests swap it via
	// [Session.SetOperAuthenticator].
	operAuth OperAuthenticator

	// modelClientFactory is the per-instance lifecycle hook the
	// session calls into when an instance attaches to a channel
	// or is killed. The factory also satisfies the LLM-side
	// preparation surface the `AddModel` handler calls into for
	// persona arbitration and unique nick generation. Required at
	// construction; see [New].
	modelClientFactory ModelClientFactory

	connectedC    chan struct{}
	connectedOnce sync.Once
	connectedAt   time.Time

	persistenceFailures metric.Int64Counter

	// tracerProvider is the OTel `TracerProvider` the session uses
	// for its spans. Defaults to `otel.GetTracerProvider()` at
	// construction time so production callers see the global
	// provider; tests inject their own via `WithTracerProvider` so
	// span recordings stay scoped to a single test even when
	// dispatch goroutines outlive the test that spawned them.
	tracerProvider trace.TracerProvider

	// activeChannels records which channel windows have seen chat
	// traffic since the poke scheduler last drained the set. The
	// scheduler skips active channels so it nudges only windows that
	// have genuinely gone quiet (AGENTS.md point 12).
	activeMu       sync.Mutex
	activeChannels map[domain.ChannelName]struct{}
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
// The user-client is constructed externally (in `cmd/modeloff`
// or a test fixture) and attaches itself to the returned session
// via [Session.Subscribe] before any command is dispatched. The
// session reads the user's `*domain.Instance` through the
// registered subscription envelope; the store never persists the
// user to disk.
func New(
	baseContext func() context.Context,
	s Store,
	factory ModelClientFactory,
) *Session {
	persistenceFailures, _ := otel.Meter("github.com/laney/modeloff/internal/session").
		Int64Counter(observability.MetricPersistenceFailures)

	return &Session{
		baseContext:         baseContext,
		store:               s,
		userModes:           make(map[domain.ChannelName]domain.NickMode),
		now:                 time.Now,
		connectedC:          make(chan struct{}),
		persistenceFailures: persistenceFailures,
		tracerProvider:      otel.GetTracerProvider(),
		subscribers:         make(map[protocol.ClientID]*serverClient),
		clientHandles:       make(map[protocol.ClientID]*serverClient),
		shuttingDown:        make(chan struct{}),
		modelClientFactory:  factory,
		operAuth:            DefaultOperAuthenticator,
		activeChannels:      make(map[domain.ChannelName]struct{}),
	}
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

// userInstance returns the user's canonical `*domain.Instance` as
// read off the registered user-client subscription. Returns nil
// when no user-client has attached yet — chat-screen tests that
// drive the session directly without a user-client encounter this
// path and rely on the consumer treating nil as "no user".
func (s *Session) userInstance() *domain.Instance {
	sc := s.lookupClientHandle(protocol.UserClientID)
	if sc == nil {
		return nil
	}

	return sc.instance
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

	sc, ok := s.ensureSubscription(id, opts.Instance)
	if !ok {
		return nil, fmt.Errorf("session.Subscribe: session is shutting down")
	}

	if opts.EchoMessage {
		sc.echo = true
	}

	for _, m := range opts.InitialModes {
		// `setUserModeAs` is idempotent on an already-held mode and
		// writes a server-narrated [domain.UserModeChange] to the
		// subscription's events channel so the first event the
		// consumer reads is the elevation. The empty `by` flags the
		// emission as server-originated rather than peer-initiated.
		s.setUserModeAs(s.baseContext(), "", sc, m, true)
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
func (s *Session) Shutdown(ctx context.Context) error {
	return observability.SpanRunner{
		Tracer: s.tracerProvider.Tracer("github.com/laney/modeloff/internal/session"),
	}.Run(ctx, "session.shutdown", nil, func(ctx context.Context, _ trace.Span) error {
		s.subsMu.Lock()
		s.shuttingDownOnce.Do(func() { close(s.shuttingDown) })
		s.subsMu.Unlock()

		if err := ctx.Err(); err != nil {
			return err
		}

		return nil
	})
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
func (s *Session) Connect(ctx context.Context) error {
	if !s.connectedAt.IsZero() {
		// No span is recorded for a no-op call: it is not a real
		// connect attempt and would otherwise inflate
		// session.connect operation counts.
		return nil
	}

	return s.inSpan(ctx, "session.connect", nil, func(ctx context.Context, _ trace.Span) error {
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

		user := s.userInstance()

		welcomeNick := domain.Nick("")
		if user != nil {
			welcomeNick = user.Nick()
		}

		// RFC 2812 RPL_WELCOME (001) is a response to the connecting
		// client, so it is delivered point-to-point to the user-client
		// via deliverToClient.
		if user != nil {
			s.deliverToClient(ctx, user.ID(), domain.Welcome{
				ServerName: domain.StatusServerName,
				Nick:       welcomeNick,
				At:         s.connectedAt,
			})

			if unclean {
				s.deliverToClient(ctx, user.ID(), domain.Reconnected{At: s.connectedAt})
			}
		}

		s.connectedOnce.Do(func() { close(s.connectedC) })

		return nil
	})
}

// cleanupUncleanShutdown resets this Session's in-memory user
// state so a post-connect view mirrors a fresh, clean start.
//
// The user is ephemeral — never persisted to the store — so
// cleanup is purely an in-memory reset of the user handle's
// channel map and the userModes map. There is no store-side
// prior-nick residue to reconcile.
func (s *Session) cleanupUncleanShutdown(ctx context.Context) error {
	_ = ctx

	if user := s.userInstance(); user != nil {
		user.MutateChannels(func(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
			for pair := m.Oldest(); pair != nil; {
				next := pair.Next()
				m.Delete(pair.Key)
				pair = next
			}
		})
	}

	s.userMu.Lock()
	s.userModes = make(map[domain.ChannelName]domain.NickMode)
	s.userMu.Unlock()

	return nil
}

// ModelClientFactory constructs and tears down the per-instance
// model-client backing each LLM participant and prepares the LLM-
// side state a fresh `ADDMODEL` needs before attach. The session
// pairs an `Attach` call with each instance-attach (JOIN /
// ADDMODEL / INVITE) and a `Detach` call with each model-actor
// reap (QUIT / KILL) so the model-client's dispatch goroutine
// joins deterministically. The interface lives in the session
// package so the session does not depend on `internal/modelmanager`
// or `internal/modelclient`.
//
// `Attach` and `PrepareInstance` receive the owning [Session] as
// a parameter so the factory does not hold a back-reference; the
// factory can be constructed before the session it serves and
// passed to [New] in the same expression.
type ModelClientFactory interface {
	// PrepareInstance resolves the persona and unique nick for a
	// new instance with the given model id and persona hint. An
	// empty persona triggers a draw from the manager's persona
	// pool (lazily generated if missing). Returns the chosen nick,
	// the resolved persona (verbatim if non-empty, else the drawn
	// pool entry), and any error from structured-output
	// validation or nick generation.
	PrepareInstance(ctx context.Context, sess *Session, modelID domain.ModelID, persona string) (domain.Nick, string, error)

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

// InstanceRepliesBefore returns up to `n` of the instance's own
// replies strictly before `before`, in chronological order.
func (s *Session) InstanceRepliesBefore(ctx context.Context, id domain.InstanceID, before *int64, n int) ([]domain.StoredEvent, error) {
	return s.store.InstanceRepliesBefore(ctx, id, before, n)
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

// ResolveNick turns a user-supplied nick into the canonical
// `*domain.Instance` for that nick. This is the single boundary
// where nick strings become handles — callers hold the handle and
// compare by pointer identity from there on. A nick matching the
// user's current display nick resolves to the user's handle; any
// other nick is looked up in the store.
func (s *Session) ResolveNick(ctx context.Context, nick domain.Nick) (*domain.Instance, error) {
	if user := s.userInstance(); user != nil && nick == user.Nick() {
		return user, nil
	}

	return s.store.ResolveNick(ctx, nick)
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
func (s *Session) userQuit(ctx context.Context, message string) error {
	return s.inSpan(ctx, "session.quit", nil, func(ctx context.Context, _ trace.Span) error {
		user := s.userInstance()
		if user == nil {
			return fmt.Errorf("user-client not registered with this session")
		}

		autojoin := s.persistableAutojoinChannels()

		userNick := user.Nick()
		channels := s.instanceChannelNames(user)
		now := s.now()

		// Order matters for crash-resilience: append per-channel quit events
		// and drop the user from the members list first, persist the
		// autojoin list second, clear in-memory state third, and clear the
		// session_active marker last. A crash between any of the earlier
		// steps leaves the next startup classified as unclean, at which
		// point cleanupUncleanShutdown handles any residual memberships.
		s.propagateActorEvent(ctx, user, actorEventConfig{
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

		user.MutateChannels(func(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
			for _, ch := range channels {
				m.Delete(ch)
			}
		})

		if err := s.store.ClearSessionActive(ctx); err != nil {
			return fmt.Errorf("clear session active: %w", err)
		}

		return nil
	})
}

// DirectoryChannels returns the public channel directory for
// `/list`. Filters to `*ChannelWindow` only — DMs and the
// status window are not in the directory. The returned entries
// are snapshots of name, member count, and topic; callers turn
// them into per-row `domain.ListReply` events themselves.
func (s *Session) DirectoryChannels(ctx context.Context) ([]domain.ChannelDirectoryEntry, error) {
	var entries []domain.ChannelDirectoryEntry

	err := s.inSpan(ctx, "session.directory_channels", nil, func(ctx context.Context, span trace.Span) error {
		windows, err := s.store.ListWindows(ctx)
		if err != nil {
			return fmt.Errorf("list windows: %w", err)
		}

		entries = make([]domain.ChannelDirectoryEntry, 0, len(windows))
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

		return nil
	})

	return entries, err
}

// ChannelWindowNames returns the names of every addressable
// `*ChannelWindow` known to the session, in store-iteration
// order. Unlike [Session.DirectoryChannels] no mode-visibility
// filter is applied — the session's poke scheduler fans across every
// channel the session knows about, including `+s` (secret) ones.
func (s *Session) ChannelWindowNames(ctx context.Context) ([]domain.ChannelName, error) {
	var names []domain.ChannelName

	err := s.inSpan(ctx, "session.channel_window_names", nil, func(ctx context.Context, _ trace.Span) error {
		windows, err := s.store.ListWindows(ctx)
		if err != nil {
			return fmt.Errorf("list windows: %w", err)
		}

		names = make([]domain.ChannelName, 0, len(windows))
		for _, w := range windows {
			if _, ok := w.(*domain.ChannelWindow); !ok {
				continue
			}

			names = append(names, w.Name())
		}

		return nil
	})

	return names, err
}

// Instances returns an iterator over every known model instance.
// The UI's tab-completion sources call this to offer `/invite`,
// `/msg`, `/whois`, and `/add-model` suggestions across the full
// session — not just the active channel's members. The user
// instance is not included; consumers hold the user-client
// handle directly and read identity through it.
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

// markRead records that the user has seen all current events in
// `ch` by stamping their last-read cursor at the rowid of the most
// recent event. Used internally by [joinAs] to keep a freshly-
// joined channel from appearing with the joiner's own arrival as
// "unread". The user-client calls into the store directly for
// `/mark-read`-style affordances.
func (s *Session) markRead(ctx context.Context, ch domain.ChannelName) error {
	return s.inSpan(ctx, "session.mark_read", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
	}, func(ctx context.Context, _ trace.Span) error {
		events, err := s.store.EventsBefore(ctx, ch, nil, 1)
		if err != nil {
			return fmt.Errorf("get latest event: %w", err)
		}

		if len(events) == 0 {
			return nil
		}

		return s.store.SetLastRead(ctx, ch, events[0].ID)
	})
}

// UnreadCount returns the number of events in a channel that arrived
// after the last-read position.
func (s *Session) UnreadCount(ctx context.Context, ch domain.ChannelName) (int, error) {
	var count int

	err := s.inSpan(ctx, "session.unread_count", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
	}, func(ctx context.Context, span trace.Span) error {
		defer func() {
			span.SetAttributes(attribute.Int("unread.count", count))
		}()

		lastID, err := s.store.GetLastRead(ctx, ch)
		if err != nil {
			return fmt.Errorf("get last read: %w", err)
		}

		if lastID == 0 {
			events, err := s.store.EventsBefore(ctx, ch, nil, 1000)
			if err != nil {
				return err
			}

			count = len(events)
			return nil
		}

		fromID := lastID + 1
		events, err := s.store.EventsFrom(ctx, ch, &fromID, 1000)
		if err != nil {
			return err
		}

		count = len(events)
		return nil
	})

	return count, err
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
func (s *Session) EventsAfter(ctx context.Context, ch domain.ChannelName, after time.Time) ([]domain.StoredEvent, error) {
	var out []domain.StoredEvent

	err := s.inSpan(ctx, "session.events_after", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
	}, func(ctx context.Context, _ trace.Span) error {
		events, err := s.store.EventsBefore(ctx, ch, nil, 500)
		if err != nil {
			return err
		}

		if after.IsZero() {
			out = events
			return nil
		}

		filtered := events[:0]
		for _, evt := range events {
			if !domain.EventTime(evt.Event).Before(after) {
				filtered = append(filtered, evt)
			}
		}

		out = filtered
		return nil
	})

	return out, err
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

// broadcastEvent is the intersection type carried through the
// session's persist-then-emit helpers: every value is both channel
// activity the store accepts and a protocol event the per-client
// fan-out delivers. The combined interface makes explicit in the
// helper signatures that persist-then-emit only ever carries channel
// activity.
type broadcastEvent interface {
	domain.ChannelActivity
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

				if err := s.commitChannel(ctx, window); err != nil {
					slog.Default().ErrorContext(ctx, "propagate actor event: commit channel",
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

	user := s.userInstance()
	if user == nil {
		return channels
	}

	userChannels := user.Channels()
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

func (s *Session) appendEvent(ctx context.Context, ch domain.ChannelName, event domain.ChannelActivity) {
	if _, err := s.store.AppendEvent(ctx, ch, event); err != nil {
		slog.Default().ErrorContext(ctx, "append event", "channel", ch, "error", err)
		s.recordPersistenceFailure(ctx, ch)
	}
}

// persistInstanceReplies records an issuer's point-to-point reply
// events in its private reply log, keyed by the issuer's identity. A
// model re-reads them as its own experience on later turns. The
// user-client (whose identity is the empty id) writes the same log;
// it is transient and never restores, so its entries are the durable
// record a future restore or inspector would read rather than
// anything the user sees again this session.
func (s *Session) persistInstanceReplies(ctx context.Context, c protocol.Client, events []domain.ProtocolEvent) {
	id := domain.InstanceID(c.Identity())
	for _, ev := range events {
		if reply, ok := ev.(domain.IssuerReply); ok {
			s.appendInstanceReply(ctx, id, reply)
		}
	}
}

// appendInstanceReply best-effort persists one reply to the
// instance's private log. A failed write is logged and counted but
// does not fail the command: the reply was already delivered live,
// and only the instance's durable memory of it is lost.
func (s *Session) appendInstanceReply(ctx context.Context, id domain.InstanceID, event domain.IssuerReply) {
	if _, err := s.store.AppendInstanceReply(ctx, id, event); err != nil {
		slog.Default().ErrorContext(ctx, "append instance reply", "instance_id", id, "error", err)
		s.recordInstancePersistenceFailure(ctx, id)
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

// recordInstancePersistenceFailure increments the persistence-failures
// counter tagged with the instance whose reply-log write failed.
func (s *Session) recordInstancePersistenceFailure(ctx context.Context, id domain.InstanceID) {
	if s.persistenceFailures != nil {
		s.persistenceFailures.Add(ctx, 1,
			metric.WithAttributes(attribute.String(observability.AttrInstanceID, string(id))))
	}
}

// inSpan brackets fn with a span and result-recording on the
// session's tracer provider. The fallback error kind is
// [observability.ErrorKindStore] — most session operations are
// persistence-backed. Sites that need to override (e.g. validation
// refusals) wrap their returned error with [errWithKind], which the
// classifier here unwraps.
func (s *Session) inSpan(
	ctx context.Context,
	op string,
	attrs []attribute.KeyValue,
	fn func(ctx context.Context, span trace.Span) error,
) error {
	return observability.SpanRunner{
		Tracer:         s.tracerProvider.Tracer("github.com/laney/modeloff/internal/session"),
		DefaultErrKind: observability.ErrorKindStore,
		ClassifyError:  classifySessionError,
	}.Run(ctx, op, attrs, fn)
}

func classifySessionError(err error) string {
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
