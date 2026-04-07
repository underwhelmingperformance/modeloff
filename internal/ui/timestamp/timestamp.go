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

// Format renders t according to the configured timestamp format and locale.
// Nil format means locale-aware %c formatting, while an explicit empty
// string disables timestamps.
func Format(t time.Time, format *string, locale language.Tag) string {
	if locale.IsRoot() {
		locale = fallbackLocale
	}

	if format == nil {
		return strftime.Format(locale, "%c", t)
	}

	if *format == "" {
		return ""
	}

	if strings.Contains(*format, "%") {
		return strftime.Format(locale, *format, t)
	}

	return t.Format(*format)
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
