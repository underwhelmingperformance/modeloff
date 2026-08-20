package timestamp

import (
	"fmt"
	"testing"
	"time"

	"github.com/KarpelesLab/strftime"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/language"
)

func TestDetectLocale_passes_through_detected_tags(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "american_english", raw: "en-US"},
		{name: "british_english", raw: "en-GB"},
		{name: "chinese", raw: "zh-CN"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectLocale(func() (string, error) { return tt.raw, nil })
			require.Equal(t, language.Make(tt.raw), got)
		})
	}
}

func TestDetectLocale_falls_back_to_british_english(t *testing.T) {
	tests := []struct {
		name   string
		detect func() (string, error)
	}{
		{name: "empty", detect: func() (string, error) { return "", nil }},
		{name: "c", detect: func() (string, error) { return "C", nil }},
		{name: "posix", detect: func() (string, error) { return "POSIX", nil }},
		{name: "error", detect: func() (string, error) { return "", fmt.Errorf("boom") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectLocale(tt.detect)
			require.Equal(t, language.BritishEnglish, got)
		})
	}
}

func TestFormat_uses_bare_24_hour_clock_when_unset(t *testing.T) {
	ref := time.Date(2026, 4, 7, 9, 30, 0, 0, time.UTC)

	got := Format(ref, nil, language.BritishEnglish)

	require.Equal(t, "09:30", got, "the unset default is a bare HH:MM, not a full locale timestamp; the date is carried by the day-change divider instead")
}

func TestFormat_supports_custom_strftime_and_go_layouts(t *testing.T) {
	ref := time.Date(2026, 4, 7, 9, 30, 0, 0, time.UTC)
	strf := "%X"
	gofmt := "2006-01-02 15:04"

	require.Equal(t, strftime.Format(language.BritishEnglish, "%X", ref), Format(ref, &strf, language.BritishEnglish))
	require.Equal(t, "2026-04-07 09:30", Format(ref, &gofmt, language.BritishEnglish))
}

func TestFormat_can_disable_timestamps(t *testing.T) {
	ref := time.Date(2026, 4, 7, 9, 30, 0, 0, time.UTC)
	disabled := ""

	require.Equal(t, "", Format(ref, &disabled, language.BritishEnglish))
}

func TestFormatDate_renders_weekday_and_month_name(t *testing.T) {
	ref := time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC)

	got := FormatDate(ref, language.BritishEnglish)

	require.Equal(t, strftime.Format(language.BritishEnglish, "%A %d %B %Y", ref), got)
}

func TestFormatDate_falls_back_to_british_english_for_root_locale(t *testing.T) {
	ref := time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC)

	got := FormatDate(ref, language.Und)

	require.Equal(t, strftime.Format(language.BritishEnglish, "%A %d %B %Y", ref), got)
}
