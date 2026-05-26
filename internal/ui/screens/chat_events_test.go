package screens_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/screens/screenstest"
	"github.com/laney/modeloff/internal/ui/uitest"
)

func TestChatScreen_PartEvent_leaving_active_switches_channel(t *testing.T) {
	h := newTestSession(t)
	user := h.user
	uitest.SeedChannel(t, user, "#general")
	uitest.SeedChannel(t, user, "#random")

	tm := newChatApp(t, h)
	// Wait for the bootstrap focus-restore to fully land on
	// #random — sidebar marker plus its scrollback rendered.
	// Calling sess.Part before that lets in-flight focus events
	// from the seeded JOIN/NAMES drain after the Part and steal
	// the visible area back to the parted channel.
	tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "▸#random") &&
			strings.Contains(view, "*** Created channel #random")
	})

	require.NoError(t, user.Part(t.Context(), "#random", ""))

	// Wait for the sidebar to settle on the post-part state:
	// scrollback shows #general (active), the `▸` marker has
	// landed on #general, and #random has been removed from
	// the sidebar. The chat-screen's `*s.active` flips
	// synchronously inside `handlePartEvent` but the sidebar
	// `ChannelActiveMsg` and `ChannelRemovedMsg` cmds are
	// delivered separately and arrive in non-deterministic
	// order under [tea.Batch].
	view := tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "▸#general") &&
			!strings.Contains(view, "#random") &&
			strings.Contains(view, "*** Created channel #general")
	})

	body, _ := uitest.SplitBodyAndStatus(view)
	columns := uitest.VisibleColumns(body)
	require.Equal(t, []string{"Channels", "&modeloff", "▸#general"}, uitest.NonEmptyColumn(columns[0]))
	require.Equal(t, []string{
		"*** Created channel #general",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(columns[1])))
}

func TestChatScreen_PartEvent_leaving_last_channel_shows_welcome(t *testing.T) {
	h := newTestSession(t)
	user := h.user
	uitest.SeedChannel(t, user, "#only")

	tm := newChatApp(t, h)
	tm.WaitFor("#only")

	require.NoError(t, user.Part(t.Context(), "#only", ""))

	tm.WaitFor(
		"Welcome to modeloff",
		"Connected as",
		"testuser",
		"/join #general",
	)
}

func TestChatScreen_PartEvent_leaving_non_active_keeps_active(t *testing.T) {
	h := newTestSession(t)
	user := h.user
	uitest.SeedChannel(t, user, "#general")
	uitest.SeedChannel(t, user, "#random")

	tm := newChatApp(t, h)
	// Wait for the bootstrap focus-restore to land on #random
	// (Init refocuses the freshest joined channel asynchronously)
	// before driving our own ChannelFocusMsg, otherwise the two
	// focuses race.
	tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "▸#random") &&
			strings.Contains(view, "*** Created channel #random")
	})

	tm.Send(chatcmd.ChannelFocusMsg{Channel: "#general", At: time.Now()})
	tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "▸#general") &&
			strings.Contains(view, "*** Created channel #general")
	})

	screenstest.SendProtocolEvent(tm.TestModel, domain.Part{
		Target:   "#random",
		Instance: user.Instance(),
		At:       time.Now(),
	}, []domain.ChannelName{"#random"})

	// Active channel should remain #general since we parted #random.
	view := tm.CurrentView()
	body, _ := uitest.SplitBodyAndStatus(view)
	require.Equal(t, []string{"Channels", "&modeloff", "▸#general", "#random"}, uitest.NonEmptyColumn(uitest.VisibleColumns(body)[0]))
}

