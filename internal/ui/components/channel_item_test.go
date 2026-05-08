package components_test

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

var testChannels = []domain.Window{
	domain.NewChannelWindow("#general", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)),
	domain.NewChannelWindow("#random", time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)),
	domain.NewChannelWindow("#dev", time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC)),
}

// dmStub builds a DM window addressed by a synthetic instance id
// derived from the counterpart nick. Test fixtures need a stable
// id-shaped name and a counterpart whose `Nick()` matches what
// the sidebar renders.
func dmStub(nick domain.Nick, created time.Time) *domain.DMWindow {
	counterpart := domain.NewModelInstance(
		domain.InstanceID("stub-"+string(nick)),
		nick,
		"test/model",
		"",
		nil,
	)
	return domain.NewDMWindow(counterpart, created)
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

func newTestChannelSidebar(channels []domain.Window, active domain.ChannelName, unread map[domain.ChannelName]int) ui.Model {
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

	require.Equal(t, []string{"Channels", "#dev", "▸#general", "#random"}, visibleLines(v))
}

func TestChannelSidebar_View_empty(t *testing.T) {
	m := newTestChannelSidebar(nil, "", nil)
	v := m.View(20, 10)

	require.Equal(t, []string{"No channels"}, visibleLines(v))
}

func TestChannelSidebar_View_active_channel_highlighted(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#random", nil)
	v := m.View(30, 10)

	require.Equal(t, []string{"Channels", "#dev", "#general", "▸#random"}, visibleLines(v))
}

func TestChannelSidebar_status_channel_stays_pinned_and_unprefixed(t *testing.T) {
	channels := []domain.Window{
		domain.NewChannelWindow("#general", time.Time{}),
		domain.NewStatusWindow(time.Time{}),
		dmStub("botty", time.Time{}),
	}

	m := newTestChannelSidebar(channels, "#general", nil)
	v := m.View(30, 10)

	require.Equal(t, []string{"Channels", "&modeloff", "▸#general", "botty"}, visibleLines(v))
	require.NotContains(t, v, "#&modeloff")
}

func TestChannelSidebar_ChannelRemovedMsg_drops_dm(t *testing.T) {
	channels := []domain.Window{
		domain.NewChannelWindow("#general", time.Time{}),
		dmStub("botty", time.Time{}),
	}

	m := newTestChannelSidebar(channels, "#general", nil)

	require.Equal(t,
		[]string{"Channels", "▸#general", "botty"},
		visibleLines(m.View(30, 10)))

	// DMs are addressed by the counterpart's InstanceID; the
	// `dmStub` test helper mints ids as `stub-<nick>`, so
	// removal is keyed by `stub-botty`.
	m, _ = m.Update(components.ChannelRemovedMsg{Channel: "stub-botty"})

	require.Equal(t,
		[]string{"Channels", "▸#general"},
		visibleLines(m.View(30, 10)))
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
		Channels: []domain.Window{
			domain.NewChannelWindow("#alpha", time.Time{}),
			domain.NewChannelWindow("#beta", time.Time{}),
		},
		Active: "#beta",
		Unread: map[domain.ChannelName]int{"#alpha": 5},
	})

	v := m.View(30, 10)
	require.Equal(t, []string{"Channels", "#alpha (5)", "▸#beta"}, visibleLines(v))
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

			require.Equal(t, []string{"Channels", "#dev", "▸#general", tt.wantText}, visibleLines(v))
		})
	}
}

func TestChannelSidebar_no_unread_indicator_when_nil(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#general", nil)
	v := m.View(30, 10)

	require.Equal(t, []string{"Channels", "#dev", "▸#general", "#random"}, visibleLines(v))
}

func TestChannelSidebar_dm_shows_at_prefix(t *testing.T) {
	channels := []domain.Window{
		domain.NewChannelWindow("#general", time.Time{}),
		dmStub("botty", time.Time{}),
	}

	m := newTestChannelSidebar(channels, "#general", nil)
	v := m.View(30, 10)

	require.Equal(t, []string{"Channels", "▸#general", "botty"}, visibleLines(v))
}

