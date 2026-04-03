// Package domain defines the core types for the modeloff application.
package domain

import (
	"time"

	"github.com/laney/modeloff/internal/set"
)

// Nick represents a user or model nickname in the system.
type Nick string

// ChannelPrefix is the prefix used for channel names.
const ChannelPrefix = "#"

// ChannelName represents a chat channel name (with # prefix).
type ChannelName string

// ModelID represents an OpenRouter model identifier (e.g. "anthropic/claude-3-haiku").
type ModelID string

// ChannelKind distinguishes channels from direct messages.
type ChannelKind int

// ChannelKind values distinguish between multi-user channels and
// one-to-one direct message conversations.
const (
	// KindChannel is a named channel that multiple users and models
	// can join (prefixed with # in the UI).
	KindChannel ChannelKind = iota

	// KindDM is a private conversation between the user and a
	// single model instance.
	KindDM
)

// Channel represents a chat channel or direct message conversation.
type Channel struct {
	Name    ChannelName
	Kind    ChannelKind
	Title   string
	Members set.Ordered[Nick]
	Created time.Time
}

// Message represents a single message in a channel.
type Message struct {
	ID      string
	Channel ChannelName
	From    Nick
	Body    string
	SentAt  time.Time
}

// ModelInstance represents a specific instance of a model that has been
// invited into the system. Each instance has its own nick and persona.
type ModelInstance struct {
	Nick     Nick
	ModelID  ModelID
	Persona  string
	Channels set.Ordered[ChannelName]
}

// NickMode represents a user's privilege level in a channel, following
// IRC conventions.
type NickMode int

const (
	// ModeNone indicates no special privileges.
	ModeNone NickMode = iota

	// ModeVoice indicates the user has voice (+), shown as "+nick".
	ModeVoice

	// ModeOp indicates the user is a channel operator (@), shown as
	// "@nick".
	ModeOp
)

// Prefix returns the IRC-style prefix for the mode: "@" for op, "+"
// for voice, or "" for none.
func (m NickMode) Prefix() string {
	switch m {
	case ModeOp:
		return "@"
	case ModeVoice:
		return "+"
	default:
		return ""
	}
}

// Member pairs a nick with its channel mode for display in the nick
// list.
type Member struct {
	Nick Nick
	Mode NickMode
}

// User represents the local user of the application.
type User struct {
	Nick Nick
}
