package session

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/set"
	storemod "github.com/laney/modeloff/internal/store"
)

var fixedTime = time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

func newTestSession(t *testing.T) (*Session, *storemod.FileStore) {
	t.Helper()

	return newTestSessionWithAPI(t, &fakeAPIClient{})
}

func newTestSessionWithAPI(t *testing.T, apiClient api.Client) (*Session, *storemod.FileStore) {
	t.Helper()

	s := storemod.NewFileStore(t.TempDir())
	sess := New(s, nil, apiClient, nil, "testuser")
	sess.now = func() time.Time { return fixedTime }

	return sess, s
}

func TestSession_Join(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	evt, err := sess.Join(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		Created: true,
		At:      fixedTime,
	}, evt)

	// Channel should be persisted.
	ch, err := s.GetChannel(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, domain.Channel{
		Name:    "#general",
		Kind:    domain.KindChannel,
		Members: set.NewOrdered[domain.Nick]("testuser"),
		Created: fixedTime,
	}, ch)

	// Last channel should be set.
	last, err := s.GetLastChannel(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.ChannelName("#general"), last)
}

func TestSession_JoinExistingChannel(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	existing := domain.Channel{
		Name:    "#existing",
		Kind:    domain.KindChannel,
		Topic:   "Already here",
		Members: set.NewOrdered[domain.Nick]("testuser"),
		Created: fixedTime.Add(-time.Hour),
	}
	require.NoError(t, s.SaveChannel(ctx, existing))

	evt, err := sess.Join(ctx, "#existing")
	require.NoError(t, err)
	require.Equal(t, domain.JoinEvent{
		Channel: "#existing",
		Nick:    "testuser",
		At:      fixedTime,
	}, evt)

	// Channel should not be overwritten.
	ch, err := s.GetChannel(ctx, "#existing")
	require.NoError(t, err)
	require.Equal(t, "Already here", ch.Topic)
}

func TestSession_Leave(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	ch := domain.Channel{
		Name:    "#leaving",
		Kind:    domain.KindChannel,
		Members: set.NewOrdered[domain.Nick]("testuser", "botty"),
		Created: fixedTime,
	}
	require.NoError(t, s.SaveChannel(ctx, ch))

	evt, err := sess.Leave(ctx, "#leaving")
	require.NoError(t, err)
	require.Equal(t, domain.PartEvent{
		Channel: "#leaving",
		Nick:    "testuser",
		At:      fixedTime,
	}, evt)

	updated, err := s.GetChannel(ctx, "#leaving")
	require.NoError(t, err)
	require.Equal(t, domain.Channel{
		Name:    "#leaving",
		Kind:    domain.KindChannel,
		Members: set.NewOrdered[domain.Nick]("botty"),
		Created: fixedTime,
	}, updated)
}

func TestSession_LeaveNonexistent(t *testing.T) {
	sess, _ := newTestSession(t)

	_, err := sess.Leave(t.Context(), "#ghost")
	require.Error(t, err)
}

func TestSession_Invite(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	ch := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Members: set.NewOrdered[domain.Nick]("testuser"),
		Created: fixedTime,
	}
	require.NoError(t, s.SaveChannel(ctx, ch))

	evt, err := sess.Invite(ctx, "#dev", "anthropic/claude-3-haiku", "")
	require.NoError(t, err)
	require.Equal(t, domain.ModelInvitedEvent{
		Channel: "#dev",
		Instance: domain.ModelInstance{
			Nick:     "fakenick",
			ModelID:  "anthropic/claude-3-haiku",
			Channels: set.NewOrdered[domain.ChannelName]("#dev"),
			JoinedAt: map[domain.ChannelName]time.Time{
				"#dev": fixedTime,
			},
		},
		At: fixedTime,
	}, evt)

	// Instance should be persisted.
	inst, err := s.GetInstance(ctx, "fakenick")
	require.NoError(t, err)
	require.Equal(t, domain.ModelInstance{
		Nick:     "fakenick",
		ModelID:  "anthropic/claude-3-haiku",
		Channels: set.NewOrdered[domain.ChannelName]("#dev"),
		JoinedAt: map[domain.ChannelName]time.Time{
			"#dev": fixedTime,
		},
	}, inst)

	// Channel should have new member.
	updated, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, set.NewOrdered[domain.Nick]("testuser", "fakenick"), updated.Members)
}

func TestSession_Kick(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	ch := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Members: set.NewOrdered[domain.Nick]("testuser", "botty"),
		Created: fixedTime,
	}
	require.NoError(t, s.SaveChannel(ctx, ch))
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#dev", "#random"),
	})

	evt, err := sess.Kick(ctx, "#dev", "botty")
	require.NoError(t, err)
	require.Equal(t, domain.ModelKickedEvent{
		Channel: "#dev",
		Nick:    "botty",
		At:      fixedTime,
	}, evt)

	// Channel should no longer have the kicked member.
	updated, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, set.NewOrdered[domain.Nick]("testuser"), updated.Members)

	inst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)
	require.Equal(t, set.NewOrdered[domain.ChannelName]("#random"), inst.Channels)
}

func TestSession_SendMessage(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser")

	evt, err := sess.SendMessage(ctx, "#general", "hello world")
	require.NoError(t, err)
	require.Equal(t, domain.MessageEvent{
		Message: domain.Message{
			ID:      fmt.Sprintf("%d", fixedTime.UnixNano()),
			Channel: "#general",
			From:    "testuser",
			Body:    "hello world",
			SentAt:  fixedTime,
		},
	}, evt)

	// Message should be persisted.
	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{evt.Message}, msgs)

	// No instances, so dispatch completes immediately.
	events := drainEvents(t, sess, 1)
	require.Equal(t, []domain.SessionEvent{
		domain.DispatchDoneEvent{Channel: "#general"},
	}, events)
}

