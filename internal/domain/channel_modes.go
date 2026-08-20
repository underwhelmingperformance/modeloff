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
// flag; parametric modes (`+l`, `+k`, `+f`) store their value in the
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

	// FloodLimit is the `+f` message limit: how many messages the
	// channel relays in one flood window before it refuses further
	// PRIVMSG and ACTION traffic. Zero means the channel sets no
	// limit of its own. The window itself is a server constant, so
	// the mode parameter is a message count.
	FloodLimit int `json:"flood_limit,omitempty"`
}

// ModeArgument says what a mode flag needs alongside it in a `MODE`
// change. Everything that parses or renders a mode change reads it,
// so a new mode is one entry in [channelModeSpecs] and no new case
// anywhere else.
type ModeArgument int

const (
	// ModeArgUnknown is a flag letter this build does not know. It is
	// the zero value so a lookup that found nothing cannot pass for a
	// boolean flag.
	ModeArgUnknown ModeArgument = iota

	// ModeArgNone is a boolean channel attribute. `+t`, `+m`, `+n`
	// and the rest take no argument in either direction.
	ModeArgNone

	// ModeArgCount is a channel attribute set to a positive integer:
	// `+l 20`, `+f 30`. The remove form takes no argument.
	ModeArgCount

	// ModeArgText is a channel attribute set to a non-empty string:
	// `+k secret`. The remove form takes no argument.
	ModeArgText

	// ModeArgNick is a privilege granted to one named member: `+o
	// alice`, `+v bob`. Its value lives on the member, not on the
	// channel, so no [ChannelModes] field describes it.
	ModeArgNick
)

// channelModeSpec describes one channel-attribute mode: what it
// takes alongside it, how to read its current setting off a
// [ChannelModes], and how to write a change into one.
type channelModeSpec struct {
	flag Mode
	arg  ModeArgument

	// setting reports the mode's `MODE` parameter as `m` has it, and
	// whether the mode is set at all. A boolean flag that is set
	// reports an empty parameter.
	setting func(m ChannelModes) (string, bool)

	// apply writes `+flag param` (add) or `-flag` (remove) into `m`.
	// The caller has already checked that `param` fits the flag's
	// [ModeArgument], so a malformed value cannot reach here.
	apply func(m *ChannelModes, add bool, param string)
}

// channelModeSpecs describes every channel-attribute mode, in the
// order [ChannelModes.IRCString] renders them. The per-member modes
// (`+o`, `+v`) are not here: [Mode.MemberMode] is what recognises
// those, and their value lives on a member.
var channelModeSpecs = []channelModeSpec{
	boolModeSpec(ModeAnonymous, func(m *ChannelModes) *bool { return &m.Anonymous }),
	boolModeSpec(ModeInviteOnly, func(m *ChannelModes) *bool { return &m.InviteOnly }),
	boolModeSpec(ModeModerated, func(m *ChannelModes) *bool { return &m.Moderated }),
	boolModeSpec(ModeNoExternal, func(m *ChannelModes) *bool { return &m.NoExternal }),
	boolModeSpec(ModePrivate, func(m *ChannelModes) *bool { return &m.Private }),
	boolModeSpec(ModeQuiet, func(m *ChannelModes) *bool { return &m.Quiet }),
	boolModeSpec(ModeSecret, func(m *ChannelModes) *bool { return &m.Secret }),
	boolModeSpec(ModeTopicLock, func(m *ChannelModes) *bool { return &m.TopicLock }),
	countModeSpec(ModeUserLimit, func(m *ChannelModes) *int { return &m.UserLimit }),
	textModeSpec(ModeKey, func(m *ChannelModes) *string { return &m.Key }),
	countModeSpec(ModeFloodLimit, func(m *ChannelModes) *int { return &m.FloodLimit }),
}

