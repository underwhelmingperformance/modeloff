// Package protocol defines the typed IRC-like protocol used to
// communicate with model instances. Models receive events as
// structured messages and respond with a typed response that can
// include a reply or an explicit "no reply" signal.
package protocol

import "time"

// MessageKind identifies the type of IRC-like message sent to or
// received from a model.
type MessageKind string

// MessageKind values mirror the IRC command set, mapping each user or
// system action to a named event that models can interpret.
const (
	// KindPrivMsg is a regular chat message sent to a channel or nick.
	KindPrivMsg MessageKind = "PRIVMSG"

	// KindJoin indicates a user or model has entered a channel.
	KindJoin MessageKind = "JOIN"

	// KindPart indicates a user or model has left a channel.
	KindPart MessageKind = "PART"

	// KindNick indicates a nickname change.
	KindNick MessageKind = "NICK"

	// KindTopic indicates a channel's title has been changed.
	KindTopic MessageKind = "TOPIC"

	// KindInvite indicates a model has been invited to a channel.
	KindInvite MessageKind = "INVITE"

	// KindKick indicates a model has been removed from a channel.
	KindKick MessageKind = "KICK"

	// KindPoke is a periodic nudge sent to models to prompt
	// unsolicited conversation.
	KindPoke MessageKind = "POKE"
)

// IRCMessage is the structured representation of an event sent to a
// model. It mirrors IRC message structure: a sender, a kind, a
// target (channel or nick), and a body.
type IRCMessage struct {
	Kind   MessageKind `json:"kind"`
	From   string      `json:"from"`
	Target string      `json:"target"`
	Body   string      `json:"body,omitempty"`
	At     time.Time   `json:"at"`
}

// ResponseKind indicates whether the model chose to reply.
type ResponseKind string

// ResponseKind values represent the two possible outcomes when a model
// processes events: it either replies with content or explicitly
// chooses to remain silent.
const (
	// ResponseReply means the model has produced a message to send.
	ResponseReply ResponseKind = "reply"

	// ResponseSilence means the model chose not to respond. The
	// Reason field on ModelResponse may explain why.
	ResponseSilence ResponseKind = "silence"
)

// ModelResponse is the typed response from a model after receiving
// events. The model must explicitly choose to reply or stay silent.
type ModelResponse struct {
	Kind   ResponseKind `json:"kind"`
	Body   string       `json:"body,omitempty"`
	Reason string       `json:"reason,omitempty"`
}