func TestSession_SendMessage_emits_dispatch_events(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("got it"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	_, err := sess.SendMessage(ctx, "#general", "hello")
	require.NoError(t, err)

	events := drainEvents(t, sess, 1)

	require.Equal(t, []domain.SessionEvent{
		domain.DispatchStartedEvent{Channel: "#general", Nicks: []domain.Nick{"botty"}},
		domain.ModelReplyEvent{
			Channel:  "#general",
			Instance: "botty",
			Message: domain.Message{
				ID:      fmt.Sprintf("%d~botty~0", fixedTime.UnixNano()),
				Channel: "#general",
				From:    "botty",
				Body:    "got it",
				SentAt:  fixedTime,
			},
			At: fixedTime,
		},
		domain.DispatchDoneEvent{Channel: "#general"},
	}, events)
}

func TestSession_DispatchToChannel_broadcasts_to_channel_instances(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("got it"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	msg, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{
		msg,
		{
			ID:      fmt.Sprintf("%d~botty~0", fixedTime.UnixNano()),
			Channel: "#general",
			From:    "botty",
			Body:    "got it",
			SentAt:  fixedTime,
		},
	}, msgs)
}

func TestSession_DispatchToChannel_does_not_broadcast_when_no_model_instances(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("should not appear"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser")

	msg, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{msg}, msgs)
}

func TestSession_DispatchToChannel_pass_response_does_not_store_model_message(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.ModelResponse{
				Kind:   protocol.ResponseSilence,
				Reason: "nothing to add",
			}, nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	msg, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{msg}, msgs)
}

func TestSession_DispatchToChannel_reply_response_stores_model_message(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("hello back"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	msg, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{
		msg,
		{
			ID:      fmt.Sprintf("%d~botty~0", fixedTime.UnixNano()),
			Channel: "#general",
			From:    "botty",
			Body:    "hello back",
			SentAt:  fixedTime,
		},
	}, msgs)
}

func TestSession_DispatchToChannel_broadcasts_only_to_members_of_that_channel(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply(fmt.Sprintf("reply from %s", modelID)), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedChannelWithMembers(t, s, "#random", "testuser", "otherbot")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model-a",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "otherbot",
		ModelID:  "test/model-b",
		Channels: set.NewOrdered[domain.ChannelName]("#random"),
	})

	msg, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	generalMsgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{
		msg,
		{
			ID:      fmt.Sprintf("%d~botty~0", fixedTime.UnixNano()),
			Channel: "#general",
			From:    "botty",
			Body:    "reply from test/model-a",
			SentAt:  fixedTime,
		},
	}, generalMsgs)

	randomMsgs, err := s.ListMessages(ctx, "#random")
	require.NoError(t, err)
	require.Empty(t, randomMsgs)
}

func TestSession_DispatchToChannel_reply_is_not_rebroadcast_in_same_dispatch(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("reply once"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	msg, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{
		msg,
		{
			ID:      fmt.Sprintf("%d~botty~0", fixedTime.UnixNano()),
			Channel: "#general",
			From:    "botty",
			Body:    "reply once",
			SentAt:  fixedTime,
		},
	}, msgs)
}

func TestSession_DispatchToChannel_multiple_instances_each_reply_once(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply(fmt.Sprintf("reply from %s", modelID)), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "bot-a", "bot-b")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "bot-a",
		ModelID:  "test/model-a",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "bot-b",
		ModelID:  "test/model-b",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	msg, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)

	require.Equal(t, msg, msgs[0])
	require.ElementsMatch(t, []domain.Message{
		{
			ID:      fmt.Sprintf("%d~bot-a~0", fixedTime.UnixNano()),
			Channel: "#general",
			From:    "bot-a",
			Body:    "reply from test/model-a",
			SentAt:  fixedTime,
		},
		{
			ID:      fmt.Sprintf("%d~bot-b~0", fixedTime.UnixNano()),
			Channel: "#general",
			From:    "bot-b",
			Body:    "reply from test/model-b",
			SentAt:  fixedTime,
		},
	}, msgs[1:])
}

func TestSession_DispatchToChannel_ignores_empty_reply_body(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("   "), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	msg, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{msg}, msgs)
}

func TestSession_DispatchToChannel_api_error_continues_to_next_instance(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			if modelID == "test/model-a" {
				return protocol.ModelResponse{}, fmt.Errorf("network timeout")
			}

			return protocol.Reply("reply from bot-b"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "bot-a", "bot-b")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "bot-a",
		ModelID:  "test/model-a",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "bot-b",
		ModelID:  "test/model-b",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	msg, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.Error(t, err, "should surface the API error")
	require.ErrorContains(t, err, "network timeout")

	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{
		msg,
		{
			ID:      fmt.Sprintf("%d~bot-b~0", fixedTime.UnixNano()),
			Channel: "#general",
			From:    "bot-b",
			Body:    "reply from bot-b",
			SentAt:  fixedTime,
		},
	}, msgs)
}

func TestSession_Poke_api_error_emits_error_event(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			if modelID == "test/model-a" {
				return protocol.ModelResponse{}, fmt.Errorf("rate limited")
			}

			return protocol.Reply("still here"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "bot-a")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "bot-a",
		ModelID:  "test/model-a",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})
	seedChannelWithMembers(t, s, "#random", "testuser", "bot-b")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "bot-b",
		ModelID:  "test/model-b",
		Channels: set.NewOrdered[domain.ChannelName]("#random"),
	})

	require.NoError(t, sess.Poke(ctx))
	events := drainEvents(t, sess, 2)

	var hasError bool
	var hasReply bool

	for _, evt := range events {
		switch evt.(type) {
		case domain.ErrorEvent:
			hasError = true
		case domain.ModelReplyEvent:
			hasReply = true
		}
	}

	require.True(t, hasError, "should emit an ErrorEvent for the failed channel")
	require.True(t, hasReply, "should emit a ModelReplyEvent for the successful channel")

	msgs, err := s.ListMessages(ctx, "#random")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{
		{
			ID:      fmt.Sprintf("%d~bot-b~0", fixedTime.UnixNano()),
			Channel: "#random",
			From:    "bot-b",
			Body:    "still here",
			SentAt:  fixedTime,
		},
	}, msgs)
}

func TestSession_SetTopic(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	ch := domain.Channel{Name: "#dev", Kind: domain.KindChannel, Created: fixedTime}
	require.NoError(t, s.SaveChannel(ctx, ch))

	evt, err := sess.SetTopic(ctx, "#dev", "Development Chat")
	require.NoError(t, err)
	require.Equal(t, domain.TopicChangeEvent{
		Channel: "#dev",
		Topic:   "Development Chat",
		By:      "testuser",
		At:      fixedTime,
	}, evt)

	// Channel topic and metadata should be updated.
	updated, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, domain.Channel{
		Name:       "#dev",
		Kind:       domain.KindChannel,
		Topic:      "Development Chat",
		TopicSetBy: "testuser",
		TopicSetAt: fixedTime,
		Created:    fixedTime,
	}, updated)
}

