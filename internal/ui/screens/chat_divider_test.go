package screens_test

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/screens"
	"github.com/laney/modeloff/internal/ui/uitest"
)

// flattenCmd runs a command and flattens any batch into the concrete
// messages it produced.
func flattenCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}

	msg := cmd()

	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}

	var msgs []tea.Msg
	for _, c := range batch {
		msgs = append(msgs, flattenCmd(c)...)
	}

	return msgs
}

// withDividerMarker replaces the new-messages divider with a marker.
// The divider is drawn to fill the content column, so an assertion
// on the marker pins the line it sits above and leaves the width to
// the renderer.
func withDividerMarker(lines []string) []string {
	out := make([]string, len(lines))

	for i, line := range lines {
		out[i] = line
		if strings.Contains(line, " new messages ") {
			out[i] = "<new messages>"
		}
	}

	return out
}

// TestChatScreen_divider_marks_messages_that_arrived_while_away is
// the whole unread story end to end: the user reads a window, moves
// to another one, a message lands in the window they left, the
// sidebar counts it, and on return the divider sits above it. The
// seen mark lives on the window record, so leaving and coming back
// does not lose the reader's place.
func TestChatScreen_divider_marks_messages_that_arrived_while_away(t *testing.T) {
	h := newTestSession(t)
	uitest.SeedChannel(t, h.user, "#general")
	uitest.SeedChannel(t, h.user, "#random")

	tm := newChatApp(t, h)

	// Startup lands on the freshest join. Waiting for its banner
	// means #random has been read to the bottom before the user
	// leaves it.
	tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "▸#random") &&
			strings.Contains(view, "Created channel #random")
	})

	tm.Send(chatcmd.ChannelFocusMsg{Channel: "#general", At: time.Now()})
	tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "▸#general") &&
			strings.Contains(view, "Created channel #general")
	})

	uitest.SeedMessage(t, h.sess, "#random", "arrived while away")

	tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "#random (1)")
	})

	tm.Send(chatcmd.ChannelFocusMsg{Channel: "#random", At: time.Now()})

	// Reading the window sweeps its badge, so waiting for the count
	// to go with the focus marker anchors the snapshot to the frame
	// after the switch has fully landed.
	view := tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "▸#random") &&
			!strings.Contains(view, "#random (1)") &&
			strings.Contains(view, "arrived while away")
	})

	body, _ := uitest.SplitBodyAndStatus(view)
	columns := uitest.VisibleColumns(body)

	require.Equal(t, []string{"Channels", "&modeloff", "#general", "▸#random"},
		uitest.NonEmptyColumn(columns[0]))
	require.Equal(t, []string{
		"*** Created channel #random",
		"<new messages>",
		"<seedbot> arrived while away",
		"testuser >",
	}, withDividerMarker(normaliseContent(uitest.NonEmptyColumn(columns[1]))))
}

// TestChatScreen_no_divider_for_a_line_that_lands_during_a_switch
// drives the interleaving a window switch invites: the switch is
// decided on the Update goroutine, and the message list is told about
// it in a command Bubble Tea runs later, so an event can be buffered
// into the window in between. The reader is already in the window and
// at the bottom of it when that line arrives, so it must render with
// no divider above it.
//
// The test holds the focus command back, injects the event, and only
// then delivers the messages the focus produced.
func TestChatScreen_no_divider_for_a_line_that_lands_during_a_switch(t *testing.T) {
	h := newTestSession(t)
	uitest.SeedChannel(t, h.user, "#general")

	screen, err := screens.NewChatScreen(t.Context, h.sess, h.mgr, h.user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	var model ui.Model = screen

	// Init fills the window cache. Its commands include the
	// protocol-bus listener, which this test drives by hand.
	_ = model.Init()

	model, _ = model.Update(tea.WindowSizeMsg{Width: termWidth, Height: termHeight})

	model, focusCmd := model.Update(chatcmd.ChannelFocusMsg{
		Channel: "#general",
		At:      time.Now(),
	})

	model, _ = model.Update(screens.NewProtocolEventForTest(domain.Message{
		Target: "#general",
		From:   "alice",
		Body:   "landed during the switch",
		At:     time.Now(),
	}, nil))

	for _, msg := range flattenCmd(focusCmd) {
		model, _ = model.Update(msg)
	}

	body, _ := uitest.SplitBodyAndStatus(model.View(termWidth, termHeight))
	columns := uitest.VisibleColumns(body)

	require.Equal(t, []string{
		"<alice> landed during the switch",
		"testuser >",
	}, withDividerMarker(normaliseContent(uitest.NonEmptyColumn(columns[1]))))
}
