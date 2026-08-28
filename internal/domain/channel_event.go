package domain

import (
	"encoding/json"
	"errors"
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
// An event's [Source] records the IRC prefix one recipient observed
// at event time. It never exposes the mutable actor that produced the
// event. Event subjects, such as a KICK target or MODE parameter, use
// their own protocol fields.
type PersistableEvent interface {
	Event
	persistableEvent()
	persistableEventTime() time.Time
}

// ChannelActivity is the subset of `PersistableEvent` that records
// genuine channel activity: the conversation and membership events
// that belong in a channel's shared event log and are broadcast to
// its members. Numeric replies, command output, and local notices
// are `PersistableEvent` but not `ChannelActivity`, so the channel
// log's write API — which accepts only `ChannelActivity` — rejects
// them at compile time.
type ChannelActivity interface {
	PersistableEvent
	channelActivity()
}

// ChannelDepartureEvent is the closed subset of channel activity that removes
// one member while keeping the member's connection record. PART and KICK are
// committed with the membership change they announce.
type ChannelDepartureEvent interface {
	ChannelActivity
	ProtocolEvent
	channelDepartureEvent()
}

// IssuerReply is the subset of `PersistableEvent` that records a
// point-to-point reply or notice an actor received in answer to its
// own command (WHOIS, LIST, a refused INVITE, a topic query). These
// are an issuer's private memory: they live in the per-instance reply
// log keyed by the issuer's identity, never in a channel's shared log.
// The reply-log write API accepts only `IssuerReply`, so a channel-
// activity event reaching it is a compile error.
//
// `ChannelActivity` and `IssuerReply` partition `PersistableEvent`:
// the two sets are disjoint and together cover every persistable type.
// `TestPersistableEvent_partition` checks that each persistable type
// it lists is classified as exactly one of the two.
type IssuerReply interface {
	PersistableEvent
	issuerReply()
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
	_ PersistableEvent = ChannelModeChange{}
	_ PersistableEvent = Invited{}
	_ PersistableEvent = Inviting{}
	_ PersistableEvent = Kicked{}
	_ PersistableEvent = NickChange{}
	_ PersistableEvent = TopicInfo{}
	_ PersistableEvent = Whois{}
	_ PersistableEvent = ListReply{}
	_ PersistableEvent = CommandError{}
	_ PersistableEvent = SystemNotice{}
	_ PersistableEvent = PersonasList{}

	// Render-only DTOs: renderable on the chat-screen, never
	// persisted, so they satisfy `Event` but not `PersistableEvent`.
	_ Event = Help{}
	_ Event = UsageHint{}

	// ListEnd rides the wire as a `/list` terminator but carries no
	// content and is never stored, so it is `ProtocolEvent`-only.
	_ ProtocolEvent = ListEnd{}

	_ ChannelActivity = Message{}
	_ ChannelActivity = Join{}
	_ ChannelActivity = Part{}
	_ ChannelActivity = Quit{}
	_ ChannelActivity = TopicChange{}
	_ ChannelActivity = ChannelModeChange{}
	_ ChannelActivity = Invited{}
	_ ChannelActivity = Kicked{}
	_ ChannelActivity = NickChange{}

	_ IssuerReply = Whois{}
	_ IssuerReply = Inviting{}
	_ IssuerReply = ListReply{}
	_ IssuerReply = TopicInfo{}
	_ IssuerReply = CommandError{}
	_ IssuerReply = SystemNotice{}
	_ IssuerReply = PersonasList{}
)

// Message records a PRIVMSG or action sent to a channel or client.
type Message struct {
	Source Source      `json:"source"`
	Target ChannelName `json:"channel"`
	Body   string      `json:"body"`
	Action bool        `json:"action,omitempty"`
	At     time.Time   `json:"at"`
}

func (Message) persistableEvent()                 {}
func (e Message) persistableEventTime() time.Time { return e.At }
func (Message) channelActivity()                  {}

// AuthoredBy reports whether this recipient authored the delivered
// message.
func (e Message) AuthoredBy(recipient InstanceID) bool {
	id, ok := e.Source.InstanceID()
	return ok && id == recipient
}

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
		id, identified := e.Source.InstanceID()
		if identified && id == self {
			return e.Target, true
		}

		if ChannelName(self) == e.Target {
			if !identified {
				return "", false
			}

			return ChannelName(id), true
		}

		return "", false
	}

	return "", false
}

