package screens_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/testclient"
	uipkg "github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/screens"
	"github.com/laney/modeloff/internal/ui/screens/screenstest"
	"github.com/laney/modeloff/internal/ui/uitest"
)

// seedInstance persists a model instance so the chat-screen can
// resolve its id back to the canonical handle a DM window is built
// around.
func seedInstance(t *testing.T, h *testHarness, id domain.InstanceID, nick domain.Nick) {
	t.Helper()

	require.NoError(t, h.sess.SaveInstance(t.Context(), domain.NewModelInstance(id, nick, "test/model", "", nil)))
}

// altWindow returns the alt+<n> keypress that switches straight to
// the window at the given one-based sidebar position, which the
// binding covers up to 9.
func altWindow(position int) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(strconv.Itoa(position)), Alt: true}
}

// TestChatScreen_inbound_dm_opens_a_query_window pins first contact.
// A model the user has never messaged sends them a DM: nothing in
// the UI has a window for it, and the line has to open one rather
// than disappear. This is autocreate-on-message, the behaviour every
// IRC client has for an unsolicited private message.
func TestChatScreen_inbound_dm_opens_a_query_window(t *testing.T) {
	h := newTestSession(t)
	uitest.SeedChannel(t, h.user, "#general")

	tm := newChatApp(t, h)
	waitForChannelSeedDrain(tm)

	// The sender shares no channel with the user, so the sidebar
	// entry can only have come from the DM itself.
	bot := testclient.New("botty", h.sess, testclient.WithInstanceID("inst-botty"))
	require.NoError(t, bot.Attach(t.Context()))
	t.Cleanup(bot.Detach)

	resp, err := bot.Send(t.Context(), protocol.PrivMsg{
		Target: protocol.NickTarget("testuser"),
		Body:   "psst, are you there?",
	})
	require.NoError(t, err)
	require.NoError(t, resp.Err)

	// The query window appears in the sidebar, badged with the line
	// the user has not read.
	tm.WaitForViewContains("botty (1)")

	// And the line itself is in that window's scrollback: the
	// sidebar entry is worth nothing if the message that opened it
	// was dropped on the way. &modeloff, #general, botty.
	tm.Send(altWindow(3))
	tm.WaitForViewContains("psst, are you there?")
}

// TestChatScreen_inbound_dm_from_a_deleted_instance_is_narrated
// pins the one case where a delivered DM does not reach a window.
// The window is built around the counterpart's handle, so a line
// whose sender the store no longer holds has nowhere to go; a KILL
// landing between the delivery and the lookup is how that happens.
// The line is discarded, and `&modeloff` says so, naming the id: a
// message the server delivered never disappears in silence.
//
// `/help` is what brings `&modeloff` into view on a session with no
// channels, so the notice has somewhere to render.
func TestChatScreen_inbound_dm_from_a_deleted_instance_is_narrated(t *testing.T) {
	h := newTestSession(t)

	tm := newChatApp(t, h)
	tm.WaitFor("Welcome to modeloff")

	tm.Submit("/help")
	tm.WaitFor("/query")

	screenstest.SendProtocolEvent(tm.TestModel, domain.Message{
		Target:     domain.ChannelName(protocol.UserClientID),
		From:       "ghost",
		InstanceID: "inst-ghost",
		Body:       "anybody there?",
		At:         time.Now(),
	}, nil)

	tm.WaitFor("Dropped 1 line(s) from inst-ghost: no such instance.")
}

// TestChatScreen_inbound_dm_is_recorded_for_the_next_run pins that
// a window the user did not open themselves still joins the set the
// next run reopens.
func TestChatScreen_inbound_dm_is_recorded_for_the_next_run(t *testing.T) {
	h := newTestSession(t)
	uitest.SeedChannel(t, h.user, "#general")

	tm := newChatApp(t, h)
	waitForChannelSeedDrain(tm)

	bot := testclient.New("botty", h.sess, testclient.WithInstanceID("inst-botty"))
	require.NoError(t, bot.Attach(t.Context()))
	t.Cleanup(bot.Detach)

	resp, err := bot.Send(t.Context(), protocol.PrivMsg{
		Target: protocol.NickTarget("testuser"),
		Body:   "psst, are you there?",
	})
	require.NoError(t, err)
	require.NoError(t, resp.Err)

	tm.WaitForViewContains("botty")

	requireOpenDMWindows(t, h, "inst-botty")
}

// TestChatScreen_Init_reopens_recorded_dm_windows pins the bootstrap
// half of the same record. A channel comes back through autojoin,
// which announces itself with a JOIN; a DM window is not a
// membership the server holds, so without this the user returns to a
// sidebar missing every conversation they had open.
func TestChatScreen_Init_reopens_recorded_dm_windows(t *testing.T) {
	h := newTestSession(t)
	uitest.SeedChannel(t, h.user, "#general")
	seedInstance(t, h, "inst-botty", "botty")
	require.NoError(t, h.store.AddDMWindow(t.Context(), "inst-botty"))

	tm := newChatApp(t, h)

	tm.WaitForViewContains("#general", "botty")
}

