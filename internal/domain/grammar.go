package domain

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

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

	if !isASCIILetter(s[0]) && !strings.ContainsRune(nickSpecials, rune(s[0])) {
		return NickBadFirstCharacter
	}

	for i := 1; i < len(s); i++ {
		c := s[i]
		if isASCIILetter(c) || (c >= '0' && c <= '9') || c == '-' || strings.ContainsRune(nickSpecials, rune(c)) {
			continue
		}

		return NickBadCharacter
	}

	return NickAccepted
}

// isASCIILetter reports whether c is an ASCII letter. The nick and
// memory-key grammars are both byte-oriented: a multi-byte UTF-8
// sequence fails here on its first byte, which is the answer RFC 2812
// gives for a non-ASCII nick.
func isASCIILetter(c byte) bool {
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

// PersonaMaxLen bounds a persona description. A persona is one line
// describing an IRC regular, which is what the app asks the small
// model for and what the `/config persona` help text describes.
const PersonaMaxLen = 400

// PersonaRejection names why [ValidatePersona] refused a persona
// description. The zero value, [PersonaAccepted], means it did not.
type PersonaRejection int

const (
	// PersonaAccepted means the description is within the bound.
	PersonaAccepted PersonaRejection = iota
	// PersonaTooLong means the description is longer than
	// [PersonaMaxLen].
	PersonaTooLong
	// PersonaControlCharacter means the description contains a
	// control character.
	PersonaControlCharacter
)

func (r PersonaRejection) String() string {
	switch r {
	case PersonaAccepted:
		return "accepted"
	case PersonaTooLong:
		return "too long"
	case PersonaControlCharacter:
		return "may not contain control characters"
	}

	return "rejected"
}

// ValidatePersona bounds a persona description. An empty description
// is accepted: a persona is optional, and an instance without one
// carries no persona line at all.
//
// A persona reaches the model as lower-authority instance state. The
// length bound keeps it to the one line it is meant to be, and
// refusing control characters prevents it from laying out a document
// that imitates the surrounding record. This is structural validation;
// it does not make the persona semantically trusted.
func ValidatePersona(description string) PersonaRejection {
	if len(description) > PersonaMaxLen {
		return PersonaTooLong
	}

	for i := range len(description) {
		if description[i] < 0x20 || description[i] == 0x7f {
			return PersonaControlCharacter
		}
	}

	return PersonaAccepted
}

// TopicMaxLen is this server's TOPICLEN. RFC 1459 and RFC 2812 place
// no bound on a channel topic, but TOPICLEN is the convention modern
// ircds advertise via ISUPPORT; 390 is a typical value among them. A
// channel's topic is repeated into every dispatch turn's prompt for
// that channel, so leaving it unbounded would let it grow into an
// open-ended transcript cost.
const TopicMaxLen = 390

// TopicRejection names why [ValidateTopic] refused a topic. The zero
// value, [TopicAccepted], means it did not.
type TopicRejection int

const (
	// TopicAccepted means the topic is within [TopicMaxLen].
	TopicAccepted TopicRejection = iota
	// TopicTooLong means the topic is longer than [TopicMaxLen].
	TopicTooLong
)

func (r TopicRejection) String() string {
	switch r {
	case TopicAccepted:
		return "accepted"
	case TopicTooLong:
		return "too long"
	}

	return "rejected"
}

// ValidateTopic bounds a channel topic to [TopicMaxLen]. An empty
// topic is accepted: `/topic` with no body clears the channel's
// topic.
func ValidateTopic(topic string) TopicRejection {
	if len(topic) > TopicMaxLen {
		return TopicTooLong
	}

	return TopicAccepted
}

// MemoryKeyMaxLen bounds a memory's key. The key addresses the
// memory: it is what `write_memory` overwrites under and what
// `delete_memory` names, and it is rendered beside the content in the
// memory line of every prompt. Sixty-four bytes holds a multi-word
// snake_case identifier such as `preferred_editor` several times
// over, while keeping the key far too small to carry the fact itself
// past [MemoryContentMaxLen].
const MemoryKeyMaxLen = 64

// MemoryContentMaxLen bounds a memory's content in characters, so the
// bound is the same whatever script it is written in. A memory holds one
// fact, which is the same amount of text [PersonaMaxLen] allows a
// persona description, an experience summary or a tendency. The two
// bounds are equal by coincidence of what one sentence needs, not
// because either derives from the other.
const MemoryContentMaxLen = 400

// memoryKeySpecials are the non-alphanumeric characters a memory key
// may contain. They are the separators an identifier is written with;
// everything else, including whitespace and the brackets the memory
// line delimits an entry with, is refused.
const memoryKeySpecials = "_-."

// MemoryRejection names why [ValidateMemory] refused a memory. The
// zero value, [MemoryAccepted], means it did not.
type MemoryRejection int

const (
	// MemoryAccepted means the key and content both satisfy the
	// grammar.
	MemoryAccepted MemoryRejection = iota
	// MemoryKeyEmpty means the key has no characters, so it
	// addresses no memory.
	MemoryKeyEmpty
	// MemoryKeyTooLong means the key is longer than
	// [MemoryKeyMaxLen].
	MemoryKeyTooLong
	// MemoryKeyBadCharacter means the key contains a character
	// outside the identifier grammar.
	MemoryKeyBadCharacter
	// MemoryContentEmpty means the content has no characters, so the
	// memory records no fact.
	MemoryContentEmpty
	// MemoryContentTooLong means the content is longer than
	// [MemoryContentMaxLen].
	MemoryContentTooLong
	// MemoryContentControlCharacter means the content contains a
	// control character.
	MemoryContentControlCharacter
)

func (r MemoryRejection) String() string {
	switch r {
	case MemoryAccepted:
		return "accepted"
	case MemoryKeyEmpty:
		return "a memory key cannot be empty"
	case MemoryKeyTooLong:
		return "the key is too long"
	case MemoryKeyBadCharacter:
		return "the key may contain only letters, digits and " + memoryKeySpecials
	case MemoryContentEmpty:
		return "a memory cannot be empty"
	case MemoryContentTooLong:
		return "the content is too long"
	case MemoryContentControlCharacter:
		return "the content may not contain control characters"
	}

	return "rejected"
}

// ValidateMemory bounds one memory an instance writes about itself.
// The key must be a non-empty identifier within [MemoryKeyMaxLen],
// and the content a non-empty single line within
// [MemoryContentMaxLen].
//
// A memory reaches the model as lower-authority instance state, in
// the same position a persona description does, so it carries the
// same structural bounds for the same reasons: the length keeps one
// memory to the one fact it is meant to hold, and refusing control
// characters prevents it from laying out a document that imitates the
// surrounding record. This is structural validation; it does not make
// a stored memory semantically trusted.
func ValidateMemory(key string, content string) MemoryRejection {
	switch {
	case key == "":
		return MemoryKeyEmpty
	case len(key) > MemoryKeyMaxLen:
		return MemoryKeyTooLong
	}

	for i := range len(key) {
		c := key[i]
		if isASCIILetter(c) || (c >= '0' && c <= '9') || strings.ContainsRune(memoryKeySpecials, rune(c)) {
			continue
		}

		return MemoryKeyBadCharacter
	}

	switch {
	case content == "":
		return MemoryContentEmpty
	case utf8.RuneCountInString(content) > MemoryContentMaxLen:
		return MemoryContentTooLong
	}

	// Every Unicode control, not only the ASCII ones. U+0085, U+2028 and
	// U+2029 all end a line for a renderer, so admitting them would let a
	// memory lay out a document that imitates the record around it.
	for _, r := range content {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return MemoryContentControlCharacter
		}
	}

	return MemoryAccepted
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