func TestChatScreen_TopicChangeEvent_different_channel(t *testing.T) {
	h := newTestSession(t)
	user := h.user
	uitest.SeedChannel(t, user, "#general")
	uitest.SeedChannel(t, user, "#random")

	tm := newChatApp(t, h)
	// Wait for the bootstrap focus-restore to land on #random
	// before driving our own ChannelFocusMsg, otherwise the two
	// focuses race.
	tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "▸#random") &&
			strings.Contains(view, "*** Created channel #random")
	})

	tm.Send(chatcmd.ChannelFocusMsg{Channel: "#general", At: time.Now()})
	tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "▸#general") &&
			strings.Contains(view, "*** Created channel #general")
	})

	screenstest.SendProtocolEvent(tm.TestModel, domain.TopicChange{
		Target: "#random",
		Topic:  "Random topic",
		By:     "someone",
		At:     time.Now(),
	}, []domain.ChannelName{"#random"})

	view := tm.CurrentView()
	body, _ := uitest.SplitBodyAndStatus(view)
	require.Equal(t, []string{
		"*** Created channel #general",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(uitest.VisibleColumns(body)[1])))
}

func TestChatScreen_QuitEvent_shows_quit_message(t *testing.T) {
	h := newTestSession(t)
	sess := h.sess
	user := h.user
	uitest.SeedChannel(t, user, "#general")

	uitest.AddModel(t, user, "#general", "anthropic/claude-3-haiku", "")

	inst, err := sess.ResolveNick(t.Context(), "fakenick")
	require.NoError(t, err)

	tm := newChatApp(t, h)
	// Wait for focus-restore to settle and the seed events to drain
	// into the chat screen's buffer so the quit banner has stable
	// context to render against.
	tm.WaitFor("Created channel #general", "fakenick has joined #general")

	screenstest.SendProtocolEvent(tm.TestModel, domain.Quit{
		Nick:     inst.Nick(),
		Instance: inst,
		Message:  "shutting down",
		At:       time.Now(),
	}, []domain.ChannelName{"#general"})

	tm.WaitFor("fakenick has quit (shutting down)")
}

func TestChatScreen_QuitEvent_removes_instance_from_nick_list(t *testing.T) {
	t.Skip("Pending MessageList redesign: events arrive via two paths today" +
		" — `bufferEvent` appends to `s.scrollback`, and the active-channel" +
		" handler emits a live `StoredEvent` for `MessageList.appendEvent`." +
		" When `handleChannelFocus.scrollbackCmd` snapshots scrollback for" +
		" a `HistoryLoadedMsg`, any event also live-appended renders twice." +
		" The fix is to drop the dual-render: have MessageList read from a" +
		" `func() []domain.StoredEvent` getter pointed at the chat-screen's" +
		" scrollback, removing `HistoryLoadedMsg`/`loadHistory` entirely.")

	h := newTestSession(t)
	sess := h.sess
	user := h.user
	uitest.SeedChannel(t, user, "#general")

	uitest.AddModel(t, user, "#general", "anthropic/claude-3-haiku", "")

	inst, err := sess.ResolveNick(t.Context(), "fakenick")
	require.NoError(t, err)

	tm := newChatApp(t, h)
	// Wait for focus-restore to settle and the seed events to drain
	// into the chat screen's buffer so the quit banner has stable
	// context to render against.
	tm.WaitFor("Created channel #general", "fakenick has joined #general")

	screenstest.SendProtocolEvent(tm.TestModel, domain.Quit{
		Nick:     inst.Nick(),
		Instance: inst,
		At:       time.Now(),
	}, []domain.ChannelName{"#general"})

	view := tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "fakenick has quit")
	})
	body, _ := uitest.SplitBodyAndStatus(view)
	require.Equal(t, []string{
		"*** Created channel #general",
		"*** fakenick has joined #general",
		"*** fakenick has quit",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(uitest.VisibleColumns(body)[1])))
}

