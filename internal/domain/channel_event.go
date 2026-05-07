package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// PersistableEvent is the persistable subset of `Event`: any event that
// can be written to a channel's event log and replayed from the
// store satisfies this interface. The umbrella `Event` interface
// covers both persistable types and pure-live types (dispatch
// lifecycle, focus changes, model replies); the store accepts only
// `PersistableEvent`.
//
// Every persistable type that carries a `Nick` field
// (`Join.Nick`, `Part.Nick`, `Quit.Nick`,
// `TopicChange.By`, `ModeChange.Nick` and `.By`,
// `ModelInvited.Nick`, `ModelKicked.Nick`,
// `NickChange.OldNick`/`.NewNick`) holds a snapshot of the
// nick at event time. These values are point-in-time records and
// may differ from the instance handle's current nick after a later
// rename; renderers that want the live nick should resolve via
// `InstanceID` where present.
//
// The same struct flows through both persistence and live emission:
// a `Join` is appended to the channel event log AND emitted
// on the session's event channel. Live consumers populate the
// `Instance *Instance` field (excluded from JSON via `json:"-"`) so
// they can mutate state by pointer identity; replay paths leave
// `Instance` nil and rely on the snapshot fields plus a registry
// lookup if a live handle is later needed.
type PersistableEvent interface {
	Event
	persistableEvent()
	persistableEventTime() time.Time
	// ModelVisible reports whether this event should be included in
	// the context sent to model instances. Conversation events
	// (messages, joins, parts) return true; client-local events
	// (command output, errors) return false.
	ModelVisible() bool
}

// StoredEvent pairs a channel event with its persistent row ID.
type StoredEvent struct {
	ID    int64
	Event PersistableEvent
}

// Static interface compliance.
var (
	_ PersistableEvent = Message{}
	_ PersistableEvent = Join{}
	_ PersistableEvent = Part{}
	_ PersistableEvent = Quit{}
	_ PersistableEvent = TopicChange{}
	_ PersistableEvent = ModeChange{}
	_ PersistableEvent = ModelInvited{}
	_ PersistableEvent = ModelKicked{}
	_ PersistableEvent = NickChange{}
	_ PersistableEvent = TopicInfo{}
	_ PersistableEvent = Help{}
	_ PersistableEvent = Whois{}
	_ PersistableEvent = ListReply{}
	_ PersistableEvent = ListEnd{}
	_ PersistableEvent = CommandError{}
	_ PersistableEvent = UsageHint{}
	_ PersistableEvent = SystemNotice{}
	_ PersistableEvent = PersonasList{}
)

// Message records a message sent in a channel.
type Message struct {
	Target     ChannelName `json:"channel"`
	From       Nick        `json:"from"`
	InstanceID InstanceID  `json:"instance_id,omitzero"`
	Body       string      `json:"body"`
	Action     bool        `json:"action,omitempty"`
	At         time.Time   `json:"at"`
}

func (Message) persistableEvent()                 {}
func (e Message) persistableEventTime() time.Time { return e.At }

// ModelVisible implements PersistableEvent.
func (Message) ModelVisible() bool { return true }

// RoutingKey returns the conversation key this message belongs
// to from `self`'s point of view. For channel- and status-shaped
// targets it is the target itself. For DMs it is the *peer* —
// the non-self party — derived from `Target` and `InstanceID`:
//
//   - if `self` is the sender (`e.InstanceID == self`), the
//     peer is the recipient (`e.Target`);
//   - if `self` is the recipient (`ChannelName(self) ==
//     e.Target`), the peer is the sender (`e.InstanceID`);
//   - otherwise the message belongs to a foreign DM that does
//     not involve `self`, and the second return is false.
//
// `self` is the empty `InstanceID` for the human user, the
// model's id for a model. The returned key is what the chat
// screen and the model dispatch context-builder use to decide
// which window/thread the event lands in.
func (e Message) RoutingKey(self InstanceID) (ChannelName, bool) {
	switch InferChannelKind(e.Target) {
	case KindChannel, KindStatus:
		return e.Target, true
	case KindDM:
		if e.InstanceID == self {
			return e.Target, true
		}

		if ChannelName(self) == e.Target {
			return ChannelName(e.InstanceID), true
		}

		return "", false
	}

	return "", false
}