// boolModeSpec describes a boolean channel attribute stored in the
// field `field` points at.
func boolModeSpec(flag Mode, field func(*ChannelModes) *bool) channelModeSpec {
	return channelModeSpec{
		flag: flag,
		arg:  ModeArgNone,
		setting: func(m ChannelModes) (string, bool) {
			return "", *field(&m)
		},
		apply: func(m *ChannelModes, add bool, _ string) {
			*field(m) = add
		},
	}
}

// countModeSpec describes a channel attribute stored as a positive
// integer. Zero is the unset value, so `-flag` writes zero.
func countModeSpec(flag Mode, field func(*ChannelModes) *int) channelModeSpec {
	return channelModeSpec{
		flag: flag,
		arg:  ModeArgCount,
		setting: func(m ChannelModes) (string, bool) {
			n := *field(&m)

			return strconv.Itoa(n), n > 0
		},
		apply: func(m *ChannelModes, add bool, param string) {
			if !add {
				*field(m) = 0

				return
			}

			// Validation upstream has already accepted this parameter
			// as a positive integer, so a parse failure here would be
			// a bug in that check, and zero is the value that leaves
			// the mode unset.
			n, _ := strconv.Atoi(param)
			*field(m) = n
		},
	}
}

// textModeSpec describes a channel attribute stored as a string. The
// empty string is the unset value, so `-flag` writes it.
func textModeSpec(flag Mode, field func(*ChannelModes) *string) channelModeSpec {
	return channelModeSpec{
		flag: flag,
		arg:  ModeArgText,
		setting: func(m ChannelModes) (string, bool) {
			s := *field(&m)

			return s, s != ""
		},
		apply: func(m *ChannelModes, add bool, param string) {
			if !add {
				*field(m) = ""

				return
			}

			*field(m) = param
		},
	}
}

// channelModeSpecFor finds the description of a channel-attribute
// mode. It reports false for a per-member mode and for a letter this
// build does not know, without distinguishing the two;
// [ModeArgumentFor] is what tells them apart.
func channelModeSpecFor(flag Mode) (channelModeSpec, bool) {
	for _, spec := range channelModeSpecs {
		if spec.flag == flag {
			return spec, true
		}
	}

	return channelModeSpec{}, false
}

// ModeArgumentFor reports what `flag` takes alongside it in a `MODE`
// change, or [ModeArgUnknown] for a letter this build does not know.
// It is the single answer to that question, so the command parser
// that consumes the argument, the validator that checks it and the
// renderer that puts it back on the wire cannot disagree about which
// flags have one.
func ModeArgumentFor(flag Mode) ModeArgument {
	if flag.MemberMode() {
		return ModeArgNick
	}

	spec, ok := channelModeSpecFor(flag)
	if !ok {
		return ModeArgUnknown
	}

	return spec.arg
}

// ApplyChannelMode writes one `MODE` change into `m`: `+flag param`
// when `add`, `-flag` otherwise. A per-member mode or an unknown
// letter leaves `m` alone, since neither has a [ChannelModes] field.
// The caller has already checked `param` against the flag's
// [ModeArgument].
func (m *ChannelModes) ApplyChannelMode(flag Mode, add bool, param string) {
	spec, ok := channelModeSpecFor(flag)
	if !ok {
		return
	}

	spec.apply(m, add, param)
}

// IRCString renders the mode set in canonical RFC 2812 form: a
// leading `+` followed by the set flags in canonical order, then
// any parameters in matching order separated by spaces. The empty
// mode set returns `+`.
//
// Canonical order is [channelModeSpecs]' order, which follows the
// order [ChannelModes] declares its fields, so two equal mode sets
// always render identically.
func (m ChannelModes) IRCString() string {
	var flags strings.Builder
	var params []string

	flags.WriteByte('+')

	for _, spec := range channelModeSpecs {
		param, set := spec.setting(m)
		if !set {
			continue
		}

		flags.WriteRune(rune(spec.flag))

		if spec.arg != ModeArgNone {
			params = append(params, param)
		}
	}

	if len(params) == 0 {
		return flags.String()
	}

	return flags.String() + " " + strings.Join(params, " ")
}

