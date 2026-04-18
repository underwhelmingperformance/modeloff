package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// ChannelEvent is a historical record of something that happened in a
// channel. These are persisted in the event log and displayed in the
// scrollback. They carry no state-change semantics — live domain
// events (JoinEvent, PartEvent, etc.) are the signals that trigger
// UI updates.
//
// Every stored-event type that carries a `Nick` field (e.g.
// `ChannelJoin.Nick`, `ChannelPart.Nick`, `ChannelQuit.Nick`,
// `ChannelTopicChange.By`, `ChannelModeChange.Nick` and `.By`,
// `ChannelModelInvited.Nick`, `ChannelModelKicked.Nick`,
// `ChannelNickChange.OldNick`/`.NewNick`) holds a snapshot of the
// nick at event time. These values are point-in-time records and
// may differ from the instance handle's current nick after a later
// rename; renderers that want the live nick should resolve via
// `InstanceID` where present.
type ChannelEvent interface {
	channelEvent()
	channelEventTime() time.Time
	// ModelVisible reports whether this event should be included in
	// the context sent to model instances. Conversation events
	// (messages, joins, parts) return true; client-local events
	// (command output, errors) return false.
	ModelVisible() bool
}

// StoredEvent pairs a channel event with its persistent row ID.
type StoredEvent struct {
	ID    int64
	Event ChannelEvent
}

// Static interface compliance.
var (
	_ ChannelEvent = ChannelMessage{}
	_ ChannelEvent = ChannelJoin{}
	_ ChannelEvent = ChannelPart{}
	_ ChannelEvent = ChannelQuit{}
	_ ChannelEvent = ChannelTopicChange{}
	_ ChannelEvent = ChannelModeChange{}
	_ ChannelEvent = ChannelModelInvited{}
	_ ChannelEvent = ChannelModelKicked{}
	_ ChannelEvent = ChannelNickChange{}
	_ ChannelEvent = ChannelTopicInfo{}
	_ ChannelEvent = ChannelHelp{}
	_ ChannelEvent = ChannelWhois{}
	_ ChannelEvent = ChannelListOutput{}
	_ ChannelEvent = ChannelCommandError{}
	_ ChannelEvent = ChannelUsageHint{}
	_ ChannelEvent = ChannelSystemNotice{}
	_ ChannelEvent = ChannelPersonasList{}
)

// ChannelMessage records a message sent in a channel.
type ChannelMessage struct {
	Channel    ChannelName `json:"channel"`
	From       Nick        `json:"from"`
	InstanceID InstanceID  `json:"instance_id,omitzero"`
	Body       string      `json:"body"`
	Action     bool        `json:"action,omitempty"`
	At         time.Time   `json:"at"`
}

func (ChannelMessage) channelEvent()                 {}
func (e ChannelMessage) channelEventTime() time.Time { return e.At }

// ModelVisible implements ChannelEvent.
func (ChannelMessage) ModelVisible() bool { return true }

// ChannelJoin records a user or model joining a channel.
type ChannelJoin struct {
	Channel ChannelName `json:"channel"`
	Nick    Nick        `json:"nick"`
	Created bool        `json:"created,omitempty"`
	Message string      `json:"message,omitempty"`
	At      time.Time   `json:"at"`
}

func (ChannelJoin) channelEvent()                 {}
func (e ChannelJoin) channelEventTime() time.Time { return e.At }

// ModelVisible implements ChannelEvent.
func (ChannelJoin) ModelVisible() bool { return true }

// ChannelPart records a user or model leaving a channel.
type ChannelPart struct {
	Channel ChannelName `json:"channel"`
	Nick    Nick        `json:"nick"`
	Message string      `json:"message,omitempty"`
	At      time.Time   `json:"at"`
}

func (ChannelPart) channelEvent()                 {}
func (e ChannelPart) channelEventTime() time.Time { return e.At }

// ModelVisible implements ChannelEvent.
func (ChannelPart) ModelVisible() bool { return true }

// ChannelQuit records a user or model quitting the server. One event
// is recorded per channel the actor was in.
type ChannelQuit struct {
	Channel ChannelName `json:"channel"`
	Nick    Nick        `json:"nick"`
	Message string      `json:"message,omitempty"`
	At      time.Time   `json:"at"`
}

func (ChannelQuit) channelEvent()                 {}
func (e ChannelQuit) channelEventTime() time.Time { return e.At }

// ModelVisible implements ChannelEvent.
func (ChannelQuit) ModelVisible() bool { return true }

// ChannelTopicChange records a topic change.
type ChannelTopicChange struct {
	Channel ChannelName `json:"channel"`
	Topic   string      `json:"topic"`
	By      Nick        `json:"by"`
	At      time.Time   `json:"at"`
}

