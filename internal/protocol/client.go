package protocol

import (
	"context"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
)

// Client is a connected participant on the wire. The dispatcher does
// not know whether it is talking to a chat-screen client or a model
// client: capability parity is enforced because both implementations
// flow [Command]s through the same `Send` and receive [Event]s from
// the same `Events` channel.
//
// Lifetime is implicit. The user-client lives for the session; each
// model-client lives for its `*domain.Instance`. The server reaps
// subscriptions inside the [AddModel]/[Quit]/[Kill] handlers and
// inside `Session.Shutdown`; there is no separate Disconnect call.
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
	// The server is the sole writer and owns the channel's
	// lifecycle: it closes the channel on [Quit] / [Kill] / session
	// shutdown.
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

// Subscription is the handle a client carries after attaching to a
// session. It exposes the per-client delivery stream, a "done"
// signal that fires when the subscription is reaped (either by the
// client calling Unsubscribe or by the session removing it via a
// QUIT / KILL handler), and the release mechanism.
type Subscription interface {
	// Events returns the read end of the per-client delivery
	// stream. Same semantics as [Client.Events] — the
	// subscription handle is the canonical way to get at it once
	// a client has been attached via [Session.Subscribe].
	Events() <-chan Delivery

	// Done returns a channel that closes when the subscription is
	// reaped from any source. Long-running consumers (e.g. a
	// model-client's dispatch goroutine) select on Done alongside
	// Events to exit cleanly when the session has detached them.
	Done() <-chan struct{}

	// Unsubscribe removes the client from the session's subscriber
	// registry and closes [Done]. Idempotent.
	Unsubscribe()
}

// SubscribeOptions configures a Subscribe call. Instance is the
// canonical actor handle the dispatcher reads to resolve the actor
// for any command this client issues; it is required.
// InitialModes applies the given modes to the subscription before
// the first event can be delivered, so a client granted +o at
// subscribe time sees the [domain.UserModeChange] event as the first
// item on its bus.
type SubscribeOptions struct {
	Instance     *domain.Instance
	InitialModes []domain.Mode

	// EchoMessage grants IRCv3 echo-message: the session delivers the
	// client's own PRIVMSG / ACTION back to it over Events, so a
	// consumer renders its sent lines from the bus like any other
	// event. Without it, a client follows RFC 2812 §3.3.1 and never
	// sees its own chat traffic echoed.
	EchoMessage bool
}