func TestSession_ChangeNick(t *testing.T) {
	cfgStore := &fakeConfigStore{
		cfg: config.Config{
			UserNick:     "testuser",
			PokeInterval: 5 * time.Minute,
		},
	}
	s := storemod.NewFileStore(t.TempDir())
	sess := New(s, nil, &fakeAPIClient{}, cfgStore, "testuser")
	sess.now = func() time.Time { return fixedTime }

	evt, err := sess.ChangeNick(t.Context(), "newname")
	require.NoError(t, err)
	require.Equal(t, domain.NickChangeEvent{
		OldNick: "testuser",
		NewNick: "newname",
		At:      fixedTime,
	}, evt)

	require.Equal(t, domain.Nick("newname"), sess.UserNick())
	require.Equal(t, "newname", cfgStore.saved.UserNick)
	require.Equal(t, 1, cfgStore.saveCalls)
}

func TestSession_ChangeNick_save_failure_does_not_update_runtime(t *testing.T) {
	cfgStore := &fakeConfigStore{
		cfg: config.Config{
			UserNick:     "testuser",
			PokeInterval: 5 * time.Minute,
		},
		saveErr: fmt.Errorf("disk full"),
	}
	s := storemod.NewFileStore(t.TempDir())
	sess := New(s, nil, &fakeAPIClient{}, cfgStore, "testuser")
	sess.now = func() time.Time { return fixedTime }

	_, err := sess.ChangeNick(t.Context(), "newname")
	require.Error(t, err)
	require.Equal(t, domain.Nick("testuser"), sess.UserNick())
}

func TestSession_Whois(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	inst := domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Persona:  "A test bot",
		Channels: set.NewOrdered[domain.ChannelName]("#dev"),
	}
	require.NoError(t, s.SaveInstance(ctx, inst))

	got, err := sess.Whois(ctx, "botty")
	require.NoError(t, err)
	require.Equal(t, inst, got)
}

func TestSession_WhoisNotFound(t *testing.T) {
	sess, _ := newTestSession(t)

	_, err := sess.Whois(t.Context(), "ghost")
	require.Error(t, err)
}

func TestSession_InviteNonexistentChannel(t *testing.T) {
	sess, _ := newTestSession(t)

	_, err := sess.Invite(t.Context(), "#ghost", "anthropic/claude-3-haiku", "")
	require.Error(t, err)
}

func TestSession_Invite_existing_instance_to_nonexistent_channel_does_not_corrupt(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	_, err := sess.Invite(ctx, "#ghost", "botty", "")
	require.Error(t, err)

	// Instance should not have the phantom channel in its set.
	inst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)
	require.Equal(t, set.NewOrdered[domain.ChannelName]("#general"), inst.Channels)
}

func TestSession_InviteGenerateNickError(t *testing.T) {
	fake := &fakeAPIClient{
		generateNickFn: func(_ context.Context, _, _ domain.ModelID) (domain.Nick, error) {
			return "", fmt.Errorf("API unavailable")
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	ch := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Members: set.NewOrdered[domain.Nick]("testuser"),
		Created: fixedTime,
	}
	require.NoError(t, s.SaveChannel(ctx, ch))

	_, err := sess.Invite(ctx, "#dev", "anthropic/claude-3-haiku", "")
	require.Error(t, err)
}

func TestSession_Invite_persists_persona(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser")

	evt, err := sess.Invite(ctx, "#general", "anthropic/claude-3-haiku", "Helpful assistant")
	require.NoError(t, err)
	require.Equal(t, "Helpful assistant", evt.Instance.Persona)

	inst, err := s.GetInstance(ctx, "fakenick")
	require.NoError(t, err)
	require.Equal(t, "Helpful assistant", inst.Persona)
}

func TestSession_Invite_reuses_existing_instance(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser")
	seedChannelWithMembers(t, s, "#random", "testuser")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Persona:  "Helpful assistant",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	evt, err := sess.Invite(ctx, "#random", "botty", "")
	require.NoError(t, err)
	require.Equal(t, domain.ModelInvitedEvent{
		Channel: "#random",
		Instance: domain.ModelInstance{
			Nick:     "botty",
			ModelID:  "test/model",
			Persona:  "Helpful assistant",
			Channels: set.NewOrdered[domain.ChannelName]("#general", "#random"),
			JoinedAt: map[domain.ChannelName]time.Time{
				"#random": fixedTime,
			},
		},
		At: fixedTime,
	}, evt)

	inst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)
	require.Equal(t, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Persona:  "Helpful assistant",
		Channels: set.NewOrdered[domain.ChannelName]("#general", "#random"),
		JoinedAt: map[domain.ChannelName]time.Time{
			"#random": fixedTime,
		},
	}, inst)

	channel, err := s.GetChannel(ctx, "#random")
	require.NoError(t, err)
	require.Equal(t, set.NewOrdered[domain.Nick]("testuser", "botty"), channel.Members)
}

func TestSession_Invite_existing_instance_is_idempotent(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	_, err := sess.Invite(ctx, "#general", "botty", "")
	require.NoError(t, err)

	inst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)
	require.Equal(t, set.NewOrdered[domain.ChannelName]("#general"), inst.Channels)

	channel, err := s.GetChannel(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, set.NewOrdered[domain.Nick]("testuser", "botty"), channel.Members)
}

func TestSession_Invite_existing_instance_preserves_persona(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser")
	seedChannelWithMembers(t, s, "#random", "testuser")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Persona:  "Existing persona",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	evt, err := sess.Invite(ctx, "#random", "botty", "New persona")
	require.NoError(t, err)
	require.Equal(t, "Existing persona", evt.Instance.Persona)

	inst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)
	require.Equal(t, "Existing persona", inst.Persona)
}

func TestSession_Invite_same_model_id_reuses_instance(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser")
	seedChannelWithMembers(t, s, "#random", "testuser")

	evt1, err := sess.Invite(ctx, "#general", "test/model", "Helpful assistant")
	require.NoError(t, err)
	require.Equal(t, domain.Nick("fakenick"), evt1.Instance.Nick)

	evt2, err := sess.Invite(ctx, "#random", "test/model", "")
	require.NoError(t, err)
	require.Equal(t, domain.ModelInvitedEvent{
		Channel: "#random",
		Instance: domain.ModelInstance{
			Nick:     "fakenick",
			ModelID:  "test/model",
			Persona:  "Helpful assistant",
			Channels: set.NewOrdered[domain.ChannelName]("#general", "#random"),
			JoinedAt: map[domain.ChannelName]time.Time{
				"#general": fixedTime,
				"#random":  fixedTime,
			},
		},
		At: fixedTime,
	}, evt2)

	instances, err := s.ListInstances(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.ModelInstance{
		{
			Nick:     "fakenick",
			ModelID:  "test/model",
			Persona:  "Helpful assistant",
			Channels: set.NewOrdered[domain.ChannelName]("#general", "#random"),
			JoinedAt: map[domain.ChannelName]time.Time{
				"#general": fixedTime,
				"#random":  fixedTime,
			},
		},
	}, instances)
}