// Join records a user or model joining a channel.
type Join struct {
	Source  Source      `json:"source"`
	Target  ChannelName `json:"channel"`
	Created bool        `json:"created,omitempty"`
	Message string      `json:"message,omitempty"`
	At      time.Time   `json:"at"`
}

func (Join) persistableEvent()                 {}
func (e Join) persistableEventTime() time.Time { return e.At }
func (Join) channelActivity()                  {}

// Part records a user or model leaving a channel.
type Part struct {
	Source  Source      `json:"source"`
	Target  ChannelName `json:"channel"`
	Message string      `json:"message,omitempty"`
	At      time.Time   `json:"at"`
}

func (Part) persistableEvent()                 {}
func (e Part) persistableEventTime() time.Time { return e.At }
func (Part) channelActivity()                  {}
func (Part) channelDepartureEvent()            {}

// Quit records a user or model quitting the server. The wire
// payload carries no channel list — RFC 2812 §3.1.7 QUIT is an
// actor-scoped notice with no target. Server-side fan-out applies
// the intersection rule (deliver to peers that share any channel
// with the actor) and each receiving client decides which of its
// own windows to update from local state.
type Quit struct {
	Source  Source    `json:"source"`
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at"`
}

func (Quit) persistableEvent()                 {}
func (e Quit) persistableEventTime() time.Time { return e.At }
func (Quit) channelActivity()                  {}

// TopicChange records a topic change.
type TopicChange struct {
	Source Source      `json:"source"`
	Target ChannelName `json:"channel"`
	Topic  string      `json:"topic"`
	At     time.Time   `json:"at"`
}

func (TopicChange) persistableEvent()                 {}
func (e TopicChange) persistableEventTime() time.Time { return e.At }
func (TopicChange) channelActivity()                  {}

// ChannelModeChange records the channel-scoped form of an RFC 2812
// MODE mutation: a member mode (`+o`/`+v` on a nick) or a channel
// attribute (`+m`, `+l <int>`, `+k <key>`). It is genuine channel
// activity — persisted to the channel log and broadcast to channel
// peers.
//
// `Param` carries the argument for parametric attribute modes
// (`+l <int>`, `+k <key>`); it is empty for member-mode and
// boolean-attribute events.
//
// Subject identifies the affected member for `+o` and `+v`. It is
// empty for a channel attribute.
type ChannelModeChange struct {
	Source  Source      `json:"source"`
	Target  ChannelName `json:"channel"`
	Subject Nick        `json:"subject,omitempty"`
	Flag    Mode        `json:"flag"`
	Add     bool        `json:"add"`
	Param   string      `json:"param,omitempty"`
	At      time.Time   `json:"at"`
}

// ServerIssued reports whether the server originated the change.
func (e ChannelModeChange) ServerIssued() bool { return e.Source.Kind() == SourceServer }

func (ChannelModeChange) persistableEvent()                 {}
func (e ChannelModeChange) persistableEventTime() time.Time { return e.At }
func (ChannelModeChange) channelActivity()                  {}

// UserModeChange records the user-scoped form of an RFC 2812 MODE
// mutation: a global flag on a single client (the `+o` operator
// grant). It is a capability signal delivered point-to-point to the
// affected client's own bus — RFC 2812 §3.1.5 scopes user-mode
// replies to the requester — never persisted and never broadcast.
type UserModeChange struct {
	Source  Source    `json:"source"`
	Subject Nick      `json:"subject"`
	Flag    Mode      `json:"flag"`
	Add     bool      `json:"add"`
	At      time.Time `json:"at"`
}

