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

func newTestChannelSidebar(channels []domain.Channel, active domain.ChannelName, unread map[domain.ChannelName]int) ui.Model {
	cl := components.NewChannelSidebar()
	m, _ := cl.Update(components.SetChannelsMsg{
		Channels: channels,
		Active:   active,
		Unread:   unread,
	})

	return m
}

// activateAndGetChannel sends a key and extracts the ChannelSelectedMsg
// from the returned Cmd.
func activateAndGetChannel(t *testing.T, m ui.Model, msg tea.Msg) (ui.Model, domain.ChannelName) {
	t.Helper()

	m, cmd := m.Update(msg)
	require.NotNil(t, cmd)

	selectMsg := cmd()
	sel, ok := selectMsg.(components.ChannelSelectedMsg)
	require.True(t, ok, "expected ChannelSelectedMsg, got %T", selectMsg)

	return m, sel.Channel
}

func TestChannelSidebar_View_shows_channels(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#general", nil)
	v := m.View(20, 10)

	require.Contains(t, v, "#general")
	require.Contains(t, v, "#random")
	require.Contains(t, v, "#dev")
}

func TestChannelSidebar_View_empty(t *testing.T) {
	m := newTestChannelSidebar(nil, "", nil)
	v := m.View(20, 10)

	require.Contains(t, v, "No channels")
}

func TestChannelSidebar_View_active_channel_highlighted(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#random", nil)
	v := m.View(30, 10)

	require.Contains(t, v, "#random")
}

func TestChannelSidebar_keyboard_navigation(t *testing.T) {
	// Sorted order: #dev, #general, #random. Active #general = index 1.
	m := newTestChannelSidebar(testChannels, "#general", nil)

	// Down once → #random (index 2).
	m, _ = m.Update(ctrlKey("ctrl+d"))

	_, ch := activateAndGetChannel(t, m, ctrlKey("ctrl+o"))
	require.Equal(t, domain.ChannelName("#random"), ch)
}

func TestChannelSidebar_keyboard_up(t *testing.T) {
	// Sorted order: #dev, #general, #random. Active #random = index 2.
	m := newTestChannelSidebar(testChannels, "#random", nil)

	// Up twice → #dev (index 0).
	m, _ = m.Update(ctrlKey("ctrl+u"))
	m, _ = m.Update(ctrlKey("ctrl+u"))

	_, ch := activateAndGetChannel(t, m, ctrlKey("ctrl+o"))
	require.Equal(t, domain.ChannelName("#dev"), ch)
}

func TestChannelSidebar_cursor_clamps_at_boundaries(t *testing.T) {
	// Sorted order: #dev, #general, #random. Active #dev = index 0.
	m := newTestChannelSidebar(testChannels, "#dev", nil)

	// Up past the top — should stay at #dev.
	m, _ = m.Update(ctrlKey("ctrl+u"))
	m, _ = m.Update(ctrlKey("ctrl+u"))

	_, ch := activateAndGetChannel(t, m, ctrlKey("ctrl+o"))
	require.Equal(t, domain.ChannelName("#dev"), ch)
}

func TestChannelSidebar_cursor_clamps_at_bottom(t *testing.T) {
	// Sorted order: #dev, #general, #random.
	m := newTestChannelSidebar(testChannels, "#dev", nil)

	for range 10 {
		m, _ = m.Update(ctrlKey("ctrl+d"))
	}

	_, ch := activateAndGetChannel(t, m, ctrlKey("ctrl+o"))
	require.Equal(t, domain.ChannelName("#random"), ch)
}

func TestChannelSidebar_mouse_click_selects_channel(t *testing.T) {
	// Sorted order: #dev (row 0+header), #general (row 1+header), #random (row 2+header).
	// Header takes 1 row, so Y=2 is index 1 = #general.
	m := newTestChannelSidebar(testChannels, "#dev", nil)
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{X: 0, Y: 0, Width: 20, Height: 10}})

	_, ch := activateAndGetChannel(t, m, tea.MouseMsg{
		X:      5,
		Y:      2,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})

	require.Equal(t, domain.ChannelName("#general"), ch)
}

func TestChannelSidebar_mouse_click_out_of_range(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#general", nil)
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{X: 0, Y: 0, Width: 20, Height: 10}})

	_, cmd := m.Update(tea.MouseMsg{
		X:      5,
		Y:      10,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})

	require.Nil(t, cmd)
}