func TestSession_KickNonexistentChannel(t *testing.T) {
	sess, _ := newTestSession(t)

	_, err := sess.Kick(t.Context(), "#ghost", "botty")
	require.Error(t, err)
}

func TestSession_KickNonMember(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	ch := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Members: set.NewOrdered[domain.Nick]("testuser"),
		Created: fixedTime,
	}
	require.NoError(t, s.SaveChannel(ctx, ch))

	evt, err := sess.Kick(ctx, "#dev", "nobody")
	require.NoError(t, err)
	require.Equal(t, domain.ModelKickedEvent{
		Channel: "#dev",
		Nick:    "nobody",
		At:      fixedTime,
	}, evt)

	// Members should be unchanged.
	updated, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, set.NewOrdered[domain.Nick]("testuser"), updated.Members)
}

func TestSession_SetTopicNonexistentChannel(t *testing.T) {
	sess, _ := newTestSession(t)

	_, err := sess.SetTopic(t.Context(), "#ghost", "topic")
	require.Error(t, err)
}

func TestSession_DispatchToChannel_includes_memory_in_prompt(t *testing.T) {
	memStore := memory.NewFileStore(t.TempDir())
	require.NoError(t, memStore.Write(t.Context(), "botty", memory.Entry{
		Key:     "mood",
		Content: "curious",
	}))

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, _ domain.ModelID, system string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			if strings.Contains(system, "Your persona: Helpful assistant") &&
				strings.Contains(system, "[mood=curious]") {
				return protocol.Reply("memory and persona received"), nil
			}

			return protocol.ModelResponse{Kind: protocol.ResponseSilence}, nil
		},
	}
	s := storemod.NewFileStore(t.TempDir())
	sess := New(s, memStore, fake, nil, "testuser")
	sess.now = func() time.Time { return fixedTime }

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Persona:  "Helpful assistant",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	msg, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(t.Context(), "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs, err := s.ListMessages(t.Context(), "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{
		msg,
		{
			ID:      fmt.Sprintf("%d~botty~0", fixedTime.UnixNano()),
			Channel: "#general",
			From:    "botty",
			Body:    "memory and persona received",
			SentAt:  fixedTime,
		},
	}, msgs)
}

func TestBuildSystemPrompt(t *testing.T) {
	ch := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Topic:   "go stuff",
		Members: set.NewOrdered[domain.Nick]("testuser", "botty"),
	}
	inst := domain.ModelInstance{
		Nick:    "botty",
		ModelID: "test/model",
		Persona: "grumpy sysadmin",
	}

	prompt := buildSystemPrompt(ch, inst, nil)

	// Must identify the model and channel.
	require.Contains(t, prompt, "botty")
	require.Contains(t, prompt, "#dev")

	// Must include topic and persona.
	require.Contains(t, prompt, "go stuff")
	require.Contains(t, prompt, "grumpy sysadmin")

	// Must instruct IRC-authentic behaviour.
	require.Contains(t, prompt, "short")
	require.Contains(t, prompt, "ASCII")
	require.Contains(t, prompt, "emoji")
	require.Contains(t, prompt, "markdown")
	require.Contains(t, prompt, "lowercase")
	require.Contains(t, prompt, "Lurk")
	require.Contains(t, prompt, "nick")
}

func TestBuildSystemPrompt_with_memories(t *testing.T) {
	ch := domain.Channel{
		Name: "#dev",
		Kind: domain.KindChannel,
	}
	inst := domain.ModelInstance{
		Nick:    "botty",
		ModelID: "test/model",
	}
	memories := []memory.Entry{
		{Key: "mood", Content: "curious"},
		{Key: "goal", Content: "learn go"},
	}

	prompt := buildSystemPrompt(ch, inst, memories)

	require.Contains(t, prompt, "[mood=curious]")
	require.Contains(t, prompt, "[goal=learn go]")
	require.Contains(t, prompt, "write_memory")
	require.Contains(t, prompt, "delete_memory")
	require.NotContains(t, prompt, "no memories yet")
}

func TestSession_Poke_emits_dispatch_events(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("poke received"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	require.NoError(t, sess.Poke(ctx))
	events := drainEvents(t, sess, 1)

	require.Equal(t, []domain.SessionEvent{
		domain.DispatchStartedEvent{Channel: "#general", Nicks: []domain.Nick{"botty"}},
		domain.ModelReplyEvent{
			Channel:  "#general",
			Instance: "botty",
			Message: domain.Message{
				ID:      fmt.Sprintf("%d~botty~0", fixedTime.UnixNano()),
				Channel: "#general",
				From:    "botty",
				Body:    "poke received",
				SentAt:  fixedTime,
			},
			At: fixedTime,
		},
		domain.DispatchDoneEvent{Channel: "#general"},
	}, events)

	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{
		{
			ID:      fmt.Sprintf("%d~botty~0", fixedTime.UnixNano()),
			Channel: "#general",
			From:    "botty",
			Body:    "poke received",
			SentAt:  fixedTime,
		},
	}, msgs)
}

func TestSession_OpenDM_creates_dm_channel(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, s, domain.ModelInstance{
		Nick:    "botty",
		ModelID: "test/model",
	})

	ch, created, err := sess.OpenDM(ctx, "botty")
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, domain.Channel{
		Name:    "botty",
		Kind:    domain.KindDM,
		Members: set.NewOrdered[domain.Nick]("testuser", "botty"),
		Created: fixedTime,
	}, ch)

	got, err := s.GetChannel(ctx, "botty")
	require.NoError(t, err)
	require.Equal(t, ch, got)

	inst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)
	require.Equal(t, set.NewOrdered[domain.ChannelName]("botty"), inst.Channels)
}

func TestSession_OpenDM_reuses_existing_dm_channel(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	existing := domain.Channel{
		Name:    "botty",
		Kind:    domain.KindDM,
		Members: set.NewOrdered[domain.Nick]("testuser", "botty"),
		Created: fixedTime.Add(-time.Hour),
	}
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("botty"),
	})
	require.NoError(t, s.SaveChannel(ctx, existing))

	ch, created, err := sess.OpenDM(ctx, "botty")
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, existing, ch)

	inst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)
	require.Equal(t, set.NewOrdered[domain.ChannelName]("botty"), inst.Channels)
}