// ParseChannelModes parses a leading-`+` string of boolean channel
// flags, e.g. "+nt", into a [ChannelModes] value. A string that does
// not start with '+' is rejected via [MalformedChannelModeError].
//
// This reads the `/config` default-channel-modes setting, so what it
// accepts is what a channel can be created with. It accepts the
// boolean flags only, and rejects both a per-member mode
// ([ModeArgNick]) and a parametric one ([ModeArgCount],
// [ModeArgText]) via [UnknownModeFlagError]. The two are rejected
// for different reasons. A per-member grant needs a member to grant
// it to, and a channel has none at the moment it is created. A
// parametric default would be a coherent setting; this grammar has
// nowhere to put its argument. Giving a new channel a user limit, a
// key or a flood limit therefore means extending the grammar to the
// parameter form [ChannelModes.IRCString] already renders ("+ntl
// 20"). Until then the two are inverses only across the boolean
// flags.
func ParseChannelModes(s string) (ChannelModes, error) {
	if len(s) == 0 || s[0] != '+' {
		return ChannelModes{}, MalformedChannelModeError{Input: s}
	}

	var modes ChannelModes

	for _, r := range s[1:] {
		flag := Mode(r)

		if ModeArgumentFor(flag) != ModeArgNone {
			return ChannelModes{}, UnknownModeFlagError{Flag: flag}
		}

		modes.ApplyChannelMode(flag, true, "")
	}

	return modes, nil
}

// Invitations is the per-channel pending-invitation set populated
// by `INVITE` and consumed by `JOIN` when `+i` is set. Each entry
// is single-use: a successful join removes the inviter's record.
//
// Entries are keyed by [InstanceID], not by nick. This is a
// deliberate departure from RFC 2812 §3.2.7, which describes the
// list in terms of nicks because a nick is all the wire carries. A
// nick is display state a client may change at will: keyed by nick,
// a client invited as `botty` and then renamed loses the invitation
// it was granted, and a second client taking the freed nick inherits
// it. Keying by the immutable id makes an invitation belong to the
// client it was issued to for as long as that client exists. Do not
// "correct" this back to nicks.
//
// The underlying type is [set.Set] so set operations stay O(1).
// JSON round-trips through a sorted id array so the on-disk shape is
// deterministic and isn't littered with the empty-struct values a
// raw map would carry.
type Invitations set.Set[InstanceID]

// Add records a pending invitation for `id`. Idempotent.
func (s *Invitations) Add(id InstanceID) {
	(*set.Set[InstanceID])(s).Add(id)
}

// Remove clears the pending invitation for `id` and reports
// whether one was present. Used by `JOIN` to consume single-use
// invitations atomically.
func (s *Invitations) Remove(id InstanceID) bool {
	return (*set.Set[InstanceID])(s).Remove(id)
}

// Contains reports whether `id` is currently invited.
func (s Invitations) Contains(id InstanceID) bool {
	return set.Set[InstanceID](s).Has(id)
}

// Clone returns an independent copy of the invitation set. A nil
// set clones to nil, matching the zero value's meaning of "nobody
// invited".
func (s Invitations) Clone() Invitations {
	if s == nil {
		return nil
	}

	return Invitations(set.Set[InstanceID](s).Clone())
}

// MarshalJSON renders the invitation set as a sorted id array so
// the on-disk representation is stable across persistence
// round-trips and reviews.
func (s Invitations) MarshalJSON() ([]byte, error) {
	if len(s) == 0 {
		return []byte("null"), nil
	}

	out := make([]InstanceID, 0, len(s))
	for id := range s {
		out = append(out, id)
	}
	slices.Sort(out)

	return json.Marshal(out)
}

// UnmarshalJSON rehydrates an invitation set from its JSON array
// form. A `null` or missing field yields the zero value.
func (s *Invitations) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*s = nil
		return nil
	}

	var arr []InstanceID
	if err := json.Unmarshal(data, &arr); err != nil {
		return err
	}

	*s = make(Invitations, len(arr))
	for _, id := range arr {
		(*s)[id] = struct{}{}
	}
	return nil
}