// Invited records a client being invited to a channel.
type Invited struct {
	Source  Source      `json:"source"`
	Target  ChannelName `json:"channel"`
	Invitee Nick        `json:"invitee"`
	At      time.Time   `json:"at"`
}

func (Invited) persistableEvent()                 {}
func (e Invited) persistableEventTime() time.Time { return e.At }
func (Invited) channelActivity()                  {}

// Inviting confirms to a command issuer that the server delivered an
// invitation. It is the RPL_INVITING reply, distinct from the
// [Invited] event delivered to the invitee.
type Inviting struct {
	Target  ChannelName `json:"channel"`
	Invitee Nick        `json:"invitee"`
	At      time.Time   `json:"at"`
}

func (Inviting) persistableEvent()                 {}
func (e Inviting) persistableEventTime() time.Time { return e.At }
func (Inviting) issuerReply()                      {}

// Kicked records a client being removed from a channel.
type Kicked struct {
	Source        Source      `json:"source"`
	Target        ChannelName `json:"channel"`
	Subject       Nick        `json:"subject"`
	SubjectIsSelf bool        `json:"subject_is_self,omitempty"`
	At            time.Time   `json:"at"`
}

func (Kicked) persistableEvent()                 {}
func (e Kicked) persistableEventTime() time.Time { return e.At }
func (Kicked) channelActivity()                  {}
func (Kicked) channelDepartureEvent()            {}

// NickChange records a nick change. The wire payload carries no
// channel list — RFC 2812 §3.1.2 NICK is an actor-scoped notice
// with no target. Server-side fan-out applies the intersection
// rule (deliver to peers that share any channel with the actor)
// and each receiving client decides which of its own windows to
// update from local state.
type NickChange struct {
	Source  Source    `json:"source"`
	NewNick Nick      `json:"new_nick"`
	At      time.Time `json:"at"`
}

func (NickChange) persistableEvent()                 {}
func (e NickChange) persistableEventTime() time.Time { return e.At }
func (NickChange) channelActivity()                  {}

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
func (TopicInfo) issuerReply()                      {}

// Help carries the `/help` output. It is a render-only DTO raised by
// the chat-screen when the user types `/help`; it never touches the
// session or the store, so it is a plain `Event` and not a
// `PersistableEvent`.
type Help struct {
	Target ChannelName `json:"channel"`
	At     time.Time   `json:"at"`
}

// Whois records /whois output. The identity-revealing fields
// (`Nick`, `ModelID`, `Persona`, `PersonaLineage`, `Channels`) are
// captured at the moment `/whois` is issued and are immutable from
// then on, so a later rename, persona edit or accepted reflection
// does not retro-edit the historical line. IRC fidelity demands that
// history is fixed once printed.
type Whois struct {
	Nick           Nick          `json:"nick,omitzero"`
	ModelID        ModelID       `json:"model_id,omitzero"`
	Persona        string        `json:"persona,omitzero"`
	PersonaLineage PersonaCounts `json:"persona_lineage,omitzero"`
	Channels       []ChannelName `json:"channels,omitzero"`
	At             time.Time     `json:"at"`
}

func (Whois) persistableEvent()                 {}
func (e Whois) persistableEventTime() time.Time { return e.At }
func (Whois) issuerReply()                      {}

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
func (ListReply) issuerReply()                      {}

// ListEnd marks the close of a `/list` response, shaped after
// IRC's end-of-list numeric (323). Carries no fields beyond the
// timestamp — the wire numeric has none either. It rides the wire
// in `Response.Events` but holds no content, so it is delivered live
// and never persisted: the model reconstructs the list boundary from
// the preceding `ListReply` sequence.
type ListEnd struct {
	At time.Time `json:"at"`
}

// JoinedChannel confirms one channel a JOIN command reached. RFC
// 2812 §3.2.1 answers each target in a multi-target JOIN with its
// own reply; `Session.handleJoin` puts one `JoinedChannel` in
// `Response.Events` for every channel it actually joined, alongside
// a typed error for every channel a gate refused. Together,
// `Response.Events` is a complete account of what a multi-target
// JOIN did: which channels joined, and why the rest did not. Like
// `ListEnd`, it rides the wire in `Response.Events` but is never
// persisted. A model reloads its channel history from the channel's
// own event log on reattach, not from this reply.
type JoinedChannel struct {
	Channel ChannelName `json:"channel"`
}

