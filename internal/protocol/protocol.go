// Package protocol defines the wire contract between the modeloff
// session (the IRC-like server) and its clients.
//
// There are two client kinds — the chat-screen (one user-client per
// running TUI) and each model instance (one model-client per
// `*domain.Instance`) — and they speak the same protocol. The
// dispatcher does not know which kind it is talking to: capability
// parity is enforced at the type level.
//
// Two sum types form the contract:
//
//   - [Command] — anything a client sends to the server. The sum is
//     sealed via an unexported `isCommand` method on each member.
//
//   - [Event] — anything the server emits to a subscribed client.
//     Aliased to [domain.ProtocolEvent]; the sum is sealed via an
//     unexported method declared on each member in the `domain`
//     package.
//
// A [Client] handle carries identity and a small surface for sending
// commands and receiving events. Each client also carries a set of
// RFC 2812 §3.1.5 [UserMode] flags; capabilities such as `AddModel`
// and `Kill` are gated on the issuing client's modes, not on its
// kind. The user-client is granted [ModeOperator] at session
// bootstrap.
package protocol

import (
	"fmt"
	"strings"
	"time"

	"github.com/laney/modeloff/internal/domain"
)

// ClientID identifies a connected client. The user-client uses
// [UserClientID] as its sentinel; each model-client uses its
// instance's id.
type ClientID = domain.InstanceID

// UserClientID is the sentinel identifying the user-client. It is
// the empty [ClientID] — every model-client carries a non-empty
// instance id, so an empty id uniquely names the user.
const UserClientID ClientID = ""

// Response is the synchronous reply to a [Command].
type Response struct {
	// Events are confirmation events the dispatcher synthesised in
	// response to the command (e.g. [domain.ListReply] /
	// [domain.ListEnd] for [List], [domain.Whois] for [Whois]).
	Events []Event

	// Err is the typed command failure, or nil on success. The
	// dispatcher returns concrete `domain` error types
	// (e.g. [domain.UnknownNickError], [domain.NotOperatorError])
	// so callers can branch on them with `errors.As`.
	Err error
}

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

	// KindQuit indicates a user or model has quit the server.
	KindQuit MessageKind = "QUIT"

	// KindPoke is a periodic nudge sent to models to prompt
	// unsolicited conversation.
	KindPoke MessageKind = "POKE"
)

// IRCMessage is the structured representation of an event sent to a
// model. It mirrors IRC message structure: a sender, a kind, a
// target (channel or nick), and a body. InstanceID identifies the
// originating model instance and is used internally for
// self-message detection; it is omitted from JSON sent to models.
type IRCMessage struct {
	Kind       MessageKind       `json:"kind"`
	From       string            `json:"from"`
	InstanceID domain.InstanceID `json:"instance_id,omitzero"`
	Target     string            `json:"target"`
	Body       string            `json:"body,omitempty"`
	At         time.Time         `json:"at"`
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
	Kind  ReplyKind   `json:"type"`
	Body  string      `json:"body,omitempty"`
	Spans []ReplySpan `json:"spans,omitempty"`
}

// ReplyStyle describes formatting to apply to a span.
type ReplyStyle struct {
	Bold      bool   `json:"bold,omitempty"`
	Italic    bool   `json:"italic,omitempty"`
	Underline bool   `json:"underline,omitempty"`
	Reverse   bool   `json:"reverse,omitempty"`
	Strike    bool   `json:"strike,omitempty"`
	FG        *uint8 `json:"fg,omitempty"`
	BG        *uint8 `json:"bg,omitempty"`
}

// ReplySpan is a run of text with optional style.
type ReplySpan struct {
	Text  string      `json:"text"`
	Style *ReplyStyle `json:"style,omitempty"`
}

// ModelResponse is the typed response from a model after receiving
// events. The model must explicitly choose to reply or stay silent.
type ModelResponse struct {
	Kind     ResponseKind `json:"kind"`
	Messages []ReplyPart  `json:"messages,omitempty"`
	Reason   string       `json:"reason,omitempty"`
}

