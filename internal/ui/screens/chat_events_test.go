package screens_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/uitest"
)

func TestChatScreen_PartEvent_leaving_active_switches_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	tm := newChatApp(t, sess)
	tm.WaitFor("#random")

	// Part #random via the session — events flow through the event channel.
	require.NoError(t, sess.Part(t.Context(), "#random", ""))

	tm.WaitFor("Created channel #general")

	view := tm.CurrentView()
	body, _ := uitest.SplitBodyAndStatus(view)
	columns := uitest.VisibleColumns(body)
	require.Equal(t, []string{"Channels", "▸#general"}, uitest.NonEmptyColumn(columns[0]))
	require.Equal(t, []string{
		"*** Created channel #general",
		"*** ChanServ sets mode +o testuser",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(columns[1])))
}

func TestChatScreen_PartEvent_leaving_last_channel_shows_welcome(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#only")

	tm := newChatApp(t, sess)
	tm.WaitFor("#only")

	require.NoError(t, sess.Part(t.Context(), "#only", ""))

	tm.WaitFor(
		"Welcome to modeloff",
		"Connected as",
		"testuser",
		"/join #general",
	)
}

func TestChatScreen_PartEvent_leaving_non_active_keeps_active(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	tm := newChatApp(t, sess)
	// Wait for the startup focus-restore (Init refocuses the last-seen
	// channel asynchronously) to settle before driving a channel
	// switch, so the test's explicit ChannelFocusEvent isn't raced by
	// the session's own FocusChannelEvent replay.
	tm.WaitFor("Created channel #random")

	tm.Send(chatcmd.ChannelFocusMsg{Channel: "#general"})
	tm.WaitFor("Created channel #general")

	tm.Send(domain.Part{
		Target:   "#random",
		Instance: sess.UserInstance(),
		At:       time.Now(),
	})

	// Active channel should remain #general since we parted #random.
	view := tm.CurrentView()
	body, _ := uitest.SplitBodyAndStatus(view)
	require.Equal(t, []string{"Channels", "▸#general", "#random"}, uitest.NonEmptyColumn(uitest.VisibleColumns(body)[0]))
}

func TestChatScreen_TopicChangeEvent_different_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	tm := newChatApp(t, sess)
	// Wait for the startup focus-restore on the last-seeded channel
	// before driving our own ChannelFocusEvent, otherwise the two
	// focuses race.
	tm.WaitFor("Created channel #random")

	tm.Send(chatcmd.ChannelFocusMsg{Channel: "#general"})
	tm.WaitFor("Created channel #general")

	tm.Send(domain.TopicChange{
		Target: "#random",
		Topic:  "Random topic",
		By:     "someone",
		At:     time.Now(),
	})

	view := tm.CurrentView()
	body, _ := uitest.SplitBodyAndStatus(view)
	require.Equal(t, []string{
		"*** Created channel #general",
		"*** ChanServ sets mode +o testuser",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(uitest.VisibleColumns(body)[1])))
}

func TestChatScreen_QuitEvent_shows_quit_message(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	require.NoError(t, sess.AddModel(t.Context(), "#general", "anthropic/claude-3-haiku", ""))

	inst, err := sess.ResolveNick(t.Context(), "fakenick")
	require.NoError(t, err)

	tm := newChatApp(t, sess)
	// Wait for focus-restore to settle and the seed events to drain
	// into the chat screen's buffer so the quit banner has stable
	// context to render against.
	tm.WaitFor("Created channel #general", "fakenick has joined #general")

	tm.Send(domain.Quit{
		Channels: []domain.ChannelName{"#general"},
		Nick:     inst.Nick(),
		Instance: inst,
		Message:  "shutting down",
		At:       time.Now(),
	})

	tm.WaitFor("fakenick has quit (shutting down)")
}

