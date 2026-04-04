package screens_test

import (
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
)

func TestChatScreen_JoinEvent_sets_active_channel(t *testing.T) {
	sess := newTestSession(t)
	m := initChatScreen(t, sess)

	m, cmd := m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		Created: true,
		At:      time.Now(),
	})

	require.NotNil(t, cmd)

	v := m.View(80, 24)
	require.Contains(t, v, "#general")
	require.Contains(t, v, "Created channel #general")
}

func TestChatScreen_JoinEvent_existing_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})

	require.NotNil(t, cmd)

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
	m, cmd := m.Update(domain.JoinEvent{
		Channel: "#random",
		Nick:    "testuser",
		At:      time.Now(),
	})
	require.NotNil(t, cmd)

	// Now leave #random (the active channel).
	m, cmd = m.Update(domain.PartEvent{
		Channel: "#random",
		Nick:    "testuser",
		At:      time.Now(),
	})

	require.NotNil(t, cmd)

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
	m, cmd := m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	require.NotNil(t, cmd)

	// Leave #random (not active).
	m, cmd = m.Update(domain.PartEvent{
		Channel: "#random",
		Nick:    "testuser",
		At:      time.Now(),
	})

	require.NotNil(t, cmd)

	// #general should still be active.
	v := m.View(80, 24)
	require.Contains(t, v, "#general")
}

func TestChatScreen_PartEvent_no_channels_remaining(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#only")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(domain.JoinEvent{
		Channel: "#only",
		Nick:    "testuser",
		At:      time.Now(),
	})
	require.NotNil(t, cmd)

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

	m, cmd := m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	require.NotNil(t, cmd)

	m, cmd = m.Update(domain.TopicChangeEvent{
		Channel: "#general",
		Topic:   "New topic",
		By:      "testuser",
		At:      time.Now(),
	})

	require.NotNil(t, cmd)

	processBatchedCommands(t, &m, cmd)

	v := m.View(80, 24)
	require.Contains(t, v, "New topic")
}

func TestChatScreen_TopicChangeEvent_different_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	seedChannel(t, sess, "#random")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	require.NotNil(t, cmd)

	m, cmd = m.Update(domain.TopicChangeEvent{
		Channel: "#random",
		Topic:   "Random topic",
		By:      "someone",
		At:      time.Now(),
	})

	require.NotNil(t, cmd)

	// Should not show the topic change for #random in the #general view.
	v := m.View(80, 24)
	require.NotContains(t, v, "Random topic")
}

func TestChatScreen_MessageEvent_active_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	seedMessage(t, sess, "#general", "existing message")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	require.NotNil(t, cmd)

	// Simulate a new message arriving.
	seedMessage(t, sess, "#general", "new message from event")

	m, cmd = m.Update(domain.MessageEvent{
		Message: domain.Message{
			Channel: "#general",
			From:    "alice",
			Body:    "new message from event",
		},
	})

	require.NotNil(t, cmd)

	// Process the batched commands.
	processBatchedCommands(t, &m, cmd)

	v := m.View(80, 24)
	require.Contains(t, v, "new message from event")
}

func TestChatScreen_MessageEvent_inactive_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")
	seedChannel(t, sess, "#random")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	require.NotNil(t, cmd)

	// Message arrives on a different channel.
	m, cmd = m.Update(domain.MessageEvent{
		Message: domain.Message{
			Channel: "#random",
			From:    "bob",
			Body:    "hello from random",
		},
	})

	require.NotNil(t, cmd)

	// Should not crash; active channel is still #general.
	v := m.View(80, 24)
	require.Contains(t, v, "#general")
	require.NotContains(t, v, "hello from random")
}

func TestChatScreen_ModelReplyEvent_clears_pending(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	require.NotNil(t, cmd)

	m, cmd = m.Update(domain.ModelReplyEvent{
		Message: domain.Message{
			Channel: "#general",
			From:    "botty",
			Body:    "model response",
		},
		Instance: "botty",
	})

	require.NotNil(t, cmd)

	// Process batched commands to clear pending indicator.
	processBatchedCommands(t, &m, cmd)

	v := m.View(80, 24)
	require.NotContains(t, v, "responding")
}

func TestChatScreen_DMOpenedEvent(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	_, err := sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)

	m := initChatScreen(t, sess)

	m, cmd := m.Update(domain.DMOpenedEvent{
		Channel: domain.Channel{
			Name: "fakenick",
			Kind: domain.KindDM,
		},
		Nick:    "fakenick",
		Created: true,
		At:      time.Now(),
	})

	require.NotNil(t, cmd)

	v := m.View(80, 24)
	require.Contains(t, v, "Opened direct message with fakenick")
}