func (ChannelTopicChange) channelEvent()                 {}
func (e ChannelTopicChange) channelEventTime() time.Time { return e.At }

// ModelVisible implements ChannelEvent.
func (ChannelTopicChange) ModelVisible() bool { return true }

// ChannelModeChange records a privilege change for a member.
type ChannelModeChange struct {
	Channel ChannelName `json:"channel"`
	Nick    Nick        `json:"nick"`
	Mode    NickMode    `json:"mode"`
	By      Nick        `json:"by"`
	At      time.Time   `json:"at"`
}

func (ChannelModeChange) channelEvent()                 {}
func (e ChannelModeChange) channelEventTime() time.Time { return e.At }

// ModelVisible implements ChannelEvent.
func (ChannelModeChange) ModelVisible() bool { return true }

// ChannelModelInvited records a model instance being added to a
// channel.
type ChannelModelInvited struct {
	Channel ChannelName `json:"channel"`
	Nick    Nick        `json:"nick"`
	By      Nick        `json:"by"`
	At      time.Time   `json:"at"`
}

func (ChannelModelInvited) channelEvent()                 {}
func (e ChannelModelInvited) channelEventTime() time.Time { return e.At }

// ModelVisible implements ChannelEvent.
func (ChannelModelInvited) ModelVisible() bool { return true }

// ChannelModelKicked records a model instance being removed from a
// channel.
type ChannelModelKicked struct {
	Channel ChannelName `json:"channel"`
	Nick    Nick        `json:"nick"`
	By      Nick        `json:"by"`
	At      time.Time   `json:"at"`
}

func (ChannelModelKicked) channelEvent()                 {}
func (e ChannelModelKicked) channelEventTime() time.Time { return e.At }

// ModelVisible implements ChannelEvent.
func (ChannelModelKicked) ModelVisible() bool { return true }

// ChannelNickChange records a nick change visible in a channel.
type ChannelNickChange struct {
	Channel ChannelName `json:"channel"`
	OldNick Nick        `json:"old_nick"`
	NewNick Nick        `json:"new_nick"`
	At      time.Time   `json:"at"`
}

func (ChannelNickChange) channelEvent()                 {}
func (e ChannelNickChange) channelEventTime() time.Time { return e.At }

// ModelVisible implements ChannelEvent.
func (ChannelNickChange) ModelVisible() bool { return true }

// ChannelTopicInfo records the current topic state when queried
// (e.g. via /topic with no arguments).
type ChannelTopicInfo struct {
	Channel    ChannelName `json:"channel"`
	Topic      string      `json:"topic"`
	TopicSetBy Nick        `json:"topic_set_by,omitempty"`
	TopicSetAt time.Time   `json:"topic_set_at,omitzero"`
	At         time.Time   `json:"at"`
}

func (ChannelTopicInfo) channelEvent()                 {}
func (e ChannelTopicInfo) channelEventTime() time.Time { return e.At }

// ModelVisible implements ChannelEvent.
func (ChannelTopicInfo) ModelVisible() bool { return false }

// ChannelHelp records /help output.
type ChannelHelp struct {
	Channel ChannelName `json:"channel"`
	At      time.Time   `json:"at"`
}

func (ChannelHelp) channelEvent()                 {}
func (e ChannelHelp) channelEventTime() time.Time { return e.At }

// ModelVisible implements ChannelEvent.
func (ChannelHelp) ModelVisible() bool { return false }

// ChannelWhois records /whois output.
type ChannelWhois struct {
	Channel  ChannelName `json:"channel"`
	Instance *Instance   `json:"instance"`
	At       time.Time   `json:"at"`
}

func (ChannelWhois) channelEvent()                 {}
func (e ChannelWhois) channelEventTime() time.Time { return e.At }

// ModelVisible implements ChannelEvent.
func (ChannelWhois) ModelVisible() bool { return false }

// ChannelListOutput records /list output.
type ChannelListOutput struct {
	Channels []Channel `json:"channels"`
	At       time.Time `json:"at"`
}

func (ChannelListOutput) channelEvent()                 {}
func (e ChannelListOutput) channelEventTime() time.Time { return e.At }

// ModelVisible implements ChannelEvent.
func (ChannelListOutput) ModelVisible() bool { return false }

// ChannelCommandError records a command error.
type ChannelCommandError struct {
	Channel ChannelName `json:"channel"`
	Err     string      `json:"error"`
	At      time.Time   `json:"at"`
}

func (ChannelCommandError) channelEvent()                 {}
func (e ChannelCommandError) channelEventTime() time.Time { return e.At }

// ModelVisible implements ChannelEvent.
func (ChannelCommandError) ModelVisible() bool { return false }

// ChannelUsageHint records a command usage hint.
type ChannelUsageHint struct {
	Channel ChannelName `json:"channel"`
	Command string      `json:"command"`
	Usage   string      `json:"usage"`
	At      time.Time   `json:"at"`
}

