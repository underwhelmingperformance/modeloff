package components_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
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

var italicSGR = regexp.MustCompile(`\x1b\[(?:[0-9]+;)*3(?:;[0-9]+)*m`)

type dmTestWindow struct {
	id      domain.InstanceID
	nick    domain.Nick
	created time.Time
}

func dmStub(nick domain.Nick, created time.Time) *dmTestWindow {
	return &dmTestWindow{id: domain.InstanceID("stub-" + string(nick)), nick: nick, created: created}
}

func (w *dmTestWindow) Name() domain.ChannelName { return domain.ChannelName(w.id) }
func (w *dmTestWindow) Created() time.Time       { return w.created }
func (*dmTestWindow) Kind() domain.ChannelKind   { return domain.KindDM }
func (w *dmTestWindow) DisplayName() string      { return string(w.nick) }
func (w *dmTestWindow) Less(other domain.Window) bool {
	if other.Kind() != domain.KindDM {
		return false
	}

	return w.Name() < other.Name()
}

func key(k string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: rune(k[0]), Text: k}
}

func ctrlKey(k string) tea.KeyPressMsg {
	switch k {
	case "alt+down":
		return tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModAlt}
	case "alt+up":
		return tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt}
	case "ctrl+o":
		return tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}
	default:
		return tea.KeyPressMsg{Code: rune(k[0]), Text: k}
	}
}

