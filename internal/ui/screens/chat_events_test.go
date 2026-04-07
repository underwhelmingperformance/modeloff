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
	require.Contains(t, view, "#general")
	require.Contains(t, view, "Created channel #general")
	require.NotContains(t, view, "#random")
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

	tm.Send(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	tm.WaitFor("#general")

	tm.Send(domain.PartEvent{
		Channel: "#random",
		Nick:    "testuser",
		At:      time.Now(),
	})

	// Active channel should remain #general since we parted #random.
	view := tm.CurrentView()
	require.Contains(t, view, "#general")
}

func TestChatScreen_TopicChangeEvent_different_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	tm := newChatApp(t, sess)
	tm.WaitFor("#random")

	tm.Send(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	tm.WaitFor("#general")

	tm.Send(domain.TopicChangeEvent{
		Channel: "#random",
		Topic:   "Random topic",
		By:      "someone",
		At:      time.Now(),
	})

	view := tm.CurrentView()
	require.NotContains(t, view, "Random topic")
}

func TestChatScreen_QuitEvent_shows_quit_message(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	require.NoError(t, sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", ""))
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

	require.NoError(t, sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", ""))
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
	require.Contains(t, view, "fakenick has quit")
}

func TestChatScreen_MessageEvent_inactive_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	tm := newChatApp(t, sess)
	tm.WaitFor("#random")

	tm.Send(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	tm.WaitFor("#general")

	tm.Send(domain.MessageEvent{
		Event: domain.ChannelMessage{
			Channel: "#random",
			From:    "bob",
			Body:    "hello from random",
		},
	})

	view := tm.CurrentView()
	require.Contains(t, view, "#general")
	require.NotContains(t, view, "hello from random")
}
