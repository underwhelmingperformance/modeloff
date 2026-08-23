package domain

import (
	"encoding/json"
	"fmt"
)

// SourceKind distinguishes the origins an IRC-style event can show
// to one recipient.
type SourceKind uint8

const (
	// SourceInvalid is the zero value and does not identify an event
	// origin.
	SourceInvalid SourceKind = iota
	// SourceClient identifies a connected client.
	SourceClient
	// SourceServer identifies the IRC server.
	SourceServer
	// SourceAnonymous identifies a client whose identity was masked.
	SourceAnonymous
)

// Source is the origin prefix one recipient observed on an event.
// It is a value, not an actor handle.
type Source struct {
	kind          SourceKind
	nick          Nick
	instanceID    InstanceID
	hasInstanceID bool
}

// ClientSource constructs an identified client source. The empty ID
// is valid for the user client.
func ClientSource(id InstanceID, nick Nick) Source {
	return Source{
		kind:          SourceClient,
		nick:          nick,
		instanceID:    id,
		hasInstanceID: true,
	}
}

// LegacyClientSource constructs a client source from an event that
// predates stable source IDs.
func LegacyClientSource(nick Nick) Source {
	return Source{kind: SourceClient, nick: nick}
}

// ServerSource constructs a server-originated source.
func ServerSource(name Nick) Source {
	return Source{kind: SourceServer, nick: name}
}

// AnonymousSource constructs the source shown for a masked actor.
func AnonymousSource() Source {
	return Source{kind: SourceAnonymous}
}

// Kind reports which source variant this value contains.
func (s Source) Kind() SourceKind { return s.kind }

// Nick returns the source nick shown to the recipient. Anonymous
// sources use the reserved anonymous nick.
func (s Source) Nick() Nick {
	if s.kind == SourceAnonymous {
		return AnonymousNick
	}

	return s.nick
}

// InstanceID returns the stable client ID when the event disclosed
// one. The boolean remains true for the user's empty ID.
func (s Source) InstanceID() (InstanceID, bool) {
	return s.instanceID, s.kind == SourceClient && s.hasInstanceID
}

// IsClient reports whether the source identifies a client, including
// a legacy client event without a stable ID.
func (s Source) IsClient() bool { return s.kind == SourceClient }

// IsZero reports whether no source has been set.
func (s Source) IsZero() bool { return s.kind == SourceInvalid }

// WithoutInstanceID returns the same visible source without its
// session-internal stable client identifier.
func (s Source) WithoutInstanceID() Source {
	if s.kind != SourceClient {
		return s
	}

	return LegacyClientSource(s.nick)
}

type sourceJSON struct {
	Kind       string      `json:"kind"`
	Nick       Nick        `json:"nick,omitempty"`
	InstanceID *InstanceID `json:"instance_id,omitempty"`
}

// MarshalJSON encodes the source variant while preserving a present
// empty user ID.
func (s Source) MarshalJSON() ([]byte, error) {
	encoded := sourceJSON{Nick: s.nick}

	switch s.kind {
	case SourceClient:
		encoded.Kind = "client"
		if s.hasInstanceID {
			id := s.instanceID
			encoded.InstanceID = &id
		}
	case SourceServer:
		encoded.Kind = "server"
	case SourceAnonymous:
		encoded.Kind = "anonymous"
	default:
		return nil, fmt.Errorf("marshal invalid source kind %d", s.kind)
	}

	return json.Marshal(encoded)
}

// UnmarshalJSON decodes an event source.
func (s *Source) UnmarshalJSON(data []byte) error {
	var encoded sourceJSON
	if err := json.Unmarshal(data, &encoded); err != nil {
		return err
	}

	switch encoded.Kind {
	case "client":
		*s = LegacyClientSource(encoded.Nick)
		if encoded.InstanceID != nil {
			*s = ClientSource(*encoded.InstanceID, encoded.Nick)
		}
	case "server":
		*s = ServerSource(encoded.Nick)
	case "anonymous":
		*s = AnonymousSource()
	default:
		return fmt.Errorf("unmarshal unknown source kind %q", encoded.Kind)
	}

	return nil
}