func newTestChannelSidebar(channels []domain.Window, active domain.ChannelName, unread map[domain.ChannelName]int) ui.Component {
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
func activateAndGetChannel(t *testing.T, m ui.Component, msg tea.Msg) (ui.Component, domain.ChannelName) {
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
	v := renderToBuffer(m, 20, 10)

	require.Equal(t, []string{"Channels", "#dev", "▸#general", "#random"}, visibleLines(v))
}

func TestChannelSidebar_View_empty(t *testing.T) {
	m := newTestChannelSidebar(nil, "", nil)
	v := renderToBuffer(m, 20, 10)

	require.Equal(t, []string{"No channels"}, visibleLines(v))
}

func TestChannelSidebar_ContentWidth_tracks_visible_rows(t *testing.T) {
	sidebar := components.NewChannelSidebar()
	require.Equal(t, ansi.StringWidth("No channels")+2, sidebar.ContentWidth())

	channel := domain.NewChannelWindow("#a-very-long-channel", time.Time{})
	updated, _ := sidebar.Update(components.SetChannelsMsg{
		Channels: []domain.Window{channel},
		Unread:   map[domain.ChannelName]int{channel.Name(): 12},
	})
	sidebar = updated.(components.ChannelSidebar)

	require.Equal(t, ansi.StringWidth(" #a-very-long-channel (12)")+2, sidebar.ContentWidth())
}

func TestChannelSidebar_View_active_channel_highlighted(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#random", nil)
	v := renderToBuffer(m, 30, 10)

	require.Equal(t, []string{"Channels", "#dev", "#general", "▸#random"}, visibleLines(v))
}

func TestChannelSidebar_status_channel_stays_pinned_and_unprefixed(t *testing.T) {
	channels := []domain.Window{
		domain.NewChannelWindow("#general", time.Time{}),
		domain.NewStatusWindow(time.Time{}),
		dmStub("botty", time.Time{}),
	}

	m := newTestChannelSidebar(channels, "#general", nil)
	v := renderToBuffer(m, 30, 10)

	require.Equal(t, []string{"Channels", "&modeloff", "▸#general", "Queries", "botty"}, visibleLines(v))
	require.NotContains(t, v, "#&modeloff")
}

func TestChannelSidebar_ChannelRemovedMsg_drops_dm(t *testing.T) {
	channels := []domain.Window{
		domain.NewChannelWindow("#general", time.Time{}),
		dmStub("botty", time.Time{}),
	}

	m := newTestChannelSidebar(channels, "#general", nil)

	require.Equal(t,
		[]string{"Channels", "▸#general", "Queries", "botty"},
		visibleLines(renderToBuffer(m, 30, 10)))

	// DMs are addressed by the counterpart's InstanceID; the
	// `dmStub` test helper mints ids as `stub-<nick>`, so
	// removal is keyed by `stub-botty`.
	m, _ = m.Update(components.ChannelRemovedMsg{Channel: "stub-botty"})

	require.Equal(t,
		[]string{"Channels", "▸#general"},
		visibleLines(renderToBuffer(m, 30, 10)))
}

func TestChannelSidebar_keyboard_navigation(t *testing.T) {
	// Sorted order: #dev, #general, #random. Active #general = index 1.
	m := newTestChannelSidebar(testChannels, "#general", nil)

	// Down once → #random (index 2).
	m, _ = m.Update(ctrlKey("alt+down"))

	_, ch := activateAndGetChannel(t, m, ctrlKey("ctrl+o"))
	require.Equal(t, domain.ChannelName("#random"), ch)
}

func TestChannelSidebar_keyboard_up(t *testing.T) {
	// Sorted order: #dev, #general, #random. Active #random = index 2.
	m := newTestChannelSidebar(testChannels, "#random", nil)

	// Up twice → #dev (index 0).
	m, _ = m.Update(ctrlKey("alt+up"))
	m, _ = m.Update(ctrlKey("alt+up"))

	_, ch := activateAndGetChannel(t, m, ctrlKey("ctrl+o"))
	require.Equal(t, domain.ChannelName("#dev"), ch)
}

func TestChannelSidebar_cursor_clamps_at_boundaries(t *testing.T) {
	// Sorted order: #dev, #general, #random. Active #dev = index 0.
	m := newTestChannelSidebar(testChannels, "#dev", nil)

	// Up past the top — should stay at #dev.
	m, _ = m.Update(ctrlKey("alt+up"))
	m, _ = m.Update(ctrlKey("alt+up"))

	_, ch := activateAndGetChannel(t, m, ctrlKey("ctrl+o"))
	require.Equal(t, domain.ChannelName("#dev"), ch)
}

func TestChannelSidebar_cursor_clamps_at_bottom(t *testing.T) {
	// Sorted order: #dev, #general, #random.
	m := newTestChannelSidebar(testChannels, "#dev", nil)

	for range 10 {
		m, _ = m.Update(ctrlKey("alt+down"))
	}

	_, ch := activateAndGetChannel(t, m, ctrlKey("ctrl+o"))
	require.Equal(t, domain.ChannelName("#random"), ch)
}

func TestChannelSidebar_mouse_click_selects_channel(t *testing.T) {
	// Sorted order: #dev (row 0+header), #general (row 1+header), #random (row 2+header).
	// Header takes 1 row, so Y=2 is index 1 = #general.
	m := newTestChannelSidebar(testChannels, "#dev", nil)
	m, _ = m.Update(ui.BoundsMsg{Rect: uv.Rect(0, 0, 20, 10)})

	_, ch := activateAndGetChannel(t, m, tea.MouseClickMsg{
		X:      5,
		Y:      2,
		Button: tea.MouseLeft,
	})

	require.Equal(t, domain.ChannelName("#general"), ch)
}

func TestChannelSidebar_mouse_click_out_of_range(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#general", nil)
	m, _ = m.Update(ui.BoundsMsg{Rect: uv.Rect(0, 0, 20, 10)})

	_, cmd := m.Update(tea.MouseClickMsg{
		X:      5,
		Y:      10,
		Button: tea.MouseLeft,
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

	v := renderToBuffer(m, 30, 10)
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
			v := renderToBuffer(m, 30, 10)

			require.Equal(t, []string{"Channels", "#dev", "▸#general", tt.wantText}, visibleLines(v))
		})
	}
}

func TestChannelSidebar_no_unread_indicator_when_nil(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#general", nil)
	v := renderToBuffer(m, 30, 10)

	require.Equal(t, []string{"Channels", "#dev", "▸#general", "#random"}, visibleLines(v))
}