// TestChatScreen_Init_lands_on_the_dm_window_left_open pins the
// landing half of the restore. The window the user left open is
// their standing preference wherever it was, and the bootstrap that
// lands them reads their channels, which a DM window is not one of.
func TestChatScreen_Init_lands_on_the_dm_window_left_open(t *testing.T) {
	h := newTestSession(t)
	ctx := t.Context()

	uitest.SeedChannel(t, h.user, "#general")
	seedInstance(t, h, "inst-botty", "botty")
	require.NoError(t, h.store.AddDMWindow(ctx, "inst-botty"))
	require.NoError(t, h.store.SetLastChannel(ctx, "inst-botty"))

	chatScreen, err := screens.NewChatScreen(t.Context, h.sess, h.mgr, h.user, newFakeConfigStore(), h.store, domain.KindStatus)
	require.NoError(t, err)

	tm := uitest.New(t, uipkg.NewRoot(chatScreen), teatest.WithInitialTermSize(termWidth, termHeight))

	tm.WaitForViewContains("▸botty")
}

// TestChatScreen_query_records_the_open_dm_window pins the write
// side of the same record for a window the user opened themselves.
func TestChatScreen_query_records_the_open_dm_window(t *testing.T) {
	h := newTestSession(t)
	uitest.SeedChannel(t, h.user, "#general")
	uitest.AddModel(t, h.user, "#general", "anthropic/claude-3-haiku", "")

	tm := newChatApp(t, h)
	waitForChannelAndModelSeedDrain(tm)

	tm.Submit("/query fakenick")
	tm.WaitForViewContains("fakenick")

	requireOpenDMWindows(t, h, dmWindowID(t, h, "fakenick"))
}

// TestChatScreen_close_closes_the_focused_query pins `/close` on a
// DM: the window goes, the record of it goes with it, and the user
// lands back on a channel. `/part` cannot do this, because a DM is
// not a channel and the server refuses it, so this is the command
// that makes a query window closeable at all.
func TestChatScreen_close_closes_the_focused_query(t *testing.T) {
	h := newTestSession(t)
	uitest.SeedChannel(t, h.user, "#general")
	seedInstance(t, h, "inst-botty", "botty")

	tm := newChatApp(t, h)
	waitForChannelSeedDrain(tm)

	tm.Submit("/query botty")
	tm.WaitForViewContains("botty")

	tm.Submit("/close")
	view := tm.WaitForView(func(view string) bool {
		return !strings.Contains(view, "botty")
	})
	require.Contains(t, view, "#general")

	requireOpenDMWindows(t, h)
}

// TestChatScreen_close_alias_closes_the_focused_query pins the two
// aliases every IRC client offers for this: irssi's `/wc` and the
// `/unquery` that undoes a `/query`.
func TestChatScreen_close_alias_closes_the_focused_query(t *testing.T) {
	tests := []string{"/wc", "/unquery"}

	for _, alias := range tests {
		t.Run(alias, func(t *testing.T) {
			h := newTestSession(t)
			uitest.SeedChannel(t, h.user, "#general")
			seedInstance(t, h, "inst-botty", "botty")

			tm := newChatApp(t, h)
			waitForChannelSeedDrain(tm)

			tm.Submit("/query botty")
			tm.WaitForViewContains("botty")

			tm.Submit(alias)
			tm.WaitForView(func(view string) bool {
				return !strings.Contains(view, "botty")
			})
		})
	}
}

// TestChatScreen_close_parts_a_channel_window pins the other half of
// the rule: a channel window exists because the user is in the
// channel, so closing it is leaving. This is what `/wc` does in
// irssi and what `/close` does in WeeChat.
func TestChatScreen_close_parts_a_channel_window(t *testing.T) {
	h := newTestSession(t)
	uitest.SeedChannel(t, h.user, "#general")

	tm := newChatApp(t, h)
	waitForChannelSeedDrain(tm)

	tm.Submit("/close")
	tm.WaitFor("Welcome to modeloff", "No channels joined")

	require.Zero(t, h.user.JoinedAt("#general"))
}

// TestChatScreen_close_refuses_the_status_window pins the one window
// that has no close: `&modeloff` is the client's own view of the
// server and lives as long as the session does. The `/help` reply is
// what brings that window into focus on a session with no channels.
func TestChatScreen_close_refuses_the_status_window(t *testing.T) {
	h := newTestSession(t)

	tm := newChatApp(t, h)
	tm.WaitFor("Welcome to modeloff")

	tm.Submit("/help")
	tm.WaitFor("/close")

	tm.Submit("/close")
	tm.WaitFor("&modeloff stays open")
}

// requireOpenDMWindows asserts the client-owned record of open DM
// windows holds exactly `want`.
func requireOpenDMWindows(t *testing.T, h *testHarness, want ...domain.InstanceID) {
	t.Helper()

	if want == nil {
		want = []domain.InstanceID{}
	}

	require.Eventually(t, func() bool {
		open, err := h.store.ListDMWindows(t.Context())
		if err != nil {
			return false
		}

		return slices.Equal(open, want)
	}, time.Second, 10*time.Millisecond,
		"open DM windows should be %v", want)
}

// dmWindowID resolves a nick to the instance id a DM window with it
// is keyed by.
func dmWindowID(t *testing.T, h *testHarness, nick domain.Nick) domain.InstanceID {
	t.Helper()

	inst, err := h.sess.ResolveNick(t.Context(), nick)
	require.NoError(t, err)

	return inst.ID()
}