func TestSession_OpenDM_unknown_instance(t *testing.T) {
	sess, _ := newTestSession(t)

	_, _, err := sess.OpenDM(t.Context(), "ghost")
	require.Error(t, err)
}

func TestSession_DispatchToChannel_dm_only_targets_that_instance(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, _ domain.ModelID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("dm reply"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedInstance(t, s, domain.ModelInstance{
		Nick:    "botty",
		ModelID: "test/model-a",
	})
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "otherbot",
		ModelID:  "test/model-b",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	_, _, err := sess.OpenDM(ctx, "botty")
	require.NoError(t, err)

	msg, ircMsg := seedUserMessage(t, s, "botty", "hello in dm")

	_, err = sess.DispatchToChannel(ctx, "botty", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs, err := s.ListMessages(ctx, "botty")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{
		msg,
		{
			ID:      fmt.Sprintf("%d~botty~0", fixedTime.UnixNano()),
			Channel: "botty",
			From:    "botty",
			Body:    "dm reply",
			SentAt:  fixedTime,
		},
	}, msgs)
}

func TestSession_MarkRead_and_UnreadCount(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser")

	require.NoError(t, s.SaveMessage(ctx, domain.Message{
		ID: "msg-1", Channel: "#general", From: "testuser", Body: "first", SentAt: fixedTime,
	}))
	require.NoError(t, s.SaveMessage(ctx, domain.Message{
		ID: "msg-2", Channel: "#general", From: "testuser", Body: "second", SentAt: fixedTime,
	}))

	count, err := sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 2, count)

	require.NoError(t, sess.MarkRead(ctx, "#general"))

	count, err = sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 0, count)
}

func TestSession_UnreadCount_after_new_messages(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser")

	require.NoError(t, s.SaveMessage(ctx, domain.Message{
		ID: "msg-1", Channel: "#general", From: "testuser", Body: "first", SentAt: fixedTime,
	}))

	require.NoError(t, sess.MarkRead(ctx, "#general"))

	require.NoError(t, s.SaveMessage(ctx, domain.Message{
		ID: "msg-2", Channel: "#general", From: "testuser", Body: "second", SentAt: fixedTime,
	}))
	require.NoError(t, s.SaveMessage(ctx, domain.Message{
		ID: "msg-3", Channel: "#general", From: "testuser", Body: "third", SentAt: fixedTime,
	}))

	count, err := sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 2, count)
}

func TestSession_Join_marks_channel_as_read(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser")
	require.NoError(t, s.SaveMessage(ctx, domain.Message{
		ID: "msg-1", Channel: "#general", From: "testuser", Body: "old", SentAt: fixedTime,
	}))

	_, err := sess.Join(ctx, "#general")
	require.NoError(t, err)

	count, err := sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 0, count)
}

func TestSession_SetAPIKey(t *testing.T) {
	cfgStore := &fakeConfigStore{
		cfg: config.Config{
			UserNick:     "testuser",
			PokeInterval: 5 * time.Minute,
		},
	}
	s := storemod.NewFileStore(t.TempDir())
	initial := &fakeAPIClient{}
	replacement := &fakeAPIClient{}
	sess := New(s, nil, initial, cfgStore, "testuser")
	sess.SetAPIFactory(func(apiKey string) (api.Client, error) {
		require.Equal(t, "test-key", apiKey)
		return replacement, nil
	})

	cfg, err := sess.SetAPIKey(t.Context(), "test-key")
	require.NoError(t, err)
	require.Equal(t, "test-key", cfg.APIKey)
	require.Equal(t, "test-key", cfgStore.saved.APIKey)
	require.Equal(t, 1, cfgStore.saveCalls)
	require.Equal(t, "test-key", sess.apiKey)
	require.Same(t, replacement, sess.api)
}

func TestSession_SetAPIKey_factory_failure_keeps_existing_client(t *testing.T) {
	cfgStore := &fakeConfigStore{
		cfg: config.Config{
			UserNick:     "testuser",
			PokeInterval: 5 * time.Minute,
		},
	}
	s := storemod.NewFileStore(t.TempDir())
	initial := &fakeAPIClient{}
	sess := New(s, nil, initial, cfgStore, "testuser")
	sess.SetAPIFactory(func(string) (api.Client, error) {
		return nil, fmt.Errorf("boom")
	})

	_, err := sess.SetAPIKey(context.Background(), "test-key")
	require.Error(t, err)
	require.Equal(t, 0, cfgStore.saveCalls)
	require.Empty(t, cfgStore.saved.APIKey)
	require.Same(t, initial, sess.api)
	require.Empty(t, sess.apiKey)
}

func TestSession_SetPokeInterval(t *testing.T) {
	cfgStore := &fakeConfigStore{
		cfg: config.Config{
			APIKey:       "test-key",
			UserNick:     "testuser",
			PokeInterval: 5 * time.Minute,
		},
	}
	s := storemod.NewFileStore(t.TempDir())
	sess := New(s, nil, &fakeAPIClient{}, cfgStore, "testuser")

	cfg, err := sess.SetPokeInterval(t.Context(), 10*time.Minute)
	require.NoError(t, err)
	require.Equal(t, 10*time.Minute, cfg.PokeInterval)
	require.Equal(t, 10*time.Minute, cfgStore.saved.PokeInterval)
	require.Equal(t, "test-key", cfgStore.saved.APIKey)
	require.Equal(t, 1, cfgStore.saveCalls)
}

func TestSession_SetHighlightWords(t *testing.T) {
	cfgStore := &fakeConfigStore{
		cfg: config.Config{
			APIKey:         "test-key",
			UserNick:       "testuser",
			HighlightWords: []string{"$nick"},
		},
	}
	s := storemod.NewFileStore(t.TempDir())
	sess := New(s, nil, &fakeAPIClient{}, cfgStore, "testuser")

	cfg, err := sess.SetHighlightWords(t.Context(), []string{"$nick", "important", "urgent"})
	require.NoError(t, err)
	require.Equal(t, []string{"$nick", "important", "urgent"}, cfg.HighlightWords)
	require.Equal(t, []string{"$nick", "important", "urgent"}, cfgStore.saved.HighlightWords)
	require.Equal(t, "test-key", cfgStore.saved.APIKey)
	require.Equal(t, 1, cfgStore.saveCalls)
}

