package protocol

import (
	"context"
	"errors"
	"fmt"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
)

var (
	// ErrSubscriptionClosed means the server has revoked a subscription's
	// authority and its actor-bound capabilities can no longer be used.
	ErrSubscriptionClosed = errors.New("subscription is closed")

	// ErrInvalidWindowTarget means a caller supplied no channel or direct
	// conversation to an actor-bound window operation.
	ErrInvalidWindowTarget = errors.New("invalid window target")

	// ErrWindowAuthorityChanged means the actor lost access to the requested
	// window while an actor-bound read was in progress.
	ErrWindowAuthorityChanged = errors.New("window authority changed")
)

// JoinExecutionError identifies the JOIN target whose server-side operation
// failed after command validation.
type JoinExecutionError struct {
	Channel domain.ChannelName
	Err     error
}

func (e JoinExecutionError) Error() string {
	return fmt.Sprintf("join %s: %v", e.Channel, e.Err)
}

func (e JoinExecutionError) Unwrap() error { return e.Err }

// Client is a connected participant on the wire. The dispatcher does
// not know whether it is talking to a chat-screen client or a model
// client: capability parity is enforced because both implementations
// flow [Command]s through the same `Send` and receive [Event]s from
// the same `Events` channel.
//
// Lifetime is implicit. The user-client lives for the session; each
// model-client lives for one registered actor. The server reaps
// subscriptions inside the [AddModel]/[Quit]/[Kill] handlers and
// inside `Session.Shutdown`; there is no separate Disconnect call.
// Implementations use non-nil pointer values so the session can bind
// a subscription to one exact connection object.
type Client interface {
	// Identity returns the client's stable [ClientID].
	// [UserClientID] names the user-client; any non-empty id is the
	// originating instance.
	Identity() ClientID

	// Send dispatches a command synchronously and returns a
	// [Response] carrying confirmation events plus an optional typed
	// error. Broadcast side effects flow asynchronously to peers via
	// [Client.Events].
	Send(ctx context.Context, cmd Command) (Response, error)

	// Events returns the read end of the per-client delivery
	// stream. Each [Delivery] wraps an [Event] alongside the
	// originating handler's span context for OTel trace continuity.
	// The server is the sole writer. Consumers use
	// [Subscription.Done] to observe the end of a subscription;
	// the delivery channel itself remains open.
	Events() <-chan Delivery

	// Caps exposes the client's modes (and any future runtime
	// state) as a [command.CapabilityHolder] so the chatcmd
	// grammar's `caps:` filter can hide commands the client cannot
	// use. It answers from the same live mode set the dispatcher's
	// operator gate reads, keyed by [Client.Identity], so an [Oper]
	// elevation reaches both without the client object changing.
	// [LiveCaps] is the implementation both client kinds return.
	Caps() command.CapabilityHolder
}

// CapsRegistry is the session-side read a client delegates
// [Client.Caps] to: the registry holding the per-subscription mode
// set. `*session.Session` satisfies it.
type CapsRegistry interface {
	// ClientCaps reports the capabilities granted to the subscription
	// registered under `id`. An identity with no subscription holds
	// none.
	ClientCaps(id ClientID) command.CapabilityHolder
}

// LiveCaps binds `registry` and `id` into a [command.CapabilityHolder]
// that consults the registry afresh on every question. Both client
// kinds return one from [Client.Caps], so the answer is the server's:
// a mode the session grants or clears is reflected the next time the
// command-visibility filter or the model tool registry asks, and the
// client side holds no copy to keep in step.
func LiveCaps(registry CapsRegistry, id ClientID) command.CapabilityHolder {
	return liveCaps{registry: registry, id: id}
}

type liveCaps struct {
	registry CapsRegistry
	id       ClientID
}

func (c liveCaps) Has(capability command.Capability) bool {
	return c.registry.ClientCaps(c.id).Has(capability)
}

// CapOperator is the visibility capability backed by
// [domain.ModeOperator] (+o). Chatcmd grammar entries declaring
// `caps:"operator"` are filtered out of completion suggestions,
// `/help` output, and the model tool registry for clients whose
// [Client.Caps] holder does not hold +o.
const CapOperator command.Capability = "operator"

