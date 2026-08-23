package protocol

import (
	"github.com/laney/modeloff/internal/domain"
)

// MsgTarget is the closed sum of what a [PrivMsg] or an [Action] may
// address, which is RFC 2812 §3.3.1's `<msgtarget>`. The sum is
// sealed by the unexported `isMsgTarget` method. The protocol tests
// derive its members from those methods and compare them with each
// type switch that interprets a target.
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

// WindowTarget is the closed sum of conversations for which an
// attached client may request current context. A channel is addressed
// by its IRC name. A direct conversation is addressed by the peer's
// stable identity.
type WindowTarget interface {
	isWindowTarget()
	windowKey() domain.ChannelName
	windowKind() domain.ChannelKind
}

type channelWindowTarget domain.ChannelName
type directWindowTarget domain.InstanceID

func (t channelWindowTarget) isWindowTarget() {}
func (t channelWindowTarget) windowKey() domain.ChannelName {
	return domain.ChannelName(t)
}
func (channelWindowTarget) windowKind() domain.ChannelKind {
	return domain.KindChannel
}

func (t directWindowTarget) isWindowTarget() {}
func (t directWindowTarget) windowKey() domain.ChannelName {
	return domain.ChannelName(t)
}
func (directWindowTarget) windowKind() domain.ChannelKind {
	return domain.KindDM
}

// ChannelWindowTarget identifies a channel conversation.
func ChannelWindowTarget(name domain.ChannelName) WindowTarget {
	return channelWindowTarget(name)
}

// DirectWindowTarget identifies a direct conversation by peer ID.
func DirectWindowTarget(peer domain.InstanceID) WindowTarget {
	return directWindowTarget(peer)
}

// WindowTargetForKey converts an IRC window key into its typed
// actor-bound target. The status window has no server-side
// conversation and returns nil.
func WindowTargetForKey(key domain.ChannelName) WindowTarget {
	switch domain.InferChannelKind(key) {
	case domain.KindChannel:
		return ChannelWindowTarget(key)
	case domain.KindDM:
		return DirectWindowTarget(domain.InstanceID(key))
	}

	return nil
}

// WindowKey returns the internal history key for a typed target. No
// target is the status window, which has no server-side conversation
// and no key.
func WindowKey(target WindowTarget) domain.ChannelName {
	if target == nil {
		return ""
	}

	return target.windowKey()
}

// WindowTargetKind reports whether a target is a channel or direct
// conversation. No target is the status window, which is what
// [WindowTargetForKey] answers with for a key naming neither.
func WindowTargetKind(target WindowTarget) domain.ChannelKind {
	if target == nil {
		return domain.KindStatus
	}

	return target.windowKind()
}

// ChannelWindowName returns the channel named by target. The second
// return is false for a direct conversation or no target.
func ChannelWindowName(target WindowTarget) (domain.ChannelName, bool) {
	if target == nil || target.windowKind() != domain.KindChannel {
		return "", false
	}

	return target.windowKey(), true
}

// DirectWindowPeer returns the peer identified by target. The second
// return is false for a channel conversation or no target.
func DirectWindowPeer(target WindowTarget) (domain.InstanceID, bool) {
	if target == nil || target.windowKind() != domain.KindDM {
		return "", false
	}

	return domain.InstanceID(target.windowKey()), true
}

// EqualWindowTarget reports whether two targets identify the same
// conversation.
func EqualWindowTarget(a, b WindowTarget) bool {
	if a == nil {
		return b == nil
	}
	if b == nil || a.windowKind() != b.windowKind() {
		return false
	}
	if a.windowKind() == domain.KindChannel {
		return domain.KeyForChannel(a.windowKey()) == domain.KeyForChannel(b.windowKey())
	}

	return a.windowKey() == b.windowKey()
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

// TargetForWindow addresses the conversation represented by a typed
// window. A channel window addresses its channel. A DM window
// addresses its counterpart by immutable identity, including the
// user whose identity is the empty [domain.InstanceID].
//
// The second return is false for a status window, which has no
// server-side message target.
func TargetForWindow(window domain.Window) (MsgTarget, bool) {
	switch window.Kind() {
	case domain.KindChannel:
		return ChannelTarget(window.Name()), true
	case domain.KindDM:
		return ClientTarget(window.Name()), true
	}

	return nil, false
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
	case NickTarget:
		return "", false
	}

	return "", false
}