// FromChannelEvent converts a model-visible channel event into an
// IRC-style protocol message. Returns the message and true if the
// event type is supported, or a zero message and false otherwise.
func FromChannelEvent(evt domain.PersistableEvent) (IRCMessage, bool) {
	switch e := evt.(type) {
	case domain.Message:
		kind := KindPrivMsg
		if e.Action {
			kind = KindAction
		}

		return IRCMessage{
			Kind:       kind,
			From:       string(e.From),
			InstanceID: e.InstanceID,
			Target:     string(e.Target),
			Body:       e.Body,
			At:         e.At,
		}, true

	case domain.Join:
		return IRCMessage{
			Kind:       KindJoin,
			From:       string(e.Nick),
			InstanceID: e.InstanceID,
			Target:     string(e.Target),
			Body:       e.Message,
			At:         e.At,
		}, true

	case domain.Part:
		return IRCMessage{
			Kind:       KindPart,
			From:       string(e.Nick),
			InstanceID: e.InstanceID,
			Target:     string(e.Target),
			Body:       e.Message,
			At:         e.At,
		}, true

	case domain.Quit:
		// QUIT carries no target on the wire (RFC 2812 §3.1.7);
		// the dispatch context is already per-channel, so the
		// model knows which window the line belongs to without
		// a target field on the protocol message.
		return IRCMessage{
			Kind:       KindQuit,
			From:       string(e.Nick),
			InstanceID: e.InstanceID,
			Body:       e.Message,
			At:         e.At,
		}, true

	case domain.TopicChange:
		return IRCMessage{
			Kind:       KindTopic,
			From:       string(e.By),
			InstanceID: e.InstanceID,
			Target:     string(e.Target),
			Body:       e.Topic,
			At:         e.At,
		}, true

	case domain.NickChange:
		return IRCMessage{
			Kind:       KindNick,
			From:       string(e.OldNick),
			InstanceID: e.InstanceID,
			Target:     string(e.NewNick),
			At:         e.At,
		}, true

	case domain.ModelInvited:
		return IRCMessage{
			Kind:       KindInvite,
			From:       string(e.Nick),
			InstanceID: e.InstanceID,
			Target:     string(e.Target),
			At:         e.At,
		}, true

	case domain.ModelKicked:
		return IRCMessage{
			Kind:       KindKick,
			From:       string(e.Nick),
			InstanceID: e.InstanceID,
			Target:     string(e.Target),
			At:         e.At,
		}, true

	default:
		return IRCMessage{}, false
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

// ValidateReplyPart reports whether a reply part is structurally valid.
func ValidateReplyPart(part ReplyPart) error {
	hasBody := strings.TrimSpace(part.Body) != ""
	hasSpans := len(part.Spans) > 0

	if hasBody == hasSpans {
		return fmt.Errorf("reply part must contain exactly one of body or spans")
	}

	if hasBody {
		if strings.Contains(part.Body, "\n") {
			return fmt.Errorf("reply body must not contain newlines")
		}

		return nil
	}

	for index, span := range part.Spans {
		if span.Text == "" {
			return fmt.Errorf("span %d is empty", index)
		}
		if strings.Contains(span.Text, "\n") {
			return fmt.Errorf("span %d contains a newline", index)
		}
		if span.Style == nil {
			continue
		}
		if err := validateReplyStyle(*span.Style); err != nil {
			return fmt.Errorf("span %d: %w", index, err)
		}
	}

	return nil
}

func validateReplyStyle(style ReplyStyle) error {
	if style.FG != nil && *style.FG > 15 {
		return fmt.Errorf("foreground colour %d is out of range", *style.FG)
	}
	if style.BG != nil && *style.BG > 15 {
		return fmt.Errorf("background colour %d is out of range", *style.BG)
	}

	return nil
}
