package components_test

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

var testChannels = []domain.Channel{
	{Name: "#general", Kind: domain.KindChannel, Created: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
	{Name: "#random", Kind: domain.KindChannel, Created: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)},
	{Name: "#dev", Kind: domain.KindChannel, Created: time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC)},
}

func key(k string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

func ctrlKey(k string) tea.KeyMsg {
	switch k {
	case "ctrl+d":
		return tea.KeyMsg{Type: tea.KeyCtrlD}
	case "ctrl+u":
		return tea.KeyMsg{Type: tea.KeyCtrlU}
	case "ctrl+o":
		return tea.KeyMsg{Type: tea.KeyCtrlO}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
}

func updateSidebar(t *testing.T, m ui.Model, msg tea.Msg) (ui.Model, tea.Cmd) {
	t.Helper()

	return m.Update(msg)
}

func TestSidebar_View_shows_channels(t *testing.T) {
	s := components.NewSidebar(testChannels, "#general", nil)
	v := s.View(20, 10)

	require.Contains(t, v, "#general")
	require.Contains(t, v, "#random")
	require.Contains(t, v, "#dev")
}

func TestSidebar_View_empty(t *testing.T) {
	s := components.NewSidebar(nil, "", nil)
	v := s.View(20, 10)

	require.Contains(t, v, "No channels")
}

func TestSidebar_View_active_channel_highlighted(t *testing.T) {
	s := components.NewSidebar(testChannels, "#random", nil)
	v := s.View(30, 10)

	require.Contains(t, v, "▸")
	require.Contains(t, v, "#random")
}

func TestSidebar_keyboard_navigation(t *testing.T) {
	s := components.NewSidebar(testChannels, "#general", nil)
	var m ui.Model = s

	// Move down.
	m, _ = updateSidebar(t, m, ctrlKey("ctrl+d"))
	m, _ = updateSidebar(t, m, ctrlKey("ctrl+d"))

	// Select with ctrl+o — should select #dev (index 2).
	_, cmd := updateSidebar(t, m, ctrlKey("ctrl+o"))

	require.NotNil(t, cmd)

	msg := cmd()
	selected, ok := msg.(components.ChannelSelectedMsg)
	require.True(t, ok, "expected ChannelSelectedMsg, got %T", msg)
	require.Equal(t, domain.ChannelName("#dev"), selected.Channel)
}

func TestSidebar_keyboard_up(t *testing.T) {
	s := components.NewSidebar(testChannels, "#dev", nil)
	var m ui.Model = s

	// Cursor starts at #dev (index 2). Move up twice.
	m, _ = updateSidebar(t, m, ctrlKey("ctrl+u"))
	m, _ = updateSidebar(t, m, ctrlKey("ctrl+u"))

	// Select — should be #general (index 0).
	_, cmd := updateSidebar(t, m, ctrlKey("ctrl+o"))

	require.NotNil(t, cmd)

	msg := cmd()
	selected, ok := msg.(components.ChannelSelectedMsg)
	require.True(t, ok, "expected ChannelSelectedMsg, got %T", msg)
	require.Equal(t, domain.ChannelName("#general"), selected.Channel)
}

func TestSidebar_cursor_clamps_at_boundaries(t *testing.T) {
	s := components.NewSidebar(testChannels, "#general", nil)
	var m ui.Model = s

	// Move up past the top — should stay at 0.
	m, _ = updateSidebar(t, m, ctrlKey("ctrl+u"))
	m, _ = updateSidebar(t, m, ctrlKey("ctrl+u"))

	_, cmd := updateSidebar(t, m, ctrlKey("ctrl+o"))
	msg := cmd()
	selected, ok := msg.(components.ChannelSelectedMsg)
	require.True(t, ok, "expected ChannelSelectedMsg, got %T", msg)
	require.Equal(t, domain.ChannelName("#general"), selected.Channel)
}

func TestSidebar_cursor_clamps_at_bottom(t *testing.T) {
	s := components.NewSidebar(testChannels, "#general", nil)
	var m ui.Model = s

	// Move down past the bottom.
	for i := 0; i < 10; i++ {
		m, _ = updateSidebar(t, m, ctrlKey("ctrl+d"))
	}

	_, cmd := updateSidebar(t, m, ctrlKey("ctrl+o"))
	msg := cmd()
	selected, ok := msg.(components.ChannelSelectedMsg)
	require.True(t, ok, "expected ChannelSelectedMsg, got %T", msg)
	require.Equal(t, domain.ChannelName("#dev"), selected.Channel)
}

func TestSidebar_mouse_click_selects_channel(t *testing.T) {
	s := components.NewSidebar(testChannels, "#general", nil)
	var m ui.Model = s
	m, _ = updateSidebar(t, m, ui.BoundsMsg{Rect: ui.Rect{X: 0, Y: 0, Width: 20, Height: 10}})

	// Click on the second channel (Y=1).
	_, cmd := updateSidebar(t, m, tea.MouseMsg{
		X:      5,
		Y:      1,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})

	require.NotNil(t, cmd)

	msg := cmd()
	selected, ok := msg.(components.ChannelSelectedMsg)
	require.True(t, ok, "expected ChannelSelectedMsg, got %T", msg)
	require.Equal(t, domain.ChannelName("#random"), selected.Channel)
}

func TestSidebar_mouse_click_out_of_range(t *testing.T) {
	s := components.NewSidebar(testChannels, "#general", nil)
	var m ui.Model = s
	m, _ = updateSidebar(t, m, ui.BoundsMsg{Rect: ui.Rect{X: 0, Y: 0, Width: 20, Height: 10}})

	// Click below the channel list.
	_, cmd := updateSidebar(t, m, tea.MouseMsg{
		X:      5,
		Y:      10,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})

	require.Nil(t, cmd)
}

func TestSidebar_ctrl_o_with_no_channels(t *testing.T) {
	s := components.NewSidebar(nil, "", nil)
	var m ui.Model = s

	_, cmd := updateSidebar(t, m, ctrlKey("ctrl+o"))
	require.Nil(t, cmd)
}

func TestSidebar_channels_updated_msg(t *testing.T) {
	s := components.NewSidebar(testChannels, "#general", nil)
	var m ui.Model = s

	newChannels := []domain.Channel{
		{Name: "#alpha", Kind: domain.KindChannel},
		{Name: "#beta", Kind: domain.KindChannel},
	}

	m, _ = updateSidebar(t, m, components.ChannelsUpdatedMsg{
		Channels: newChannels,
		Active:   "#beta",
		Unread:   map[domain.ChannelName]int{"#alpha": 5},
	})

	v := m.View(30, 10)
	require.Contains(t, v, "#alpha")
	require.Contains(t, v, "#beta")
	require.NotContains(t, v, "#general")

	// Unread count from the message is applied.
	require.Contains(t, v, "#alpha (5)")
}

func TestSidebar_unread_indicator(t *testing.T) {
	tests := []struct {
		name     string
		count    int
		wantText string
	}{
		{"single unread", 1, "#random (1)"},
		{"several unread", 5, "#random (5)"},
		{"many unread", 99, "#random (99)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unread := map[domain.ChannelName]int{
				"#random": tt.count,
			}

			s := components.NewSidebar(testChannels, "#general", unread)
			v := s.View(30, 10)

			require.Contains(t, v, tt.wantText)
			require.NotContains(t, v, "#general (")
			require.NotContains(t, v, "#dev (")
		})
	}
}