func TestChatScreen_ConfigChangedEvent_with_active_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	require.NotNil(t, cmd)

	m, cmd = m.Update(domain.ConfigChangedEvent{
		Operation: "API key updated",
		At:        time.Now(),
	})

	require.NotNil(t, cmd)

	processBatchedCommands(t, &m, cmd)

	v := m.View(80, 24)
	require.Contains(t, v, "API key updated")
}

func TestChatScreen_ConfigChangedEvent_no_active_channel(t *testing.T) {
	sess := newTestSession(t)
	m := initChatScreen(t, sess)

	// No channel joined — s.active is empty.
	m, cmd := m.Update(domain.ConfigChangedEvent{
		Operation: "API key updated",
		At:        time.Now(),
	})

	// Should not crash.
	if cmd != nil {
		processBatchedCommands(t, &m, cmd)
	}

	v := m.View(80, 24)
	require.NotEmpty(t, v)
}

func TestChatScreen_ErrorEvent_with_active_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	require.NotNil(t, cmd)

	m, cmd = m.Update(domain.ErrorEvent{
		Operation: "model invocation",
		Err:       errors.New("connection refused"),
		At:        time.Now(),
	})

	require.NotNil(t, cmd)

	processBatchedCommands(t, &m, cmd)

	v := m.View(80, 24)
	require.Contains(t, v, "model invocation")
	require.Contains(t, v, "connection refused")
}

func TestChatScreen_ErrorEvent_no_active_channel(t *testing.T) {
	sess := newTestSession(t)
	m := initChatScreen(t, sess)

	m, cmd := m.Update(domain.ErrorEvent{
		Operation: "startup failure",
		Err:       errors.New("no api key"),
		At:        time.Now(),
	})

	// Should not crash.
	if cmd != nil {
		processBatchedCommands(t, &m, cmd)
	}

	v := m.View(80, 24)
	require.NotEmpty(t, v)
}

func TestChatScreen_NickChangeEvent(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	require.NotNil(t, cmd)

	m, cmd = m.Update(domain.NickChangeEvent{
		OldNick: "testuser",
		NewNick: "newnick",
		At:      time.Now(),
	})

	require.NotNil(t, cmd)

	processBatchedCommands(t, &m, cmd)

	v := m.View(80, 24)
	require.Contains(t, v, "testuser is now known as newnick")
}

func TestChatScreen_ModelInvitedEvent_active_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	m := initChatScreen(t, sess)

	m, cmd := m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	require.NotNil(t, cmd)

	m, cmd = m.Update(domain.ModelInvitedEvent{
		Channel: "#general",
		Instance: domain.ModelInstance{
			Nick:    "botty",
			ModelID: "anthropic/claude-3-haiku",
		},
		At: time.Now(),
	})

	require.NotNil(t, cmd)

	processBatchedCommands(t, &m, cmd)

	v := m.View(80, 24)
	require.Contains(t, v, "botty (anthropic/claude-3-haiku) has joined #general")
}

func TestChatScreen_ModelKickedEvent_active_channel(t *testing.T) {
	sess := newTestSession(t)
	seedChannel(t, sess, "#general")

	_, err := sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)

	m := initChatScreen(t, sess)

	m, cmd := m.Update(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	require.NotNil(t, cmd)

	m, cmd = m.Update(domain.ModelKickedEvent{
		Channel: "#general",
		Nick:    "fakenick",
		At:      time.Now(),
	})

	require.NotNil(t, cmd)

	processBatchedCommands(t, &m, cmd)

	v := m.View(80, 24)
	require.Contains(t, v, "fakenick has been kicked from #general")
}

// processBatchedCommands runs a tea.Cmd and feeds any resulting
// messages back into the model. This handles tea.Batch commands which
// produce multiple messages.
func processBatchedCommands(t *testing.T, m *ui.Model, cmd func() tea.Msg) {
	t.Helper()

	if cmd == nil {
		return
	}

	msg := cmd()
	if msg == nil {
		return
	}

	// tea.Batch returns a function that returns a tea.BatchMsg (a
	// slice of tea.Cmd). Process each sub-command.
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, sub := range batch {
			if sub == nil {
				continue
			}

			subMsg := sub()
			if subMsg == nil {
				continue
			}

			*m, _ = (*m).Update(subMsg)
		}

		return
	}

	*m, _ = (*m).Update(msg)
}