func TestSession_DispatchToChannel_filters_history_before_join(t *testing.T) {
	beforeJoin := fixedTime.Add(-10 * time.Minute)
	atJoin := fixedTime
	afterJoin := fixedTime.Add(10 * time.Minute)

	var receivedHistory []protocol.IRCMessage

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, _ domain.ModelID, _ string, history []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			receivedHistory = history
			return protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "pass"}, nil
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")

	// Save a message from before the model joined.
	require.NoError(t, s.SaveMessage(ctx, domain.Message{
		ID:      "before",
		Channel: "#general",
		From:    "testuser",
		Body:    "old message",
		SentAt:  beforeJoin,
	}))

	// Save a message from after the model joined.
	require.NoError(t, s.SaveMessage(ctx, domain.Message{
		ID:      "after",
		Channel: "#general",
		From:    "testuser",
		Body:    "new message",
		SentAt:  afterJoin,
	}))

	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
		JoinedAt: map[domain.ChannelName]time.Time{
			"#general": atJoin,
		},
	})

	newEvent := protocol.IRCMessage{
		Kind:   protocol.KindPrivMsg,
		From:   "testuser",
		Target: "#general",
		Body:   "ping",
	}
	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{newEvent})
	require.NoError(t, err)

	// The model should only see the message from after it joined, not the
	// one from before.
	require.Equal(t, []protocol.IRCMessage{
		{
			Kind:   protocol.KindPrivMsg,
			From:   "testuser",
			Target: "#general",
			Body:   "new message",
			At:     afterJoin,
		},
	}, receivedHistory)
}

func TestSession_DispatchToChannel_forwards_replies_to_subsequent_models(t *testing.T) {
	// Track the events each model receives.
	eventsByModel := map[domain.ModelID][]protocol.IRCMessage{}

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (protocol.ModelResponse, error) {
			eventsByModel[modelID] = append([]protocol.IRCMessage{}, events...)

			if modelID == "test/alpha" {
				return protocol.Reply("alpha says hi"), nil
			}

			return protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "pass"}, nil
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "alpha", "beta")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "alpha",
		ModelID:  "test/alpha",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
		JoinedAt: map[domain.ChannelName]time.Time{"#general": fixedTime},
	})
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "beta",
		ModelID:  "test/beta",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
		JoinedAt: map[domain.ChannelName]time.Time{"#general": fixedTime},
	})

	userEvent := protocol.IRCMessage{
		Kind:   protocol.KindPrivMsg,
		From:   "testuser",
		Target: "#general",
		Body:   "hello everyone",
	}

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{userEvent})
	require.NoError(t, err)

	// Alpha should see only the user's message.
	require.Equal(t, []protocol.IRCMessage{userEvent}, eventsByModel["test/alpha"])

	// Beta should see the user's message AND alpha's reply.
	require.Equal(t, []protocol.IRCMessage{
		userEvent,
		{
			Kind:   protocol.KindPrivMsg,
			From:   "alpha",
			Target: "#general",
			Body:   "alpha says hi",
			At:     fixedTime,
		},
	}, eventsByModel["test/beta"])
}

// --- Fake API client ---

type fakeAPIClient struct {
	listModelsFn              func(context.Context) ([]api.ModelInfo, error)
	sendEventsFn              func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error)
	sendEventsFullFn          func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error)
	continueWithToolResultsFn func(context.Context, *api.Conversation, []api.ToolResult) (api.CompletionResult, error)
	generateNickFn            func(context.Context, domain.ModelID, domain.ModelID) (domain.Nick, error)
}

type fakeConfigStore struct {
	cfg       config.Config
	loadErr   error
	saveErr   error
	saveCalls int
	saved     config.Config
}

func (f *fakeAPIClient) ListModels(ctx context.Context) ([]api.ModelInfo, error) {
	if f.listModelsFn != nil {
		return f.listModelsFn(ctx)
	}

	return nil, nil
}

func (f *fakeAPIClient) SendEvents(
	ctx context.Context,
	modelID domain.ModelID,
	system string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
) (api.CompletionResult, error) {
	if f.sendEventsFullFn != nil {
		return f.sendEventsFullFn(ctx, modelID, system, history, events)
	}

	if f.sendEventsFn != nil {
		response, err := f.sendEventsFn(ctx, modelID, system, history, events)
		return api.CompletionResult{Response: response}, err
	}

	return api.CompletionResult{
		Response: protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "fake"},
	}, nil
}

func (f *fakeAPIClient) ContinueWithToolResults(
	ctx context.Context,
	conv *api.Conversation,
	results []api.ToolResult,
) (api.CompletionResult, error) {
	if f.continueWithToolResultsFn != nil {
		return f.continueWithToolResultsFn(ctx, conv, results)
	}

	return api.CompletionResult{
		Response: protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "fake"},
	}, nil
}

func (f *fakeAPIClient) GenerateNick(ctx context.Context, nickModel domain.ModelID, modelID domain.ModelID) (api.NicknameResult, error) {
	if f.generateNickFn != nil {
		nick, err := f.generateNickFn(ctx, nickModel, modelID)
		return api.NicknameResult{Nick: nick}, err
	}

	return api.NicknameResult{Nick: "fakenick"}, nil
}

type failingMemoryStore struct {
	writeErr  error
	deleteErr error
}

func (f *failingMemoryStore) Read(_ context.Context, _ domain.Nick) ([]memory.Entry, error) {
	return nil, nil
}

func (f *failingMemoryStore) Write(_ context.Context, _ domain.Nick, _ memory.Entry) error {
	return f.writeErr
}

func (f *failingMemoryStore) Delete(_ context.Context, _ domain.Nick, _ string) error {
	return f.deleteErr
}

func (f *failingMemoryStore) Reset(_ context.Context) error {
	return nil
}

func (f *fakeConfigStore) Load() (config.Config, error) {
	if f.loadErr != nil {
		return config.Config{}, f.loadErr
	}

	return f.cfg, nil
}

func (f *fakeConfigStore) Save(cfg config.Config) error {
	if f.saveErr != nil {
		return f.saveErr
	}

	f.saveCalls++
	f.saved = cfg
	f.cfg = cfg

	return nil
}