// TestChatScreen_QuitEvent_surfaces_in_open_DM exercises the
// frontend policy that a channel-scoped QUIT for an actor the
// user has an open DM with is also rendered in that DM's
// scrollback. The session emits one Quit (RFC-aligned) and the
// chat screen surfaces it into both the channel and the DM,
// without duplication when multiple channels are listed.
func TestChatScreen_QuitEvent_surfaces_in_open_DM(t *testing.T) {
	h := newTestSession(t)
	sess := h.sess
	user := h.user
	uitest.SeedChannel(t, user, "#general")
	uitest.AddModel(t, user, "#general", "anthropic/claude-3-haiku", "")

	inst, err := sess.ResolveNick(t.Context(), "fakenick")
	require.NoError(t, err)

	tm := newChatApp(t, h)
	tm.WaitFor("Created channel #general", "fakenick has joined #general")

	// Open a DM with fakenick and switch focus to it. /query
	// returns a chatcmd.DMOpenedMsg that the chat screen handles
	// by inserting the DM into its sidebar cache and focusing;
	// wait for the sidebar focus marker on the DM to confirm
	// the focus has actually landed before sending the QUIT.
	tm.Submit("/query fakenick")
	tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "▸fakenick")
	})

	screenstest.SendProtocolEvent(tm.TestModel, domain.Quit{
		Nick:     inst.Nick(),
		Instance: inst,
		Message:  "shutting down",
		At:       time.Now(),
	}, []domain.ChannelName{"#general"})

	// The active window is the DM. The QUIT line should land
	// here as a frontend policy choice — the user wants to see
	// "fakenick has quit" in the DM they had open, not just in
	// the channel scrollback.
	tm.WaitFor("fakenick has quit (shutting down)")
}

// TestChatScreen_NickChangeEvent_surfaces_in_open_DM mirrors the
// Quit-in-DM coverage for nick changes. The wire payload carries
// no target — NICK is actor-scoped. The chat-screen routes the
// line into every channel where it knows the actor was a member
// and into any open DM whose counterpart is the actor: when an
// instance the user has an open DM with renames, the "is now
// known as" line appears in that DM alongside the channel
// scrollback.
func TestChatScreen_NickChangeEvent_surfaces_in_open_DM(t *testing.T) {
	h := newTestSession(t)
	sess := h.sess
	user := h.user
	uitest.SeedChannel(t, user, "#general")
	uitest.AddModel(t, user, "#general", "anthropic/claude-3-haiku", "")

	inst, err := sess.ResolveNick(t.Context(), "fakenick")
	require.NoError(t, err)

	tm := newChatApp(t, h)
	tm.WaitFor("Created channel #general", "fakenick has joined #general")

	// Open a DM with fakenick and switch focus to it.
	tm.Submit("/query fakenick")
	tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "▸fakenick")
	})

	// `Instance.Nick()` is already the new value by the time the
	// event reaches the chat-screen — the session renames before
	// emitting. Mirror that by setting the live nick on the
	// canonical handle before dispatch.
	inst.SetNick("renamedbot")

	screenstest.SendProtocolEvent(tm.TestModel, domain.NickChange{
		OldNick:  "fakenick",
		NewNick:  "renamedbot",
		Instance: inst,
		At:       time.Now(),
	}, []domain.ChannelName{"#general"})

	tm.WaitFor("fakenick is now known as renamedbot")
}

func TestChatScreen_ignores_join_for_unknown_channel(t *testing.T) {
	h := newTestSession(t)
	user := h.user
	uitest.SeedChannel(t, user, "#general")

	tm := newChatApp(t, h)
	// Wait for focus-restore to settle so subsequent MessageEvents for
	// #general render in the active channel rather than accruing as
	// unread.
	tm.WaitFor("Created channel #general")

	// A model joins a channel the user isn't in.
	screenstest.SendProtocolEvent(tm.TestModel, domain.Join{
		Target:   "#secret",
		Instance: domain.NewModelInstance("bot-1", "botty", "test/model", "", nil),
		At:       time.Now(),
	}, []domain.ChannelName{"#secret"})

	// Send a subsequent event to #general to ensure the join event
	// has been fully processed before we inspect the view.
	tm.Send(domain.Message{
		Target: "#general",
		From:   "alice",
		Body:   "sync marker",
		At:     time.Now(),
	})
	tm.WaitFor("sync marker")

	// The sidebar should NOT show #secret.
	view := tm.CurrentView()
	body, _ := uitest.SplitBodyAndStatus(view)
	require.Equal(t, []string{"Channels", "&modeloff", "▸#general"}, uitest.NonEmptyColumn(uitest.VisibleColumns(body)[0]))
}