func TestChatScreen_QuitEvent_removes_instance_from_nick_list(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	require.NoError(t, sess.AddModel(t.Context(), "#general", "anthropic/claude-3-haiku", ""))

	inst, err := sess.ResolveNick(t.Context(), "fakenick")
	require.NoError(t, err)

	tm := newChatApp(t, sess)
	// Wait for focus-restore to settle and the seed events to drain
	// into the chat screen's buffer so the quit banner has stable
	// context to render against.
	tm.WaitFor("Created channel #general", "fakenick has joined #general")

	tm.Send(domain.Quit{
		Channels: []domain.ChannelName{"#general"},
		Nick:     inst.Nick(),
		Instance: inst,
		At:       time.Now(),
	})

	tm.WaitFor("fakenick has quit")

	view := tm.CurrentView()
	body, _ := uitest.SplitBodyAndStatus(view)
	require.Equal(t, []string{
		"*** Created channel #general",
		"*** ChanServ sets mode +o testuser",
		"*** fakenick has joined #general",
		"*** ChanServ sets mode +v fakenick",
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
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	require.NoError(t, sess.AddModel(t.Context(), "#general", "anthropic/claude-3-haiku", ""))

	inst, err := sess.ResolveNick(t.Context(), "fakenick")
	require.NoError(t, err)

	tm := newChatApp(t, sess)
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

	tm.Send(domain.Quit{
		Channels: []domain.ChannelName{"#general"},
		Nick:     inst.Nick(),
		Instance: inst,
		Message:  "shutting down",
		At:       time.Now(),
	})

	// The active window is the DM. The QUIT line should land
	// here as a frontend policy choice — the user wants to see
	// "fakenick has quit" in the DM they had open, not just in
	// the channel scrollback.
	tm.WaitFor("fakenick has quit (shutting down)")
}

// TestChatScreen_NickChangeEvent_surfaces_in_open_DM mirrors the
// Quit-in-DM coverage for nick changes. Under the protocol
// framing, `NickChange.Channels` only lists real channels —
// DMs are not channels at the wire layer. The chat-screen layers
// in DM-window rendering on top of the wire's actor-scoped event:
// when an instance the user has an open DM with renames, the
// "is now known as" line appears in that DM alongside the
// channel scrollback.
func TestChatScreen_NickChangeEvent_surfaces_in_open_DM(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	require.NoError(t, sess.AddModel(t.Context(), "#general", "anthropic/claude-3-haiku", ""))

	inst, err := sess.ResolveNick(t.Context(), "fakenick")
	require.NoError(t, err)

	tm := newChatApp(t, sess)
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

	tm.Send(domain.NickChange{
		Channels: []domain.ChannelName{"#general"},
		OldNick:  "fakenick",
		NewNick:  "renamedbot",
		Instance: inst,
		At:       time.Now(),
	})

	tm.WaitFor("fakenick is now known as renamedbot")
}

func TestChatScreen_ignores_join_for_unknown_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	// Wait for focus-restore to settle so subsequent MessageEvents for
	// #general render in the active channel rather than accruing as
	// unread.
	tm.WaitFor("Created channel #general")

	// A model joins a channel the user isn't in.
	tm.Send(domain.Join{
		Target:   "#secret",
		Instance: domain.NewModelInstance("bot-1", "botty", "test/model", "", nil),
		At:       time.Now(),
	})

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
	require.Equal(t, []string{"Channels", "▸#general"}, uitest.NonEmptyColumn(uitest.VisibleColumns(body)[0]))
}

func TestChatScreen_model_join_does_not_switch_active(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	tm := newChatApp(t, sess)
	// Wait for startup focus-restore to settle before driving our own
	// ChannelFocusEvent, otherwise the two focuses race.
	tm.WaitFor("Created channel #random")

	// Switch to #general so it's the active channel.
	tm.Send(chatcmd.ChannelFocusMsg{Channel: "#general"})
	tm.WaitFor("Created channel #general")

	// A model joins #random (which the user is in).
	tm.Send(domain.Join{
		Target:   "#random",
		Instance: domain.NewModelInstance("bot-1", "botty", "test/model", "", nil),
		At:       time.Now(),
	})

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
		"*** ChanServ sets mode +o testuser",
		"<alice> sync marker",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(uitest.VisibleColumns(body)[1])))
}