func TestSession_Reset(t *testing.T) {
	s := storemod.NewFileStore(t.TempDir())
	memStore := memory.NewFileStore(t.TempDir())
	sess := New(s, memStore, &fakeAPIClient{}, nil, "testuser")
	sess.now = func() time.Time { return fixedTime }
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})
	require.NoError(t, s.SaveMessage(ctx, domain.Message{
		ID: "msg-1", Channel: "#general", From: "testuser", Body: "hello", SentAt: fixedTime,
	}))
	require.NoError(t, memStore.Write(ctx, "botty", memory.Entry{Key: "mood", Content: "happy"}))

	require.NoError(t, sess.Reset(ctx))

	channels, err := s.ListChannels(ctx)
	require.NoError(t, err)
	require.Empty(t, channels)

	instances, err := s.ListInstances(ctx)
	require.NoError(t, err)
	require.Empty(t, instances)

	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Empty(t, msgs)

	memories, err := memStore.Read(ctx, "botty")
	require.NoError(t, err)
	require.Empty(t, memories)
}

func TestSession_Reset_nil_memory_store(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser")

	require.NoError(t, sess.Reset(ctx))

	channels, err := s.ListChannels(ctx)
	require.NoError(t, err)
	require.Empty(t, channels)
}

func TestBuildSystemPrompt_instructs_single_line_messages(t *testing.T) {
	ch := domain.Channel{Name: "#dev", Kind: domain.KindChannel}
	inst := domain.ModelInstance{Nick: "botty", ModelID: "test/model"}

	prompt := buildSystemPrompt(ch, inst, nil)

	require.Contains(t, prompt, "newline")
}

func TestSession_DispatchToChannel_retries_on_multiline_reply(t *testing.T) {
	calls := 0
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			calls++
			if calls == 1 {
				return protocol.Reply("line one\nline two"), nil
			}

			return protocol.Reply("clean reply"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, 2, calls)
	require.Equal(t, []domain.ModelReplyEvent{
		{
			Channel:  "#general",
			Instance: "botty",
			Message: domain.Message{
				ID:      fmt.Sprintf("%d~botty~0", fixedTime.UnixNano()),
				Channel: "#general",
				From:    "botty",
				Body:    "clean reply",
				SentAt:  fixedTime,
			},
			At: fixedTime,
		},
	}, replies)
}

func TestSession_DispatchToChannel_drops_reply_after_max_retries(t *testing.T) {
	calls := 0
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			calls++
			return protocol.Reply("always\nmultiline"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	userMsg, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, 3, calls)
	require.Empty(t, replies)

	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{userMsg}, msgs)
}

func TestSession_DispatchToChannel_accepts_single_line_reply(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("no newlines here"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, []domain.ModelReplyEvent{
		{
			Channel:  "#general",
			Instance: "botty",
			Message: domain.Message{
				ID:      fmt.Sprintf("%d~botty~0", fixedTime.UnixNano()),
				Channel: "#general",
				From:    "botty",
				Body:    "no newlines here",
				SentAt:  fixedTime,
			},
			At: fixedTime,
		},
	}, replies)
}

func newTestSessionWithMemory(t *testing.T, apiClient api.Client) (*Session, *storemod.FileStore, *memory.FileStore) {
	t.Helper()

	s := storemod.NewFileStore(t.TempDir())
	m := memory.NewFileStore(t.TempDir())
	sess := New(s, m, apiClient, nil, "testuser")
	sess.now = func() time.Time { return fixedTime }

	return sess, s, m
}

func TestSession_DispatchToChannel_write_memory_then_reply(t *testing.T) {
	var continueResults []api.ToolResult
	fake := &fakeAPIClient{
		sendEventsFullFn: func(_ context.Context, _ domain.ModelID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				Conversation: &api.Conversation{},
				PendingToolCalls: []api.PendingToolCall{
					{ID: "call_1", Kind: api.ToolCallWriteMemory, Key: "mood", Body: "happy"},
				},
			}, nil
		},
		continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
			continueResults = results
			return api.CompletionResult{
				Response: protocol.Reply("noted!"),
			}, nil
		},
	}

	sess, s, memStore := newTestSessionWithMemory(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, []api.ToolResult{
		{ToolCallID: "call_1", Content: "ok"},
	}, continueResults)

	require.Equal(t, []domain.ModelReplyEvent{
		{
			Channel:  "#general",
			Instance: "botty",
			Message: domain.Message{
				ID:      fmt.Sprintf("%d~botty~0", fixedTime.UnixNano()),
				Channel: "#general",
				From:    "botty",
				Body:    "noted!",
				SentAt:  fixedTime,
			},
			At: fixedTime,
		},
	}, replies)

	memories, err := memStore.Read(ctx, "botty")
	require.NoError(t, err)
	require.Equal(t, []memory.Entry{{Key: "mood", Content: "happy"}}, memories)
}

func TestSession_DispatchToChannel_delete_memory_then_pass(t *testing.T) {
	var continueResults []api.ToolResult
	fake := &fakeAPIClient{
		sendEventsFullFn: func(_ context.Context, _ domain.ModelID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				Conversation: &api.Conversation{},
				PendingToolCalls: []api.PendingToolCall{
					{ID: "call_1", Kind: api.ToolCallDeleteMemory, Key: "old_stuff"},
				},
			}, nil
		},
		continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
			continueResults = results
			return api.CompletionResult{
				Response: protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "nothing to say"},
			}, nil
		},
	}

	sess, s, memStore := newTestSessionWithMemory(t, fake)
	ctx := t.Context()

	require.NoError(t, memStore.Write(ctx, "botty", memory.Entry{Key: "old_stuff", Content: "remove me"}))

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)
	require.Empty(t, replies)

	require.Equal(t, []api.ToolResult{
		{ToolCallID: "call_1", Content: "ok"},
	}, continueResults)

	memories, err := memStore.Read(ctx, "botty")
	require.NoError(t, err)
	require.Empty(t, memories)
}

func TestSession_DispatchToChannel_memory_write_error_returns_error_to_model(t *testing.T) {
	var continueResults []api.ToolResult
	fake := &fakeAPIClient{
		sendEventsFullFn: func(_ context.Context, _ domain.ModelID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				Conversation: &api.Conversation{},
				PendingToolCalls: []api.PendingToolCall{
					{ID: "call_1", Kind: api.ToolCallWriteMemory, Key: "mood", Body: "happy"},
				},
			}, nil
		},
		continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
			continueResults = results
			return api.CompletionResult{
				Response: protocol.Reply("ok anyway"),
			}, nil
		},
	}

	s := storemod.NewFileStore(t.TempDir())
	memStore := &failingMemoryStore{writeErr: fmt.Errorf("disk full")}
	sess := New(s, memStore, fake, nil, "testuser")
	sess.now = func() time.Time { return fixedTime }
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, []api.ToolResult{
		{ToolCallID: "call_1", Content: "disk full"},
	}, continueResults)

	require.Equal(t, []domain.ModelReplyEvent{
		{
			Channel:  "#general",
			Instance: "botty",
			Message: domain.Message{
				ID:      fmt.Sprintf("%d~botty~0", fixedTime.UnixNano()),
				Channel: "#general",
				From:    "botty",
				Body:    "ok anyway",
				SentAt:  fixedTime,
			},
			At: fixedTime,
		},
	}, replies)
}

