package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStatusWindow_accessors(t *testing.T) {
	created := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)

	w := NewStatusWindow(created)

	require.Equal(t, StatusChannelName, w.Name())
	require.Equal(t, created, w.Created())
	require.Equal(t, KindStatus, w.Kind())
	require.Equal(t, string(StatusChannelName), w.DisplayName())
}

func TestChannelWindow_accessors(t *testing.T) {
	created := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)

	w := NewChannelWindow("general", created)

	require.Equal(t, ChannelName("#general"), w.Name())
	require.Equal(t, created, w.Created())
	require.Equal(t, KindChannel, w.Kind())
	require.Equal(t, "#general", w.DisplayName())
	require.Equal(t, 0, w.Members.Len())
}

func TestChannelWindow_normalises_bare_name(t *testing.T) {
	w := NewChannelWindow("general", time.Time{})

	require.Equal(t, ChannelName("#general"), w.Name())
}

func TestChannelWindow_keeps_prefixed_name(t *testing.T) {
	w := NewChannelWindow("#general", time.Time{})

	require.Equal(t, ChannelName("#general"), w.Name())
}

func TestDMWindow_accessors(t *testing.T) {
	created := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	bot := NewModelInstance("id-1", "botty", "anthropic/claude-3-haiku", "", nil)

	w := NewDMWindow(bot, created)

	require.Equal(t, ChannelName("id-1"), w.Name())
	require.Equal(t, created, w.Created())
	require.Equal(t, KindDM, w.Kind())
	require.Equal(t, "botty", w.DisplayName())
	require.Same(t, bot, w.Counterpart)
}

func TestDMWindow_DisplayName_tracks_counterpart_rename(t *testing.T) {
	bot := NewModelInstance("id-1", "botty", "anthropic/claude-3-haiku", "", nil)
	w := NewDMWindow(bot, time.Time{})

	require.Equal(t, "botty", w.DisplayName())

	bot.SetNick("foobar")

	require.Equal(t, "foobar", w.DisplayName())
	require.Equal(t, ChannelName("id-1"), w.Name())
}

func TestWindow_interface_satisfaction(t *testing.T) {
	bot := NewModelInstance("id-1", "botty", "anthropic/claude-3-haiku", "", nil)

	windows := []Window{
		NewStatusWindow(time.Time{}),
		NewChannelWindow("general", time.Time{}),
		NewDMWindow(bot, time.Time{}),
	}

	kinds := make([]ChannelKind, 0, len(windows))
	for _, w := range windows {
		kinds = append(kinds, w.Kind())
	}

	require.Equal(t, []ChannelKind{KindStatus, KindChannel, KindDM}, kinds)
}
