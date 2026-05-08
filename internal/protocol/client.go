package protocol

import "context"

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

	// Events returns the read end of the per-client event stream.
	// The server is the sole writer and owns the channel's
	// lifecycle: it closes the channel on [Quit] / [Kill] / session
	// shutdown.
	Events() <-chan Event

	// HasMode reports whether the client carries the given user
	// mode. Operator-gated handlers consult this with [ModeOperator]
	// and return [NotOperatorError] on failure.
	HasMode(m UserMode) bool
}

// UserMode is a single RFC 2812 §3.1.5 user-mode flag. Today only
// [ModeOperator] is meaningful; the type is kept open so future
// modes (`+i`, `+a`, `+w`, …) can be added without churn.
//
// `rune` is the natural carrier — IRC mode flags are single ASCII
// letters.
type UserMode rune

// ModeOperator is `+o` (RFC 2812 §3.1.5). Today only the
// user-client carries it; it is granted at session bootstrap.
// Operator-gated commands ([AddModel], [Kill]) consult
// [Client.HasMode] in their handler and return [NotOperatorError]
// on failure. The gate is on the *capability*, not on the client
// kind, so any future client granted `+o` would pass it.
const ModeOperator UserMode = 'o'
