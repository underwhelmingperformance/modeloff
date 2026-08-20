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
//
// `Name` returns the IRC mnemonic the command corresponds to
// (uppercase, RFC 2812 names where one exists).
type Command interface {
	isCommand()
	Name() string
}

// MaxJoinTargets is the most channels a single JOIN may name, the
// TARGMAX RFC 2812 ISUPPORT advertises for a real ircd; real ircds
// vary between four and ten, and this picks the upper end. The
// dispatcher refuses an over-cap JOIN before joining any of its
// channels: RFC 1459 §8.10 charges the whole command one flat
// penalty regardless of its channel count, so an uncapped list
// would turn that flat charge into unbounded per-command work.
// `/join`'s help text and `userclient.JoinAutojoinChannels`'s
// chunking both track this value.
const MaxJoinTargets = 10

// Join asks the server to add the issuing client to every channel
// in Channels, creating each one that does not yet exist. RFC 2812
// §3.2.1 lets one JOIN name several channels at once
// ("JOIN #a,#b,#c"); the dispatcher processes the whole list as a
// single command, so the flood-control penalty (RFC 1459 §8.10) is
// charged once no matter how many channels it names, up to
// [MaxJoinTargets]. Key carries one channel password and applies to
// every channel in the list; RFC 2812 supports a parallel
// per-channel key list, but nothing in modeloff needs more than one
// keyed channel per JOIN yet.
type Join struct {
	Channels []domain.ChannelName
	Key      string
}

// Part asks the server to remove the issuing client from the named
// channel. Reason is broadcast to remaining members.
type Part struct {
	Channel domain.ChannelName
	Reason  string
}

// PrivMsg sends a chat message to a channel or to another client
// (RFC 2812 §3.3.1). The same command shape covers both: `Target`
// names the form the client addressed it with, and the dispatcher
// resolves that into the conversation the message belongs to. A
// target naming nobody is refused with [domain.UnknownNickError].
type PrivMsg struct {
	Target MsgTarget
	Body   string
}

// Action sends a /me-style action message. It addresses its target
// exactly as [PrivMsg] does.
type Action struct {
	Target MsgTarget
	Body   string
}

// Topic sets the channel topic. Setting an empty body clears it.
type Topic struct {
	Channel domain.ChannelName
	Body    string
}

// Invite asks the server to add a model instance to the named
// channel. Both the user-client and a model (via the `invite` tool)
// issue it over this wire command; on a `+i` channel it is
// operator-gated, otherwise any member may invite.
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
// the named instance. `Channel` carries the window the command was
// issued in; the dispatcher stamps it onto the reply's
// [domain.Whois.Target] so the issuer renders the response in the
// window it asked from.
type Whois struct {
	Nick    domain.Nick
	Channel domain.ChannelName
}

// List asks the server to emit a stream of [domain.ListReply] events
// terminated by [domain.ListEnd], shaped after IRC's RPL_LIST and
// end-of-list (323) numerics.
type List struct{}

// AddModel creates a new model instance, persists it, registers a
// model-client subscription for it, and attaches it to the named
// channel. Operator-gated: the issuing client must carry
// [domain.ModeOperator].
type AddModel struct {
	Channel domain.ChannelName
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
// [domain.ModeOperator]. The killed client is seen to QUIT — the
// dispatcher broadcasts QUIT to peers with the conventional
// "Killed by <oper> (<reason>)" reason and reaps the subscription.
type Kill struct {
	Nick   domain.Nick
	Reason string
}

// Oper is RFC 2812 §3.1.4 self-elevation. The dispatcher delegates
// credential validation to a configurable authenticator on the
// session; on success the server issues the canonical MODE
// response (a [domain.UserModeChange]) to the requesting client. On
// failure it returns [domain.OperFailedError].
//
// The default authenticator rejects every caller — there is no
// client-side path to +o today. The local user (the user-client)
// gets +o via server-initiated bootstrap, not via this command;
// future credentialed model elevation slots in by swapping the
// authenticator.
type Oper struct {
	User     string
	Password string
}

// ChannelMode is RFC 2812 §3.2.3 channel-mode mutation. A single
// `MODE` carries a sequence of changes that the dispatcher
// applies atomically: the channel-op precondition runs once
// against the whole batch, and the shape of every change is
// validated up front so a malformed entry rejects the batch
// before any side effect runs. The first runtime failure
// (e.g. unknown nick on `+o`) stops the loop; already-applied
// changes remain, matching typical ircd behaviour.
type ChannelMode struct {
	Channel domain.ChannelName
	Changes []ChannelModeChange
}

// ChannelModeChange is one `+X` / `-X` mutation inside a
// [ChannelMode] batch. The dispatcher's validator enforces
// shape: member modes (`+o` / `+v`) require `Target`; parametric
// attribute modes (`+l` on add, `+k` on add) require `Param`;
// boolean attribute modes accept neither.
type ChannelModeChange struct {
	Flag   domain.Mode
	Add    bool
	Target domain.Nick
	Param  string
}

func (Join) isCommand()        {}
func (Part) isCommand()        {}
func (PrivMsg) isCommand()     {}
func (Action) isCommand()      {}
func (Topic) isCommand()       {}
func (Invite) isCommand()      {}
func (Kick) isCommand()        {}
func (Nick) isCommand()        {}
func (Whois) isCommand()       {}
func (List) isCommand()        {}
func (AddModel) isCommand()    {}
func (Quit) isCommand()        {}
func (Kill) isCommand()        {}
func (Oper) isCommand()        {}
func (ChannelMode) isCommand() {}

// Name returns the wire verb "JOIN".
func (Join) Name() string { return "JOIN" }

// Name returns the wire verb "PART".
func (Part) Name() string { return "PART" }

// Name returns the wire verb "PRIVMSG".
func (PrivMsg) Name() string { return "PRIVMSG" }

// Name returns the wire verb "ACTION".
func (Action) Name() string { return "ACTION" }

// Name returns the wire verb "TOPIC".
func (Topic) Name() string { return "TOPIC" }

// Name returns the wire verb "INVITE".
func (Invite) Name() string { return "INVITE" }

// Name returns the wire verb "KICK".
func (Kick) Name() string { return "KICK" }

// Name returns the wire verb "NICK".
func (Nick) Name() string { return "NICK" }

// Name returns the wire verb "WHOIS".
func (Whois) Name() string { return "WHOIS" }

// Name returns the wire verb "LIST".
func (List) Name() string { return "LIST" }

// Name returns the wire verb "ADDMODEL".
func (AddModel) Name() string { return "ADDMODEL" }

// Name returns the wire verb "QUIT".
func (Quit) Name() string { return "QUIT" }

// Name returns the wire verb "KILL".
func (Kill) Name() string { return "KILL" }

// Name returns the wire verb "OPER".
func (Oper) Name() string { return "OPER" }

// Name returns the wire verb "MODE".
func (ChannelMode) Name() string { return "MODE" }
