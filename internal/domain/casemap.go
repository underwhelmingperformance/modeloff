package domain

// Casemapping is how the server decides whether two nicks, or two
// channel names, name the same thing. RFC 2812 §2.2 requires the
// comparison to be case-insensitive; this server uses the `ascii`
// casemapping, in which only `A`-`Z` fold to `a`-`z`.
//
// The `rfc1459` casemapping additionally folds `[]\~` onto `{}|^`,
// which would make `foo[1]` and `foo{1}` the same nick. Those
// characters are ordinary nick characters here (see [ValidateNick]),
// so folding them would take names away from clients for no gain.
// Unicode folding is not an option either: it is locale-sensitive and
// would make the answer depend on the Go version's case tables.
//
// The `ascii` fold is also exactly what SQLite's NOCASE collation
// does, so a case-insensitive store lookup is an index seek and the
// store never has to read a row to decide whether it matches.

// CaseFold returns s with every ASCII upper-case letter lowered.
// Bytes outside `A`-`Z`, including every byte of a multi-byte UTF-8
// sequence, are copied through unchanged. This is the server's one
// fold rule (see the package comment above); EqualNick and
// KeyForChannel apply it to nicks and channel names, and
// command.Set's name resolution applies it to command and subcommand
// names for the same reason — an exact-byte comparison would treat
// `/CONFIG` and `/config` as different commands.
func CaseFold(s string) string {
	var b []byte

	for i := range len(s) {
		c := s[i]
		if c < 'A' || c > 'Z' {
			continue
		}

		if b == nil {
			b = []byte(s)
		}

		b[i] = c + ('a' - 'A')
	}

	if b == nil {
		return s
	}

	return string(b)
}

// EqualNick reports whether two nicks name the same client under the
// server's casemapping.
func EqualNick(a, b Nick) bool {
	return CaseFold(string(a)) == CaseFold(string(b))
}

// ChannelKey is a channel name reduced to its casemapped form. It is
// the key under which the session files a channel, so `#Dev` and
// `#dev` reach the same record. The display name stays on
// [ChannelWindow.Name]: that is the spelling the channel was created
// with, and what the server puts on the wire. A `ChannelKey` is
// never rendered.
type ChannelKey string

// KeyForChannel returns the key `name` files under.
func KeyForChannel(name ChannelName) ChannelKey {
	return ChannelKey(CaseFold(string(name)))
}

// EqualChannel reports whether two channel names name the same
// channel under the server's casemapping.
func EqualChannel(a, b ChannelName) bool {
	return KeyForChannel(a) == KeyForChannel(b)
}
