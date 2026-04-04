// Package protocol defines the typed IRC-like protocol used to
// communicate with model instances. Models receive events as
// structured messages and respond with a typed response that can
// include a reply or an explicit "no reply" signal.
package protocol

import (
	"time"

	"github.com/laney/modeloff/internal/domain"
)

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

	// KindTopic indicates a channel's topic has been changed.
	KindTopic MessageKind = "TOPIC"

	// KindInvite indicates a model has been invited to a channel.
	KindInvite MessageKind = "INVITE"

	// KindKick indicates a model has been removed from a channel.
	KindKick MessageKind = "KICK"

	// KindAction is a /me action message (e.g. "* nick does something").
	KindAction MessageKind = "ACTION"

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

// ReplyKind distinguishes regular messages from actions in a model
// reply.
type ReplyKind string

const (
	// ReplyMessage is a regular chat message.
	ReplyMessage ReplyKind = "message"

	// ReplyAction is a /me action (e.g. "* nick waves").
	ReplyAction ReplyKind = "action"
)

// ReplyPart is a single typed message within a model's reply.
type ReplyPart struct {
	Kind ReplyKind `json:"type"`
	Body string    `json:"body"`
}

// ModelResponse is the typed response from a model after receiving
// events. The model must explicitly choose to reply or stay silent.
type ModelResponse struct {
	Kind     ResponseKind `json:"kind"`
	Messages []ReplyPart  `json:"messages,omitempty"`
	Reason   string       `json:"reason,omitempty"`
}

// FromMessage converts a stored domain message into an IRC-style
// protocol message for model consumption.
func FromMessage(msg domain.Message) IRCMessage {
	kind := KindPrivMsg
	if msg.Action {
		kind = KindAction
	}

	return IRCMessage{
		Kind:   kind,
		From:   string(msg.From),
		Target: string(msg.Channel),
		Body:   msg.Body,
		At:     msg.SentAt,
	}
}

// FromJoinEvent converts a join event into an IRC-style protocol
// message.
func FromJoinEvent(evt domain.JoinEvent) IRCMessage {
	return IRCMessage{
		Kind:   KindJoin,
		From:   string(evt.Nick),
		Target: string(evt.Channel),
		At:     evt.At,
	}
}

// FromPartEvent converts a part event into an IRC-style protocol
// message.
func FromPartEvent(evt domain.PartEvent) IRCMessage {
	return IRCMessage{
		Kind:   KindPart,
		From:   string(evt.Nick),
		Target: string(evt.Channel),
		At:     evt.At,
	}
}

// FromTopicChangeEvent converts a topic change event into an IRC-style
// protocol message.
func FromTopicChangeEvent(evt domain.TopicChangeEvent) IRCMessage {
	return IRCMessage{
		Kind:   KindTopic,
		From:   string(evt.By),
		Target: string(evt.Channel),
		Body:   evt.Topic,
		At:     evt.At,
	}
}

// FromNickChangeEvent converts a nick change event into an IRC-style
// protocol message.
func FromNickChangeEvent(evt domain.NickChangeEvent) IRCMessage {
	return IRCMessage{
		Kind:   KindNick,
		From:   string(evt.OldNick),
		Target: string(evt.NewNick),
		At:     evt.At,
	}
}

// Reply creates a ModelResponse containing a single regular message.
func Reply(body string) ModelResponse {
	return ModelResponse{
		Kind:     ResponseReply,
		Messages: []ReplyPart{{Kind: ReplyMessage, Body: body}},
	}
}

// ActionReply creates a ModelResponse containing a single action
// message.
func ActionReply(body string) ModelResponse {
	return ModelResponse{
		Kind:     ResponseReply,
		Messages: []ReplyPart{{Kind: ReplyAction, Body: body}},
	}
}
