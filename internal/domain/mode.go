package domain

import (
	"encoding/json"
	"log/slog"
)

// Mode is a single RFC 2812 mode flag letter. The same letter carries
// different semantics by the event carrying it: 'o' on a
// [ChannelModeChange] is channel-op; 'o' on a [UserModeChange] is
// server-OPER. `rune` is the natural carrier — IRC mode flags are
// single ASCII letters.
type Mode rune

// Per-member modes from RFC 2812 §3.2.3. `+o` doubles as the
// user-mode flag per §3.1.5 when the carrying event has no
// channel target.
const (
	ModeOperator     Mode = 'o'
	ModeChannelVoice Mode = 'v'
)

// Channel-attribute modes from RFC 2811 §4.2 / RFC 2812 §3.2.3.
// Each scopes a behaviour on the channel as a whole: the boolean
// ones toggle a flag; `+l` and `+k` take a parameter (user-limit,
// channel key).
const (
	ModeAnonymous  Mode = 'a'
	ModeInviteOnly Mode = 'i'
	ModeModerated  Mode = 'm'
	ModeNoExternal Mode = 'n'
	ModePrivate    Mode = 'p'
	ModeQuiet      Mode = 'q'
	ModeSecret     Mode = 's'
	ModeTopicLock  Mode = 't'
	ModeUserLimit  Mode = 'l'
	ModeKey        Mode = 'k'
)

// MemberMode reports whether the flag is one of the per-member
// modes ([ModeOperator], [ModeChannelVoice]) — the ones whose
// `MODE` form takes a nick target inside a channel. The remaining
// flags are channel attributes whose `MODE` form takes either no
// parameter or a single value.
func (m Mode) MemberMode() bool {
	return m == ModeOperator || m == ModeChannelVoice
}

// IRCString renders a mode flag in the conventional `+x` / `-x`
// shape per the `add` direction. The empty Mode renders as the
// empty string.
func (m Mode) IRCString(add bool) string {
	if m == 0 {
		return ""
	}

	if add {
		return "+" + string(rune(m))
	}

	return "-" + string(rune(m))
}

// MemberModes is the set of per-member privileges one member holds
// in one channel. RFC 2811 §4.1 makes channel-operator and voice
// independent privileges: a member may hold both at once, and
// granting or revoking either one leaves the other alone.
//
// The zero value is an ordinary member with neither privilege.
type MemberModes struct {
	Operator bool
	Voice    bool
}

// Has reports whether the member holds `flag`. It reports false for
// any flag that is not a per-member mode.
func (m MemberModes) Has(flag Mode) bool {
	switch flag {
	case ModeOperator:
		return m.Operator
	case ModeChannelVoice:
		return m.Voice
	default:
		return false
	}
}

// With applies `+flag` (add) or `-flag` to the receiver and returns
// the result. Only the named privilege changes. For a flag that is
// not a per-member mode it returns the receiver unchanged.
func (m MemberModes) With(flag Mode, add bool) MemberModes {
	switch flag {
	case ModeOperator:
		m.Operator = add
	case ModeChannelVoice:
		m.Voice = add
	}

	return m
}

// Rank returns the highest privilege the member holds. The nick-list
// prefix and the member sort order both use it. The ordering is the
// ISUPPORT PREFIX one: `@` outranks `+`.
func (m MemberModes) Rank() NickMode {
	switch {
	case m.Operator:
		return ModeOp
	case m.Voice:
		return ModeVoice
	default:
		return ModeNone
	}
}

// IRCString renders the held privileges as their mode letters in
// rank order (`ov`, `o`, `v`, or the empty string).
func (m MemberModes) IRCString() string {
	var flags []rune

	if m.Operator {
		flags = append(flags, rune(ModeOperator))
	}
	if m.Voice {
		flags = append(flags, rune(ModeChannelVoice))
	}

	return string(flags)
}

// MarshalJSON encodes the set as its mode letters, so a persisted
// member record spells out the privileges the member holds.
func (m MemberModes) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.IRCString())
}

// UnmarshalJSON decodes the mode-letter form [MemberModes.MarshalJSON]
// writes. It keeps the letters this build understands and drops the
// rest, naming them in a single log line.
//
// Dropping rather than refusing is deliberate. A record written by a
// build that knows a per-member mode this one does not, which is what
// a downgrade or a later half-op letter produces, reaches this method
// through the channel-window decode, and an error here fails
// `store.GetWindow` and so the JOIN. One member's privileges must not
// decide whether anyone can enter the channel. Refusing an
// unrecognised flag belongs to [ParseChannelModes] and the MODE
// command's own validation, where a caller is checking input it can
// still correct.
func (m *MemberModes) UnmarshalJSON(data []byte) error {
	var letters string
	if err := json.Unmarshal(data, &letters); err != nil {
		return err
	}

	var parsed MemberModes
	var unknown []rune

	for _, r := range letters {
		flag := Mode(r)
		if !flag.MemberMode() {
			unknown = append(unknown, r)

			continue
		}

		parsed = parsed.With(flag, true)
	}

	if len(unknown) > 0 {
		slog.Default().Warn("dropped member mode letters this build does not know",
			"component", "domain",
			"letters", string(unknown),
		)
	}

	*m = parsed

	return nil
}

// NickMode is a per-member display rank used by the nick-list
// renderer for sort ordering and the `@`/`+` prefix. It is derived
// from a member's [MemberModes] via [MemberModes.Rank]: the wire
// carries the single-letter [Mode] flags, and a rank names only the
// highest privilege a member holds.
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

// modesForRank maps a display rank back to the privilege set that
// produces it. Member records written before the two privileges
// became independent store a single rank; see [memberJSON].
func modesForRank(rank NickMode) MemberModes {
	switch rank {
	case ModeOp:
		return MemberModes{Operator: true}
	case ModeVoice:
		return MemberModes{Voice: true}
	default:
		return MemberModes{}
	}
}