func TestChannelSidebar_dm_cursor_uses_dm_style(t *testing.T) {
	channels := []domain.Window{
		domain.NewChannelWindow("#general", time.Time{}),
		dmStub("botty", time.Time{}),
	}

	m := newTestChannelSidebar(channels, "#general", nil)
	m, _ = m.Update(ctrlKey("ctrl+d"))

	v := m.View(30, 10)
	require.Equal(t, []string{"Channels", "#general", "▸botty"}, visibleLines(v))
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
		Channels: []domain.Window{
			domain.NewChannelWindow("#alpha", time.Time{}),
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

func TestChannelSidebar_mention_renders_differently_from_normal_unread(t *testing.T) {
	// Force colour output so style differences are visible in test.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	// Set up two sidebars with the same unread count: one with mention,
	// one without.
	mNormal := newTestChannelSidebar(testChannels, "#general", nil)
	mNormal, _ = mNormal.Update(components.ChannelUnreadMsg{
		Channel: "#random",
		Count:   3,
	})

	mMention := newTestChannelSidebar(testChannels, "#general", nil)
	mMention, _ = mMention.Update(components.ChannelUnreadMsg{
		Channel: "#random",
		Count:   3,
		Mention: true,
	})

	vNormal := mNormal.View(30, 10)
	vMention := mMention.View(30, 10)

	require.Equal(t, []string{"Channels", "#dev", "▸#general", "#random (3)"}, visibleLines(vNormal))
	require.Equal(t, []string{"Channels", "#dev", "▸#general", "#random (3)"}, visibleLines(vMention))

	// Isolate the #random line from each view so the assertion targets
	// the line the mention styling applies to.
	normalLine := findLineContaining(t, vNormal, "#random")
	mentionLine := findLineContaining(t, vMention, "#random")

	// Each line must contain at least one SGR introducer — rules out
	// the renderer returning the bare string.
	require.Regexp(t, `\x1b\[[0-9;]+m`, normalLine)
	require.Regexp(t, `\x1b\[[0-9;]+m`, mentionLine)

	// The visible text is identical — rules out whitespace-only drift.
	require.Equal(t, ansi.Strip(normalLine), ansi.Strip(mentionLine))

	// The raw lines differ, and since the visible text matches the
	// difference must be due to styling.
	require.NotEqual(t, normalLine, mentionLine,
		"mention unread should render with a distinct style")
}

// findLineContaining returns the first rendered line that contains the
// given substring after ANSI stripping.
func findLineContaining(t *testing.T, view, substr string) string {
	t.Helper()

	for line := range strings.SplitSeq(view, "\n") {
		if strings.Contains(ansi.Strip(line), substr) {
			return line
		}
	}

	t.Fatalf("no line containing %q in view:\n%s", substr, view)

	return ""
}

func TestChannelSidebar_mention_clears_on_zero_count(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#general", nil)

	// Set a mention.
	m, _ = m.Update(components.ChannelUnreadMsg{
		Channel: "#random",
		Count:   3,
		Mention: true,
	})
	require.Equal(t, []string{"Channels", "#dev", "▸#general", "#random (3)"}, visibleLines(m.View(30, 10)))

	// Clear the unread count.
	m, _ = m.Update(components.ChannelUnreadMsg{
		Channel: "#random",
		Count:   0,
	})

	// After clearing, there should be no unread indicator.
	v := m.View(30, 10)
	require.Equal(t, []string{"Channels", "#dev", "▸#general", "#random"}, visibleLines(v))
}

func TestChannelSidebar_mention_clears_on_activation(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	m := newTestChannelSidebar(testChannels, "#general", nil)

	// Set a mention on #random.
	m, _ = m.Update(components.ChannelUnreadMsg{
		Channel: "#random",
		Count:   3,
		Mention: true,
	})

	vBefore := m.View(30, 10)

	// Activate #random (simulates switching to that channel).
	m, _ = m.Update(components.ChannelActiveMsg{Channel: "#random"})

	// Send a new non-mention unread to verify the mention style is gone.
	m, _ = m.Update(components.ChannelUnreadMsg{
		Channel: "#random",
		Count:   3,
	})

	vAfter := m.View(30, 10)
	require.NotEqual(t, vBefore, vAfter,
		"mention style should be cleared after activating channel")
}

func TestChannelSidebar_ignores_other_messages(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#general", nil)

	m, cmd := m.Update(key("x"))
	require.Nil(t, cmd)

	v := m.View(20, 10)
	require.Equal(t, []string{"Channels", "#dev", "▸#general", "#random"}, visibleLines(v))
}