func TestChatScreen_model_join_does_not_switch_active(t *testing.T) {
	h := newTestSession(t)
	user := h.user
	uitest.SeedChannel(t, user, "#general")
	uitest.SeedChannel(t, user, "#random")

	tm := newChatApp(t, h)
	// Wait for the bootstrap focus-restore to land on #random
	// before driving our own ChannelFocusMsg, otherwise the two
	// focuses race.
	tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "▸#random") &&
			strings.Contains(view, "*** Created channel #random")
	})

	// Switch to #general so it's the active channel.
	tm.Send(chatcmd.ChannelFocusMsg{Channel: "#general", At: time.Now()})
	tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "▸#general") &&
			strings.Contains(view, "*** Created channel #general")
	})

	// A model joins #random (which the user is in).
	screenstest.SendProtocolEvent(tm.TestModel, domain.Join{
		Target:   "#random",
		Instance: domain.NewModelInstance("bot-1", "botty", "test/model", "", nil),
		At:       time.Now(),
	}, []domain.ChannelName{"#random"})

	// Send a subsequent event to ensure the join event has been processed.
	tm.Send(domain.Message{
		Target: "#general",
		From:   "alice",
		Body:   "sync marker",
		At:     time.Now(),
	})
	tm.WaitFor("sync marker")

	// Active channel should remain #general — the view should show
	// #general's content, not #random's.
	view := tm.CurrentView()
	body, _ := uitest.SplitBodyAndStatus(view)
	require.Equal(t, []string{
		"*** Created channel #general",
		"<alice> sync marker",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(uitest.VisibleColumns(body)[1])))
}

func TestChatScreen_rapid_switch_does_not_revert(t *testing.T) {
	h := newTestSession(t)
	user := h.user
	uitest.SeedChannel(t, user, "#general")
	uitest.SeedChannel(t, user, "#random")
	uitest.SeedChannel(t, user, "#chat")

	tm := newChatApp(t, h)
	// Wait for startup focus-restore to settle: #chat was seeded last,
	// so Init's LastChannel+FocusChannel path lands on #chat. The
	// explicit focus below used to drive that transition in the old
	// Init flow; now it would just replay against the already-active
	// channel.
	tm.WaitFor("Created channel #chat")

	// Simulate rapid switch: JoinEvents from two switches arrive
	// back to back. With the fix, these no longer change the active
	// channel — they only update the sidebar.
	screenstest.SendProtocolEvent(tm.TestModel, domain.Join{
		Target:   "#random",
		Instance: user.Instance(),
		At:       time.Now(),
	}, []domain.ChannelName{"#random"})
	screenstest.SendProtocolEvent(tm.TestModel, domain.Join{
		Target:   "#general",
		Instance: user.Instance(),
		At:       time.Now(),
	}, []domain.ChannelName{"#general"})

	// Send a sync marker to #chat to ensure the JoinEvents have
	// been fully processed.
	tm.Send(domain.Message{
		Target: "#chat",
		From:   "alice",
		Body:   "sync marker",
		At:     time.Now(),
	})
	tm.WaitFor("sync marker")

	// Active channel should still be #chat — JoinEvents for the
	// user should not have switched the active channel. The sync
	// marker was sent to #chat so it should be in the final view.
	view := tm.CurrentView()
	body, _ := uitest.SplitBodyAndStatus(view)
	require.Equal(t, []string{
		"*** Created channel #chat",
		"<alice> sync marker",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(uitest.VisibleColumns(body)[1])),
		"#chat content should be visible — active channel should still be #chat")
}

