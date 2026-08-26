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
	"reflect"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
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
	// Server-owned status and channel windows live in the `channels`
	// table. Direct-message windows are client state and use the
	// separate open-window store surface.
	ListWindows(ctx context.Context) ([]domain.Window, error)
	GetWindow(ctx context.Context, name domain.ChannelName) (domain.Window, error)
	SaveWindow(ctx context.Context, w domain.Window) error
	CommitChannelJoin(ctx context.Context, join store.ChannelJoin) (store.CommittedChannelEvent, error)
	CommitChannelEvent(ctx context.Context, event store.ChannelEvent) (store.CommittedChannelEvent, error)
	CommitChannelUpdate(ctx context.Context, update store.ChannelUpdate) (store.CommittedChannelEvent, error)
	CommitChannelDeparture(ctx context.Context, departure store.ChannelDeparture) (store.CommittedChannelEvent, error)
	CommitActorRename(ctx context.Context, rename store.ActorRename) (store.CommittedActorRename, error)
	CommitInstanceDeletion(ctx context.Context, deletion store.InstanceDeletion) (store.CommittedInstanceDeletion, error)
	DeleteWindow(ctx context.Context, name domain.ChannelName) error

	// Event log.

	AppendEvent(ctx context.Context, ch domain.ChannelName, event domain.ChannelActivity) (int64, error)
	EventsBefore(ctx context.Context, ch domain.ChannelName, before *int64, n int) ([]domain.StoredEvent, error)
	AppendChannelScrollback(ctx context.Context, records []store.ChannelScrollbackRecord) ([]int64, error)
	ChannelScrollback(ctx context.Context, actor domain.InstanceID, ch domain.ChannelName, n int) ([]domain.StoredEvent, error)
	ChannelScrollbackBefore(ctx context.Context, actor domain.InstanceID, ch domain.ChannelName, before *int64, n int) ([]domain.StoredEvent, error)
	DeleteChannelScrollback(ctx context.Context, actor domain.InstanceID, ch domain.ChannelName) error

	// CountEventsFrom counts the channel's events at or after the
	// given event id, or all of them when `from` is nil. It is how
	// the unread badge is answered: a count, not a page of decoded
	// rows nobody reads.
	CountEventsFrom(ctx context.Context, ch domain.ChannelName, from *int64) (int, error)

	// CountDMEventsFrom is [Store.CountEventsFrom] for a DM: it
	// counts the messages of both directions of the thread between
	// `self` and `peer`, which are logged under different keys.
	CountDMEventsFrom(ctx context.Context, self, peer domain.InstanceID, from *int64) (int, error)

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
	AppendInstanceReply(ctx context.Context, id domain.InstanceID, window protocol.WindowTarget, event domain.IssuerReply) (int64, error)
	InstanceRepliesBefore(ctx context.Context, id domain.InstanceID, before *int64, n int) ([]store.InstanceReplyRecord, error)
	InstanceRepliesForWindowBefore(ctx context.Context, id domain.InstanceID, window protocol.WindowTarget, before *int64, n int) ([]store.InstanceReplyRecord, error)
	DeleteInstanceRepliesForWindow(ctx context.Context, id domain.InstanceID, window protocol.WindowTarget) error
	DeleteModelTurnsForWindow(ctx context.Context, id domain.InstanceID, window protocol.WindowTarget) error
	BeginModelTurn(ctx context.Context, turn store.ModelTurn, input store.ModelTurnEntry) (store.ModelTurnID, error)
	AppendModelTurnEntry(ctx context.Context, turnID store.ModelTurnID, entry store.ModelTurnEntry) error
	ContextSummaries(ctx context.Context, id domain.InstanceID, window protocol.WindowTarget) ([]store.ContextSummary, error)
	CommitContextSummary(ctx context.Context, update store.ContextSummaryUpdate) (store.ContextSummary, error)

	// Model instances. These methods remain on the session's private
	// persistence boundary; no actor handle crosses the client
	// protocol.
	ListInstances(ctx context.Context) ([]*domain.Instance, error)
	GetInstanceByID(ctx context.Context, id domain.InstanceID) (*domain.Instance, error)
	SaveInstance(ctx context.Context, inst *domain.Instance) error
	DeleteInstanceByID(ctx context.Context, id domain.InstanceID) error
	MarkInstancePendingDeletion(ctx context.Context, id domain.InstanceID) error

	// ResolveNick returns the stored instance whose current display
	// nick matches the argument. It returns [store.ErrNoSuchNick]
	// when no instance matches.
	ResolveNick(ctx context.Context, nick domain.Nick) (*domain.Instance, error)

	// Session-active marker. Set on `Connect`; a non-empty value
	// on the next `Connect` signals an unclean prior shutdown so
	// the user's stale membership state can be reconciled.
	// Clearing it belongs to the client whose connection it
	// describes, which writes it through its own store surface
	// (`userclient.Store.ClearSessionActive`).
	GetSessionActive(ctx context.Context) (string, error)
	SetSessionActive(ctx context.Context, value string) error
	ClearSessionActive(ctx context.Context) error

	// Last-read tracking. The event id high-watermark per
	// channel that the chat-screen has rendered. Where the
	// cursor sits is the client's business — the user-client
	// writes it through its own store surface — and the session
	// only reads it, to answer the unread-badge query the
	// chat-screen asks. GetDMLastRead is the DM counterpart,
	// keyed by the counterpart's InstanceID rather than by a
	// channel name, since a DM cursor lives in its own table.
	GetLastRead(ctx context.Context, ch domain.ChannelName) (int64, error)
	GetDMLastRead(ctx context.Context, peer domain.InstanceID) (int64, error)
}