func TestChannelSidebar_dm_shows_at_prefix(t *testing.T) {
	channels := []domain.Window{
		domain.NewChannelWindow("#general", time.Time{}),
		dmStub("botty", time.Time{}),
	}

	m := newTestChannelSidebar(channels, "#general", nil)
	v := renderToBuffer(m, 30, 10)

	require.Equal(t, []string{"Channels", "▸#general", "Queries", "botty"}, visibleLines(v))
}

func TestChannelSidebar_dm_cursor_uses_dm_style(t *testing.T) {
	channels := []domain.Window{
		domain.NewChannelWindow("#general", time.Time{}),
		dmStub("botty", time.Time{}),
	}

	m := newTestChannelSidebar(channels, "#general", nil)
	m, _ = m.Update(ctrlKey("alt+down"))

	v := renderToBuffer(m, 30, 10)
	require.Equal(t, []string{"Channels", "#general", "Queries", "▸botty"}, visibleLines(v))
}

func TestChannelSidebar_cursor_follows_active_on_set_channels(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#general", nil)

	m, _ = m.Update(ctrlKey("alt+down"))
	m, _ = m.Update(ctrlKey("alt+down"))

	m, _ = m.Update(components.SetChannelsMsg{
		Channels: testChannels,
		Active:   "#random",
	})

	_, ch := activateAndGetChannel(t, m, ctrlKey("ctrl+o"))
	require.Equal(t, domain.ChannelName("#random"), ch)
}

func TestChannelSidebar_cursor_clamps_when_active_not_in_list(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#general", nil)

	m, _ = m.Update(ctrlKey("alt+down"))
	m, _ = m.Update(ctrlKey("alt+down"))

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
	m, _ = m.Update(ui.BoundsMsg{Rect: uv.Rect(0, 0, 20, 10)})

	// Scroll down — should move cursor but NOT activate (no cmd).
	m, cmd := m.Update(tea.MouseWheelMsg{
		X:      5,
		Y:      2,
		Button: tea.MouseWheelDown,
	})

	require.Nil(t, cmd, "wheel scroll should not activate a channel")

	// Ctrl+O to confirm cursor moved to #general.
	_, ch := activateAndGetChannel(t, m, ctrlKey("ctrl+o"))
	require.Equal(t, domain.ChannelName("#general"), ch)
}

func TestChannelSidebar_mention_renders_differently_from_normal_unread(t *testing.T) {
	// Force colour output so style differences are visible in test.
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

	vNormal := renderToBuffer(mNormal, 30, 10)
	vMention := renderToBuffer(mMention, 30, 10)

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
	require.Equal(t, []string{"Channels", "#dev", "▸#general", "#random (3)"}, visibleLines(renderToBuffer(m, 30, 10)))

	// Clear the unread count.
	m, _ = m.Update(components.ChannelUnreadMsg{
		Channel: "#random",
		Count:   0,
	})

	// After clearing, there should be no unread indicator.
	v := renderToBuffer(m, 30, 10)
	require.Equal(t, []string{"Channels", "#dev", "▸#general", "#random"}, visibleLines(v))
}

func TestChannelSidebar_mention_clears_on_activation(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#general", nil)

	// Set a mention on #random.
	m, _ = m.Update(components.ChannelUnreadMsg{
		Channel: "#random",
		Count:   3,
		Mention: true,
	})

	vBefore := renderToBuffer(m, 30, 10)

	// Activate #random (simulates switching to that channel).
	m, _ = m.Update(components.ChannelActiveMsg{Channel: "#random"})

	// Send a new non-mention unread to verify the mention style is gone.
	m, _ = m.Update(components.ChannelUnreadMsg{
		Channel: "#random",
		Count:   3,
	})

	vAfter := renderToBuffer(m, 30, 10)
	require.NotEqual(t, vBefore, vAfter,
		"mention style should be cleared after activating channel")
}

func TestChannelSidebar_ignores_other_messages(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#general", nil)

	m, cmd := m.Update(key("x"))
	require.Nil(t, cmd)

	v := renderToBuffer(m, 20, 10)
	require.Equal(t, []string{"Channels", "#dev", "▸#general", "#random"}, visibleLines(v))
}

