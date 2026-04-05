package screens_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/uitest"
)

func TestChatScreen_JoinEvent_sets_active_channel(t *testing.T) {
	sess := newTestSession(t)
	tm := newChatApp(t, sess)
	tm.WaitFor("Welcome to modeloff")

	tm.Send(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		Created: true,
		At:      time.Now(),
	})

	tm.WaitFor("#general", "Created channel #general")

	view := tm.FinalView()
	require.Contains(t, view, "#general")
	require.Contains(t, view, "Created channel #general")
}

func TestChatScreen_JoinEvent_existing_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Send(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})

	tm.WaitFor("testuser has joined #general")

	view := tm.FinalView()
	require.Contains(t, view, "#general")
	require.Contains(t, view, "testuser has joined #general")
}

func TestChatScreen_PartEvent_leaving_active_switches_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	tm := newChatApp(t, sess)
	tm.WaitFor("#random")

	tm.Send(domain.JoinEvent{
		Channel: "#random",
		Nick:    "testuser",
		At:      time.Now(),
	})
	tm.WaitFor("testuser has joined #random")

	tm.Send(domain.PartEvent{
		Channel: "#random",
		Nick:    "testuser",
		At:      time.Now(),
	})

	tm.WaitFor("#general")

	view := tm.FinalView()
	require.Contains(t, view, "#general")
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

	view := tm.FinalView()
	require.Contains(t, view, "#general")
}

func TestChatScreen_PartEvent_no_channels_remaining(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#only")

	tm := newChatApp(t, sess)
	tm.WaitFor("#only")

	tm.Send(domain.JoinEvent{
		Channel: "#only",
		Nick:    "testuser",
		At:      time.Now(),
	})
	tm.WaitFor("testuser has joined #only")

	tm.Send(domain.PartEvent{
		Channel: "#only",
		Nick:    "testuser",
		At:      time.Now(),
	})

	view := tm.FinalView()
	require.NotEmpty(t, view)
}

func TestChatScreen_TopicChangeEvent_active_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Send(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	tm.WaitFor("testuser has joined #general")

	tm.Send(domain.TopicChangeEvent{
		Channel: "#general",
		Topic:   "New topic",
		By:      "testuser",
		At:      time.Now(),
	})

	tm.WaitFor("New topic")

	view := tm.FinalView()
	require.Contains(t, view, "New topic")
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

func TestChatScreen_MessageEvent_active_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedMessage(t, sess, "#general", "existing message")

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Send(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	tm.WaitFor("testuser has joined #general")

	uitest.SeedMessage(t, sess, "#general", "new message from event")

	tm.Send(domain.MessageEvent{
		Message: domain.Message{
			Channel: "#general",
			From:    "alice",
			Body:    "new message from event",
		},
	})

	tm.WaitFor("new message from event")

	view := tm.FinalView()
	require.Contains(t, view, "new message from event")
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
		Message: domain.Message{
			Channel: "#random",
			From:    "bob",
			Body:    "hello from random",
		},
	})

	view := tm.CurrentView()
	require.Contains(t, view, "#general")
	require.NotContains(t, view, "hello from random")
}

func TestChatScreen_ModelReplyEvent_clears_pending(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Send(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	tm.WaitFor("testuser has joined #general")

	tm.Send(domain.ModelReplyEvent{
		Message: domain.Message{
			Channel: "#general",
			From:    "botty",
			Body:    "model response",
		},
		Instance: "botty",
	})

	view := tm.CurrentView()
	require.NotContains(t, view, "responding")
}

func TestChatScreen_DMOpenedEvent(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	_, err := sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Send(domain.DMOpenedEvent{
		Channel: domain.Channel{
			Name: "fakenick",
			Kind: domain.KindDM,
		},
		Nick:    "fakenick",
		Created: true,
		At:      time.Now(),
	})

	tm.WaitFor("Opened direct message with fakenick")

	view := tm.FinalView()
	require.Contains(t, view, "Opened direct message with fakenick")
}

func TestChatScreen_ConfigChangedEvent_with_active_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Send(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	tm.WaitFor("testuser has joined #general")

	tm.Send(domain.ConfigChangedEvent{
		Operation: "API key updated",
		At:        time.Now(),
	})

	tm.WaitFor("API key updated")

	view := tm.FinalView()
	require.Contains(t, view, "API key updated")
}

func TestChatScreen_ConfigChangedEvent_no_active_channel(t *testing.T) {
	sess := newTestSession(t)

	tm := newChatApp(t, sess)
	tm.WaitFor("Welcome to modeloff")

	tm.Send(domain.ConfigChangedEvent{
		Operation: "API key updated",
		At:        time.Now(),
	})

	view := tm.FinalView()
	require.NotEmpty(t, view)
}

func TestChatScreen_ErrorEvent_with_active_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Send(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	tm.WaitFor("testuser has joined #general")

	tm.Send(domain.ErrorEvent{
		Operation: "model invocation",
		Err:       errors.New("connection refused"),
		At:        time.Now(),
	})

	tm.WaitFor("model invocation", "connection refused")

	view := tm.FinalView()
	require.Contains(t, view, "model invocation")
	require.Contains(t, view, "connection refused")
}

func TestChatScreen_ErrorEvent_no_active_channel(t *testing.T) {
	sess := newTestSession(t)

	tm := newChatApp(t, sess)
	tm.WaitFor("Welcome to modeloff")

	tm.Send(domain.ErrorEvent{
		Operation: "startup failure",
		Err:       errors.New("no api key"),
		At:        time.Now(),
	})

	view := tm.FinalView()
	require.NotEmpty(t, view)
}

func TestChatScreen_NickChangeEvent(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Send(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	tm.WaitFor("testuser has joined #general")

	tm.Send(domain.NickChangeEvent{
		OldNick: "testuser",
		NewNick: "newnick",
		At:      time.Now(),
	})

	tm.WaitFor("testuser is now known as newnick")

	view := tm.FinalView()
	require.Contains(t, view, "testuser is now known as newnick")
}

func TestChatScreen_ModelInvitedEvent_active_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Send(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	tm.WaitFor("testuser has joined #general")

	tm.Send(domain.ModelInvitedEvent{
		Channel: "#general",
		Instance: domain.ModelInstance{
			Nick:    "botty",
			ModelID: "anthropic/claude-3-haiku",
		},
		At: time.Now(),
	})

	tm.WaitFor("botty (anthropic/claude-3-haiku) has joined #general")

	view := tm.FinalView()
	require.Contains(t, view, "botty (anthropic/claude-3-haiku) has joined #general")
}

func TestChatScreen_ModelKickedEvent_active_channel(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")

	_, err := sess.Invite(t.Context(), "#general", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)

	tm := newChatApp(t, sess)
	tm.WaitFor("#general")

	tm.Send(domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		At:      time.Now(),
	})
	tm.WaitFor("testuser has joined #general")

	tm.Send(domain.ModelKickedEvent{
		Channel: "#general",
		Nick:    "fakenick",
		At:      time.Now(),
	})

	tm.WaitFor("fakenick has been kicked from #general")

	view := tm.FinalView()
	require.Contains(t, view, "fakenick has been kicked from #general")
}