// Join records a user or model joining a channel.
//
// `Instance` is the live actor handle, populated when the session
// emits the event so live consumers can mutate state by pointer
// identity (member-list ops, "is this me?" checks). It is excluded
// from JSON; replay from store leaves it nil. Renderers consult the
// snapshot fields (`Nick`) which are persistent.
type Join struct {
	Target     ChannelName `json:"channel"`
	Nick       Nick        `json:"nick"`
	InstanceID InstanceID  `json:"instance_id,omitzero"`
	Created    bool        `json:"created,omitempty"`
	Message    string      `json:"message,omitempty"`
	At         time.Time   `json:"at"`

	Instance *Instance `json:"-"`
}

func (Join) persistableEvent()                 {}
func (e Join) persistableEventTime() time.Time { return e.At }

// ModelVisible implements PersistableEvent.
func (Join) ModelVisible() bool { return true }

// Part records a user or model leaving a channel. See
// `Join` for the `Instance` / `InstanceID` contract.
type Part struct {
	Target     ChannelName `json:"channel"`
	Nick       Nick        `json:"nick"`
	InstanceID InstanceID  `json:"instance_id,omitzero"`
	Message    string      `json:"message,omitempty"`
	At         time.Time   `json:"at"`

	Instance *Instance `json:"-"`
}

func (Part) persistableEvent()                 {}
func (e Part) persistableEventTime() time.Time { return e.At }

// ModelVisible implements PersistableEvent.
func (Part) ModelVisible() bool { return true }

// Quit records a user or model quitting the server. `Channels`
// is the actor's membership snapshot at quit time so receivers
// route the line into each affected window without re-querying
// membership. See `Join` for the `Instance` / `InstanceID`
// contract.
type Quit struct {
	Channels   []ChannelName `json:"channels"`
	Nick       Nick          `json:"nick"`
	InstanceID InstanceID    `json:"instance_id,omitzero"`
	Message    string        `json:"message,omitempty"`
	At         time.Time     `json:"at"`

	Instance *Instance `json:"-"`
}

func (Quit) persistableEvent()                 {}
func (e Quit) persistableEventTime() time.Time { return e.At }

// ModelVisible implements PersistableEvent.
func (Quit) ModelVisible() bool { return true }

// TopicChange records a topic change. `By` is the actor's
// nick at the time of the change; `ByInstance` is the live handle,
// populated on emission and ignored by JSON.
type TopicChange struct {
	Target ChannelName `json:"channel"`
	Topic  string      `json:"topic"`
	By     Nick        `json:"by"`
	At     time.Time   `json:"at"`

	ByInstance *Instance `json:"-"`
}

func (TopicChange) persistableEvent()                 {}
func (e TopicChange) persistableEventTime() time.Time { return e.At }

// ModelVisible implements PersistableEvent.
func (TopicChange) ModelVisible() bool { return true }

// ModeChange records a privilege change for a member.
// `Instance` is the live affected member, populated on emission and
// ignored by JSON; `Actor` is a free-form actor name (often
// "ChanServ" for synthetic mode changes) and not snapshotted into
// the typed handle.
type ModeChange struct {
	Target     ChannelName `json:"channel"`
	Nick       Nick        `json:"nick"`
	InstanceID InstanceID  `json:"instance_id,omitzero"`
	Mode       NickMode    `json:"mode"`
	By         Nick        `json:"by"`
	At         time.Time   `json:"at"`

	Instance *Instance `json:"-"`
	Actor    string    `json:"-"`
}

func (ModeChange) persistableEvent()                 {}
func (e ModeChange) persistableEventTime() time.Time { return e.At }

// ModelVisible implements PersistableEvent.
func (ModeChange) ModelVisible() bool { return true }