// Session is the backend coordinator. It bridges the UI layer and
// the underlying stores and API client.
//
// The user's `*domain.Instance` is owned by an external
// `*userclient.UserClient` and reaches the session through the
// registered subscription envelope; the session reads it through
// [Session.userInstance] for the two things addressed to that one
// client, the welcome numerics and the unclean-shutdown
// reconciliation.
type Session struct {
	store               Store
	directoryGeneration atomic.Uint64

	now            func() time.Time
	userCredential *protocol.UserCredential

	// defaultChannelModes returns the modes a newly created channel
	// starts with. Nil means no modes; see
	// [Session.newChannelModes].
	defaultChannelModes DefaultChannelModes

	// shuttingDown is closed by [Session.Shutdown] under `subsMu`
	// so [Session.ensureSubscription] sees the close and declines
	// to register a fresh subscription. The shape mirrors
	// [net/http.Server]'s `inShutdown` flag — new work is
	// rejected at the registration point, not documented away.
	shuttingDown     chan struct{}
	shuttingDownOnce sync.Once
	handlersMu       sync.Mutex
	handlers         sync.WaitGroup
	reapers          sync.WaitGroup
	handlersClosed   bool

	subsMu             sync.RWMutex
	subscribers        map[protocol.ClientID]*serverClient
	clientHandles      map[protocol.ClientID]*serverClient
	attachments        map[protocol.ClientID]*protocol.Attachment
	attachmentVerifier func(protocol.ClientID, *protocol.Attachment) bool

	// dmHistoryMu orders each direct message's audit-row write and
	// live queue insertion against a concurrent scrollback read. The
	// reader can then exclude queued traffic by row id without a
	// committed-but-not-yet-queued interval.
	dmHistoryMu sync.Mutex

	// writerQ hands commands to the session's command loop and
	// writerStopped closes when that loop exits. The handoff is
	// unbuffered so a command accepted onto the queue is a command
	// the loop has committed to running. See [Session.onWriter].
	writerQ       chan writerJob
	writerStopped chan struct{}

	// channels is the session's live channel state — the member
	// lists, topics, modes and invitation sets every command reads
	// and every reader consults. See [channelState].
	channels *channelState

	// flood is the server's flood-control setting, applied to every
	// connection alike. See [floodPolicy].
	flood floodPolicy

	// channelFlood counts messages per flood window for the channels
	// that set `+f`. See [channelFlood].
	channelFlood *channelFlood

	// operAuth gates [protocol.Oper]. The default rejects every
	// client; the session grants `+o` to its sentinel user identity
	// at attach time, so the authenticator is consulted only for
	// future credentialled OPER promotions. Tests swap it via
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

	// pokeBackoffMu guards pokeBackoffState, the poke scheduler's
	// per-channel exponential backoff (see [Session.pokeQuietWindows]).
	// It is a separate lock from activeMu because the two track
	// different lifetimes: activeChannels resets every cycle, while
	// pokeBackoffState persists across cycles for a channel that
	// keeps going unanswered.
	pokeBackoffMu    sync.Mutex
	pokeBackoffState map[domain.ChannelName]pokeBackoff

	// pokeWake interrupts the poke scheduler's current sleep, so a
	// [PokeSchedule] change (a `/config poke-interval` edit, a
	// freshly-set API key) takes effect on this cycle even while a
	// longer sleep from the previous cycle is already under way.
	// Buffered by one so a wake that lands while the loop is between
	// sleeps is not lost, and [Session.WakePoke] never blocks its
	// caller.
	pokeWake chan struct{}
}

// DefaultChannelModes returns the modes to give a channel at the
// moment it is created. The session calls it on every JOIN that
// creates a channel, so an implementation that tracks a live setting
// reaches the next channel created without a restart. A channel that
// already exists keeps the modes it has.
//
// An implementation must return without blocking. The session calls
// this on its command loop, where every other client's command waits
// behind it, so an implementation backed by a configuration file
// reads a value it refreshed elsewhere instead of reading the file
// here.
//
// If the supplier is nil, the session creates channels with no modes
// set.
type DefaultChannelModes func(ctx context.Context) domain.ChannelModes

// Option configures a Session at construction time.
type Option func(*Session)

// WithUserCredential binds the sentinel user identity to credential.
// The user-client must present the same pointer when it subscribes.
func WithUserCredential(credential *protocol.UserCredential) Option {
	return func(s *Session) { s.userCredential = credential }
}

// withModelAttachmentVerifier replaces the check [Session.Subscribe]
// makes on a subscribing client's attachment. A session that keeps
// its own accepts only the tokens it issued.
func withModelAttachmentVerifier(verifier func(protocol.ClientID, *protocol.Attachment) bool) Option {
	return func(s *Session) { s.attachmentVerifier = verifier }
}

// New creates a Session whose dispatch goroutines run for the lifetime
// of `ctx`.
//
// The returned session has its command loop already running (see
// [Session.runWriter]); every client command is processed on it, in
// arrival order. The loop stops when `ctx` is cancelled
// or [Session.Shutdown] runs, after which further commands are
// refused.
//
// Cancelling `ctx` wakes those goroutines; they exit and
// [Session.Shutdown] joins them. The session does not retain `ctx` or
// use it as an ambient context for later synchronous operations.
//
// The user-client is constructed externally (in `cmd/modeloff`
// or a test fixture) and attaches itself to the returned session
// via [Session.Subscribe] before any command is dispatched. The
// session reads the user's `*domain.Instance` through the
// registered subscription envelope; the client writes its own
// connection record on the way in, which is what makes that handle
// the canonical one for the empty [domain.InstanceID].
func New(
	ctx context.Context,
	s Store,
	factory ModelClientFactory,
	defaultChannelModes DefaultChannelModes,
	options ...Option,
) *Session {
	persistenceFailures, _ := otel.Meter("github.com/laney/modeloff/internal/session").
		Int64Counter(observability.MetricPersistenceFailures)

	sess := &Session{
		store:               s,
		defaultChannelModes: defaultChannelModes,
		now:                 time.Now,
		connectedC:          make(chan struct{}),
		persistenceFailures: persistenceFailures,
		tracerProvider:      otel.GetTracerProvider(),
		subscribers:         make(map[protocol.ClientID]*serverClient),
		clientHandles:       make(map[protocol.ClientID]*serverClient),
		attachments:         make(map[protocol.ClientID]*protocol.Attachment),
		shuttingDown:        make(chan struct{}),
		modelClientFactory:  factory,
		operAuth:            DefaultOperAuthenticator,
		activeChannels:      make(map[domain.ChannelName]struct{}),
		pokeBackoffState:    make(map[domain.ChannelName]pokeBackoff),
		pokeWake:            make(chan struct{}, 1),
		writerQ:             make(chan writerJob),
		writerStopped:       make(chan struct{}),
		channels:            newChannelState(),
		flood:               rfcFloodPolicy,
		channelFlood:        newChannelFlood(),
	}
	for _, option := range options {
		option(sess)
	}

	go sess.runWriter(ctx)

	return sess
}

