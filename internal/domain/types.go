// Package domain defines the core types for the modeloff application.
package domain

import "time"

// Nick represents a user or model nickname in the system.
type Nick string

// RoomPrefix is the prefix used for channel room names.
const RoomPrefix = "#"

// RoomName represents a chat room name (with # prefix for channels).
type RoomName string

// ModelID represents an OpenRouter model identifier (e.g. "anthropic/claude-3-haiku").
type ModelID string

// RoomKind distinguishes channels from direct messages.
type RoomKind int

// RoomKind values distinguish between multi-user channels and
// one-to-one direct message conversations.
const (
	// RoomChannel is a named channel that multiple users and models
	// can join (prefixed with # in the UI).
	RoomChannel RoomKind = iota

	// RoomDM is a private conversation between the user and a
	// single model instance.
	RoomDM
)

// Room represents a chat room or direct message conversation.
type Room struct {
	Name    RoomName
	Kind    RoomKind
	Title   string
	Members []Nick
	Created time.Time
}

// Message represents a single message in a room.
type Message struct {
	ID     string
	Room   RoomName
	From   Nick
	Body   string
	SentAt time.Time
}

// ModelInstance represents a specific instance of a model that has been
// invited into the system. Each instance has its own nick and persona.
type ModelInstance struct {
	Nick    Nick
	ModelID ModelID
	Persona string
	Rooms   []RoomName
}

// User represents the local user of the application.
type User struct {
	Nick Nick
}