// CommandError records a command error.
type CommandError struct {
	Target ChannelName `json:"channel"`
	Err    string      `json:"error"`
	At     time.Time   `json:"at"`
}

func (CommandError) persistableEvent()                 {}
func (e CommandError) persistableEventTime() time.Time { return e.At }
func (CommandError) issuerReply()                      {}

// UsageHint carries a command usage hint. It is a render-only DTO the
// chat-screen raises when the user mistypes a command or issues one
// out of context; the chatcmd grammar rejects the input client-side
// and nothing reaches the session, so it is a plain `Event` and not a
// `PersistableEvent`.
type UsageHint struct {
	Target  ChannelName `json:"channel"`
	Command string      `json:"command"`
	Usage   string      `json:"usage"`
	At      time.Time   `json:"at"`
}

// SystemNotice records a system notification (API key saved,
// poke interval changed, etc.).
type SystemNotice struct {
	Target ChannelName `json:"channel"`
	Text   string      `json:"text"`
	At     time.Time   `json:"at"`
}

func (SystemNotice) persistableEvent()                 {}
func (e SystemNotice) persistableEventTime() time.Time { return e.At }
func (SystemNotice) issuerReply()                      {}

// PersonasList records /personas output.
type PersonasList struct {
	Personas []Persona `json:"personas"`
	At       time.Time `json:"at"`
}

func (PersonasList) persistableEvent()                 {}
func (e PersonasList) persistableEventTime() time.Time { return e.At }
func (PersonasList) issuerReply()                      {}

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
	case Quit, NickChange:
		// Actor-scoped; receivers route via local membership state.
		return ""
	case TopicChange:
		return v.Target
	case ChannelModeChange:
		return v.Target
	case Invited:
		return v.Target
	case Inviting:
		return v.Target
	case Kicked:
		return v.Target
	case TopicInfo:
		return v.Target
	case Whois:
		return ""
	case ListReply:
		return ""
	case CommandError:
		return v.Target
	case SystemNotice:
		return v.Target
	case PersonasList:
		return ""
	}

	return ""
}

// EventType returns the discriminator string for a channel
// event. These are a fixed, hand-maintained vocabulary persisted in
// the store — not derived from the Go type name — so a type rename
// never orphans existing rows. `Invited` and `Kicked` keep their
// historic "model_invited"/"model_kicked" tags for exactly this
// reason.
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
	case ChannelModeChange:
		return "mode_change"
	case Invited:
		return "model_invited"
	case Inviting:
		return "inviting"
	case Kicked:
		return "model_kicked"
	case NickChange:
		return "nick_change"
	case TopicInfo:
		return "topic_info"
	case Whois:
		return "whois"
	case ListReply:
		return "list_reply"
	case CommandError:
		return "command_error"
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
	Version int             `json:"version,omitempty"`
	Type    string          `json:"type"`
	Data    json.RawMessage `json:"data"`
}

// MarshalPersistableEvent encodes a channel event as JSON with a type
// discriminator.
func MarshalPersistableEvent(e PersistableEvent) ([]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("marshal channel event data: %w", err)
	}

	return json.Marshal(persistableEventEnvelope{
		Version: 2,
		Type:    EventType(e),
		Data:    data,
	})
}

// ErrUnknownEventType reports that a stored event row carries a type
// discriminator this build no longer recognises. An older database may
// hold rows for event kinds that have since left the persistable
// hierarchy; the channel-log read path skips such rows rather than
// failing the whole batch. Callers test for it with `errors.Is`.
var ErrUnknownEventType = errors.New("unknown channel event type")

