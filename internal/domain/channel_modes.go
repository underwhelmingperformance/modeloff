package domain

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/laney/modeloff/internal/set"
)

// ChannelModes is the per-channel attribute mode set. Each boolean
// field tracks the corresponding RFC 2811 §4.2 / RFC 2812 §3.2.3
// flag; parametric modes (`+l`, `+k`) carry their value in the
// corresponding scalar field and are considered set iff the value
// is non-zero.
//
// The zero value is the absence of every flag — newly created
// channels start there, and the user opts in to behaviour by
// issuing `MODE` against the channel.
type ChannelModes struct {
	Anonymous  bool   `json:"anonymous,omitempty"`
	InviteOnly bool   `json:"invite_only,omitempty"`
	Moderated  bool   `json:"moderated,omitempty"`
	NoExternal bool   `json:"no_external,omitempty"`
	Private    bool   `json:"private,omitempty"`
	Quiet      bool   `json:"quiet,omitempty"`
	Secret     bool   `json:"secret,omitempty"`
	TopicLock  bool   `json:"topic_lock,omitempty"`
	UserLimit  int    `json:"user_limit,omitempty"`
	Key        string `json:"key,omitempty"`
}

// IRCString renders the mode set in canonical RFC 2812 form: a
// leading `+` followed by the set flags in canonical order, then
// any parameters in matching order separated by spaces. The empty
// mode set returns `+`.
//
// Canonical order is the order [ChannelModes] declares its fields
// (anonymous, invite-only, moderated, no-external, private, quiet,
// secret, topic-lock, user-limit, key), so two equal mode sets
// always render identically.
func (m ChannelModes) IRCString() string {
	var flags strings.Builder
	var params []string

	flags.WriteByte('+')

	if m.Anonymous {
		flags.WriteRune(rune(ModeAnonymous))
	}
	if m.InviteOnly {
		flags.WriteRune(rune(ModeInviteOnly))
	}
	if m.Moderated {
		flags.WriteRune(rune(ModeModerated))
	}
	if m.NoExternal {
		flags.WriteRune(rune(ModeNoExternal))
	}
	if m.Private {
		flags.WriteRune(rune(ModePrivate))
	}
	if m.Quiet {
		flags.WriteRune(rune(ModeQuiet))
	}
	if m.Secret {
		flags.WriteRune(rune(ModeSecret))
	}
	if m.TopicLock {
		flags.WriteRune(rune(ModeTopicLock))
	}
	if m.UserLimit > 0 {
		flags.WriteRune(rune(ModeUserLimit))
		params = append(params, strconv.Itoa(m.UserLimit))
	}
	if m.Key != "" {
		flags.WriteRune(rune(ModeKey))
		params = append(params, m.Key)
	}

	if len(params) == 0 {
		return flags.String()
	}

	return flags.String() + " " + strings.Join(params, " ")
}

// InvitedNicks is the per-channel pending-invitation set populated
// by `INVITE` and consumed by `JOIN` when `+i` is set. Each entry
// is single-use: a successful join removes the inviter's record.
//
// The underlying type is [set.Set] so set operations stay O(1).
// JSON round-trips through a sorted nick array so the on-disk
// shape is deterministic and isn't littered with the empty-struct
// values a raw map would carry.
type InvitedNicks set.Set[Nick]

// Add records a pending invitation for `nick`. Idempotent.
func (s *InvitedNicks) Add(nick Nick) {
	(*set.Set[Nick])(s).Add(nick)
}

// Remove clears the pending invitation for `nick` and reports
// whether one was present. Used by `JOIN` to consume single-use
// invitations atomically.
func (s *InvitedNicks) Remove(nick Nick) bool {
	return (*set.Set[Nick])(s).Remove(nick)
}

// Contains reports whether `nick` is currently invited.
func (s InvitedNicks) Contains(nick Nick) bool {
	return set.Set[Nick](s).Has(nick)
}

// MarshalJSON renders the invitation set as a sorted nick array so
// the on-disk representation is stable across persistence
// round-trips and reviews.
func (s InvitedNicks) MarshalJSON() ([]byte, error) {
	if len(s) == 0 {
		return []byte("null"), nil
	}

	out := make([]Nick, 0, len(s))
	for n := range s {
		out = append(out, n)
	}
	slices.Sort(out)

	return json.Marshal(out)
}

// UnmarshalJSON rehydrates an invitation set from its JSON array
// form. A `null` or missing field yields the zero value.
func (s *InvitedNicks) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*s = nil
		return nil
	}

	var arr []Nick
	if err := json.Unmarshal(data, &arr); err != nil {
		return err
	}

	*s = make(InvitedNicks, len(arr))
	for _, n := range arr {
		(*s)[n] = struct{}{}
	}
	return nil
}