// ModelInvited records a model instance being added to a
// channel. `Instance` is the live invitee handle, populated on
// emission and ignored by JSON.
type ModelInvited struct {
	Target     ChannelName `json:"channel"`
	Nick       Nick        `json:"nick"`
	InstanceID InstanceID  `json:"instance_id,omitzero"`
	By         Nick        `json:"by"`
	At         time.Time   `json:"at"`

	Instance *Instance `json:"-"`
}

func (ModelInvited) persistableEvent()                 {}
func (e ModelInvited) persistableEventTime() time.Time { return e.At }

// ModelVisible implements PersistableEvent.
func (ModelInvited) ModelVisible() bool { return true }

// ModelKicked records a model instance being removed from a
// channel. `Instance` is the live kicked-target handle, populated on
// emission and ignored by JSON.
type ModelKicked struct {
	Target     ChannelName `json:"channel"`
	Nick       Nick        `json:"nick"`
	InstanceID InstanceID  `json:"instance_id,omitzero"`
	By         Nick        `json:"by"`
	At         time.Time   `json:"at"`

	Instance *Instance `json:"-"`
}

func (ModelKicked) persistableEvent()                 {}
func (e ModelKicked) persistableEventTime() time.Time { return e.At }

// ModelVisible implements PersistableEvent.
func (ModelKicked) ModelVisible() bool { return true }

// NickChange records a nick change. `Channels` is the actor's
// membership snapshot at rename time so receivers route the
// line into each affected window without re-querying membership.
// `Instance` is the live renamed handle, populated on emission
// and ignored by JSON.
type NickChange struct {
	Channels   []ChannelName `json:"channels"`
	OldNick    Nick          `json:"old_nick"`
	NewNick    Nick          `json:"new_nick"`
	InstanceID InstanceID    `json:"instance_id,omitzero"`
	At         time.Time     `json:"at"`

	Instance *Instance `json:"-"`
}

func (NickChange) persistableEvent()                 {}
func (e NickChange) persistableEventTime() time.Time { return e.At }

// ModelVisible implements PersistableEvent.
func (NickChange) ModelVisible() bool { return true }

// TopicInfo records the current topic state when queried
// (e.g. via /topic with no arguments).
type TopicInfo struct {
	Target     ChannelName `json:"channel"`
	Topic      string      `json:"topic"`
	TopicSetBy Nick        `json:"topic_set_by,omitempty"`
	TopicSetAt time.Time   `json:"topic_set_at,omitzero"`
	At         time.Time   `json:"at"`
}

func (TopicInfo) persistableEvent()                 {}
func (e TopicInfo) persistableEventTime() time.Time { return e.At }

// ModelVisible implements PersistableEvent.
func (TopicInfo) ModelVisible() bool { return false }

// Help records /help output.
type Help struct {
	Target ChannelName `json:"channel"`
	At     time.Time   `json:"at"`
}

func (Help) persistableEvent()                 {}
func (e Help) persistableEventTime() time.Time { return e.At }

// ModelVisible implements PersistableEvent.
func (Help) ModelVisible() bool { return false }

// Whois records /whois output. Identity-revealing fields
// (`Nick`, `ModelID`, `Persona`, `Channels`) are captured at the
// moment `/whois` is issued and then immutable, so a later rename
// or persona edit does not retro-edit the historical line — IRC
// fidelity demands history is fixed once printed. `Instance` is
// retained as the legacy carrier for events written before the
// snapshot fields existed; renderers fall back to it only when the
// snapshot is empty.
type Whois struct {
	Target   ChannelName   `json:"channel"`
	Nick     Nick          `json:"nick,omitzero"`
	ModelID  ModelID       `json:"model_id,omitzero"`
	Persona  string        `json:"persona,omitzero"`
	Channels []ChannelName `json:"channels,omitzero"`
	Instance *Instance     `json:"instance,omitempty"`
	At       time.Time     `json:"at"`
}

func (Whois) persistableEvent()                 {}
func (e Whois) persistableEventTime() time.Time { return e.At }

// ModelVisible implements PersistableEvent.
func (Whois) ModelVisible() bool { return false }