// UnmarshalPersistableEvent decodes a channel event from JSON, using the
// type discriminator to select the concrete type. A discriminator this
// build does not know yields [ErrUnknownEventType] so the read path can
// skip the row.
func UnmarshalPersistableEvent(b []byte) (PersistableEvent, error) {
	var env persistableEventEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("unmarshal channel event envelope: %w", err)
	}

	unmarshal := func(target any) error {
		return json.Unmarshal(env.Data, target)
	}
	if env.Version < 2 {
		return unmarshalLegacyPersistableEvent(env.Type, env.Data)
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
		var e ChannelModeChange
		return e, unmarshal(&e)
	case "model_invited":
		var e Invited
		return e, unmarshal(&e)
	case "inviting":
		var e Inviting
		return e, unmarshal(&e)
	case "model_kicked":
		var e Kicked
		return e, unmarshal(&e)
	case "nick_change":
		var e NickChange
		return e, unmarshal(&e)
	case "topic_info":
		var e TopicInfo
		return e, unmarshal(&e)
	case "whois":
		var e Whois
		return e, unmarshal(&e)
	case "list_reply":
		var e ListReply
		return e, unmarshal(&e)
	case "command_error":
		var e CommandError
		return e, unmarshal(&e)
	case "system_notice":
		var e SystemNotice
		return e, unmarshal(&e)
	case "personas_list":
		var e PersonasList
		return e, unmarshal(&e)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownEventType, env.Type)
	}
}

func legacySource(id *InstanceID, nick Nick) Source {
	if nick == AnonymousNick {
		return AnonymousSource()
	}
	if id == nil {
		return LegacyClientSource(nick)
	}

	return ClientSource(*id, nick)
}

func unmarshalLegacyPersistableEvent(eventType string, data json.RawMessage) (PersistableEvent, error) {
	decode := func(target any) error { return json.Unmarshal(data, target) }

	switch eventType {
	case "message":
		var old struct {
			Target     ChannelName `json:"channel"`
			From       Nick        `json:"from"`
			InstanceID *InstanceID `json:"instance_id"`
			Body       string      `json:"body"`
			Action     bool        `json:"action"`
			At         time.Time   `json:"at"`
		}
		if err := decode(&old); err != nil {
			return nil, err
		}
		return Message{
			Source: legacySource(old.InstanceID, old.From), Target: old.Target,
			Body: old.Body, Action: old.Action, At: old.At,
		}, nil
	case "join":
		var old struct {
			Target     ChannelName `json:"channel"`
			Nick       Nick        `json:"nick"`
			InstanceID *InstanceID `json:"instance_id"`
			Created    bool        `json:"created"`
			Message    string      `json:"message"`
			At         time.Time   `json:"at"`
		}
		if err := decode(&old); err != nil {
			return nil, err
		}
		return Join{Source: legacySource(old.InstanceID, old.Nick), Target: old.Target, Created: old.Created, Message: old.Message, At: old.At}, nil
	case "part":
		var old struct {
			Target     ChannelName `json:"channel"`
			Nick       Nick        `json:"nick"`
			InstanceID *InstanceID `json:"instance_id"`
			Message    string      `json:"message"`
			At         time.Time   `json:"at"`
		}
		if err := decode(&old); err != nil {
			return nil, err
		}
		return Part{Source: legacySource(old.InstanceID, old.Nick), Target: old.Target, Message: old.Message, At: old.At}, nil
	case "quit":
		var old struct {
			Nick       Nick        `json:"nick"`
			InstanceID *InstanceID `json:"instance_id"`
			Message    string      `json:"message"`
			At         time.Time   `json:"at"`
		}
		if err := decode(&old); err != nil {
			return nil, err
		}
		return Quit{Source: legacySource(old.InstanceID, old.Nick), Message: old.Message, At: old.At}, nil
	case "topic_change":
		var old struct {
			Target     ChannelName `json:"channel"`
			Topic      string      `json:"topic"`
			By         Nick        `json:"by"`
			InstanceID *InstanceID `json:"instance_id"`
			At         time.Time   `json:"at"`
		}
		if err := decode(&old); err != nil {
			return nil, err
		}
		return TopicChange{Source: legacySource(old.InstanceID, old.By), Target: old.Target, Topic: old.Topic, At: old.At}, nil
	case "mode_change":
		var old struct {
			Target       ChannelName `json:"channel"`
			Nick         Nick        `json:"nick"`
			Flag         Mode        `json:"flag"`
			Add          bool        `json:"add"`
			Param        string      `json:"param"`
			By           Nick        `json:"by"`
			ByInstanceID *InstanceID `json:"by_instance_id"`
			At           time.Time   `json:"at"`
		}
		if err := decode(&old); err != nil {
			return nil, err
		}
		source := ServerSource("")
		if old.By != "" {
			source = legacySource(old.ByInstanceID, old.By)
		}
		return ChannelModeChange{Source: source, Target: old.Target, Subject: old.Nick, Flag: old.Flag, Add: old.Add, Param: old.Param, At: old.At}, nil
	case "model_invited":
		var old struct {
			Target       ChannelName `json:"channel"`
			Nick         Nick        `json:"nick"`
			By           Nick        `json:"by"`
			ByInstanceID *InstanceID `json:"by_instance_id"`
			At           time.Time   `json:"at"`
		}
		if err := decode(&old); err != nil {
			return nil, err
		}
		return Invited{Source: legacySource(old.ByInstanceID, old.By), Target: old.Target, Invitee: old.Nick, At: old.At}, nil
	case "model_kicked":
		var old struct {
			Target       ChannelName `json:"channel"`
			Nick         Nick        `json:"nick"`
			By           Nick        `json:"by"`
			ByInstanceID *InstanceID `json:"by_instance_id"`
			At           time.Time   `json:"at"`
		}
		if err := decode(&old); err != nil {
			return nil, err
		}
		return Kicked{Source: legacySource(old.ByInstanceID, old.By), Target: old.Target, Subject: old.Nick, At: old.At}, nil
	case "nick_change":
		var old struct {
			OldNick    Nick        `json:"old_nick"`
			NewNick    Nick        `json:"new_nick"`
			InstanceID *InstanceID `json:"instance_id"`
			At         time.Time   `json:"at"`
		}
		if err := decode(&old); err != nil {
			return nil, err
		}
		return NickChange{Source: legacySource(old.InstanceID, old.OldNick), NewNick: old.NewNick, At: old.At}, nil
	default:
		return unmarshalPersistableEventV2(eventType, data)
	}
}

