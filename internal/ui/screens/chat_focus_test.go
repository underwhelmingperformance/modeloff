package screens

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
)

// channelBodies reads the message bodies each named channel's event
// log holds. Collecting every named channel in one map lets a caller
// assert on where a send landed across all of them at once.
func channelBodies(t *testing.T, sess *session.Session, names ...domain.ChannelName) map[domain.ChannelName][]string {
	t.Helper()

	out := make(map[domain.ChannelName][]string, len(names))

	for _, name := range names {
		stored, err := sess.EventsBefore(t.Context(), name, nil, 100)
		require.NoError(t, err)

		bodies := []string{}

		for _, ev := range stored {
			if msg, ok := ev.Event.(domain.Message); ok {
				bodies = append(bodies, msg.Body)
			}
		}

		out[name] = bodies
	}

	return out
}

// TestChatScreen_send_targets_the_channel_it_was_typed_in pins the
// value-semantics contract for the focused window: the send command
// a submit returns carries the window the line was typed in, so a
// channel switch between the submit and the command running cannot
// redirect the line. Bubble Tea runs a returned `tea.Cmd` on its own
// goroutine at an unspecified later point, which is exactly the
// window in which a user can hit a channel-switch key.
func TestChatScreen_send_targets_the_channel_it_was_typed_in(t *testing.T) {
	sess, mgr, user := newTestSession(t)
	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#general")))
	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#random")))

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	for _, name := range []domain.ChannelName{"#general", "#random"} {
		screen.channels.Insert(newWindow(domain.NewChannelWindow(name, time.Time{})))
	}

	screen, _ = screen.focus("#general")

	updated, send := screen.Update(components.MessageSubmitMsg{Text: "typed in general"})
	screen = updated.(ChatScreen)
	require.NotNil(t, send)

	// The user switches window before the send command is run.
	updated, _ = screen.Update(chatcmd.ChannelFocusMsg{Channel: "#random", At: time.Now()})
	screen = updated.(ChatScreen)

	require.Nil(t, send(), "a successful send renders through the echo-message bus, not a message")

	require.Equal(t, map[domain.ChannelName][]string{
		"#general": {"typed in general"},
		"#random":  {},
	}, channelBodies(t, sess, "#general", "#random"))
}

// TestChatScreen_own_nick_change_confirms_in_the_focused_window pins
// the user-side NICK feedback: the prompt nick, the highlight set and
// the checklist follow the user whatever window is in view, and the
// confirmation line is rendered in the focused window exactly when
// the fan-out did not already file it there. `&modeloff` is never a
// NICK target, so the status-window case covers a rename the fan-out
// reaches no visible window with.
func TestChatScreen_own_nick_change_confirms_in_the_focused_window(t *testing.T) {
	targets := []domain.ChannelName{"#general"}

	tests := map[string]struct {
		focused          domain.ChannelName
		wantMsgs         []string
		wantConfirmation bool
	}{
		"focused window is not a target": {
			focused: domain.StatusChannelName,
			wantMsgs: []string{
				"components.UserNickMsg",
				"components.HighlightWordsMsg",
				"components.ScrollbackUpdatedMsg",
				"components.ChannelHasLifecycleMsg",
			},
			wantConfirmation: true,
		},
		"focused window is a target": {
			focused: "#general",
			wantMsgs: []string{
				"components.NickListUpdatedMsg",
				"components.UserNickMsg",
				"components.HighlightWordsMsg",
			},
			// The fan-out files the line into every target window
			// before the handler runs, so the handler adds nothing.
			wantConfirmation: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			screen := newScreenFixture(t)

			screen.channels.Insert(newWindow(domain.NewStatusWindow(time.Time{})))
			screen.channels.Insert(newWindow(domain.NewChannelWindow("#general", time.Time{})))

			screen, _ = screen.focus(tc.focused)

			user := screen.user.Instance()
			user.SetNick("newnick")

			change := domain.NickChange{
				OldNick:  "testuser",
				NewNick:  "newnick",
				Instance: user,
			}

			screen, cmd := screen.handleNickChangeEvent(change, targets)

			msgs := collectMsgs(cmd)
			require.Equal(t, tc.wantMsgs, msgsTypes(msgs))

			nick, ok := containsMsg[components.UserNickMsg](msgs)
			require.True(t, ok)
			require.Equal(t, components.UserNickMsg{Nick: "newnick"}, nick)

			highlight, ok := containsMsg[components.HighlightWordsMsg](msgs)
			require.True(t, ok)
			require.Equal(t, components.HighlightWordsMsg{
				Words:    screen.highlightWords,
				UserNick: "newnick",
			}, highlight)

			var wantScrollback []domain.Event
			if tc.wantConfirmation {
				wantScrollback = []domain.Event{change}
			}

			require.Equal(t, wantScrollback, screen.scrollbackOf(tc.focused))
			require.Equal(t, domain.Nick("newnick"), screen.checklist.nick,
				"the welcome checklist must follow the rename")
		})
	}
}
