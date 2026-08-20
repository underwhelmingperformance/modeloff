package domain

import "strings"

// NickMaxLen is this server's NICKLEN (RFC 2812 ISUPPORT). RFC 2812
// §1.2.1 sets nine as the floor every client may assume; ircds have
// advertised more than that for two decades, and a persona-derived
// nick reads better with room to spell a word out.
const NickMaxLen = 30

// AnonymousNick is the origin every message on a `+a` channel is
// attributed to (RFC 2811 §4.2.1). It is reserved: no client may take
// it, or a client on an anonymous channel could impersonate the mask.
const AnonymousNick Nick = "anonymous"

// nickSpecials are the non-alphanumeric characters RFC 2812 §2.3.1
// admits in a nick. The first character must be a letter or one of
// these; later characters may also be digits or `-`.
const nickSpecials = `[]\` + "`_^{|}"

// NickRejection names why [ValidateNick] refused a nick. The zero
// value, [NickAccepted], means it did not.
type NickRejection int

const (
	// NickAccepted means the nick satisfies the grammar.
	NickAccepted NickRejection = iota
	// NickEmpty means the nick has no characters.
	NickEmpty
	// NickTooLong means the nick is longer than [NickMaxLen].
	NickTooLong
	// NickBadFirstCharacter means the nick starts with something
	// other than a letter or one of the RFC 2812 §2.3.1 specials.
	NickBadFirstCharacter
	// NickBadCharacter means the nick contains a character the
	// grammar does not admit anywhere.
	NickBadCharacter
	// NickReserved means the nick is one the server keeps for itself
	// (see [AnonymousNick]).
	NickReserved
)

func (r NickRejection) String() string {
	switch r {
	case NickAccepted:
		return "accepted"
	case NickEmpty:
		return "a nick cannot be empty"
	case NickTooLong:
		return "too long"
	case NickBadFirstCharacter:
		return "must start with a letter or one of " + nickSpecials
	case NickBadCharacter:
		return "may contain only letters, digits, `-` and " + nickSpecials
	case NickReserved:
		return "reserved by the server"
	}

	return "rejected"
}

// ValidateNick checks a nick against RFC 2812 §2.3.1: a letter or one
// of [nickSpecials] first, then any number of letters, digits,
// specials and `-`, up to [NickMaxLen] characters. [AnonymousNick] is
// refused whatever its case, since the casemapping makes every
// spelling of it the same nick.
//
// The result carries no timestamp: callers that surface the refusal
// on the wire wrap it in an [ErroneousNicknameError] and stamp it
// from their own clock.
func ValidateNick(n Nick) NickRejection {
	s := string(n)

	switch {
	case s == "":
		return NickEmpty
	case len(s) > NickMaxLen:
		return NickTooLong
	case EqualNick(n, AnonymousNick):
		return NickReserved
	}

	if !isNickLetter(s[0]) && !strings.ContainsRune(nickSpecials, rune(s[0])) {
		return NickBadFirstCharacter
	}

	for i := 1; i < len(s); i++ {
		c := s[i]
		if isNickLetter(c) || (c >= '0' && c <= '9') || c == '-' || strings.ContainsRune(nickSpecials, rune(c)) {
			continue
		}

		return NickBadCharacter
	}

	return NickAccepted
}

// isNickLetter reports whether c is an ASCII letter. The grammar is
// byte-oriented: a multi-byte UTF-8 sequence fails here on its first
// byte, which is the answer RFC 2812 gives for a non-ASCII nick.
func isNickLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// ChannelNameMaxLen is this server's CHANNELLEN (RFC 2812 §1.3),
// counting the prefix character.
const ChannelNameMaxLen = 50

// forbiddenChannelChars are the characters RFC 2812 §1.3 excludes
// from a channel name. Space, `,` and NUL are wire framing; `:`
// separates a name from a key in the JOIN parameter list; BEL is
// excluded because it is not printable.
const forbiddenChannelChars = " \a,:\x00"

// ChannelNameRejection names why [ValidateChannelName] refused a
// channel name. The zero value, [ChannelNameAccepted], means it did
// not.
type ChannelNameRejection int

const (
	// ChannelNameAccepted means the name satisfies the grammar.
	ChannelNameAccepted ChannelNameRejection = iota
	// ChannelNameMissingPrefix means the name does not start with a
	// channel prefix character.
	ChannelNameMissingPrefix
	// ChannelNameBare means the name is a prefix character and
	// nothing else.
	ChannelNameBare
	// ChannelNameTooLong means the name is longer than
	// [ChannelNameMaxLen].
	ChannelNameTooLong
	// ChannelNameBadCharacter means the name contains one of the
	// characters RFC 2812 §1.3 excludes.
	ChannelNameBadCharacter
)

func (r ChannelNameRejection) String() string {
	switch r {
	case ChannelNameAccepted:
		return "accepted"
	case ChannelNameMissingPrefix:
		return "must start with " + strings.Join(strings.Split(ChannelPrefixes, ""), " or ")
	case ChannelNameBare:
		return "a channel prefix on its own names no channel"
	case ChannelNameTooLong:
		return "too long"
	case ChannelNameBadCharacter:
		return "may not contain a space, a comma, a colon, BEL or NUL"
	}

	return "rejected"
}

// ValidateChannelName checks a channel name against RFC 2812 §1.3.
// Callers that mean to accept a bare name and let the server supply
// the prefix run [NormaliseChannelName] first.
func ValidateChannelName(ch ChannelName) ChannelNameRejection {
	s := string(ch)

	switch {
	case !HasChannelPrefix(ch):
		return ChannelNameMissingPrefix
	case len(s) == 1:
		return ChannelNameBare
	case len(s) > ChannelNameMaxLen:
		return ChannelNameTooLong
	case strings.ContainsAny(s[1:], forbiddenChannelChars):
		return ChannelNameBadCharacter
	}

	return ChannelNameAccepted
}
