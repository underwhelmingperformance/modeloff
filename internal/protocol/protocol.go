// Package protocol defines the typed IRC-like protocol used to
// communicate with model instances. Models receive events as
// structured messages and respond with a typed response that can
// include a reply or an explicit "no reply" signal.
package protocol

import "time"

// MessageKind identifies the type of IRC-like message sent to or
// received from a model.
type MessageKind string

const (
	KindPrivMsg MessageKind = "PRIVMSG"
	KindJoin    MessageKind = "JOIN"
	KindPart    MessageKind = "PART"
	KindNick    MessageKind = "NICK"
	KindTopic   MessageKind = "TOPIC"
	KindInvite  MessageKind = "INVITE"
	KindKick    MessageKind = "KICK"
	KindPoke    MessageKind = "POKE"
)

// IRCMessage is the structured representation of an event sent to a
// model. It mirrors IRC message structure: a sender, a kind, a
// target (room or nick), and a body.
type IRCMessage struct {
	Kind   MessageKind `json:"kind"`
	From   string      `json:"from"`
	Target string      `json:"target"`
	Body   string      `json:"body,omitempty"`
	At     time.Time   `json:"at"`
}

// ResponseKind indicates whether the model chose to reply.
type ResponseKind string

const (
	ResponseReply   ResponseKind = "reply"
	ResponseSilence ResponseKind = "silence"
)

// ModelResponse is the typed response from a model after receiving
// events. The model must explicitly choose to reply or stay silent.
type ModelResponse struct {
	Kind   ResponseKind `json:"kind"`
	Body   string       `json:"body,omitempty"`
	Reason string       `json:"reason,omitempty"`
}