func TestSession_DispatchToChannel_multiple_memory_calls_in_one_response(t *testing.T) {
	var continueResults []api.ToolResult
	fake := &fakeAPIClient{
		sendEventsFullFn: func(_ context.Context, _ domain.ModelID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				Conversation: &api.Conversation{},
				PendingToolCalls: []api.PendingToolCall{
					{ID: "call_1", Kind: api.ToolCallWriteMemory, Key: "mood", Body: "happy"},
					{ID: "call_2", Kind: api.ToolCallWriteMemory, Key: "topic", Body: "go programming"},
				},
			}, nil
		},
		continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
			continueResults = results
			return api.CompletionResult{
				Response: protocol.Reply("stored both"),
			}, nil
		},
	}

	sess, s, memStore := newTestSessionWithMemory(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, []api.ToolResult{
		{ToolCallID: "call_1", Content: "ok"},
		{ToolCallID: "call_2", Content: "ok"},
	}, continueResults)

	require.Equal(t, []domain.ModelReplyEvent{
		{
			Channel:  "#general",
			Instance: "botty",
			Message: domain.Message{
				ID:      fmt.Sprintf("%d~botty~0", fixedTime.UnixNano()),
				Channel: "#general",
				From:    "botty",
				Body:    "stored both",
				SentAt:  fixedTime,
			},
			At: fixedTime,
		},
	}, replies)

	memories, err := memStore.Read(ctx, "botty")
	require.NoError(t, err)
	require.Equal(t, []memory.Entry{
		{Key: "mood", Content: "happy"},
		{Key: "topic", Content: "go programming"},
	}, memories)
}

func TestSession_DispatchToChannel_memory_loop_respects_max_turns(t *testing.T) {
	// The model never calls reply/pass — just keeps writing memories
	// forever. The loop should stop after maxToolLoopTurns continue
	// calls and return no replies.
	var writtenKeys []string
	fake := &fakeAPIClient{
		sendEventsFullFn: func(_ context.Context, _ domain.ModelID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				Conversation: &api.Conversation{},
				PendingToolCalls: []api.PendingToolCall{
					{ID: "call_init", Kind: api.ToolCallWriteMemory, Key: "k0", Body: "v0"},
				},
			}, nil
		},
		continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
			for _, r := range results {
				writtenKeys = append(writtenKeys, r.ToolCallID)
			}

			// Return another memory write — the loop should eventually stop.
			nextKey := fmt.Sprintf("k%d", len(writtenKeys))
			return api.CompletionResult{
				Conversation: &api.Conversation{},
				PendingToolCalls: []api.PendingToolCall{
					{ID: "call_" + nextKey, Kind: api.ToolCallWriteMemory, Key: nextKey, Body: "val"},
				},
			}, nil
		},
	}

	sess, s, _ := newTestSessionWithMemory(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)
	require.Empty(t, replies)

	// 1 initial SendEvents + maxToolLoopTurns continues = maxToolLoopTurns
	// tool result deliveries.
	require.Equal(t, []string{
		"call_init",
		"call_k1",
		"call_k2",
		"call_k3",
		"call_k4",
	}, writtenKeys)
}

func TestBuildSystemPrompt_mentions_memory_tools(t *testing.T) {
	ch := domain.Channel{Name: "#dev", Kind: domain.KindChannel}
	inst := domain.ModelInstance{Nick: "botty", ModelID: "test/model"}

	prompt := buildSystemPrompt(ch, inst, nil)

	require.Contains(t, prompt, "write_memory")
	require.Contains(t, prompt, "delete_memory")
	require.Contains(t, prompt, "no memories yet")
}

func seedChannelWithMembers(t *testing.T, s *storemod.FileStore, name domain.ChannelName, members ...domain.Nick) {
	t.Helper()

	require.NoError(t, s.SaveChannel(t.Context(), domain.Channel{
		Name:    name,
		Kind:    domain.KindChannel,
		Members: set.NewOrdered(members...),
		Created: fixedTime,
	}))
}

func seedInstance(t *testing.T, s *storemod.FileStore, inst domain.ModelInstance) {
	t.Helper()

	require.NoError(t, s.SaveInstance(t.Context(), inst))
}

// seedUserMessage saves a user message directly to the store and
// returns the message and its protocol representation. Unlike
// sess.SendMessage, this does not trigger background dispatch.
func seedUserMessage(t *testing.T, s *storemod.FileStore, ch domain.ChannelName, body string) (domain.Message, protocol.IRCMessage) {
	t.Helper()

	msg := domain.Message{
		ID:      fmt.Sprintf("%d", fixedTime.UnixNano()),
		Channel: ch,
		From:    "testuser",
		Body:    body,
		SentAt:  fixedTime,
	}

	require.NoError(t, s.SaveMessage(t.Context(), msg))

	return msg, protocol.FromMessage(msg)
}

func TestSession_DispatchToChannel_content_filtered_returns_silence(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFullFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{}, api.ErrContentFiltered
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)
	require.Empty(t, replies)
}

func TestSession_DispatchToChannel_model_refused_returns_silence(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFullFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{}, &api.ErrModelRefused{Reason: "I cannot help with that"}
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)
	require.Empty(t, replies)
}

func TestSession_DispatchToChannel_truncated_returns_error(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFullFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{}, api.ErrResponseTruncated
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.ErrorIs(t, err, api.ErrResponseTruncated)
}

// drainEvents reads from the session events channel until n
// DispatchDoneEvent values have been received, and returns all
// events in order.
func drainEvents(t *testing.T, sess *Session, doneCount int) []domain.SessionEvent {
	t.Helper()

	var events []domain.SessionEvent
	done := 0

	for evt := range sess.Events() {
		events = append(events, evt)
		if _, ok := evt.(domain.DispatchDoneEvent); ok {
			done++
			if done >= doneCount {
				return events
			}
		}
	}

	t.Fatal("events channel closed before receiving all DispatchDoneEvents")

	return nil
}
