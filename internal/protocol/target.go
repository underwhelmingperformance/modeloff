package protocol

import "github.com/laney/modeloff/internal/domain"

// MsgTarget is the closed sum of what a [PrivMsg] or an [Action] may
// address, which is RFC 2812 §3.3.1's `<msgtarget>`. The sum is
// sealed by the unexported `isMsgTarget` method, so a new addressing
// form makes every switch over it fail to build until it is handled.
//
// The server resolves the target: the dispatcher turns whichever form
// the client used into the conversation key the message is logged and
// routed under, and answers a target that names nobody with
// [domain.UnknownNickError] (numeric 401 ERR_NOSUCHNICK). A client
// therefore says what it is addressing and never has to work out
// which conversation the server keeps it under.
type MsgTarget interface {
	isMsgTarget()

	// String returns the target as the client named it, for error
	// text and tool-result summaries.
	String() string
}

// ChannelTarget names a channel by name. The channel's own spelling
// wins over the one used here: two names that fold together under the
// server's casemapping are one channel (see [domain.KeyForChannel]).
type ChannelTarget domain.ChannelName

// NickTarget names another client by its current nick. This is the
// form a person types and the form a model reads off the chat traffic
// it is shown, and it is matched under the server's casemapping.
type NickTarget domain.Nick

// ClientTarget names another client by its immutable
// [domain.InstanceID]. A nick is display state its holder may change
// at will, so a client that already holds a conversation open with
// somebody addresses them by identity: the id it was addressed under
// still names the same client after a rename. This is the same choice
// [domain.Invitations] makes, and for the same reason.
//
// The user-client's own id is the empty [domain.InstanceID], so
// `ClientTarget("")` addresses the user.
type ClientTarget domain.InstanceID

func (ChannelTarget) isMsgTarget() {}
func (NickTarget) isMsgTarget()    {}
func (ClientTarget) isMsgTarget()  {}

// String returns the channel name.
func (t ChannelTarget) String() string { return string(t) }

// String returns the nick.
func (t NickTarget) String() string { return string(t) }

// String returns the instance id.
func (t ClientTarget) String() string { return string(t) }

// ParseMsgTarget reads a target the way a server reads the
// `<msgtarget>` parameter: a name carrying one of
// [domain.ChannelPrefixes] is a channel, and anything else is a nick.
// This is the form a person types after `/msg` and the form a model
// passes to the `msg` tool.
//
// A name that matches no channel and no nick is not quietly taken for
// something else. The server answers it with 401, which is how the
// client learns it addressed nobody.
func ParseMsgTarget(raw string) MsgTarget {
	if domain.HasChannelPrefix(domain.ChannelName(raw)) {
		return ChannelTarget(raw)
	}

	return NickTarget(raw)
}

// TargetForWindow addresses the conversation a client already has
// open, given the name it keeps that window under. A channel window
// addresses its channel; a DM window is keyed by its counterpart's
// [domain.InstanceID] (see [domain.DMWindow]), so it addresses that
// client by identity.
func TargetForWindow(name domain.ChannelName) MsgTarget {
	if domain.InferChannelKind(name) == domain.KindDM {
		return ClientTarget(name)
	}

	return ChannelTarget(name)
}

// WindowName is [TargetForWindow]'s inverse: the name of the window a
// target addresses, for a caller that needs the key rather than the
// address. A `ChannelTarget` names its channel and a `ClientTarget`
// names the DM window its counterpart's id keys.
//
// The second return is false for a `NickTarget`, which names a client
// without saying which of the caller's windows that conversation is,
// and for a nil target, which names nothing.
func WindowName(t MsgTarget) (domain.ChannelName, bool) {
	switch t := t.(type) {
	case ChannelTarget:
		return domain.ChannelName(t), true
	case ClientTarget:
		return domain.ChannelName(t), true
	}

	return "", false
}
