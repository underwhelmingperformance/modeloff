package screens_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
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
	body, _ := splitBodyAndStatus(view)
	columns := uitest.VisibleColumns(body)
	require.Equal(t, []string{"Channels", "▸#general (2)"}, uitest.NonEmptyColumn(columns[0]))
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
	tm.WaitFor("#random")

	tm.Send(domain.ChannelFocusEvent{Channel: "#general"})
	tm.WaitFor("Created channel #general")

	tm.Send(domain.PartEvent{
		Channel: "#random",
		Nick:    "testuser",
		At:      time.Now(),
	})

	// Active channel should remain #general since we parted #random.
	view := tm.CurrentView()
	body, _ := splitBodyAndStatus(view)
	require.Equal(t, []string{"Channels", "▸#general", "#random (2)"}, uitest.NonEmptyColumn(uitest.VisibleColumns(body)[0]))
}

func TestChatScreen_TopicChangeEvent_different_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	tm := newChatApp(t, sess)
	tm.WaitFor("#random")

	tm.Send(domain.ChannelFocusEvent{Channel: "#general"})
	tm.WaitFor("Created channel #general")

	tm.Send(domain.TopicChangeEvent{
		Channel: "#random",
		Topic:   "Random topic",
		By:      "someone",
		At:      time.Now(),
	})

	view := tm.CurrentView()
	body, _ := splitBodyAndStatus(view)
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
	uitest.DrainEvents(sess)

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Send(domain.QuitEvent{
		Nick:    "fakenick",
		Message: "shutting down",
		At:      time.Now(),
	})

	tm.WaitFor("fakenick has quit (shutting down)")
}

func TestChatScreen_QuitEvent_removes_instance_from_nick_list(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	require.NoError(t, sess.AddModel(t.Context(), "#general", "anthropic/claude-3-haiku", ""))
	uitest.DrainEvents(sess)

	tm := newChatApp(t, sess)
	tm.WaitFor("#general", "fakenick")

	tm.Send(domain.QuitEvent{
		Nick:    "fakenick",
		Message: "",
		At:      time.Now(),
	})

	tm.WaitFor("fakenick has quit")

	view := tm.CurrentView()
	body, _ := splitBodyAndStatus(view)
	require.Equal(t, []string{
		"*** Created channel #general",
		"*** ChanServ sets mode +o testuser",
		"*** fakenick has joined #general",
		"*** ChanServ sets mode +v fakenick",
		"*** fakenick has quit",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(uitest.VisibleColumns(body)[1])))
}

func TestChatScreen_ignores_join_for_unknown_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	// A model joins a channel the user isn't in.
	tm.Send(domain.JoinEvent{
		Channel: "#secret",
		Nick:    "botty",
		At:      time.Now(),
	})

	// Send a subsequent event to #general to ensure the join event
	// has been fully processed before we inspect the view.
	tm.Send(domain.MessageEvent{
		Event: domain.ChannelMessage{
			Channel: "#general",
			From:    "alice",
			Body:    "sync marker",
			At:      time.Now(),
		},
	})
	tm.WaitFor("sync marker")

	// The sidebar should NOT show #secret.
	view := tm.CurrentView()
	body, _ := splitBodyAndStatus(view)
	require.Equal(t, []string{"Channels", "▸#general (2)"}, uitest.NonEmptyColumn(uitest.VisibleColumns(body)[0]))
}

func TestChatScreen_model_join_does_not_switch_active(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	tm := newChatApp(t, sess)
	tm.WaitFor("#random")

	// Switch to #general so it's the active channel.
	tm.Send(domain.ChannelFocusEvent{Channel: "#general"})
	tm.WaitFor("Created channel #general")

	// A model joins #random (which the user is in).
	tm.Send(domain.JoinEvent{
		Channel: "#random",
		Nick:    "botty",
		At:      time.Now(),
	})

	// Send a subsequent event to ensure the join event has been processed.
	tm.Send(domain.MessageEvent{
		Event: domain.ChannelMessage{
			Channel: "#general",
			From:    "alice",
			Body:    "sync marker",
			At:      time.Now(),
		},
	})
	tm.WaitFor("sync marker")

	// Active channel should remain #general — the view should show
	// #general's content, not #random's.
	view := tm.CurrentView()
	body, _ := splitBodyAndStatus(view)
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
	tm.WaitFor("#chat")

	// Switch to #chat so it's the active channel.
	tm.Send(domain.ChannelFocusEvent{Channel: "#chat"})
	tm.WaitFor("Created channel #chat")

	// Simulate rapid switch: JoinEvents from two switches arrive
	// back to back. With the fix, these no longer change the active
	// channel — they only update the sidebar.
	tm.Send(domain.JoinEvent{
		Channel: "#random",
		Nick:    "testuser",
		At:      time.Now(),
	})
	tm.Send(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})

	// Send a sync marker to #chat to ensure the JoinEvents have
	// been fully processed.
	tm.Send(domain.MessageEvent{
		Event: domain.ChannelMessage{
			Channel: "#chat",
			From:    "alice",
			Body:    "sync marker",
			At:      time.Now(),
		},
	})
	tm.WaitFor("sync marker")

	// Active channel should still be #chat — JoinEvents for the
	// user should not have switched the active channel. The sync
	// marker was sent to #chat so it should be in the final view.
	view := tm.CurrentView()
	body, _ := splitBodyAndStatus(view)
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
	tm.WaitFor("#general")

	// ChannelFocusEvent for a channel that hasn't been joined yet.
	// This can happen when /join triggers ChannelFocusEvent before
	// the backend JoinEvent arrives.
	tm.Send(domain.ChannelFocusEvent{Channel: "#newchannel"})
	tm.WaitFor("#newchannel")

	view := tm.CurrentView()
	body, _ := splitBodyAndStatus(view)
	columns := uitest.VisibleColumns(body)
	require.Equal(t, []string{"Channels", "#general (2)", "▸#newchannel"}, uitest.NonEmptyColumn(columns[0]),
		"new channel should appear in the sidebar")
	require.Equal(t, []string{
		"No messages yet",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(columns[1])),
		"#general content should not be shown — #newchannel is active")
}

func TestChatScreen_MessageEvent_inactive_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	tm := newChatApp(t, sess)
	tm.WaitFor("#random")

	// Switch to #general via ChannelFocusEvent (the authoritative
	// channel-switch mechanism).
	tm.Send(domain.ChannelFocusEvent{Channel: "#general"})
	tm.WaitFor("Created channel #general")

	tm.Send(domain.MessageEvent{
		Event: domain.ChannelMessage{
			Channel: "#random",
			From:    "bob",
			Body:    "hello from random",
		},
	})

	// Send a sync marker to #general to ensure the MessageEvent
	// for #random has been fully processed.
	tm.Send(domain.MessageEvent{
		Event: domain.ChannelMessage{
			Channel: "#general",
			From:    "alice",
			Body:    "sync marker",
			At:      time.Now(),
		},
	})
	tm.WaitFor("sync marker")

	view := tm.CurrentView()
	body, _ := splitBodyAndStatus(view)
	columns := uitest.VisibleColumns(body)
	require.Equal(t, []string{"Channels", "▸#general", "#random (2)"}, uitest.NonEmptyColumn(columns[0]))
	require.Equal(t, []string{
		"*** Created channel #general",
		"*** ChanServ sets mode +o testuser",
		"<alice> sync marker",
		"testuser >",
	}, normaliseContent(uitest.NonEmptyColumn(columns[1])))
}
