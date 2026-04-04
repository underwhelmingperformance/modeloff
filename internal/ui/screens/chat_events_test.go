package screens_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

func TestChatScreen_JoinEvent_sets_active_channel(t *testing.T) {
	sess := newTestSession(t)
	m := initChatScreen(t, sess)

	m, _ = m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		Created: true,
		At:      time.Now(),
	})

	v := m.View(80, 24)
	require.Contains(t, v, "#general")
	require.Contains(t, v, "Created channel #general")
}

func TestChatScreen_JoinEvent_existing_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, _ = m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})

	v := m.View(80, 24)
	require.Contains(t, v, "#general")
	require.Contains(t, v, "testuser has joined #general")
}

func TestChatScreen_PartEvent_leaving_active_switches_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	seedChannel(t, sess, "#random")

	m := initChatScreen(t, sess)

	// Join #random to make it active.
	m, _ = m.Update(domain.JoinEvent{
		Channel: "#random",
		Nick:    "testuser",
		At:      time.Now(),
	})

	// Now leave #random (the active channel).
	m, _ = m.Update(domain.PartEvent{
		Channel: "#random",
		Nick:    "testuser",
		At:      time.Now(),
	})

	// Should switch to the first remaining channel.
	v := m.View(80, 24)
	require.Contains(t, v, "#general")
}

func TestChatScreen_PartEvent_leaving_non_active_keeps_active(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	seedChannel(t, sess, "#random")

	m := initChatScreen(t, sess)

	// Make #general active.
	m, _ = m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})

	// Leave #random (not active).
	m, _ = m.Update(domain.PartEvent{
		Channel: "#random",
		Nick:    "testuser",
		At:      time.Now(),
	})

	// #general should still be active.
	v := m.View(80, 24)
	require.Contains(t, v, "#general")
}

func TestChatScreen_PartEvent_no_channels_remaining(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#only")

	m := initChatScreen(t, sess)

	m, _ = m.Update(domain.JoinEvent{
		Channel: "#only",
		Nick:    "testuser",
		At:      time.Now(),
	})

	m, _ = m.Update(domain.PartEvent{
		Channel: "#only",
		Nick:    "testuser",
		At:      time.Now(),
	})

	// Should render without crashing, even with no active channel.
	v := m.View(80, 24)
	require.NotEmpty(t, v)
}

func TestChatScreen_TopicChangeEvent_active_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, _ = m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})

	m, _ = m.Update(domain.TopicChangeEvent{
		Channel: "#general",
		Topic:   "New topic",
		By:      "testuser",
		At:      time.Now(),
	})

	v := m.View(80, 24)
	require.Contains(t, v, "New topic")
}

func TestChatScreen_TopicChangeEvent_different_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	seedChannel(t, sess, "#random")

	m := initChatScreen(t, sess)

	m, _ = m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})

	m, _ = m.Update(domain.TopicChangeEvent{
		Channel: "#random",
		Topic:   "Random topic",
		By:      "someone",
		At:      time.Now(),
	})

	// Should not show the topic change for #random in the #general view.
	v := m.View(80, 24)
	require.NotContains(t, v, "Random topic")
}

func TestChatScreen_MessageEvent_active_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	seedMessage(t, sess, "#general", "existing message")

	m := initChatScreen(t, sess)

	m, _ = m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})

	// Simulate a new message arriving.
	seedMessage(t, sess, "#general", "new message from event")

	m, _ = m.Update(domain.MessageEvent{
		Message: domain.Message{
			Channel: "#general",
			From:    "alice",
			Body:    "new message from event",
		},
	})

	// Process the batched commands.

	v := m.View(80, 24)
	require.Contains(t, v, "new message from event")
}

