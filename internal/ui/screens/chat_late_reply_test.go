package screens

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/chatcmd"
)

// lateReplyArms are the point-to-point reply arms that render on the
// window their event names. Each is issued from a window that closes
// before the reply comes back, which is the case the shared path in
// [ChatScreen.logAndShowOn] has to answer the same way for all of
// them.
var lateReplyArms = []struct {
	name  string
	reply func(target domain.ChannelName, at time.Time) domain.Event
}{
	{
		name: "whois",
		reply: func(target domain.ChannelName, at time.Time) domain.Event {
			return domain.Whois{Target: target, Nick: "botty", ModelID: "test/model", At: at}
		},
	},
	{
		name: "invited",
		reply: func(target domain.ChannelName, at time.Time) domain.Event {
			return domain.Invited{Target: target, Nick: "botty", By: "testuser", At: at}
		},
	},
	{
		name: "system notice",
		reply: func(target domain.ChannelName, at time.Time) domain.Event {
			return domain.SystemNotice{Target: target, Text: "no such nick: botty", At: at}
		},
	},
}

// lateReplyFixture builds a chat screen holding a DM with `botty` and
// the channel `#other`, with the DM in view. It is the state a command
// issued from a query window leaves the screen in.
func lateReplyFixture(t *testing.T) (ChatScreen, *domain.DMWindow) {
	t.Helper()

	sess, mgr, user := newTestSession(t)

	counterpart := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	require.NoError(t, sess.SaveInstance(t.Context(), counterpart))

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	dm := domain.NewDMWindow(counterpart, time.Time{})
	screen.channels.Insert(newWindow(dm))
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#other", time.Time{})))

	screen, _ = screen.focus(dm.Name())

	return screen, dm
}

// TestChatScreen_late_reply_to_a_closed_dm covers a reply arriving
// after the query window it was issued from has been closed. A DM
// window is built around its counterpart's instance handle, which
// [ChatScreen.appendToScrollback] cannot synthesise, so routing the
// reply straight to its own target would drop it where the user would
// never see it.
func TestChatScreen_late_reply_to_a_closed_dm(t *testing.T) {
	for _, arm := range lateReplyArms {
		t.Run(arm.name, func(t *testing.T) {
			screen, dm := lateReplyFixture(t)

			at := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
			reply := arm.reply(dm.Name(), at)

			screen, closeCmd := screen.closeWindow(dm.Name(), time.Now())
			collectMsgs(closeCmd)
			require.Equal(t, domain.ChannelName("#other"), screen.active,
				"closing the window in view must land the user on #other")

			screen, _ = screen.update(reply)

			_, reopened := screen.windowByName(dm.Name())
			require.False(t, reopened, "a late reply must not recreate the closed DM window")

			require.Equal(t, []domain.Event{reply}, screen.scrollbackOf("#other"))
		})
	}
}

// TestChatScreen_late_reply_to_a_parted_channel is the channel
// counterpart. [ChatScreen.appendToScrollback] does create a window
// for an unknown channel, but that path is for live traffic arriving
// before a join is seen; a stale reply taking it would put a channel
// the user has left back in the window set with no membership behind
// it.
func TestChatScreen_late_reply_to_a_parted_channel(t *testing.T) {
	for _, arm := range lateReplyArms {
		t.Run(arm.name, func(t *testing.T) {
			screen := newScreenFixture(t)
			screen.channels.Insert(newWindow(domain.NewChannelWindow("#a", time.Time{})))
			screen.channels.Insert(newWindow(domain.NewChannelWindow("#b", time.Time{})))

			screen, _ = screen.focus("#a")

			at := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
			reply := arm.reply("#a", at)

			screen, closeCmd := screen.closeWindow("#a", time.Now())
			collectMsgs(closeCmd)
			screen, _ = screen.focus("#b")

			screen, _ = screen.update(reply)

			_, reappeared := screen.windowByName("#a")
			require.False(t, reappeared, "a late reply must not resurrect a parted channel")

			require.Equal(t, []domain.Event{reply}, screen.scrollbackOf("#b"))
		})
	}
}

// TestChatScreen_late_reply_with_no_window_in_view covers the user
// parting their last channel while the command is still in flight.
// [ChatScreen.firstRealChannel] skips `&modeloff`, so closeWindow
// leaves them looking at nothing and the fallback has no window to
// answer with. The reply takes [ChatScreen.logAndShow]'s answer:
// `&modeloff`, with the focus moved there so the user sees it.
func TestChatScreen_late_reply_with_no_window_in_view(t *testing.T) {
	for _, arm := range lateReplyArms {
		t.Run(arm.name, func(t *testing.T) {
			screen := newScreenFixture(t)
			screen.channels.Insert(newWindow(domain.NewStatusWindow(time.Time{})))
			screen.channels.Insert(newWindow(domain.NewChannelWindow("#a", time.Time{})))

			screen, _ = screen.focus("#a")

			at := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
			reply := arm.reply("#a", at)

			screen, closeCmd := screen.closeWindow("#a", time.Now())
			collectMsgs(closeCmd)
			require.Equal(t, domain.ChannelName(""), screen.active,
				"parting the only real channel leaves the user looking at nothing")

			screen, cmd := screen.update(reply)

			_, reappeared := screen.windowByName("#a")
			require.False(t, reappeared, "a late reply must not resurrect a parted channel")

			require.Equal(t, []domain.Event{reply}, screen.scrollbackOf(domain.StatusChannelName))

			focus, moved := containsMsg[chatcmd.ChannelFocusMsg](collectMsgs(cmd))
			require.True(t, moved, "the user must be moved to the window holding the reply")
			require.Equal(t, domain.StatusChannelName, focus.Channel)
		})
	}
}

// TestChatScreen_reply_renders_on_its_own_target_while_the_window_is_open
// pins the ordinary case the fallback must leave alone: a reply naming
// a window the user still has open renders there, whatever window they
// are looking at now.
func TestChatScreen_reply_renders_on_its_own_target_while_the_window_is_open(t *testing.T) {
	for _, arm := range lateReplyArms {
		t.Run(arm.name, func(t *testing.T) {
			screen, dm := lateReplyFixture(t)

			at := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
			reply := arm.reply(dm.Name(), at)

			screen, _ = screen.focus("#other")
			screen, _ = screen.update(reply)

			require.Equal(t, []domain.Event{reply}, screen.scrollbackOf(dm.Name()))
			require.Empty(t, screen.scrollbackOf("#other"),
				"the reply belongs to the window it was issued from, not the one in view")
		})
	}
}

// TestChatScreen_names_reply_for_a_parted_channel_is_dropped records
// the one point-to-point arm that needs no fallback.
// [ChatScreen.handleNamesReply] applies a member-list snapshot to a
// window and renders nothing, so a channel the user has left leaves it
// with no window to update and nothing to show.
func TestChatScreen_names_reply_for_a_parted_channel_is_dropped(t *testing.T) {
	screen := newScreenFixture(t)
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#b", time.Time{})))
	screen, _ = screen.focus("#b")

	screen, cmd := screen.handleNamesReply(domain.NamesReplyEvent{
		Channel: "#a",
		Members: domain.NewMemberList(),
	})

	require.Nil(t, cmd)

	_, reappeared := screen.windowByName("#a")
	require.False(t, reappeared, "a names reply must not resurrect a parted channel")
	require.Empty(t, screen.scrollbackOf("#b"))
}