func TestSidebar_no_unread_indicator_when_nil(t *testing.T) {
	s := components.NewSidebar(testChannels, "#general", nil)
	v := s.View(30, 10)

	require.NotContains(t, v, "(")
}

func TestSidebar_dm_shows_at_prefix(t *testing.T) {
	channels := []domain.Channel{
		{Name: "#general", Kind: domain.KindChannel},
		{Name: "botty", Kind: domain.KindDM},
	}

	s := components.NewSidebar(channels, "#general", nil)
	v := s.View(30, 10)

	require.Contains(t, v, "#general")
	require.Contains(t, v, "@botty")
	require.NotContains(t, v, "@#general")
}

func TestSidebar_dm_cursor_uses_dm_style(t *testing.T) {
	channels := []domain.Channel{
		{Name: "#general", Kind: domain.KindChannel},
		{Name: "botty", Kind: domain.KindDM},
	}

	s := components.NewSidebar(channels, "#general", nil)
	var m ui.Model = s

	// Move cursor to the DM entry.
	m, _ = updateSidebar(t, m, ctrlKey("ctrl+d"))

	v := m.View(30, 10)
	require.Contains(t, v, "@botty")
}

func TestSidebar_cursor_follows_active_on_update(t *testing.T) {
	s := components.NewSidebar(testChannels, "#general", nil)
	var m ui.Model = s

	// Move cursor to #dev (index 2).
	m, _ = updateSidebar(t, m, ctrlKey("ctrl+d"))
	m, _ = updateSidebar(t, m, ctrlKey("ctrl+d"))

	// Receive an update that changes the active channel to #random.
	m, _ = updateSidebar(t, m, components.ChannelsUpdatedMsg{
		Channels: testChannels,
		Active:   "#random",
	})

	// Cursor should have moved to #random. Pressing ctrl+o should
	// select it.
	_, cmd := updateSidebar(t, m, ctrlKey("ctrl+o"))

	require.NotNil(t, cmd)

	msg := cmd()
	selected, ok := msg.(components.ChannelSelectedMsg)
	require.True(t, ok, "expected ChannelSelectedMsg, got %T", msg)
	require.Equal(t, domain.ChannelName("#random"), selected.Channel)
}

func TestSidebar_cursor_clamps_when_active_not_in_list(t *testing.T) {
	s := components.NewSidebar(testChannels, "#general", nil)
	var m ui.Model = s

	// Move cursor to #dev (index 2).
	m, _ = updateSidebar(t, m, ctrlKey("ctrl+d"))
	m, _ = updateSidebar(t, m, ctrlKey("ctrl+d"))

	// Update with a list that doesn't contain the active channel.
	newChannels := []domain.Channel{
		{Name: "#alpha", Kind: domain.KindChannel},
	}

	m, _ = updateSidebar(t, m, components.ChannelsUpdatedMsg{
		Channels: newChannels,
		Active:   "#gone",
	})

	// Cursor should clamp to the only available index (0).
	_, cmd := updateSidebar(t, m, ctrlKey("ctrl+o"))

	require.NotNil(t, cmd)

	msg := cmd()
	selected, ok := msg.(components.ChannelSelectedMsg)
	require.True(t, ok, "expected ChannelSelectedMsg, got %T", msg)
	require.Equal(t, domain.ChannelName("#alpha"), selected.Channel)
}

func TestSidebar_ignores_other_messages(t *testing.T) {
	s := components.NewSidebar(testChannels, "#general", nil)
	var m ui.Model = s

	m, cmd := updateSidebar(t, m, key("x"))
	require.Nil(t, cmd)

	v := m.View(20, 10)
	require.Contains(t, v, "#general")
}
