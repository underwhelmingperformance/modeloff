package session

import (
	"context"
	"fmt"
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
		Title:   "Already here",
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
	require.Equal(t, "Already here", ch.Title)
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
		},
		At: fixedTime,
	}, evt)

	// Instance should be persisted.
	inst, err := s.GetInstance(ctx, "fakenick")
	require.NoError(t, err)
	require.Equal(t, domain.ModelID("anthropic/claude-3-haiku"), inst.ModelID)

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
}

func TestSession_SendMessage_broadcasts_to_channel_instances(t *testing.T) {
	fake := &fakeAPIClient{}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	evt, err := sess.SendMessage(ctx, "#general", "hello world")
	require.NoError(t, err)

	require.Len(t, fake.sendEventsCalls, 1)
	require.Equal(t, sendEventsCall{
		modelID: "test/model",
		system:  "You are botty, a participant in an IRC-style chat on #general. Reply only when you have something useful to add.",
		events: []protocol.IRCMessage{
			protocol.FromMessage(evt.Message),
		},
	}, fake.sendEventsCalls[0])
}

func TestSession_SendMessage_does_not_broadcast_when_no_model_instances(t *testing.T) {
	fake := &fakeAPIClient{}
	sess, s := newTestSessionWithAPI(t, fake)

	seedChannelWithMembers(t, s, "#general", "testuser")

	_, err := sess.SendMessage(t.Context(), "#general", "hello world")
	require.NoError(t, err)
	require.Empty(t, fake.sendEventsCalls)
}

func TestSession_SendMessage_pass_response_does_not_store_model_message(t *testing.T) {
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

	evt, err := sess.SendMessage(ctx, "#general", "hello world")
	require.NoError(t, err)

	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{evt.Message}, msgs)
}

func TestSession_SendMessage_reply_response_stores_model_message(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.ModelResponse{
				Kind: protocol.ResponseReply,
				Body: "hello back",
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

	evt, err := sess.SendMessage(ctx, "#general", "hello world")
	require.NoError(t, err)

	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{
		evt.Message,
		{
			ID:      fmt.Sprintf("%d~botty", fixedTime.UnixNano()),
			Channel: "#general",
			From:    "botty",
			Body:    "hello back",
			SentAt:  fixedTime,
		},
	}, msgs)
}

func TestSession_SendMessage_broadcasts_only_to_members_of_that_channel(t *testing.T) {
	fake := &fakeAPIClient{}
	sess, s := newTestSessionWithAPI(t, fake)

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

	_, err := sess.SendMessage(t.Context(), "#general", "hello world")
	require.NoError(t, err)

	require.Len(t, fake.sendEventsCalls, 1)
	require.Equal(t, domain.ModelID("test/model-a"), fake.sendEventsCalls[0].modelID)
}

func TestSession_SendMessage_reply_is_not_rebroadcast_in_same_send(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.ModelResponse{
				Kind: protocol.ResponseReply,
				Body: "reply once",
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

	evt, err := sess.SendMessage(ctx, "#general", "hello world")
	require.NoError(t, err)

	require.Len(t, fake.sendEventsCalls, 1)

	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{
		evt.Message,
		{
			ID:      fmt.Sprintf("%d~botty", fixedTime.UnixNano()),
			Channel: "#general",
			From:    "botty",
			Body:    "reply once",
			SentAt:  fixedTime,
		},
	}, msgs)
}

func TestSession_SendMessage_multiple_instances_each_reply_once(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.ModelResponse{
				Kind: protocol.ResponseReply,
				Body: fmt.Sprintf("reply from %s", modelID),
			}, nil
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

	evt, err := sess.SendMessage(ctx, "#general", "hello world")
	require.NoError(t, err)

	require.Len(t, fake.sendEventsCalls, 2)

	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{
		evt.Message,
		{
			ID:      fmt.Sprintf("%d~bot-a", fixedTime.UnixNano()),
			Channel: "#general",
			From:    "bot-a",
			Body:    "reply from test/model-a",
			SentAt:  fixedTime,
		},
		{
			ID:      fmt.Sprintf("%d~bot-b", fixedTime.UnixNano()),
			Channel: "#general",
			From:    "bot-b",
			Body:    "reply from test/model-b",
			SentAt:  fixedTime,
		},
	}, msgs)
}