// TestChannelSidebar_lifecycle_renders_italic pins the styling
// rule: a window flagged as having unseen actor-scoped lifecycle
// events (a peer's QUIT, a peer's NICK rename) renders with the
// italic-dim style, distinct from the inactive default. The
// sidebar should not show a count — lifecycle is yes/no, not
// numeric.
func TestChannelSidebar_lifecycle_renders_italic(t *testing.T) {
	mIdle := newTestChannelSidebar(testChannels, "#general", nil)
	mLifecycle := newTestChannelSidebar(testChannels, "#general", nil)
	mLifecycle, _ = mLifecycle.Update(components.ChannelHasLifecycleMsg{Channel: "#random"})

	vIdle := renderToBuffer(mIdle, 30, 10)
	vLifecycle := renderToBuffer(mLifecycle, 30, 10)

	// No count appears either way — lifecycle is yes/no.
	require.Equal(t, []string{"Channels", "#dev", "▸#general", "#random"}, visibleLines(vIdle))
	require.Equal(t, []string{"Channels", "#dev", "▸#general", "#random"}, visibleLines(vLifecycle))

	idleLine := findLineContaining(t, vIdle, "#random")
	lifecycleLine := findLineContaining(t, vLifecycle, "#random")

	require.Equal(t, ansi.Strip(idleLine), ansi.Strip(lifecycleLine))
	require.NotEqual(t, idleLine, lifecycleLine,
		"a window with unseen lifecycle activity should render distinctly from inactive")

	// SGR code 3 is italic; the lifecycle style applies it and the
	// inactive default does not.
	require.True(t, italicSGR.MatchString(lifecycleLine), "lifecycle style should set italic")
	require.False(t, italicSGR.MatchString(idleLine), "inactive style should not set italic")
}

// TestChannelSidebar_unread_overrides_lifecycle pins precedence:
// if a window has both unread messages and unseen lifecycle
// activity, the bold-with-count unread style wins. Lifecycle is
// the quietest indicator and never displaces a louder one.
func TestChannelSidebar_unread_overrides_lifecycle(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#general", nil)
	m, _ = m.Update(components.ChannelHasLifecycleMsg{Channel: "#random"})
	m, _ = m.Update(components.ChannelUnreadMsg{Channel: "#random", Count: 2})

	screen := uv.NewScreenBuffer(30, 10)
	m.Draw(screen, screen.Bounds())
	v := screen.Render()
	require.Equal(t, []string{"Channels", "#dev", "▸#general", "#random (2)"}, visibleLines(v))

	var hash *uv.Cell
	for x := range screen.Bounds().Dx() {
		cell := screen.CellAt(x, 3)
		if cell.Content == "#" {
			hash = cell
			break
		}
	}

	require.NotNil(t, hash)
	require.NotZero(t, hash.Style.Attrs&uv.AttrBold,
		"unread style should set bold even when lifecycle is also flagged")
}

// TestChannelSidebar_lifecycle_clears_on_activation pins the
// sweep semantic: focusing a window clears its lifecycle flag
// alongside mentions. Reactivation of a previously-flagged window
// should leave it indistinguishable from a never-flagged one.
func TestChannelSidebar_lifecycle_clears_on_activation(t *testing.T) {
	m := newTestChannelSidebar(testChannels, "#general", nil)
	m, _ = m.Update(components.ChannelHasLifecycleMsg{Channel: "#random"})

	flagged := findLineContaining(t, renderToBuffer(m, 30, 10), "#random")

	m, _ = m.Update(components.ChannelActiveMsg{Channel: "#random"})
	// Re-set active back to #general so #random is rendered as
	// non-active again — that's the case we're checking the style
	// of after the lifecycle clear.
	m, _ = m.Update(components.ChannelActiveMsg{Channel: "#general"})

	cleared := findLineContaining(t, renderToBuffer(m, 30, 10), "#random")

	require.NotEqual(t, flagged, cleared, "lifecycle styling should clear after activation")
	require.False(t, italicSGR.MatchString(cleared), "post-clear style should not be italic")
}