// UserCredential is an unforgeable capability shared by the session
// constructor and its one user-client. Equality is by pointer: a
// different credential does not authenticate the sentinel identity.
type UserCredential struct{ nonce byte }

// NewUserCredential creates the capability used to bind one session
// to its user-client.
func NewUserCredential() *UserCredential { return &UserCredential{nonce: 1} }

// Attachment authorises one model client to attach as the identity for
// which the session registered it. Constructing an Attachment does not
// grant authority; the session accepts only the exact value it issued.
type Attachment struct{ nonce byte }

// NewAttachment creates an opaque attachment value for the session to
// register before it asks a model-client factory to attach an actor.
func NewAttachment() *Attachment { return &Attachment{nonce: 1} }

// MaxScrollbackEntries bounds one actor-scoped scrollback read. The
// store can grow past its retention headroom between process starts,
// so the session enforces this limit independently of retention.
const MaxScrollbackEntries = 2000

// Subscription is the handle a client carries after attaching to a
// session. It exposes the per-client delivery stream, a "done"
// signal that fires when the subscription is reaped (either by the
// client calling Unsubscribe or by the session removing it via a
// QUIT / KILL handler), and the release mechanism.
type Subscription interface {
	// Events returns the read end of the per-client delivery
	// stream. Same semantics as [Client.Events]: the
	// subscription handle is the canonical way to get at it once
	// a client has been attached via [Session.Subscribe].
	Events() <-chan Delivery

	// Done returns a channel that closes when the subscription is
	// reaped from any source. Long-running consumers (e.g. a
	// model-client's dispatch goroutine) select on Done alongside
	// Events to exit cleanly when the session has detached them.
	Done() <-chan struct{}

	// Scrollback returns the actor's current view of one channel or
	// direct-message window. The session binds the actor to this
	// subscription, so callers cannot choose an identity. Each call
	// rechecks that the actor may still read the window and caps the
	// result at [MaxScrollbackEntries].
	Scrollback(ctx context.Context, window WindowTarget, limit int) ([]ScrollbackEntry, error)

	// Replies returns this actor's private issuer replies for exactly
	// one window. A nil window selects session-wide replies. The
	// session binds the identity to this subscription, rechecks window
	// authority, and caps each read at [MaxScrollbackEntries].
	Replies(ctx context.Context, window WindowTarget, limit int) ([]ReplyEntry, error)

	// DirectoryChannels returns the channel directory visible to this
	// subscription's actor.
	DirectoryChannels(ctx context.Context) ([]domain.ChannelDirectoryEntry, error)

	// Nick returns the nick the server currently holds for this
	// subscription's actor. A NICK rename is server state, so the
	// answer comes from the session and not from whatever copy of
	// the actor the client was constructed with.
	Nick() domain.Nick

	// GuardWindow captures this subscription's current authority for a
	// window. Valid becomes false if the subscription closes, a channel
	// membership interval ends, or a DM counterpart disconnects.
	GuardWindow(ctx context.Context, window WindowTarget) (WindowGuard, error)

	// GuardInvitation captures the actor's current invitation to a
	// channel. It becomes invalid if the channel disappears or the
	// invitation is consumed or revoked.
	GuardInvitation(ctx context.Context, channel domain.ChannelName) (WindowGuard, error)

	// Activate ends attach-time replay and releases deliveries that
	// arrived after the scrollback snapshots. It is idempotent. A
	// subscription created without ReplayHistory is active already.
	Activate()

	// Unsubscribe removes the client from the session's subscriber
	// registry and closes [Done]. Idempotent.
	Unsubscribe()
}

// WindowGuard checks whether one subscription still has the same
// authority for a window. It exposes no store identifier or membership
// generation.
//
// Send is part of the same authority. The session checks the guard
// on its command loop immediately before the command's handler runs,
// so the command reaches the handler only while the authority still
// stands.
type WindowGuard interface {
	Valid(ctx context.Context) bool
	Context(ctx context.Context) (WindowContext, error)
	RunWithAuthority(ctx context.Context, operation func() error) error
	Send(ctx context.Context, client Client, cmd Command) (Response, error)
}