func TestSession_SendMessage_ignores_empty_reply_body(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.ModelResponse{
				Kind: protocol.ResponseReply,
				Body: "   ",
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

	evt, err := sess.SendMessage(ctx, "#general", "hello world")
	require.NoError(t, err)

	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{evt.Message}, msgs)
}

func TestSession_SendMessage_api_error_continues_to_next_instance(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			if modelID == "test/model-a" {
				return protocol.ModelResponse{}, fmt.Errorf("network timeout")
			}

			return protocol.ModelResponse{
				Kind: protocol.ResponseReply,
				Body: "reply from bot-b",
			}, nil
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

	evt, err := sess.SendMessage(ctx, "#general", "hello world")
	require.Error(t, err, "should surface the API error")
	require.ErrorContains(t, err, "network timeout")

	require.Len(t, fake.sendEventsCalls, 2, "both instances should be dispatched to")

	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{
		evt.Message,
		{
			ID:      fmt.Sprintf("%d~bot-b", fixedTime.UnixNano()),
			Channel: "#general",
			From:    "bot-b",
			Body:    "reply from bot-b",
			SentAt:  fixedTime,
		},
	}, msgs)
}

func TestSession_Poke_api_error_continues_to_next_channel(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			if modelID == "test/model-a" {
				return protocol.ModelResponse{}, fmt.Errorf("rate limited")
			}

			return protocol.ModelResponse{
				Kind: protocol.ResponseReply,
				Body: "still here",
			}, nil
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

	err := sess.Poke(ctx)
	require.Error(t, err, "should surface the API error")
	require.ErrorContains(t, err, "rate limited")

	msgs, err := s.ListMessages(ctx, "#random")
	require.NoError(t, err)
	require.Len(t, msgs, 1, "bot-b reply should still be saved")
	require.Equal(t, "still here", msgs[0].Body)
}

func TestSession_SetTitle(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	ch := domain.Channel{Name: "#dev", Kind: domain.KindChannel, Created: fixedTime}
	require.NoError(t, s.SaveChannel(ctx, ch))

	evt, err := sess.SetTitle(ctx, "#dev", "Development Chat")
	require.NoError(t, err)
	require.Equal(t, domain.TopicChangeEvent{
		Channel: "#dev",
		Title:   "Development Chat",
		By:      "testuser",
		At:      fixedTime,
	}, evt)

	// Channel title should be updated.
	updated, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, "Development Chat", updated.Title)
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
		generateNickFn: func(_ context.Context, _ domain.ModelID) (domain.Nick, error) {
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
		},
		At: fixedTime,
	}, evt)

	inst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)
	require.Equal(t, set.NewOrdered[domain.ChannelName]("#general", "#random"), inst.Channels)

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
		},
		At: fixedTime,
	}, evt2)

	instances, err := s.ListInstances(ctx)
	require.NoError(t, err)
	require.Len(t, instances, 1, "should not create a duplicate instance")
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

func TestSession_SetTitleNonexistentChannel(t *testing.T) {
	sess, _ := newTestSession(t)

	_, err := sess.SetTitle(t.Context(), "#ghost", "title")
	require.Error(t, err)
}

func TestSession_SendMessage_includes_memory_in_prompt(t *testing.T) {
	memStore := memory.NewFileStore(t.TempDir())
	require.NoError(t, memStore.Write(t.Context(), "botty", memory.Entry{
		Key:     "mood",
		Content: "curious",
	}))

	fake := &fakeAPIClient{}
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

	_, err := sess.SendMessage(t.Context(), "#general", "hello world")
	require.NoError(t, err)

	require.Len(t, fake.sendEventsCalls, 1)
	require.Contains(t, fake.sendEventsCalls[0].system, `Your persona is "Helpful assistant".`)
	require.Contains(t, fake.sendEventsCalls[0].system, "[mood=curious]")
}