// newChannelModes reads the session's [DefaultChannelModes] supplier
// for the modes to give a channel that is being created. A session
// constructed without a supplier creates channels with no modes set.
func (s *Session) newChannelModes(ctx context.Context) domain.ChannelModes {
	if s.defaultChannelModes == nil {
		return domain.ChannelModes{}
	}

	return s.defaultChannelModes(ctx)
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

func (s *Session) activeClientHandle(id protocol.ClientID) *serverClient {
	client := s.lookupClientHandle(id)
	if client == nil {
		return nil
	}

	_, active := client.connection()
	if !active {
		return nil
	}

	return client
}

type activeConnection struct {
	client     *serverClient
	generation uint64
}

func (s *Session) activeConnections() map[protocol.ClientID]activeConnection {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()

	connections := make(map[protocol.ClientID]activeConnection, len(s.clientHandles))
	for id, client := range s.clientHandles {
		generation, active := client.connection()
		if active {
			connections[id] = activeConnection{client: client, generation: generation}
		}
	}

	return connections
}

// lookupClientByNick returns the active client currently holding
// `nick`, or nil if no client does. The match runs under the server's
// casemapping via [domain.EqualNick], so `Botty` and `botty` are one
// client.
//
// The answer comes from the registry of connected clients, which is
// what the question means: a nick names somebody a message can
// reach, and a client the server holds no subscription for cannot be
// reached. The registry is in memory, so a
// caller on the command loop pays no I/O for the lookup. It holds one
// entry per connected client, which is the user plus the model
// instances in this session.
func (s *Session) lookupClientByNick(nick domain.Nick) *serverClient {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()

	for _, sc := range s.clientHandles {
		_, active := sc.connection()
		if active && domain.EqualNick(sc.instance.Nick(), nick) {
			return sc
		}
	}

	return nil
}

// Subscribe registers `c` with the session and returns the
// per-client [protocol.Subscription] handle the caller uses to
// read events and to release the subscription. The session creates
// (or reuses, on a repeat call for the same identity) an internal
// envelope keyed by `c.Identity()` that carries the canonical
// actor `*domain.Instance` resolved from the store and the per-
// subscription mode set.
//
// Subscribe is idempotent for the client that already holds the
// identity: a repeat call returns the same subscription. A different
// client asking for an identity that is already registered is refused
// with [ErrIdentityInUse]. A subscription's events channel has one
// reader; handing it to a second client would have both goroutines
// receive from it, and each delivery would reach only one of them.
// An identity becomes free again when its subscription is reaped.
//
// Returns an error if the identity has no persisted instance or if
// [Session.Shutdown] has begun.
func (s *Session) Subscribe(
	ctx context.Context,
	c protocol.Client,
	opts protocol.SubscribeOptions,
) (protocol.Subscription, error) {
	if !validClientHandle(c) {
		return nil, fmt.Errorf("session.Subscribe: %w", ErrInvalidClientHandle)
	}

	if c.Identity() == protocol.UserClientID {
		if opts.UserCredential == nil || opts.UserCredential != s.userCredential {
			return nil, fmt.Errorf("session.Subscribe: %w", ErrInvalidUserCredential)
		}
		if opts.Attachment != nil {
			return nil, fmt.Errorf("session.Subscribe: model attachment supplied for user identity")
		}
	} else if opts.UserCredential != nil {
		return nil, fmt.Errorf("session.Subscribe: user credential supplied for model identity %q", c.Identity())
	}

	instanceID := domain.InstanceID(c.Identity())
	canonical, err := s.store.GetInstanceByID(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("session.Subscribe: resolve instance %q: %w", instanceID, err)
	}
	attachmentVerified := c.Identity() == protocol.UserClientID ||
		s.attachmentValid(c.Identity(), opts.Attachment)
	if !attachmentVerified {
		return nil, fmt.Errorf("session.Subscribe: %w for %q", ErrInvalidModelAttachment, c.Identity())
	}

	sc, err := s.ensureSubscription(c, canonical, opts, attachmentVerified)
	if err != nil {
		return nil, err
	}

	if c.Identity() == protocol.UserClientID {
		// The session-owned client is the server operator. The server
		// grants that mode from the authenticated identity; no
		// caller-controlled subscribe option can grant capabilities.
		s.setUserModeAs(ctx, "", sc, domain.ModeOperator, true)
	}

	return sc, nil
}

func validClientHandle(c protocol.Client) bool {
	if c == nil {
		return false
	}

	value := reflect.ValueOf(c)
	return value.Kind() == reflect.Pointer && !value.IsNil()
}

func (s *Session) attachmentValid(id protocol.ClientID, attachment *protocol.Attachment) bool {
	if attachment == nil {
		return false
	}
	if s.attachmentVerifier != nil {
		return s.attachmentVerifier(id, attachment)
	}

	s.subsMu.RLock()
	defer s.subsMu.RUnlock()

	return s.attachments[id] == attachment
}

// IssueAttachment returns the attachment token that authorises a
// client to subscribe as `id`, creating one if the identity has none.
// It is what [Session.Subscribe] checks a subscribing client's
// [protocol.SubscribeOptions.Attachment] against.
func (s *Session) IssueAttachment(id protocol.ClientID) *protocol.Attachment {
	return s.issueAttachment(id)
}

func (s *Session) issueAttachment(id protocol.ClientID) *protocol.Attachment {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()

	if existing := s.attachments[id]; existing != nil {
		return existing
	}

	attachment := protocol.NewAttachment()
	s.attachments[id] = attachment

	return attachment
}

func (s *Session) revokeAttachment(id protocol.ClientID, attachment *protocol.Attachment) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()

	if s.attachments[id] == attachment {
		delete(s.attachments, id)
	}
}