func TestChannelSidebar_ctrl_o_with_no_channels(t *testing.T) {
	m := newTestChannelSidebar(nil, "", nil)

	_, cmd := m.Update(ctrlKey("ctrl+o"))
	require.Nil(t, cmd)
}

func TestChannelSidebar_set_channels_msg(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#general", nil)

	m, _ = m.Update(components.SetChannelsMsg{
		Channels: []domain.Channel{
			{Name: "#alpha", Kind: domain.KindChannel},
			{Name: "#beta", Kind: domain.KindChannel},
		},
		Active: "#beta",
		Unread: map[domain.ChannelName]int{"#alpha": 5},
	})

	v := m.View(30, 10)
	require.Contains(t, v, "#alpha")
	require.Contains(t, v, "#beta")
	require.NotContains(t, v, "#general")
	require.Contains(t, v, "#alpha (5)")
}

func TestChannelSidebar_unread_indicator(t *testing.T) {
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

			m := newTestChannelSidebar(testChannels, "#general", unread)
			v := m.View(30, 10)

			require.Contains(t, v, tt.wantText)
			require.NotContains(t, v, "#general (")
			require.NotContains(t, v, "#dev (")
		})
	}
}

func TestChannelSidebar_no_unread_indicator_when_nil(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#general", nil)
	v := m.View(30, 10)

	require.NotContains(t, v, "(")
}

func TestChannelSidebar_dm_shows_at_prefix(t *testing.T) {
	channels := []domain.Channel{
		{Name: "#general", Kind: domain.KindChannel},
		{Name: "botty", Kind: domain.KindDM},
	}

	m := newTestChannelSidebar(channels, "#general", nil)
	v := m.View(30, 10)

	require.Contains(t, v, "#general")
	require.Contains(t, v, "@botty")
	require.NotContains(t, v, "@#general")
}

func TestChannelSidebar_dm_cursor_uses_dm_style(t *testing.T) {
	channels := []domain.Channel{
		{Name: "#general", Kind: domain.KindChannel},
		{Name: "botty", Kind: domain.KindDM},
	}

	m := newTestChannelSidebar(channels, "#general", nil)
	m, _ = m.Update(ctrlKey("ctrl+d"))

	v := m.View(30, 10)
	require.Contains(t, v, "@botty")
}

func TestChannelSidebar_cursor_follows_active_on_set_channels(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#general", nil)

	m, _ = m.Update(ctrlKey("ctrl+d"))
	m, _ = m.Update(ctrlKey("ctrl+d"))

	m, _ = m.Update(components.SetChannelsMsg{
		Channels: testChannels,
		Active:   "#random",
	})

	_, ch := activateAndGetChannel(t, m, ctrlKey("ctrl+o"))
	require.Equal(t, domain.ChannelName("#random"), ch)
}

func TestChannelSidebar_cursor_clamps_when_active_not_in_list(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#general", nil)

	m, _ = m.Update(ctrlKey("ctrl+d"))
	m, _ = m.Update(ctrlKey("ctrl+d"))

	m, _ = m.Update(components.SetChannelsMsg{
		Channels: []domain.Channel{
			{Name: "#alpha", Kind: domain.KindChannel},
		},
		Active: "#gone",
	})

	_, ch := activateAndGetChannel(t, m, ctrlKey("ctrl+o"))
	require.Equal(t, domain.ChannelName("#alpha"), ch)
}

func TestChannelSidebar_mouse_wheel_moves_cursor_without_activating(t *testing.T) {
	// Sorted order: #dev, #general, #random. Active #dev = index 0.
	m := newTestChannelSidebar(testChannels, "#dev", nil)
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{X: 0, Y: 0, Width: 20, Height: 10}})

	// Scroll down — should move cursor but NOT activate (no cmd).
	m, cmd := m.Update(tea.MouseMsg{
		X:      5,
		Y:      2,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonWheelDown,
	})

	require.Nil(t, cmd, "wheel scroll should not activate a channel")

	// Ctrl+O to confirm cursor moved to #general.
	_, ch := activateAndGetChannel(t, m, ctrlKey("ctrl+o"))
	require.Equal(t, domain.ChannelName("#general"), ch)
}

func TestChannelSidebar_ignores_other_messages(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#general", nil)

	m, cmd := m.Update(key("x"))
	require.Nil(t, cmd)

	v := m.View(20, 10)
	require.Contains(t, v, "#general")
}