func TestChatScreen_MessageEvent_inactive_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	seedChannel(t, sess, "#random")

	m := initChatScreen(t, sess)

	m, _ = m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})

	// Message arrives on a different channel.
	m, _ = m.Update(domain.MessageEvent{
		Message: domain.Message{
			Channel: "#random",
			From:    "bob",
			Body:    "hello from random",
		},
	})

	// Should not crash; active channel is still #general.
	v := m.View(80, 24)
	require.Contains(t, v, "#general")
	require.NotContains(t, v, "hello from random")
}

func TestChatScreen_ModelReplyEvent_clears_pending(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, _ = m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})

	m, _ = m.Update(domain.ModelReplyEvent{
		Message: domain.Message{
			Channel: "#general",
			From:    "botty",
			Body:    "model response",
		},
		Instance: "botty",
	})

	// Process batched commands to clear pending indicator.

	v := m.View(80, 24)
	require.NotContains(t, v, "responding")
}

func TestChatScreen_DMOpenedEvent(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	_, err := sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)

	m := initChatScreen(t, sess)

	m, _ = m.Update(domain.DMOpenedEvent{
		Channel: domain.Channel{
			Name: "fakenick",
			Kind: domain.KindDM,
		},
		Nick:    "fakenick",
		Created: true,
		At:      time.Now(),
	})

	v := m.View(80, 24)
	require.Contains(t, v, "Opened direct message with fakenick")
}

func TestChatScreen_ConfigChangedEvent_with_active_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, _ = m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})

	m, _ = m.Update(domain.ConfigChangedEvent{
		Operation: "API key updated",
		At:        time.Now(),
	})

	v := m.View(80, 24)
	require.Contains(t, v, "API key updated")
}

func TestChatScreen_ConfigChangedEvent_no_active_channel(t *testing.T) {
	sess := newTestSession(t)
	m := initChatScreen(t, sess)

	// No channel joined — s.active is empty.
	m, _ = m.Update(domain.ConfigChangedEvent{
		Operation: "API key updated",
		At:        time.Now(),
	})

	// Should not crash.
	v := m.View(80, 24)
	require.NotEmpty(t, v)
}

func TestChatScreen_ErrorEvent_with_active_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, _ = m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})

	m, _ = m.Update(domain.ErrorEvent{
		Operation: "model invocation",
		Err:       errors.New("connection refused"),
		At:        time.Now(),
	})

	v := m.View(80, 24)
	require.Contains(t, v, "model invocation")
	require.Contains(t, v, "connection refused")
}

func TestChatScreen_ErrorEvent_no_active_channel(t *testing.T) {
	sess := newTestSession(t)
	m := initChatScreen(t, sess)

	m, _ = m.Update(domain.ErrorEvent{
		Operation: "startup failure",
		Err:       errors.New("no api key"),
		At:        time.Now(),
	})

	// Should not crash.
	v := m.View(80, 24)
	require.NotEmpty(t, v)
}

func TestChatScreen_NickChangeEvent(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, _ = m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})

	m, _ = m.Update(domain.NickChangeEvent{
		OldNick: "testuser",
		NewNick: "newnick",
		At:      time.Now(),
	})

	v := m.View(80, 24)
	require.Contains(t, v, "testuser is now known as newnick")
}

func TestChatScreen_ModelInvitedEvent_active_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, _ = m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})

	m, _ = m.Update(domain.ModelInvitedEvent{
		Channel: "#general",
		Instance: domain.ModelInstance{
			Nick:    "botty",
			ModelID: "anthropic/claude-3-haiku",
		},
		At: time.Now(),
	})

	v := m.View(80, 24)
	require.Contains(t, v, "botty (anthropic/claude-3-haiku) has joined #general")
}

func TestChatScreen_ModelKickedEvent_active_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	_, err := sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)

	m := initChatScreen(t, sess)

	m, _ = m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})

	m, _ = m.Update(domain.ModelKickedEvent{
		Channel: "#general",
		Nick:    "fakenick",
		At:      time.Now(),
	})

	v := m.View(80, 24)
	require.Contains(t, v, "fakenick has been kicked from #general")
}
