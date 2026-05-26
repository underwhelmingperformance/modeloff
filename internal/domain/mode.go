package domain

// Mode is a single RFC 2812 mode flag letter. The same letter
// carries different semantics depending on the target type of the
// carrying [ModeChange]: 'o' on a channel target is channel-op;
// 'o' with no channel target is server-OPER (a user mode).
// `rune` is the natural carrier — IRC mode flags are single ASCII
// letters.
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

// NickMode is a per-member display rank used by the nick-list
// renderer for sort ordering and the `@`/`+` prefix. It is a
// display concern distinct from the wire-flag [Mode]; the two
// coexist because the chat-screen sorts members by privilege
// while the wire carries single-letter flags. Use [WireFlag]
// to convert.
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

// IRCMode returns the IRC mode-change string for the mode, e.g. "+v"
// for voice and "+o" for op.
func (m NickMode) IRCMode() string {
	switch m {
	case ModeOp:
		return "+o"
	case ModeVoice:
		return "+v"
	default:
		return ""
	}
}

// WireFlag returns the wire-protocol [Mode] letter for this rank,
// suitable for populating [ModeChange.Flag]. The zero value
// [ModeNone] returns the zero [Mode].
func (m NickMode) WireFlag() Mode {
	switch m {
	case ModeOp:
		return ModeOperator
	case ModeVoice:
		return ModeChannelVoice
	default:
		return 0
	}
}

// NickModeFor inverts [NickMode.WireFlag]: it maps a channel-scoped
// wire flag back to its display rank. The zero value is returned
// for any flag that is not a channel-member mode, or when `add` is
// false (the member loses the rank).
func NickModeFor(flag Mode, add bool) NickMode {
	if !add {
		return ModeNone
	}

	switch flag {
	case ModeOperator:
		return ModeOp
	case ModeChannelVoice:
		return ModeVoice
	default:
		return ModeNone
	}
}
