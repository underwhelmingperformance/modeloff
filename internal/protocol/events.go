package protocol

import "github.com/laney/modeloff/internal/domain"

// Event is the closed sum of messages the server emits to subscribed
// clients. See [domain.ProtocolEvent] for the canonical list of
// member types.
type Event = domain.ProtocolEvent

// Killed is emitted on the killed client's Events channel as the
// final wire event before the server closes it. Renderers display it
// as the scrollback's last word.
type Killed = domain.Killed

// NotOperatorError is the protocol-shaped form of ERR_NOPRIVILEGES
// (RFC 2812 numeric 481). Returned in [Response.Err] when an
// operator-gated command was rejected for lack of [ModeOperator]; it
// is not part of the [Event] sum.
type NotOperatorError = domain.NotOperatorError
