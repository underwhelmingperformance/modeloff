// Package timestamp formats user-visible timestamps for the TUI.
package timestamp

import (
	"strings"
	"time"

	"github.com/KarpelesLab/strftime"
	locale "github.com/jeandeaual/go-locale"
	"golang.org/x/text/language"
)

var fallbackLocale = language.BritishEnglish

// DefaultFormat is the timestamp format a message line uses when no
// explicit [Format] override is configured: irssi's convention of a
// bare 24-hour clock. The date itself is not repeated on every line;
// it is carried by the day-change divider the message list draws
// whenever consecutive events fall on different calendar days.
const DefaultFormat = "%H:%M"

// dayChangedFormat is the format a day-change divider renders its
// date label in: the full weekday and month name, so the divider
// reads as a sentence rather than a numeric date stamp.
const dayChangedFormat = "%A %d %B %Y"

// CurrentLocale returns the current system locale, falling back to
// en-GB if detection fails or returns an unusable value.
func CurrentLocale() language.Tag {
	return DetectLocale(locale.GetLocale)
}

// DetectLocale converts the detected locale identifier into a BCP 47
// language tag. Invalid, empty, or POSIX-style fallback locales are
// treated as en-GB.
func DetectLocale(detect func() (string, error)) language.Tag {
	if detect == nil {
		return fallbackLocale
	}

	raw, err := detect()
	if err != nil {
		return fallbackLocale
	}

	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallbackLocale
	}

	if strings.EqualFold(raw, "C") || strings.EqualFold(raw, "POSIX") {
		return fallbackLocale
	}

	tag, err := language.Parse(raw)
	if err == nil {
		return tag
	}

	normalised := normaliseLocale(raw)
	tag, err = language.Parse(normalised)
	if err == nil {
		return tag
	}

	return fallbackLocale
}

// Format renders t according to the configured timestamp format and
// locale. A nil format means [DefaultFormat], while an explicit empty
// string disables timestamps.
func Format(t time.Time, format *string, locale language.Tag) string {
	if locale.IsRoot() {
		locale = fallbackLocale
	}

	if format == nil {
		return strftime.Format(locale, DefaultFormat, t)
	}

	if *format == "" {
		return ""
	}

	if strings.Contains(*format, "%") {
		return strftime.Format(locale, *format, t)
	}

	return t.Format(*format)
}

// FormatDate renders the calendar date t falls on, for a day-change
// divider: the full weekday and month name, locale-aware.
func FormatDate(t time.Time, locale language.Tag) string {
	if locale.IsRoot() {
		locale = fallbackLocale
	}

	return strftime.Format(locale, dayChangedFormat, t)
}

func normaliseLocale(raw string) string {
	normalised := raw

	if idx := strings.Index(normalised, "."); idx >= 0 {
		normalised = normalised[:idx]
	}

	if idx := strings.Index(normalised, "@"); idx >= 0 {
		normalised = normalised[:idx]
	}

	normalised = strings.ReplaceAll(normalised, "_", "-")

	return normalised
}