// ListReply records a single per-channel entry in a `/list`
// response, shaped after IRC's RPL_LIST numeric. There is no
// `Target` field — RPL_LIST is a server-to-client reply that
// carries no addressable target on the wire; the persisting
// client picks where to log each reply.
type ListReply struct {
	Channel ChannelName `json:"channel"`
	Members int         `json:"members"`
	Topic   string      `json:"topic,omitempty"`
	At      time.Time   `json:"at"`
}

func (ListReply) persistableEvent()                 {}
func (e ListReply) persistableEventTime() time.Time { return e.At }

// ModelVisible implements PersistableEvent.
func (ListReply) ModelVisible() bool { return false }

// ListEnd marks the close of a `/list` response, shaped after
// IRC's end-of-list numeric (323). Carries no fields beyond the
// timestamp — the wire numeric has none either.
type ListEnd struct {
	At time.Time `json:"at"`
}

func (ListEnd) persistableEvent()                 {}
func (e ListEnd) persistableEventTime() time.Time { return e.At }

// ModelVisible implements PersistableEvent.
func (ListEnd) ModelVisible() bool { return false }

// CommandError records a command error.
type CommandError struct {
	Target ChannelName `json:"channel"`
	Err    string      `json:"error"`
	At     time.Time   `json:"at"`
}

func (CommandError) persistableEvent()                 {}
func (e CommandError) persistableEventTime() time.Time { return e.At }

// ModelVisible implements PersistableEvent.
func (CommandError) ModelVisible() bool { return false }

// UsageHint records a command usage hint.
type UsageHint struct {
	Target  ChannelName `json:"channel"`
	Command string      `json:"command"`
	Usage   string      `json:"usage"`
	At      time.Time   `json:"at"`
}

func (UsageHint) persistableEvent()                 {}
func (e UsageHint) persistableEventTime() time.Time { return e.At }

// ModelVisible implements PersistableEvent.
func (UsageHint) ModelVisible() bool { return false }

// SystemNotice records a system notification (API key saved,
// poke interval changed, etc.).
type SystemNotice struct {
	Target ChannelName `json:"channel"`
	Text   string      `json:"text"`
	At     time.Time   `json:"at"`
}

func (SystemNotice) persistableEvent()                 {}
func (e SystemNotice) persistableEventTime() time.Time { return e.At }

// ModelVisible implements PersistableEvent.
func (SystemNotice) ModelVisible() bool { return false }

// PersonasList records /personas output.
type PersonasList struct {
	Personas []Persona `json:"personas"`
	At       time.Time `json:"at"`
}

func (PersonasList) persistableEvent()                 {}
func (e PersonasList) persistableEventTime() time.Time { return e.At }

// ModelVisible implements PersistableEvent.
func (PersonasList) ModelVisible() bool { return false }

// EventTime returns the timestamp of a channel event.
func EventTime(e PersistableEvent) time.Time {
	return e.persistableEventTime()
}

// EventTarget returns the name of the window a persistable event
// addresses. Every addressable event type carries a `Target`
// field; the helper centralises the type-switch so consumers (the
// UI's per-window scrollback, observers that need to route
// events) do not duplicate it. `ChannelList` and `PersonasList`
// are not addressable — they carry no per-window target — and
// return the zero value.
func EventTarget(e PersistableEvent) ChannelName {
	switch v := e.(type) {
	case Message:
		return v.Target
	case Join:
		return v.Target
	case Part:
		return v.Target
	case Quit:
		_ = v
		// Actor-scoped; receivers route via `Channels`.
		return ""
	case TopicChange:
		return v.Target
	case ModeChange:
		return v.Target
	case ModelInvited:
		return v.Target
	case ModelKicked:
		return v.Target
	case NickChange:
		_ = v
		// Actor-scoped; receivers route via `Channels`.
		return ""
	case TopicInfo:
		return v.Target
	case Help:
		return v.Target
	case Whois:
		return v.Target
	case ListReply:
		return ""
	case ListEnd:
		return ""
	case CommandError:
		return v.Target
	case UsageHint:
		return v.Target
	case SystemNotice:
		return v.Target
	case PersonasList:
		return ""
	}

	return ""
}

