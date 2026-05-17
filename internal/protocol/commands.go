package protocol

import "github.com/laney/modeloff/internal/domain"

// Command is the closed sum of operations a [Client] can issue. The
// sum is sealed by the unexported `isCommand` method so the
// dispatcher's exhaustive switch is checked at compile time: adding a
// new command makes every dispatcher fail to build until it is
// handled.
//
// Commands are dispatched synchronously through [Client.Send]. The
// originator receives a [Response] carrying confirmation events or a
// typed error; broadcast side effects flow asynchronously to peers
// via [Client.Events].
type Command interface {
	isCommand()
}

// Join asks the server to add the issuing client to the named
// channel, creating it if it does not yet exist.
type Join struct {
	Channel domain.ChannelName
}

// Part asks the server to remove the issuing client from the named
// channel. Reason is broadcast to remaining members.
type Part struct {
	Channel domain.ChannelName
	Reason  string
}

// PrivMsg sends a chat message to a channel or DM target. The same
// command shape covers both: the dispatcher infers routing from
// `Target`'s [domain.ChannelKind].
type PrivMsg struct {
	Target domain.ChannelName
	Body   string
}

// Action sends a /me-style action message to a channel or DM target.
type Action struct {
	Target domain.ChannelName
	Body   string
}

// Topic sets the channel topic. Setting an empty body clears it.
type Topic struct {
	Channel domain.ChannelName
	Body    string
}

// Invite asks the server to add a model instance to the named
// channel. Operator-gated for non-self invites in future revisions;
// today the user-client is the sole inviter.
type Invite struct {
	Nick    domain.Nick
	Channel domain.ChannelName
}

// Kick asks the server to remove a model instance from the named
// channel.
type Kick struct {
	Nick    domain.Nick
	Channel domain.ChannelName
}

// Nick changes the issuing client's display nick. The server is
// authoritative on uniqueness and rejects collisions with
// [domain.NickInUseError].
type Nick struct {
	New domain.Nick
}

// Whois asks the server to emit a [domain.Whois] reply describing
// the named instance.
type Whois struct {
	Nick domain.Nick
}

// List asks the server to emit a stream of [domain.ListReply] events
// terminated by [domain.ListEnd], shaped after IRC's RPL_LIST and
// end-of-list (323) numerics.
type List struct{}

// AddModel creates a new model instance, persists it, registers a
// model-client subscription for it, and broadcasts its arrival.
// Operator-gated: the issuing client must carry [ModeOperator].
type AddModel struct {
	Model   domain.ModelID
	Persona string
}

// Quit disconnects the issuing client. Broadcast semantics follow
// RFC 1459 §4.1.6: peers in shared channels receive a QUIT line and
// the issuing client's [Client.Events] channel is closed by the
// server. The instance row stays in the store; QUIT is "disconnect
// this client", not "delete this model".
type Quit struct {
	Reason string
}

// Kill is a server-initiated disconnect of another client per
// RFC 2812 §3.7.1. Operator-gated: the issuing client must carry
// [domain.ModeOperator]. On success the dispatcher emits a
// [domain.Killed] event on the killed client's Events channel,
// broadcasts QUIT to peers with the conventional "Killed by
// <oper> (<reason>)" reason, and reaps the subscription.
type Kill struct {
	Nick   domain.Nick
	Reason string
}

// Oper is RFC 2812 §3.1.4 self-elevation. The dispatcher delegates
// credential validation to a configurable authenticator on the
// session; on success the server issues the canonical MODE
// response (a [domain.ModeChange] with empty Target) to the
// requesting client. On failure it returns [domain.OperFailedError].
//
// The default authenticator rejects every caller — there is no
// client-side path to +o today. The local user (the user-client)
// gets +o via server-initiated bootstrap, not via this command;
// future credentialed model elevation slots in by swapping the
// authenticator.
type Oper struct {
	Name     string
	Password string
}

func (Join) isCommand()     {}
func (Part) isCommand()     {}
func (PrivMsg) isCommand()  {}
func (Action) isCommand()   {}
func (Topic) isCommand()    {}
func (Invite) isCommand()   {}
func (Kick) isCommand()     {}
func (Nick) isCommand()     {}
func (Whois) isCommand()    {}
func (List) isCommand()     {}
func (AddModel) isCommand() {}
func (Quit) isCommand()     {}
func (Kill) isCommand()     {}
func (Oper) isCommand()     {}