func TestChatScreen_rapid_switch_does_not_revert(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")
	uitest.SeedChannel(t, sess, "#chat")

	tm := newChatApp(t, sess)
	// Wait for startup focus-restore to settle: #chat was seeded last,
	// so Init's LastChannel+FocusChannel path lands on #chat. The
	// explicit focus below used to drive that transition in the old
	// Init flow; now it would just replay against the already-active
	// channel.
	tm.WaitFor("Created channel #chat")

	// Simulate rapid switch: JoinEvents from two switches arrive
	// back to back. With the fix, these no longer change the active
	// channel — they only update the sidebar.
	tm.Send(domain.Join{
		Target:   "#random",
		Instance: sess.UserInstance(),
		At:       time.Now(),
	})
	tm.Send(domain.Join{
		Target:   "#general",
		Instance: sess.UserInstance(),
		At:       time.Now(),
	})

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
		"*** ChanServ sets mode +o testuser",
		"<alice> sync marker",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(uitest.VisibleColumns(body)[1])),
		"#chat content should be visible — active channel should still be #chat")
}

func TestChatScreen_focus_new_channel_before_join_event(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	// Wait for startup focus-restore to settle so the subsequent focus
	// to #newchannel isn't raced by the session's own FocusChannelEvent.
	tm.WaitFor("Created channel #general")

	// ChannelFocusEvent for a channel that hasn't been joined yet.
	// This can happen when /join triggers ChannelFocusEvent before
	// the backend JoinEvent arrives.
	tm.Send(chatcmd.ChannelFocusMsg{Channel: "#newchannel"})
	tm.WaitFor("#newchannel")

	view := tm.CurrentView()
	body, _ := uitest.SplitBodyAndStatus(view)
	columns := uitest.VisibleColumns(body)
	require.Equal(t, []string{"Channels", "#general", "▸#newchannel"}, uitest.NonEmptyColumn(columns[0]),
		"new channel should appear in the sidebar")
	require.Equal(t, []string{
		"No messages yet",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(columns[1])),
		"#general content should not be shown — #newchannel is active")
}

func TestChatScreen_focus_status_channel_keeps_status_identity(t *testing.T) {
	sess := newTestSession(t)
	require.NoError(t, sess.Connect(t.Context()))
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	tm.WaitFor("&modeloff", "Created channel #general")

	tm.Send(chatcmd.ChannelFocusMsg{Channel: domain.StatusChannelName})
	tm.WaitFor("Connected to modeloff")

	view := tm.CurrentView()
	body, _ := uitest.SplitBodyAndStatus(view)
	columns := uitest.VisibleColumns(body)

	require.Equal(t, []string{"Channels", "▸&modeloff", "#general"}, uitest.NonEmptyColumn(columns[0]))
	// `&modeloff` is a virtual server window, not a channel: no
	// members, no modes, no join/part lifecycle. The only entries
	// that land here are the server-narrated notices the session
	// records via `appendStatus`.
	require.Equal(t, []string{
		"*** Connected to modeloff",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(columns[1])))
	require.NotContains(t, view, "#&modeloff")
}

func TestChatScreen_MessageEvent_inactive_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	tm := newChatApp(t, sess)
	// Wait for startup focus-restore to settle before switching.
	tm.WaitFor("Created channel #random")

	// Switch to #general via ChannelFocusEvent (the authoritative
	// channel-switch mechanism).
	tm.Send(chatcmd.ChannelFocusMsg{Channel: "#general"})
	tm.WaitFor("Created channel #general")

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
	require.Equal(t, []string{"Channels", "▸#general", "#random (2)"}, uitest.NonEmptyColumn(columns[0]))
	require.Equal(t, []string{
		"*** Created channel #general",
		"*** ChanServ sets mode +o testuser",
		"<alice> sync marker",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(columns[1])))
}