// EventType returns the discriminator string for a channel
// event.
func EventType(e PersistableEvent) string {
	switch e.(type) {
	case Message:
		return "message"
	case Join:
		return "join"
	case Part:
		return "part"
	case Quit:
		return "quit"
	case TopicChange:
		return "topic_change"
	case ModeChange:
		return "mode_change"
	case ModelInvited:
		return "model_invited"
	case ModelKicked:
		return "model_kicked"
	case NickChange:
		return "nick_change"
	case TopicInfo:
		return "topic_info"
	case Help:
		return "help"
	case Whois:
		return "whois"
	case ListReply:
		return "list_reply"
	case ListEnd:
		return "list_end"
	case CommandError:
		return "command_error"
	case UsageHint:
		return "usage_hint"
	case SystemNotice:
		return "system_notice"
	case PersonasList:
		return "personas_list"
	default:
		return ""
	}
}

// persistableEventEnvelope is the JSON wire format for a channel event,
// carrying a type discriminator alongside the event data.
type persistableEventEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// MarshalPersistableEvent encodes a channel event as JSON with a type
// discriminator.
func MarshalPersistableEvent(e PersistableEvent) ([]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("marshal channel event data: %w", err)
	}

	return json.Marshal(persistableEventEnvelope{
		Type: EventType(e),
		Data: data,
	})
}

// UnmarshalPersistableEvent decodes a channel event from JSON, using the
// type discriminator to select the concrete type.
func UnmarshalPersistableEvent(b []byte) (PersistableEvent, error) {
	var env persistableEventEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("unmarshal channel event envelope: %w", err)
	}

	unmarshal := func(target any) error {
		return json.Unmarshal(env.Data, target)
	}

	switch env.Type {
	case "message":
		var e Message
		return e, unmarshal(&e)
	case "join":
		var e Join
		return e, unmarshal(&e)
	case "part":
		var e Part
		return e, unmarshal(&e)
	case "quit":
		var e Quit
		return e, unmarshal(&e)
	case "topic_change":
		var e TopicChange
		return e, unmarshal(&e)
	case "mode_change":
		var e ModeChange
		return e, unmarshal(&e)
	case "model_invited":
		var e ModelInvited
		return e, unmarshal(&e)
	case "model_kicked":
		var e ModelKicked
		return e, unmarshal(&e)
	case "nick_change":
		var e NickChange
		return e, unmarshal(&e)
	case "topic_info":
		var e TopicInfo
		return e, unmarshal(&e)
	case "help":
		var e Help
		return e, unmarshal(&e)
	case "whois":
		var e Whois
		return e, unmarshal(&e)
	case "list_reply":
		var e ListReply
		return e, unmarshal(&e)
	case "list_end":
		var e ListEnd
		return e, unmarshal(&e)
	case "command_error":
		var e CommandError
		return e, unmarshal(&e)
	case "usage_hint":
		var e UsageHint
		return e, unmarshal(&e)
	case "system_notice":
		var e SystemNotice
		return e, unmarshal(&e)
	case "personas_list":
		var e PersonasList
		return e, unmarshal(&e)
	default:
		return nil, fmt.Errorf("unknown channel event type: %q", env.Type)
	}
}

// All PersistableEvent types also implement Event so they flow through
// the session's unified event channel.

func (Message) domainEvent()      {}
func (Join) domainEvent()         {}
func (Part) domainEvent()         {}
func (Quit) domainEvent()         {}
func (TopicChange) domainEvent()  {}
func (ModeChange) domainEvent()   {}
func (ModelInvited) domainEvent() {}
func (ModelKicked) domainEvent()  {}
func (NickChange) domainEvent()   {}
func (TopicInfo) domainEvent()    {}
func (Help) domainEvent()         {}
func (Whois) domainEvent()        {}
func (ListReply) domainEvent()    {}
func (ListEnd) domainEvent()      {}
func (CommandError) domainEvent() {}
func (UsageHint) domainEvent()    {}
func (SystemNotice) domainEvent() {}
func (PersonasList) domainEvent() {}