var (
	// ErrIdentityInUse is returned by [Session.Subscribe] when the
	// identity a client is asking for is already registered to a
	// different client. Callers branch on it with `errors.Is`.
	ErrIdentityInUse = errors.New("identity is already registered to another client")

	// ErrInvalidUserCredential means a client tried to claim the user identity
	// without the credential issued by the session.
	ErrInvalidUserCredential = errors.New("user credential is invalid")

	// ErrInvalidModelAttachment means a client tried to claim a model identity
	// without the attachment issued for that identity.
	ErrInvalidModelAttachment = errors.New("model attachment is invalid")

	// ErrSubscriptionOptionsChanged means an existing client repeated Subscribe
	// with delivery options that differ from the registered subscription.
	ErrSubscriptionOptionsChanged = errors.New("subscription options changed")

	// ErrClientNotConnected means a client does not own an active subscription
	// in this session.
	ErrClientNotConnected = errors.New("client is not connected to this session")

	// ErrInvalidClientHandle means a client implementation is not a non-nil
	// pointer and therefore has no stable object identity for ownership checks.
	ErrInvalidClientHandle = errors.New("client handle must be a non-nil pointer")
)

// ensureSubscription returns the subscription envelope `c` holds,
// allocating one if the identity is free. A subscription belongs to
// the client that registered it: an identity already registered to a
// different client is refused with [ErrIdentityInUse], because the
// envelope's events channel has one reader and a second client
// receiving from it would take deliveries away from the first.
//
// If [Session.Shutdown] has begun, registration is refused: an
// existing handle is still returned to its owner, but a fresh
// identity is not registered.
func (s *Session) ensureSubscription(
	c protocol.Client,
	inst *domain.Instance,
	opts protocol.SubscribeOptions,
	attachmentVerified bool,
) (*serverClient, error) {
	id := c.Identity()

	s.subsMu.Lock()
	defer s.subsMu.Unlock()

	if id != protocol.UserClientID {
		valid := attachmentVerified
		if s.attachmentVerifier == nil {
			valid = s.attachmentValidLocked(id, opts.Attachment)
		}
		if !valid {
			return nil, fmt.Errorf("session.Subscribe: %w for %q", ErrInvalidModelAttachment, id)
		}
	}

	if existing, ok := s.clientHandles[id]; ok {
		if existing.owner != c {
			return nil, fmt.Errorf("session.Subscribe: %q: %w", id, ErrIdentityInUse)
		}
		if existing.echo != opts.EchoMessage || existing.replayCapable != opts.ReplayHistory {
			return nil, fmt.Errorf("session.Subscribe: %w for %q", ErrSubscriptionOptionsChanged, id)
		}

		return existing, nil
	}

	select {
	case <-s.shuttingDown:
		return nil, fmt.Errorf("session.Subscribe: session is shutting down")
	default:
	}

	sc := newServerClient(s, c, inst, opts, s.shuttingDown)
	s.clientHandles[id] = sc
	s.subscribers[id] = sc

	return sc, nil
}

func (s *Session) attachmentValidLocked(
	id protocol.ClientID,
	attachment *protocol.Attachment,
) bool {
	if attachment == nil {
		return false
	}
	return s.attachments[id] == attachment
}

