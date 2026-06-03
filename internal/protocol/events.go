package protocol

import "github.com/laney/modeloff/internal/domain"

// Event is the closed sum of messages the server emits to subscribed
// clients. See [domain.ProtocolEvent] for the canonical list of
// member types.
type Event = domain.ProtocolEvent

// NotOperatorError is the protocol-shaped form of ERR_NOPRIVILEGES
// (RFC 2812 numeric 481). Returned in [Response.Err] when an
// operator-gated command was rejected for lack of [ModeOperator],
// and also a member of the [Event] sum so future paths can emit
// it on the bus.
type NotOperatorError = domain.NotOperatorError
