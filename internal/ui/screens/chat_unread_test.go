package screens

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/ui/uitest"
)

// focused moves the screen to `ch` and runs the commands the move
// returns, so the read-cursor writes the focus change schedules have
// landed by the time the caller asserts. Bubble Tea runs them for
// real; a test that only took the screen value would be asserting
// against half a focus change.
func focused(t *testing.T, s ChatScreen, ch domain.ChannelName) ChatScreen {
	t.Helper()

	s, cmd := s.focus(ch)
	collectMsgs(cmd)

	return s
}

// deliverBadges feeds the results of an unread-count command back
// into the screen and returns the messages the sidebar would receive.
// Reading the count and delivering the result are separate steps in
// production, on separate goroutines, so a test can run the read,
// drive the screen, and deliver afterwards.
func deliverBadges(t *testing.T, s ChatScreen, counted []tea.Msg) []tea.Msg {
	t.Helper()

	var delivered []tea.Msg

	for _, msg := range counted {
		var out tea.Cmd

		s, out = s.update(msg)
		delivered = append(delivered, collectMsgs(out)...)
	}

	return delivered
}

// TestChatScreen_unread_badge_counts_only_what_arrived_since_the_last_visit
// pins the read cursor to the window the user is actually reading.
// Focus is what moves it: arriving at a window marks what is already
// there as read, and leaving one marks what arrived while it was in
// view. Either way the badge raised by the next message counts that
// message alone, not everything back to the join.
func TestChatScreen_unread_badge_counts_only_what_arrived_since_the_last_visit(t *testing.T) {
	tests := map[string]struct {
		arriveWhileFocused bool
	}{
		"message arrives while the user is in another window": {arriveWhileFocused: false},
		"message arrives while the user is watching":          {arriveWhileFocused: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			sess, mgr, user := newTestSession(t)
			require.NoError(t, user.Join(t.Context(), domain.ChannelName("#general")))
			require.NoError(t, user.Join(t.Context(), domain.ChannelName("#random")))

			screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
			require.NoError(t, err)

			for _, ch := range []domain.ChannelName{"#general", "#random"} {
				screen.channels.Insert(newWindow(domain.NewChannelWindow(ch, time.Time{})))
			}

			screen = focused(t, screen, "#general")

			if !tc.arriveWhileFocused {
				uitest.SeedMessage(t, sess, "#random", "first")
			}

			screen = focused(t, screen, "#random")

			if tc.arriveWhileFocused {
				uitest.SeedMessage(t, sess, "#random", "first")
			}

			screen = focused(t, screen, "#general")

			uitest.SeedMessage(t, sess, "#random", "second")

			second := domain.Message{
				Target:     "#random",
				From:       "seedbot",
				InstanceID: "inst-seedbot",
				Body:       "second",
			}

			counted := collectMsgs(screen.renderMessage(second, "#random"))

			require.Equal(t, []tea.Msg{components.ChannelUnreadMsg{
				Channel: "#random",
				Count:   1,
			}}, deliverBadges(t, screen, counted))
		})
	}
}

// TestChatScreen_own_message_mentioning_own_nick_does_not_badge_mention
// covers the self-exemption isHighlight owes components.renderMessage:
// the render path never highlights the user's own message even when
// its body contains the user's nick, identifying it by its empty
// InstanceID. isHighlight must apply the same exemption, or the
// user's own message echoing their nick back (e.g. a reply quoting
// what someone called them) would badge the channel as mentioning
// them.
func TestChatScreen_own_message_mentioning_own_nick_does_not_badge_mention(t *testing.T) {
	sess, mgr, user := newTestSession(t)
	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#general")))
	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#random")))

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	for _, ch := range []domain.ChannelName{"#general", "#random"} {
		screen.channels.Insert(newWindow(domain.NewChannelWindow(ch, time.Time{})))
	}

	screen = focused(t, screen, "#general")

	ownMessage := domain.Message{
		Target:     "#random",
		From:       user.Nick(),
		InstanceID: "",
		Body:       "hey " + string(user.Nick()),
	}

	counted := collectMsgs(screen.renderMessage(ownMessage, "#random"))

	require.Equal(t, []tea.Msg{components.ChannelUnreadMsg{
		Channel: "#random",
		Count:   0,
		Mention: false,
	}}, deliverBadges(t, screen, counted))
}

// TestChatScreen_clear_drops_the_window_history pins the `/clear`
// bookkeeping: the scrollback goes, and the message list is told the
// window's history went with it, so the reader's place in that window
// cannot outlive the lines it pointed at.
func TestChatScreen_clear_drops_the_window_history(t *testing.T) {
	screen := newScreenFixture(t)
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#general", time.Time{})))
	screen = focused(t, screen, "#general")

	window, ok := screen.windowByName("#general")
	require.True(t, ok)

	window.Scrollback.Append(domain.Message{Target: "#general", From: "alice", Body: "hello"})

	screen, cmd := screen.update(chatcmd.ClearResult{})

	require.Equal(t, &Window{
		Window:     window.Window,
		Scrollback: window.Scrollback,
		Visits:     window.Visits,
		UserTime:   window.UserTime,
	}, window, "clearing must leave every field but the history alone")

	require.Equal(t, []domain.Event(nil), window.Scrollback.Events(),
		"the window's history is gone")

	require.Equal(t, []tea.Msg{
		components.ScrollbackClearedMsg{Channel: "#general"},
	}, collectMsgs(cmd))
}

// TestChatScreen_unread_count_read_before_a_visit_is_dropped pins the
// badge against a count that lost its race with the user. The count
// is read from the store on its own goroutine, and the user can focus
// the window before the answer comes back. Focusing clears the badge
// and marks the channel read, so a count taken before that describes
// a state the user has already left behind: applying it would put a
// badge back on a window they are reading.
func TestChatScreen_unread_count_read_before_a_visit_is_dropped(t *testing.T) {
	tests := map[string]struct {
		visited bool
		want    []tea.Msg
	}{
		"no visit while the count was in flight": {
			want: []tea.Msg{components.ChannelUnreadMsg{Channel: "#random", Count: 1}},
		},
		"user visited the window while the count was in flight": {
			visited: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			sess, mgr, user := newTestSession(t)
			require.NoError(t, user.Join(t.Context(), domain.ChannelName("#general")))
			require.NoError(t, user.Join(t.Context(), domain.ChannelName("#random")))

			screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
			require.NoError(t, err)

			for _, ch := range []domain.ChannelName{"#general", "#random"} {
				screen.channels.Insert(newWindow(domain.NewChannelWindow(ch, time.Time{})))
			}

			screen = focused(t, screen, "#general")
			uitest.SeedMessage(t, sess, "#random", "first")

			// The count is read while the user is still in
			// #general and the message is genuinely unread.
			counted := collectMsgs(screen.renderMessage(domain.Message{
				Target:     "#random",
				From:       "seedbot",
				InstanceID: "inst-seedbot",
				Body:       "first",
			}, "#random"))

			require.Equal(t, []tea.Msg{unreadCountedMsg{
				channel: "#random",
				count:   1,
			}}, counted)

			if tc.visited {
				screen = focused(t, screen, "#random")
				screen = focused(t, screen, "#general")
			}

			require.Equal(t, tc.want, deliverBadges(t, screen, counted))
		})
	}
}