// reapClient removes a model-client from the subscriber set, closes
// the subscription's `Done` channel, and joins its outbound pump so
// no goroutine outlives the subscription it serves. The user-client
// is never reaped — its lifetime equals the session. Idempotent
// across concurrent callers via `unsubOnce` on the envelope. The
// modelclient owning the subscription is responsible for joining
// its own dispatch goroutine via [modelclient.ModelClient.Detach].
//
// The pump join cannot hang: closing `done` is what releases a pump
// parked on a send to a consumer that has stopped reading.
func (s *Session) reapClient(id protocol.ClientID) {
	if id == protocol.UserClientID {
		return
	}

	s.subsMu.Lock()
	client, ok := s.subscribers[id]
	if ok {
		delete(s.subscribers, id)
		delete(s.clientHandles, id)
		delete(s.attachments, id)
	}
	s.subsMu.Unlock()

	if !ok {
		return
	}

	client.unsubOnce.Do(func() {
		client.closeOutbound()
		close(client.done)
	})

	<-client.pumpDone
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
// [Session.Handle] or [Session.Subscribe] call is refused and the
// command loop stops taking commands. It waits for handlers that
// crossed the gate before it closed, including the rollback phase of
// a multi-step ADDMODEL. The shape mirrors
// [net/http.Server.Shutdown]: new work is refused at
// the registration point, and dispatch goroutines belong to the
// model-clients holding subscriptions — they exit when their
// lifetime ctx passed to [New] is cancelled.
//
// Closing the gate also stops every subscription's outbound pump,
// which Shutdown then joins so no delivery goroutine outlives the
// call.
//
// Shutdown returns `ctx.Err()` if `ctx` is cancelled before the
// gate close and the pump join complete; otherwise nil. Safe to
// call more than once via `sync.Once` on the gate.
func (s *Session) Shutdown(ctx context.Context) error {
	return observability.SpanRunner{
		Tracer: s.tracerProvider.Tracer("github.com/laney/modeloff/internal/session"),
	}.Run(ctx, "session.shutdown", nil, func(ctx context.Context, _ trace.Span) error {
		if err := s.DrainHandlers(ctx); err != nil {
			return err
		}

		reapersDone := make(chan struct{})
		go func() {
			s.reapers.Wait()
			close(reapersDone)
		}()
		select {
		case <-reapersDone:
		case <-ctx.Done():
			return ctx.Err()
		}

		for _, sub := range s.subscriberSnapshot() {
			select {
			case <-sub.pumpDone:
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		return nil
	})
}

// DrainHandlers closes command and subscription admission, then waits for
// handlers that already crossed the gate. It leaves reaper and outbound-pump
// joining to [Session.Shutdown].
func (s *Session) DrainHandlers(ctx context.Context) error {
	s.handlersMu.Lock()
	s.handlersClosed = true
	s.handlersMu.Unlock()

	s.subsMu.Lock()
	s.shuttingDownOnce.Do(func() { close(s.shuttingDown) })
	s.subsMu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	handlersDone := make(chan struct{})
	go func() {
		s.handlers.Wait()
		close(handlersDone)
	}()
	select {
	case <-handlersDone:
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
func (s *Session) Connect(ctx context.Context) error {
	userHandle := s.lookupClientHandle(protocol.UserClientID)
	if !s.connectedAt.IsZero() {
		if userHandle != nil {
			if _, active := userHandle.connection(); !active {
				return s.reactivateUser(ctx, userHandle)
			}
		}

		// No span is recorded for a no-op call: it is not a real
		// connect attempt and would otherwise inflate
		// session.connect operation counts.
		return nil
	}

	return s.inSpan(ctx, "session.connect", nil, func(ctx context.Context, _ trace.Span) error {
		connectedAt := s.now()

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

		if err := s.store.SetSessionActive(ctx, connectedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("set session active: %w", err)
		}
		s.connectedAt = connectedAt

		if userClient := s.lookupClientHandle(protocol.UserClientID); userClient != nil {
			userClient.activateConnection()
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
				At:         connectedAt,
			})

			if unclean {
				s.deliverToClient(ctx, user.ID(), domain.Reconnected{At: connectedAt})
			}
		}

		s.connectedOnce.Do(func() { close(s.connectedC) })

		return nil
	})
}

func (s *Session) reactivateUser(ctx context.Context, userHandle *serverClient) error {
	return s.inSpan(ctx, "session.connect", nil, func(ctx context.Context, _ trace.Span) error {
		connectedAt := s.now()
		user := userHandle.instance

		_, err := s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
			if err := s.requireNickAvailable(ctx, user.Nick(), user); err != nil {
				return protocol.Response{}, err
			}
			if err := s.store.DeleteInstanceByID(ctx, user.ID()); err != nil {
				return protocol.Response{}, fmt.Errorf("remove retired user client: %w", err)
			}
			if err := s.store.SaveInstance(ctx, user); err != nil {
				return protocol.Response{}, fmt.Errorf("restore user client: %w", err)
			}

			return protocol.Response{}, nil
		})
		if err != nil {
			return err
		}

		if err := s.cleanupUncleanShutdown(ctx); err != nil {
			return fmt.Errorf("reconcile user client: %w", err)
		}

		_, err = s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
			if err := s.store.SetSessionActive(ctx, connectedAt.UTC().Format(time.RFC3339Nano)); err != nil {
				return protocol.Response{}, fmt.Errorf("set session active: %w", err)
			}

			userHandle.activateConnection()
			s.connectedAt = connectedAt
			s.deliverToClient(ctx, user.ID(), domain.Welcome{
				ServerName: domain.StatusServerName,
				Nick:       user.Nick(),
				At:         connectedAt,
			})

			return protocol.Response{}, nil
		})

		return err
	})
}

// cleanupUncleanShutdown drops the memberships the previous run left
// behind. A run that ended without a QUIT never ran the teardown
// that takes a client off its channels, so the persisted member
// lists still name it and its instance row still carries the
// channels. This is what a real server observes after a client
// disconnects without saying goodbye, and reconciling it here is
// what lets the connecting client start from a known-empty state and
// rejoin from its autojoin list.
//
// Only the connecting client is reconciled. A model-client's
// instance row is deleted by its own QUIT, and one that survived the
// previous run is reattached by the model manager at startup, which
// is what makes its memberships live again.
//
// The channel records are session state, so the work runs on the
// command loop. A channel the departure empties is destroyed, which
// is what the departure would have done had it happened in order
// (RFC 2811 §2).
func (s *Session) cleanupUncleanShutdown(ctx context.Context) error {
	user := s.userInstance()
	if user == nil {
		return nil
	}

	_, err := s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		// The channel records are what is asked, not the client's own
		// channel set: this client registered a moment ago with an
		// empty one, and what the previous run left behind is in the
		// member lists.
		windows, listErr := s.store.ListWindows(ctx)
		if listErr != nil {
			return protocol.Response{}, fmt.Errorf("list windows: %w", listErr)
		}

		for _, w := range windows {
			cw, ok := w.(*domain.ChannelWindow)
			if !ok || !cw.Members.HasInstance(user) {
				continue
			}

			window, loadErr := s.loadChannelWindow(ctx, cw.Name())
			if loadErr != nil {
				return protocol.Response{}, fmt.Errorf("load channel %q: %w", cw.Name(), loadErr)
			}

			if removeErr := s.removeDeletedMember(ctx, window, user); removeErr != nil {
				return protocol.Response{}, fmt.Errorf("drop stale membership of %q: %w", cw.Name(), removeErr)
			}
		}

		return protocol.Response{}, nil
	})

	return err
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
	// pool (lazily generated if missing). The error covers
	// structured-output validation and nick generation; a
	// preparation that fell short without failing reports it in
	// [PreparedInstance.Warnings].
	PrepareInstance(ctx context.Context, sess *Session, modelID domain.ModelID, persona string) (PreparedInstance, error)

	// Attach constructs (or returns the existing handle for) the
	// model-client backing `inst` and attaches it to `sess` via
	// [Session.Subscribe]. Idempotent on a repeat call for the
	// same identity. A successful call returns the client that owns
	// the new subscription; ADDMODEL verifies that ownership again
	// on the command loop immediately before admission.
	//
	// `inst` is a snapshot. Writing to it changes nothing the
	// session holds, and a client reads its current nick from
	// [protocol.Subscription.Nick], which a NICK rewrites.
	Attach(
		ctx context.Context,
		sess *Session,
		inst *domain.Instance,
		attachment *protocol.Attachment,
	) (protocol.Client, error)

	// InterruptTurn cancels the model's current upstream turn without
	// ending its event loop.
	InterruptTurn(id protocol.ClientID)

	// InterruptWindow cancels the model's current upstream turn only
	// when it belongs to `window`, without ending its event loop.
	InterruptWindow(id protocol.ClientID, window domain.ChannelName)

	// InstanceDeleted records that the instance row has been deleted.
	// The factory retains any dependent cleanup obligation until Detach
	// or its shutdown drain fulfils it.
	InstanceDeleted(id protocol.ClientID)

	// Detach releases the model-client for `id`: it unsubscribes
	// the client and tells its dispatch goroutine to stop. It does
	// not wait for that goroutine to finish — a model that ends its
	// own connection with the `quit` tool issues the QUIT from
	// inside it, so a wait here would be that goroutine waiting on
	// itself. Joining belongs to whoever owns the client's
	// lifetime. Idempotent on an unknown id.
	Detach(id protocol.ClientID)
}