func TestChatScreen_focus_new_channel_before_join_event(t *testing.T) {
	t.Skip("Pending MessageList redesign: `scrollbackCmd`'s snapshot can" +
		" arrive at the message list out of order with the explicit" +
		" `ChannelFocusMsg`, leaving an older channel's history loaded" +
		" over the newly-focused channel. The fix is to remove" +
		" `HistoryLoadedMsg`/`loadHistory` and have MessageList read" +
		" scrollback through a getter.")

	h := newTestSession(t)
	user := h.user
	uitest.SeedChannel(t, user, "#general")

	tm := newChatApp(t, h)
	// Wait for startup focus-restore to settle so the subsequent focus
	// to #newchannel isn't raced by the session's own FocusChannelEvent.
	tm.WaitFor("Created channel #general")

	// ChannelFocusEvent for a channel that hasn't been joined yet.
	// This can happen when /join triggers ChannelFocusEvent before
	// the backend JoinEvent arrives.
	tm.Send(chatcmd.ChannelFocusMsg{Channel: "#newchannel", At: time.Now()})
	tm.WaitFor("#newchannel")

	view := tm.CurrentView()
	body, _ := uitest.SplitBodyAndStatus(view)
	columns := uitest.VisibleColumns(body)
	require.Equal(t, []string{"Channels", "&modeloff", "#general", "▸#newchannel"}, uitest.NonEmptyColumn(columns[0]),
		"new channel should appear in the sidebar")
	require.Equal(t, []string{
		"No messages yet",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(columns[1])),
		"#general content should not be shown — #newchannel is active")
}

func TestChatScreen_focus_status_channel_keeps_status_identity(t *testing.T) {
	h := newTestSession(t)
	sess := h.sess
	user := h.user
	require.NoError(t, sess.Connect(t.Context()))
	uitest.SeedChannel(t, user, "#general")

	tm := newChatApp(t, h)
	tm.WaitFor("&modeloff", "Created channel #general")

	tm.Send(chatcmd.ChannelFocusMsg{Channel: domain.StatusChannelName, At: time.Now()})
	// Wait for both the sidebar marker AND the Welcome system
	// notice — matching on either alone races against either the
	// sidebar update or the Welcome-event buffer-append.
	view := tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "▸&modeloff") &&
			strings.Contains(view, "*** Welcome to modeloff")
	})
	body, _ := uitest.SplitBodyAndStatus(view)
	columns := uitest.VisibleColumns(body)

	require.Equal(t, []string{"Channels", "▸&modeloff", "#general"}, uitest.NonEmptyColumn(columns[0]))
	// `&modeloff` is a virtual server window, not a channel: no
	// members, no modes, no join/part lifecycle. The only entries
	// that land here are server-narrated wire events the chat-screen
	// wraps as system notices on the local scrollback.
	require.Equal(t, []string{
		"*** Welcome to modeloff, testuser",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(columns[1])))
	require.NotContains(t, view, "#&modeloff")
}

func TestChatScreen_MessageEvent_inactive_channel(t *testing.T) {
	h := newTestSession(t)
	user := h.user
	uitest.SeedChannel(t, user, "#general")
	uitest.SeedChannel(t, user, "#random")

	tm := newChatApp(t, h)
	// Wait for the bootstrap focus-restore to land on #random
	// before driving our own ChannelFocusMsg, otherwise the two
	// focuses race.
	tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "▸#random") &&
			strings.Contains(view, "*** Created channel #random")
	})

	// Switch to #general via ChannelFocusEvent (the authoritative
	// channel-switch mechanism).
	tm.Send(chatcmd.ChannelFocusMsg{Channel: "#general", At: time.Now()})
	tm.WaitForView(func(view string) bool {
		return strings.Contains(view, "▸#general") &&
			strings.Contains(view, "*** Created channel #general")
	})

	tm.Send(domain.Message{
		Target: "#random",
		From:   "bob",
		Body:   "hello from random",
	})

	// Send a sync marker to #general to ensure the MessageEvent
	// for #random has been fully processed.
	tm.Send(domain.Message{
		Target: "#general",
		From:   "alice",
		Body:   "sync marker",
		At:     time.Now(),
	})
	tm.WaitFor("sync marker")

	view := tm.CurrentView()
	body, _ := uitest.SplitBodyAndStatus(view)
	columns := uitest.VisibleColumns(body)
	require.Equal(t, []string{"Channels", "&modeloff", "▸#general", "#random (1)"}, uitest.NonEmptyColumn(columns[0]))
	require.Equal(t, []string{
		"*** Created channel #general",
		"<alice> sync marker",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(columns[1])))
}