func unmarshalPersistableEventV2(eventType string, data json.RawMessage) (PersistableEvent, error) {
	unmarshal := func(target any) error { return json.Unmarshal(data, target) }

	switch eventType {
	case "topic_info":
		var e TopicInfo
		return e, unmarshal(&e)
	case "whois":
		var e Whois
		return e, unmarshal(&e)
	case "list_reply":
		var e ListReply
		return e, unmarshal(&e)
	case "command_error":
		var e CommandError
		return e, unmarshal(&e)
	case "system_notice":
		var e SystemNotice
		return e, unmarshal(&e)
	case "personas_list":
		var e PersonasList
		return e, unmarshal(&e)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownEventType, eventType)
	}
}

// All PersistableEvent types also implement Event so they flow through
// the session's unified event channel.

func (Message) domainEvent()           {}
func (Join) domainEvent()              {}
func (Part) domainEvent()              {}
func (Quit) domainEvent()              {}
func (TopicChange) domainEvent()       {}
func (ChannelModeChange) domainEvent() {}
func (UserModeChange) domainEvent()    {}
func (Invited) domainEvent()           {}
func (Inviting) domainEvent()          {}
func (Kicked) domainEvent()            {}
func (NickChange) domainEvent()        {}
func (TopicInfo) domainEvent()         {}
func (Help) domainEvent()              {}
func (Whois) domainEvent()             {}
func (ListReply) domainEvent()         {}
func (ListEnd) domainEvent()           {}
func (JoinedChannel) domainEvent()     {}
func (CommandError) domainEvent()      {}
func (UsageHint) domainEvent()         {}
func (SystemNotice) domainEvent()      {}
func (PersonasList) domainEvent()      {}