func TestSession_Poke_sends_poke_event(t *testing.T) {
	fake := &fakeAPIClient{}
	sess, s := newTestSessionWithAPI(t, fake)

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.ModelInstance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: set.NewOrdered[domain.ChannelName]("#general"),
	})

	err := sess.Poke(t.Context())
	require.NoError(t, err)

	require.Len(t, fake.sendEventsCalls, 1)
	require.Equal(t, []protocol.IRCMessage{
		{
			Kind:   protocol.KindPoke,
			From:   "modeloff",
			Target: "#general",
			At:     fixedTime,
		},
	}, fake.sendEventsCalls[0].events)
}

func TestSession_Poke_persists_replies(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.ModelResponse{
				Kind: protocol.ResponseReply,
				Body: "still here",
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

	err := sess.Poke(ctx)
	require.NoError(t, err)

	msgs, err := s.ListMessages(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, []domain.Message{
		{
			ID:      fmt.Sprintf("%d~botty", fixedTime.UnixNano()),
			Channel: "#general",
			From:    "botty",
			Body:    "still here",
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

func TestSession_SendMessage_to_dm_only_targets_that_instance(t *testing.T) {
	fake := &fakeAPIClient{}
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

	_, err = sess.SendMessage(ctx, "botty", "hello in dm")
	require.NoError(t, err)

	require.Len(t, fake.sendEventsCalls, 1)
	require.Equal(t, domain.ModelID("test/model-a"), fake.sendEventsCalls[0].modelID)
	require.Equal(t, []protocol.IRCMessage{
		{
			Kind:   protocol.KindPrivMsg,
			From:   "testuser",
			Target: "botty",
			Body:   "hello in dm",
			At:     fixedTime,
		},
	}, fake.sendEventsCalls[0].events)
}

func TestSession_SetAPIKey(t *testing.T) {
	cfgStore := &fakeConfigStore{
		cfg: config.Config{
			UserNick:     "testuser",
			PokeInterval: 5 * time.Minute,
		},
	}
	s := storemod.NewFileStore(t.TempDir())
	sess := New(s, nil, &fakeAPIClient{}, cfgStore, "testuser")

	cfg, err := sess.SetAPIKey(t.Context(), "test-key")
	require.NoError(t, err)
	require.Equal(t, "test-key", cfg.APIKey)
	require.Equal(t, "test-key", cfgStore.saved.APIKey)
	require.Equal(t, 1, cfgStore.saveCalls)
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

// --- Fake API client ---

type fakeAPIClient struct {
	listModelsFn    func(context.Context) ([]api.ModelInfo, error)
	sendEventsFn    func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error)
	generateNickFn  func(context.Context, domain.ModelID) (domain.Nick, error)
	sendEventsCalls []sendEventsCall
}

type sendEventsCall struct {
	modelID domain.ModelID
	system  string
	history []protocol.IRCMessage
	events  []protocol.IRCMessage
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
) (protocol.ModelResponse, error) {
	f.sendEventsCalls = append(f.sendEventsCalls, sendEventsCall{
		modelID: modelID,
		system:  system,
		history: append([]protocol.IRCMessage(nil), history...),
		events:  append([]protocol.IRCMessage(nil), events...),
	})

	if f.sendEventsFn != nil {
		return f.sendEventsFn(ctx, modelID, system, history, events)
	}

	return protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "fake"}, nil
}

func (f *fakeAPIClient) GenerateNick(ctx context.Context, modelID domain.ModelID) (domain.Nick, error) {
	if f.generateNickFn != nil {
		return f.generateNickFn(ctx, modelID)
	}

	return "fakenick", nil
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