// PreparedInstance is what [ModelClientFactory.PrepareInstance]
// resolved for a new model instance, before `ADDMODEL` claims the
// nick and registers the instance.
type PreparedInstance struct {
	// Nick is the unique nick the instance claims. `ADDMODEL`
	// re-checks it on the command loop, because it was chosen off
	// the loop and a rename may have taken it since.
	Nick domain.Nick

	// Persona is the copied persona text the instance carries: a
	// matched template's description, unmatched literal text from the
	// requester, or one drawn from the pool.
	Persona string

	// Warnings describes, for the operator, each part of the
	// preparation that fell short without failing the command. A
	// persona the pool could not supply is the case that exists
	// today: the model joins and behaves differently for the rest of
	// its life, and the person who issued the `ADDMODEL` is the one
	// who can do something about it. `handleAddModel` answers each
	// warning with a [domain.SystemNotice] on the command's reply.
	Warnings []string
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

// EmitModelFailure reports a failed model turn to connected operators.
func (s *Session) EmitModelFailure(
	ctx context.Context,
	window protocol.WindowTarget,
	event domain.ModelUnavailableError,
) {
	s.fanOutProtocol(ctx, protocolEmission{event: event, scope: operatorsScope{}, window: window}, 0)
}

// ResolveInstanceByID returns the current nick of the connected client
// with `id`. A stored instance whose connection is inactive is not
// addressable and therefore does not resolve.
func (s *Session) ResolveInstanceByID(_ context.Context, id domain.InstanceID) (domain.Nick, error) {
	client := s.activeClientHandle(protocol.ClientID(id))
	if client == nil {
		return "", fmt.Errorf("instance %q: %w", id, store.ErrNoSuchNick)
	}

	return client.instance.Nick(), nil
}

// ClientConnected reports whether `id` has active connection
// authority. It does not expose the retained subscription transport.
func (s *Session) ClientConnected(id protocol.ClientID) bool {
	return s.activeClientHandle(id) != nil
}

// StartModelClients attaches each stored model instance. The user
// sentinel and an identity that is already connected are skipped.
func (s *Session) StartModelClients(ctx context.Context) error {
	instances, err := s.store.ListInstances(ctx)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}

	var firstErr error
	for _, inst := range instances {
		id := protocol.ClientID(inst.ID())
		if id == protocol.UserClientID || s.ClientConnected(id) {
			continue
		}

		if _, attachErr := s.startModelClient(ctx, inst); attachErr != nil {
			slog.Default().WarnContext(ctx, "attach boot model client",
				"component", "session",
				"instance_id", inst.ID(),
				"error", attachErr,
			)
			if firstErr == nil {
				firstErr = attachErr
			}
		}
	}

	return firstErr
}

func (s *Session) startModelClient(
	ctx context.Context,
	inst *domain.Instance,
) (protocol.Client, error) {
	id := protocol.ClientID(inst.ID())
	attachment := s.issueAttachment(id)
	client, err := s.modelClientFactory.Attach(ctx, s, inst.Snapshot(), attachment)
	if err != nil {
		s.revokeAttachment(id, attachment)
	}

	return client, err
}

func (s *Session) clientOwner(id protocol.ClientID) protocol.Client {
	sc := s.activeClientHandle(id)
	if sc == nil {
		return nil
	}

	return sc.owner
}

// ClientCaps reports the capabilities granted to the subscription
// registered under `id`, satisfying [protocol.CapsRegistry]. The
// returned holder reads the subscription's live mode set, so a
// caller may keep it and still see a later `MODE` change; an
// identity with no subscription holds nothing.
//
// This is the same mode set [Session.idHasServerOper] gates
// operator-only commands on. Both client kinds delegate their
// `Caps()` here, so the commands a client is offered and the
// commands the dispatcher will run for it are decided from one
// place.
func (s *Session) ClientCaps(id protocol.ClientID) command.CapabilityHolder {
	sc := s.activeClientHandle(id)
	if sc == nil {
		return command.NoCapabilities()
	}

	return sc.Caps()
}

