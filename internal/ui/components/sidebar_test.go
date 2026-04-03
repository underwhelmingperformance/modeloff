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
	s := components.NewSidebar(testChannels, "#general")
	v := s.View(20, 10)

	require.Contains(t, v, "#general")
	require.Contains(t, v, "#random")
	require.Contains(t, v, "#dev")
}

func TestSidebar_View_empty(t *testing.T) {
	s := components.NewSidebar(nil, "")
	v := s.View(20, 10)

	require.Contains(t, v, "No channels")
}

func TestSidebar_View_active_channel_highlighted(t *testing.T) {
	s := components.NewSidebar(testChannels, "#random")
	v := s.View(30, 10)

	require.Contains(t, v, "▸")
	require.Contains(t, v, "#random")
}

func TestSidebar_keyboard_navigation(t *testing.T) {
	s := components.NewSidebar(testChannels, "#general")
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
	s := components.NewSidebar(testChannels, "#dev")
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
	s := components.NewSidebar(testChannels, "#general")
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
	s := components.NewSidebar(testChannels, "#general")
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
	s := components.NewSidebar(testChannels, "#general")
	var m ui.Model = s

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
	s := components.NewSidebar(testChannels, "#general")
	var m ui.Model = s

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
	s := components.NewSidebar(nil, "")
	var m ui.Model = s

	_, cmd := updateSidebar(t, m, ctrlKey("ctrl+o"))
	require.Nil(t, cmd)
}

func TestSidebar_channels_updated_msg(t *testing.T) {
	s := components.NewSidebar(testChannels, "#general")
	var m ui.Model = s

	newChannels := []domain.Channel{
		{Name: "#alpha", Kind: domain.KindChannel},
		{Name: "#beta", Kind: domain.KindChannel},
	}

	m, _ = updateSidebar(t, m, components.ChannelsUpdatedMsg{
		Channels: newChannels,
		Active:   "#beta",
	})

	v := m.View(20, 10)
	require.Contains(t, v, "#alpha")
	require.Contains(t, v, "#beta")
	require.NotContains(t, v, "#general")
}

func TestSidebar_ignores_other_messages(t *testing.T) {
	s := components.NewSidebar(testChannels, "#general")
	var m ui.Model = s

	m, cmd := updateSidebar(t, m, key("x"))
	require.Nil(t, cmd)

	v := m.View(20, 10)
	require.Contains(t, v, "#general")
}