func (ChannelUsageHint) channelEvent()                 {}
func (e ChannelUsageHint) channelEventTime() time.Time { return e.At }

// ModelVisible implements ChannelEvent.
func (ChannelUsageHint) ModelVisible() bool { return false }

// ChannelSystemNotice records a system notification (API key saved,
// poke interval changed, etc.).
type ChannelSystemNotice struct {
	Channel ChannelName `json:"channel"`
	Text    string      `json:"text"`
	At      time.Time   `json:"at"`
}

func (ChannelSystemNotice) channelEvent()                 {}
func (e ChannelSystemNotice) channelEventTime() time.Time { return e.At }

// ModelVisible implements ChannelEvent.
func (ChannelSystemNotice) ModelVisible() bool { return false }

// ChannelPersonasList records /personas output.
type ChannelPersonasList struct {
	Personas []Persona `json:"personas"`
	At       time.Time `json:"at"`
}

func (ChannelPersonasList) channelEvent()                 {}
func (e ChannelPersonasList) channelEventTime() time.Time { return e.At }

// ModelVisible implements ChannelEvent.
func (ChannelPersonasList) ModelVisible() bool { return false }

// ChannelEventTime returns the timestamp of a channel event.
func ChannelEventTime(e ChannelEvent) time.Time {
	return e.channelEventTime()
}

// ChannelEventType returns the discriminator string for a channel
// event.
func ChannelEventType(e ChannelEvent) string {
	switch e.(type) {
	case ChannelMessage:
		return "message"
	case ChannelJoin:
		return "join"
	case ChannelPart:
		return "part"
	case ChannelQuit:
		return "quit"
	case ChannelTopicChange:
		return "topic_change"
	case ChannelModeChange:
		return "mode_change"
	case ChannelModelInvited:
		return "model_invited"
	case ChannelModelKicked:
		return "model_kicked"
	case ChannelNickChange:
		return "nick_change"
	case ChannelTopicInfo:
		return "topic_info"
	case ChannelHelp:
		return "help"
	case ChannelWhois:
		return "whois"
	case ChannelListOutput:
		return "list"
	case ChannelCommandError:
		return "command_error"
	case ChannelUsageHint:
		return "usage_hint"
	case ChannelSystemNotice:
		return "system_notice"
	case ChannelPersonasList:
		return "personas_list"
	default:
		return ""
	}
}

// channelEventEnvelope is the JSON wire format for a channel event,
// carrying a type discriminator alongside the event data.
type channelEventEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// MarshalChannelEvent encodes a channel event as JSON with a type
// discriminator.
func MarshalChannelEvent(e ChannelEvent) ([]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("marshal channel event data: %w", err)
	}

	return json.Marshal(channelEventEnvelope{
		Type: ChannelEventType(e),
		Data: data,
	})
}

// UnmarshalChannelEvent decodes a channel event from JSON, using the
// type discriminator to select the concrete type.
func UnmarshalChannelEvent(b []byte) (ChannelEvent, error) {
	var env channelEventEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("unmarshal channel event envelope: %w", err)
	}

	unmarshal := func(target any) error {
		return json.Unmarshal(env.Data, target)
	}

	switch env.Type {
	case "message":
		var e ChannelMessage
		return e, unmarshal(&e)
	case "join":
		var e ChannelJoin
		return e, unmarshal(&e)
	case "part":
		var e ChannelPart
		return e, unmarshal(&e)
	case "quit":
		var e ChannelQuit
		return e, unmarshal(&e)
	case "topic_change":
		var e ChannelTopicChange
		return e, unmarshal(&e)
	case "mode_change":
		var e ChannelModeChange
		return e, unmarshal(&e)
	case "model_invited":
		var e ChannelModelInvited
		return e, unmarshal(&e)
	case "model_kicked":
		var e ChannelModelKicked
		return e, unmarshal(&e)
	case "nick_change":
		var e ChannelNickChange
		return e, unmarshal(&e)
	case "topic_info":
		var e ChannelTopicInfo
		return e, unmarshal(&e)
	case "help":
		var e ChannelHelp
		return e, unmarshal(&e)
	case "whois":
		var e ChannelWhois
		return e, unmarshal(&e)
	case "list":
		var e ChannelListOutput
		return e, unmarshal(&e)
	case "command_error":
		var e ChannelCommandError
		return e, unmarshal(&e)
	case "usage_hint":
		var e ChannelUsageHint
		return e, unmarshal(&e)
	case "system_notice":
		var e ChannelSystemNotice
		return e, unmarshal(&e)
	case "personas_list":
		var e ChannelPersonasList
		return e, unmarshal(&e)
	default:
		return nil, fmt.Errorf("unknown channel event type: %q", env.Type)
	}
}