// ResolveNick turns a connected client's nick into its stable identity
// and canonical display spelling. The lookup applies the server's
// casemapping, so `/whois Botty` finds a connected `botty`.
func (s *Session) ResolveNick(_ context.Context, nick domain.Nick) (domain.InstanceID, domain.Nick, error) {
	client := s.lookupClientByNick(nick)
	if client == nil {
		return "", "", fmt.Errorf("resolve nick %q: %w", nick, store.ErrNoSuchNick)
	}

	return client.instance.ID(), client.instance.Nick(), nil
}

// NickClaimed reports whether an instance row already reserves `nick`.
// A registered model claims its nick before its connection attaches, so
// this differs from [Session.ResolveNick], which only resolves clients
// that can currently receive a command.
func (s *Session) NickClaimed(ctx context.Context, nick domain.Nick) (bool, error) {
	_, err := s.store.ResolveNick(ctx, nick)
	if errors.Is(err, store.ErrNoSuchNick) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return true, nil
}

// directoryChannels returns the channel directory `issuer` may see,
// for `/list`. It filters to `*ChannelWindow` only, since DMs and
// the status window are not in the directory, and then to the
// channels [Session.channelVisibleTo] admits, so a `+s` or `+p`
// channel the issuer is not on and holds no operator mode for is
// left out entirely. The returned entries are snapshots of name,
// member count, and topic; callers turn them into per-row
// `domain.ListReply` events themselves.
func (s *Session) directoryChannels(ctx context.Context, issuer *domain.Instance) ([]domain.ChannelDirectoryEntry, error) {
	var entries []domain.ChannelDirectoryEntry

	err := s.inSpan(ctx, "session.directory_channels", nil, func(ctx context.Context, span trace.Span) error {
		windows, err := s.directoryChannelWindows(ctx)
		if err != nil {
			return fmt.Errorf("list windows: %w", err)
		}

		entries = make([]domain.ChannelDirectoryEntry, 0, len(windows))
		for _, cw := range windows {
			if !s.channelVisibleTo(issuer, cw.Name(), cw.Modes) {
				continue
			}

			entries = append(entries, domain.ChannelDirectoryEntry{
				Channel: cw.Name(),
				Members: cw.Members.Len(),
				Topic:   cw.Topic,
			})
		}

		span.SetAttributes(attribute.Int("directory.entry_count", len(entries)))

		return nil
	})

	return entries, err
}

// ChannelWindowNames returns every addressable channel name known to
// the session. Stored channels keep their iteration order; live-only
// channels follow in name order. Unlike the actor-bound directory
// capability, no mode-visibility filter is applied because the
// session's poke scheduler visits secret channels too.
func (s *Session) ChannelWindowNames(ctx context.Context) ([]domain.ChannelName, error) {
	var names []domain.ChannelName

	err := s.inSpan(ctx, "session.channel_window_names", nil, func(ctx context.Context, _ trace.Span) error {
		windows, err := s.directoryChannelWindows(ctx)
		if err != nil {
			return fmt.Errorf("list windows: %w", err)
		}

		names = make([]domain.ChannelName, 0, len(windows))
		for _, window := range windows {
			names = append(names, window.Name())
		}

		return nil
	})

	return names, err
}

// Instances returns an iterator over every registered instance,
// which is every client with a connection record, the one behind the
// calling process included. A caller that wants everybody but itself
// excludes its own identity, which is a question only that caller
// can answer.
//
// The iterator materialises a snapshot at call time and is safe
// to range after the session state changes; subsequent mutations
// will not be visible on the same iterator.
func (s *Session) Instances(_ context.Context) iter.Seq[domain.InstanceDirectoryEntry] {
	connections := s.activeConnections()
	instances := make([]domain.InstanceDirectoryEntry, 0, len(connections))
	for _, connection := range connections {
		inst := connection.client.instance
		instances = append(instances, domain.InstanceDirectoryEntry{
			InstanceID: inst.ID(),
			Nick:       inst.Nick(),
			ModelID:    inst.ModelID,
		})
	}
	sort.Slice(instances, func(i, j int) bool { return instances[i].Nick < instances[j].Nick })

	return func(yield func(domain.InstanceDirectoryEntry) bool) {
		for _, inst := range instances {
			if !yield(inst) {
				return
			}
		}
	}
}

// UnreadCount returns the number of events in a window that arrived
// after the user's last-read position. The cursor is the user's, so
// a DM is counted from the user's side of the thread.
func (s *Session) UnreadCount(ctx context.Context, ch domain.ChannelName) (int, error) {
	var count int

	err := s.inSpan(ctx, "session.unread_count", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
	}, func(ctx context.Context, span trace.Span) error {
		defer func() {
			span.SetAttributes(attribute.Int("unread.count", count))
		}()

		// A DM's cursor lives in its own table, keyed by the
		// counterpart's InstanceID, and its two directions are
		// logged under their recipients, so the count is over the
		// thread: counting the window's own key would count the
		// lines the user sent and none of the ones it has not read.
		if domain.InferChannelKind(ch) == domain.KindDM {
			peer := domain.InstanceID(ch)

			lastID, err := s.store.GetDMLastRead(ctx, peer)
			if err != nil {
				return fmt.Errorf("get last read: %w", err)
			}

			count, err = s.store.CountDMEventsFrom(ctx, domain.InstanceID(protocol.UserClientID), peer, unreadFrom(lastID))

			return err
		}

		lastID, err := s.store.GetLastRead(ctx, ch)
		if err != nil {
			return fmt.Errorf("get last read: %w", err)
		}

		count, err = s.store.CountEventsFrom(ctx, ch, unreadFrom(lastID))

		return err
	})

	return count, err
}

