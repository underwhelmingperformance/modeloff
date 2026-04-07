// Package domain defines the core types for the modeloff application.
package domain

import (
	"encoding/json"
	"iter"
	"time"

	"github.com/laney/modeloff/internal/set"
	orderedmap "github.com/wk8/go-ordered-map/v2"
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
	Name       ChannelName
	Kind       ChannelKind
	Topic      string
	TopicSetBy Nick
	TopicSetAt time.Time
	Members    MemberList
	Created    time.Time
}

// DisplayName returns the channel name formatted for display. DM
// channels are prefixed with @ instead of shown as bare names.
func (ch Channel) DisplayName() string {
	if ch.Kind == KindDM {
		return "@" + string(ch.Name)
	}

	return string(ch.Name)
}

// Message represents a single message in a channel.
type Message struct {
	ID      string
	Channel ChannelName
	From    Nick
	Body    string
	Action  bool
	SentAt  time.Time
}

// Instance represents an actor on the IRC server. Both the human
// user and model instances share this type. The human user has an
// empty ModelID.
type Instance struct {
	Nick     Nick
	ModelID  ModelID
	Persona  string
	Channels *orderedmap.OrderedMap[ChannelName, time.Time]
}

// IsModel reports whether the instance is a model (as opposed to
// the human user).
func (i Instance) IsModel() bool { return i.ModelID != "" }

// PendingQuit records a quit that was initiated but not yet fully
// processed. Models in the listed channels still need to be notified.
type PendingQuit struct {
	Nick     Nick          `json:"nick"`
	Message  string        `json:"message,omitempty"`
	At       time.Time     `json:"at"`
	Channels []ChannelName `json:"channels"`
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

// String returns the IRC-style prefix for the mode: "@" for op, "+"
// for voice, or "" for none.
func (m NickMode) String() string {
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

func (m Member) String() string {
	return m.Mode.String() + string(m.Nick)
}

// allModes lists every NickMode in the order they appear in the
// tree (highest privilege first).
var allModes = [...]NickMode{ModeOp, ModeVoice, ModeNone}

// memberLess defines the display order for members: higher modes
// first (op > voice > none), then alphabetically by nick within
// each mode.
func memberLess(a, b Member) bool {
	if a.Mode != b.Mode {
		return a.Mode > b.Mode
	}

	return a.Nick < b.Nick
}

// MemberList is a sorted set of channel members ordered by mode
// then nick. The sort is maintained at insertion time so iteration
// and positional access are always free of re-sorting.
type MemberList struct {
	members *set.Sorted[Member]
}

// NewMemberList creates an empty member list.
func NewMemberList() MemberList {
	return MemberList{members: set.NewSorted(memberLess)}
}

// Add inserts a nick as a regular (unprivileged) member.
func (ml MemberList) Add(nick Nick) {
	ml.members.Insert(Member{Nick: nick, Mode: ModeNone})
}

// Remove deletes the given member.
func (ml MemberList) Remove(m Member) {
	ml.members.Remove(m)
}

// SetMode changes a member's privilege level. This removes and
// re-inserts the member since mode is part of the sort key.
func (ml MemberList) SetMode(nick Nick, mode NickMode) {
	if cur, ok := ml.Get(nick); ok {
		ml.members.Remove(cur)
	}

	ml.members.Insert(Member{Nick: nick, Mode: mode})
}

// Get finds a member by nick, checking each mode. Returns the
// member and true if found.
func (ml MemberList) Get(nick Nick) (Member, bool) {
	for _, mode := range allModes {
		if m, ok := ml.members.Get(Member{Nick: nick, Mode: mode}); ok {
			return m, true
		}
	}

	return Member{}, false
}

// Has reports whether the nick is present at any mode.
func (ml MemberList) Has(nick Nick) bool {
	_, ok := ml.Get(nick)

	return ok
}

// Len returns the total number of members.
func (ml MemberList) Len() int {
	return ml.members.Len()
}

// GetAt returns the member at the given display position.
func (ml MemberList) GetAt(index int) (Member, bool) {
	return ml.members.GetAt(index)
}

// SortedSet returns the underlying sorted set. This exposes the
// btree directly for use by the generic sidebar.
func (ml MemberList) SortedSet() *set.Sorted[Member] {
	return ml.members
}

// All yields every member in display order.
func (ml MemberList) All() iter.Seq2[int, Member] {
	return ml.members.Indexed()
}

// Slice returns all members as a plain slice in display order.
func (ml MemberList) Slice() []Member {
	if ml.Len() == 0 {
		return nil
	}

	out := make([]Member, 0, ml.Len())

	for _, m := range ml.All() {
		out = append(out, m)
	}

	return out
}

// Nicks returns an iterator over just the nicks in display order.
func (ml MemberList) Nicks() iter.Seq[Nick] {
	return func(yield func(Nick) bool) {
		for _, m := range ml.All() {
			if !yield(m.Nick) {
				return
			}
		}
	}
}

// MarshalJSON encodes the member list as a JSON array of members.
func (ml MemberList) MarshalJSON() ([]byte, error) {
	members := make([]Member, 0, ml.Len())

	for _, m := range ml.All() {
		members = append(members, m)
	}

	return json.Marshal(members)
}

// UnmarshalJSON decodes a JSON array of members into the list. An
// empty or null array leaves the btree nil so that the zero value
// round-trips cleanly under reflect.DeepEqual.
func (ml *MemberList) UnmarshalJSON(data []byte) error {
	var members []Member
	if err := json.Unmarshal(data, &members); err != nil {
		return err
	}

	if len(members) == 0 {
		*ml = MemberList{}
		return nil
	}

	*ml = NewMemberList()

	for _, m := range members {
		ml.members.Insert(m)
	}

	return nil
}