// ChannelMemberState is the visible nick and privileges of one channel
// member at the time a turn begins. It omits the member's stable identity,
// which is not part of the IRC state disclosed to peers.
type ChannelMemberState struct {
	Nick  domain.Nick
	Modes domain.MemberModes
}

// ChannelState is the actor-visible channel state at the time a turn begins.
// Anonymous channels contain the single masked member disclosed by NAMES.
type ChannelState struct {
	Modes   domain.ChannelModes
	Members []ChannelMemberState
}

// WindowContext is the current state from which a model turn may be
// assembled. A context describes the conversation, but it does not grant
// access to it; the [WindowGuard] remains the authority and is rechecked
// before upstream calls and tool execution.
//
// Direct conversations expose only the counterpart's stable identity. A
// nick belongs to the messages in which the server disclosed it, not to the
// conversation key, so a rename cannot leave cached presentation state here.
type WindowContext interface {
	Target() WindowTarget
	ChannelState() (ChannelState, bool)
	Topic() (domain.TopicInfo, bool)
}

// ModelDispatch tracks the recipients that observed the start of one
// model turn. Done sends the matching completion to those recipients.
type ModelDispatch interface {
	Done(ctx context.Context, event domain.ModelDispatchDone)
}

// HistorySourceKind identifies the durable log that supplied an actor-visible
// event.
type HistorySourceKind uint8

const (
	// HistorySourceEvent identifies a row in the canonical event log.
	HistorySourceEvent HistorySourceKind = iota + 1
	// HistorySourceChannelScrollback identifies a recipient-projected channel
	// scrollback row.
	HistorySourceChannelScrollback
)

// HistoryRef is an opaque identity for one event in an actor's visible
// history. It allows private derived state to deduplicate live delivery and
// replay without granting access to the underlying store row.
type HistoryRef struct {
	Kind   HistorySourceKind
	ID     int64
	Window WindowTarget
}

// ChannelHistoryRef identifies one recipient-projected channel row.
func ChannelHistoryRef(id int64, channel domain.ChannelName) HistoryRef {
	return HistoryRef{
		Kind: HistorySourceChannelScrollback, ID: id,
		Window: ChannelWindowTarget(channel),
	}
}

// DirectHistoryRef identifies one canonical event row in a direct
// conversation.
func DirectHistoryRef(id int64, peer domain.InstanceID) HistoryRef {
	return HistoryRef{
		Kind: HistorySourceEvent, ID: id,
		Window: DirectWindowTarget(peer),
	}
}

// ScrollbackEntry is one ordered event in an actor's visible scrollback.
type ScrollbackEntry struct {
	Event   domain.PersistableEvent
	History HistoryRef
}

// ReplyEntry is one private issuer reply together with the window in
// which the actor issued the command. A nil Window marks session-wide
// state that is safe to include in every window.
type ReplyEntry struct {
	Window WindowTarget
	Event  domain.IssuerReply
}

// SubscribeOptions configures a Subscribe call. The session resolves
// the canonical actor from the subscribing client's identity.
type SubscribeOptions struct {
	// Attachment authorises a model identity. The session issues it as
	// part of server-side model registration or startup. It must be nil
	// for the user identity.
	Attachment *Attachment

	// UserCredential additionally authenticates the reusable sentinel
	// user identity. It must be nil for every model identity.
	UserCredential *UserCredential

	// ReplayHistory keeps live deliveries queued until the client has
	// loaded its visible scrollback and called [Subscription.Activate].
	ReplayHistory bool

	// EchoMessage grants IRCv3 echo-message: the session delivers the
	// client's own PRIVMSG / ACTION back to it over Events, so a
	// consumer renders its sent lines from the bus like any other
	// event. Without it, a client follows RFC 2812 §3.3.1 and never
	// sees its own chat traffic echoed.
	EchoMessage bool
}