// unreadFrom turns a last-read cursor into the "at or after" argument
// CountEventsFrom / CountDMEventsFrom take: nil when nothing has been
// read, so the whole log counts, or the id right after the cursor
// otherwise.
func unreadFrom(lastID int64) *int64 {
	if lastID <= 0 {
		return nil
	}

	fromID := lastID + 1

	return &fromID
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

// AuditEventsBefore returns raw channel-log events for diagnostics.
// These rows precede recipient-specific projection and must not be
// used as an actor's scrollback.
func (s *Session) AuditEventsBefore(
	ctx context.Context,
	ch domain.ChannelName,
	before *int64,
	n int,
) ([]domain.StoredEvent, error) {
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

type protocolEmission struct {
	event  domain.ProtocolEvent
	scope  deliveryScope
	window protocol.WindowTarget
}

type deliveryScope interface{ deliveryScope() }

type channelScope struct{ channel domain.ChannelName }
type sharedChannelsScope struct {
	channels []domain.ChannelName
	masked   []domain.ChannelName
}
type clientScope struct{ client domain.InstanceID }
type operatorsScope struct{}

func (channelScope) deliveryScope()        {}
func (sharedChannelsScope) deliveryScope() {}
func (clientScope) deliveryScope()         {}
func (operatorsScope) deliveryScope()      {}

// emit hands a protocol event to the subscriber-registry fan-out.
// The context is threaded through to preserve OTel trace parenting
// and to honour cancellation during fan-out.
func (s *Session) emitScoped(
	ctx context.Context,
	evt domain.ProtocolEvent,
	scope deliveryScope,
) {
	s.fanOutProtocol(ctx, protocolEmission{event: evt, scope: scope}, 0)
}

func (s *Session) emitStored(
	ctx context.Context,
	evt domain.ProtocolEvent,
	eventID int64,
	scope deliveryScope,
) {
	s.fanOutProtocol(ctx, protocolEmission{event: evt, scope: scope}, eventID)
}

// persistAndEmit appends `evt` to the channel event log and emits
// it on the protocol bus, in that order. Persistence completing
// before emission is a session-wide invariant: any consumer that
// learns about an event must always be able to find the same
// event in the store. The event log receives the canonical value;
// fan-out derives each recipient's visible value from it.
func (s *Session) persistAndEmit(ctx context.Context, ch domain.ChannelName, evt broadcastEvent) error {
	eventID, err := s.appendEventResult(ctx, ch, evt)
	if err != nil {
		return err
	}

	s.emitStored(ctx, evt, eventID, channelScope{channel: ch})

	return nil
}

func actorEventForChannel(
	evt broadcastEvent,
	channel domain.ChannelName,
	anonymous []domain.ChannelName,
) broadcastEvent {
	quit, ok := evt.(domain.Quit)
	if !ok || !slices.Contains(anonymous, channel) {
		return evt
	}

	return domain.Part{
		Target:  channel,
		Source:  domain.AnonymousSource(),
		Message: quit.Message,
		At:      quit.At,
	}
}

func (s *Session) appendEvent(ctx context.Context, ch domain.ChannelName, event domain.ChannelActivity) int64 {
	eventID, _ := s.appendEventResult(ctx, ch, event)

	return eventID
}

func (s *Session) appendEventResult(
	ctx context.Context,
	ch domain.ChannelName,
	event domain.ChannelActivity,
) (int64, error) {
	eventID, err := s.store.AppendEvent(ctx, ch, event)
	if err != nil {
		slog.Default().ErrorContext(ctx, "append event", "channel", ch, "error", err)
		s.recordPersistenceFailure(ctx, ch)

		return 0, err
	}

	return eventID, nil
}

// persistInstanceReplies records an issuer's point-to-point reply
// events in its private reply log, keyed by the issuer's identity. A
// model re-reads them as its own experience on later turns. The
// user-client (whose identity is the empty id) writes the same log;
// it is transient and never restores, so its entries are the durable
// record a future restore or inspector would read rather than
// anything the user sees again this session.
func (s *Session) persistInstanceReplies(
	ctx context.Context,
	c protocol.Client,
	window protocol.WindowTarget,
	events []domain.ProtocolEvent,
) {
	id := domain.InstanceID(c.Identity())
	for _, ev := range events {
		if reply, ok := ev.(domain.IssuerReply); ok {
			s.appendInstanceReply(ctx, id, window, reply)
		}
	}
}

// appendInstanceReply best-effort persists one reply to the
// instance's private log. A failed write is logged and counted but
// does not fail the command: the reply was already delivered live,
// and only the instance's durable memory of it is lost.
func (s *Session) appendInstanceReply(
	ctx context.Context,
	id domain.InstanceID,
	window protocol.WindowTarget,
	event domain.IssuerReply,
) {
	if _, err := s.store.AppendInstanceReply(ctx, id, window, event); err != nil {
		slog.Default().ErrorContext(ctx, "append instance reply", "instance_id", id, "error", err)
		s.recordInstancePersistenceFailure(ctx, id)
	}
}

// recordPersistenceFailure increments the persistence-failures
// counter. A non-empty channel tags an operation confined to one
// window; batched projection writes leave the channel empty.
func (s *Session) recordPersistenceFailure(ctx context.Context, ch domain.ChannelName) {
	if s.persistenceFailures == nil {
		return
	}
	if ch == "" {
		s.persistenceFailures.Add(ctx, 1)

		return
	}

	s.persistenceFailures.Add(ctx, 1,
		metric.WithAttributes(attribute.String(observability.AttrChannel, string(ch))))
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
// refusals) wrap their returned error with
// [observability.ErrWithKind], which the classifier here unwraps.
func (s *Session) inSpan(
	ctx context.Context,
	op string,
	attrs []attribute.KeyValue,
	fn func(ctx context.Context, span trace.Span) error,
) error {
	return observability.SpanRunner{
		Tracer:         s.tracerProvider.Tracer("github.com/laney/modeloff/internal/session"),
		DefaultErrKind: observability.ErrorKindStore,
		ClassifyError:  observability.ErrorKindOf,
	}.Run(ctx, op, attrs, fn)
}
