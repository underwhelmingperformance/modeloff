package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/domain"
)

func TestJoinAs_model_actor(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: orderedmap.New[domain.ChannelName, time.Time](),
	})

	require.NoError(t, sess.JoinAs(ctx, "botty", "#dev"))

	evt := drainEvent[domain.JoinEvent](t, sess)
	require.Equal(t, domain.JoinEvent{
		Channel: "#dev",
		Nick:    "botty",
		Created: true,
		At:      fixedTime,
	}, evt)

	ch, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	require.True(t, ch.Members.Has("botty"))

	inst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)

	_, ok := inst.Channels.Get("#dev")
	require.True(t, ok)

	// Model join should not set last channel.
	last, err := s.GetLastChannel(ctx)
	require.NoError(t, err)
	require.Empty(t, last)
}

func TestPartAs_model_actor(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#dev", "testuser", "botty")
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#dev"),
	})

	require.NoError(t, sess.PartAs(ctx, "botty", "#dev", "goodbye"))

	evt := drainEvent[domain.PartEvent](t, sess)
	require.Equal(t, domain.PartEvent{
		Channel: "#dev",
		Nick:    "botty",
		Message: "goodbye",
		At:      fixedTime,
	}, evt)

	ch, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	require.False(t, ch.Members.Has("botty"))
	require.True(t, ch.Members.Has("testuser"))

	inst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)

	_, ok := inst.Channels.Get("#dev")
	require.False(t, ok)
}

func TestQuitAs_model_actor(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#dev", "testuser", "botty")
	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#dev", "#general"),
	})

	require.NoError(t, sess.QuitAs(ctx, "botty", "farewell"))

	evt := drainEvent[domain.QuitEvent](t, sess)
	require.Equal(t, domain.QuitEvent{
		Nick:    "botty",
		Message: "farewell",
		At:      fixedTime,
	}, evt)

	// Instance should be deleted.
	_, err := s.GetInstance(ctx, "botty")
	require.Error(t, err)

	// Model should be removed from both channels.
	ch1, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	require.False(t, ch1.Members.Has("botty"))

	ch2, err := s.GetChannel(ctx, "#general")
	require.NoError(t, err)
	require.False(t, ch2.Members.Has("botty"))

	// Quit events should be appended to both channels.
	types1 := channelEventTypes(t, s, "#dev")
	require.Equal(t, []string{"quit"}, types1)

	types2 := channelEventTypes(t, s, "#general")
	require.Equal(t, []string{"quit"}, types2)
}

func TestSendMessageAs_model_actor(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#dev", "testuser", "botty")

	require.NoError(t, sess.SendMessageAs(ctx, "botty", "#dev", "hello world"))

	evt := drainEvent[domain.MessageEvent](t, sess)
	require.Equal(t, domain.MessageEvent{
		Event: domain.ChannelMessage{
			Channel: "#dev",
			From:    "botty",
			Body:    "hello world",
			At:      fixedTime,
		},
	}, evt)

	msgs := channelMessages(t, s, "#dev")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#dev", From: "botty", Body: "hello world", At: fixedTime},
	}, msgs)
}

func TestSetTopicAs_model_actor(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#dev", "testuser", "botty")

	require.NoError(t, sess.SetTopicAs(ctx, "botty", "#dev", "new topic"))

	evt := drainEvent[domain.TopicChangeEvent](t, sess)
	require.Equal(t, domain.TopicChangeEvent{
		Channel: "#dev",
		Topic:   "new topic",
		By:      "botty",
		At:      fixedTime,
	}, evt)

	ch, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, "new topic", ch.Topic)
	require.Equal(t, domain.Nick("botty"), ch.TopicSetBy)
	require.Equal(t, fixedTime, ch.TopicSetAt)
}

func TestKickAs_model_actor(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#dev", "testuser", "botty", "helper")
	seedInstance(t, s, domain.Instance{
		Nick:     "helper",
		ModelID:  "test/model-b",
		Channels: testChannels("#dev"),
	})

	require.NoError(t, sess.KickAs(ctx, "botty", "helper", "#dev"))

	evt := drainEvent[domain.ModelKickedEvent](t, sess)
	require.Equal(t, domain.ModelKickedEvent{
		Channel: "#dev",
		Nick:    "helper",
		By:      "botty",
		At:      fixedTime,
	}, evt)

	ch, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	require.False(t, ch.Members.Has("helper"))
	require.True(t, ch.Members.Has("botty"))

	inst, err := s.GetInstance(ctx, "helper")
	require.NoError(t, err)

	_, ok := inst.Channels.Get("#dev")
	require.False(t, ok)
}

func TestInviteAs_model_actor(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#dev", "testuser", "botty")

	require.NoError(t, sess.InviteAs(ctx, "botty", "helper", "#dev"))

	// Model invites produce a system notice, not a real invite.
	events, err := s.EventsBefore(ctx, "#dev", nil, 100)
	require.NoError(t, err)

	var notices []domain.ChannelSystemNotice
	for _, se := range events {
		if n, ok := se.Event.(domain.ChannelSystemNotice); ok {
			notices = append(notices, n)
		}
	}

	require.Equal(t, []domain.ChannelSystemNotice{
		{Channel: "#dev", Text: "botty invited helper to #dev", At: fixedTime},
	}, notices)
}

func TestOpenDM_members_have_no_mode(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, s, domain.Instance{
		Nick:    "botty",
		ModelID: "test/model",
	})

	ch, _, err := sess.OpenDM(ctx, "botty")
	require.NoError(t, err)

	require.Equal(t, []domain.Member{
		{Nick: "botty", Mode: domain.ModeNone},
		{Nick: "testuser", Mode: domain.ModeNone},
	}, ch.Members.Slice())
}

func TestOpenDMAs_members_have_no_mode(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model-a",
		Channels: orderedmap.New[domain.ChannelName, time.Time](),
	})
	seedInstance(t, s, domain.Instance{
		Nick:     "helper",
		ModelID:  "test/model-b",
		Channels: orderedmap.New[domain.ChannelName, time.Time](),
	})

	ch, _, err := sess.OpenDMAs(ctx, "botty", "helper")
	require.NoError(t, err)

	require.Equal(t, []domain.Member{
		{Nick: "botty", Mode: domain.ModeNone},
		{Nick: "helper", Mode: domain.ModeNone},
	}, ch.Members.Slice())
}

func TestSetTopicAs_rejects_DM(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, s, domain.Instance{
		Nick:    "botty",
		ModelID: "test/model",
	})

	_, _, err := sess.OpenDM(ctx, "botty")
	require.NoError(t, err)

	err = sess.SetTopic(ctx, "botty", "some topic")
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot set topic on a direct message")
}

func TestKickAs_rejects_DM(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, s, domain.Instance{
		Nick:    "botty",
		ModelID: "test/model",
	})

	_, _, err := sess.OpenDM(ctx, "botty")
	require.NoError(t, err)

	err = sess.Kick(ctx, "botty", "botty")
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot kick from a direct message")
}

func TestJoinAs_DM_no_join_event(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	// Seed a DM channel where the user is NOT yet a member.
	members := domain.NewMemberList()
	members.Add("botty")

	require.NoError(t, s.SaveChannel(ctx, domain.Channel{
		Name:    "botty",
		Kind:    domain.KindDM,
		Members: members,
		Created: fixedTime,
	}))

	// Join the DM channel — should not emit join events.
	require.NoError(t, sess.Join(ctx, "botty"))

	types := channelEventTypes(t, s, "botty")
	require.Empty(t, types)
}

func TestOpenDMAs_model_to_model(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model-a",
		Channels: orderedmap.New[domain.ChannelName, time.Time](),
	})
	seedInstance(t, s, domain.Instance{
		Nick:     "helper",
		ModelID:  "test/model-b",
		Channels: orderedmap.New[domain.ChannelName, time.Time](),
	})

	ch, created, err := sess.OpenDMAs(ctx, "botty", "helper")
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, domain.ChannelName("helper"), ch.Name)
	require.Equal(t, domain.KindDM, ch.Kind)
	require.Equal(t, []domain.Member{
		{Nick: "botty", Mode: domain.ModeNone},
		{Nick: "helper", Mode: domain.ModeNone},
	}, ch.Members.Slice())

	// Both instances should have the DM channel attached.
	actorInst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)

	_, ok := actorInst.Channels.Get("helper")
	require.True(t, ok)

	targetInst, err := s.GetInstance(ctx, "helper")
	require.NoError(t, err)

	_, ok = targetInst.Channels.Get("helper")
	require.True(t, ok)
}

func TestOpenDMAs_model_to_model_appears_in_channel_list(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model-a",
		Channels: orderedmap.New[domain.ChannelName, time.Time](),
	})
	seedInstance(t, s, domain.Instance{
		Nick:     "helper",
		ModelID:  "test/model-b",
		Channels: orderedmap.New[domain.ChannelName, time.Time](),
	})

	_, _, err := sess.OpenDMAs(ctx, "botty", "helper")
	require.NoError(t, err)

	channels, err := s.ListChannels(ctx)
	require.NoError(t, err)

	var names []domain.ChannelName
	for _, ch := range channels {
		names = append(names, ch.Name)
	}

	require.Equal(t, []domain.ChannelName{"helper"}, names)
}

func TestOpenDMAs_model_to_model_message_dispatches(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model-a",
		Channels: orderedmap.New[domain.ChannelName, time.Time](),
	})
	seedInstance(t, s, domain.Instance{
		Nick:     "helper",
		ModelID:  "test/model-b",
		Channels: orderedmap.New[domain.ChannelName, time.Time](),
	})

	_, _, err := sess.OpenDMAs(ctx, "botty", "helper")
	require.NoError(t, err)

	require.NoError(t, sess.SendMessageAs(ctx, "botty", "helper", "hey there"))

	evt := drainEvent[domain.MessageEvent](t, sess)
	require.Equal(t, domain.MessageEvent{
		Event: domain.ChannelMessage{
			Channel: "helper",
			From:    "botty",
			Body:    "hey there",
			At:      fixedTime,
		},
	}, evt)

	// Verify dispatch was triggered (emit, not emitUIOnly) by
	// waiting for DispatchStartedEvent and DispatchDoneEvent that
	// dispatchInBackground emits.
	started := drainEvent[domain.DispatchStartedEvent](t, sess)
	require.Equal(t, domain.ChannelName("helper"), started.Channel)

	done := drainEvent[domain.DispatchDoneEvent](t, sess)
	require.Equal(t, domain.ChannelName("helper"), done.Channel)

	msgs := channelMessages(t, s, "helper")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "helper", From: "botty", Body: "hey there", At: fixedTime},
	}, msgs)
}

func TestJoinAs_normalises_channel_prefix(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: orderedmap.New[domain.ChannelName, time.Time](),
	})

	// Model joins with bare name (no # prefix).
	require.NoError(t, sess.JoinAs(ctx, "botty", "modeloff"))

	evt := drainEvent[domain.JoinEvent](t, sess)
	require.Equal(t, domain.ChannelName("#modeloff"), evt.Channel)

	// Channel should exist with the normalised name.
	ch, err := s.GetChannel(ctx, "#modeloff")
	require.NoError(t, err)
	require.True(t, ch.Members.Has("botty"))

	// The bare name should not exist.
	_, err = s.GetChannel(ctx, "modeloff")
	require.Error(t, err)
}

func TestJoinAs_user_rejoin_preserves_join_time(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.Join(ctx, "#general"))
	drainEvent[domain.JoinEvent](t, sess)

	originalJoinTime := sess.UserJoinedAt("#general")
	require.Equal(t, fixedTime, originalJoinTime)

	// Advance the clock so a second join would have a different time.
	sess.now = func() time.Time { return fixedTime.Add(time.Hour) }

	require.NoError(t, sess.Join(ctx, "#general"))

	// No join event is emitted for an already-member rejoin, and the
	// original join time is preserved.
	require.Equal(t, originalJoinTime, sess.UserJoinedAt("#general"))
}

func TestJoinAs_user_new_channel_emits_join_and_mode(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.JoinAs(ctx, "testuser", "#dev"))

	joinEvt := drainEvent[domain.JoinEvent](t, sess)
	require.Equal(t, domain.JoinEvent{
		Channel: "#dev", Nick: "testuser", Created: true, At: fixedTime,
	}, joinEvt)

	modeEvt := drainEventSkipping[domain.ModeChangeEvent](t, sess)
	require.Equal(t, domain.ModeChangeEvent{
		Channel: "#dev", Nick: "testuser", Mode: domain.ModeOp, Actor: "ChanServ", At: fixedTime,
	}, modeEvt)

	ch, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)

	m, ok := ch.Members.Get("testuser")
	require.True(t, ok)
	require.Equal(t, domain.ModeOp, m.Mode)
}

func TestJoinAs_user_existing_channel_with_topic(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, s.SaveChannel(ctx, domain.Channel{
		Name:       "#dev",
		Kind:       domain.KindChannel,
		Topic:      "Go development",
		TopicSetBy: "alice",
		TopicSetAt: fixedTime.Add(-time.Hour),
		Members:    testMembers("alice"),
		Created:    fixedTime.Add(-time.Hour),
	}))

	require.NoError(t, sess.JoinAs(ctx, "testuser", "#dev"))

	joinEvt := drainEvent[domain.JoinEvent](t, sess)
	require.Equal(t, domain.JoinEvent{
		Channel: "#dev", Nick: "testuser", Created: false, At: fixedTime,
	}, joinEvt)

	modeEvt := drainEventSkipping[domain.ModeChangeEvent](t, sess)
	require.Equal(t, domain.ModeChangeEvent{
		Channel: "#dev", Nick: "testuser", Mode: domain.ModeOp, Actor: "ChanServ", At: fixedTime,
	}, modeEvt)

	topicEvt := drainEventSkipping[domain.TopicInfoEvent](t, sess)
	require.Equal(t, domain.TopicInfoEvent{
		Channel:    "#dev",
		Topic:      "Go development",
		TopicSetBy: "alice",
		TopicSetAt: fixedTime.Add(-time.Hour),
		At:         fixedTime,
	}, topicEvt)
}

func TestJoinAs_user_existing_channel_no_topic(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, s.SaveChannel(ctx, domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Members: testMembers("alice"),
		Created: fixedTime.Add(-time.Hour),
	}))

	require.NoError(t, sess.JoinAs(ctx, "testuser", "#dev"))

	_ = drainEvent[domain.JoinEvent](t, sess)
	_ = drainEventSkipping[domain.ModeChangeEvent](t, sess)

	// No topic event should be emitted. Drain dispatch events, then verify.
	drainDispatchEvents(t, sess)
	select {
	case evt := <-sess.Events():
		t.Fatalf("unexpected event: %T %+v", evt, evt)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestJoinAs_model_no_mode_or_topic(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#dev", "testuser")
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: orderedmap.New[domain.ChannelName, time.Time](),
	})

	ch, _ := s.GetChannel(ctx, "#dev")
	ch.Topic = "some topic"
	require.NoError(t, s.SaveChannel(ctx, ch))

	require.NoError(t, sess.JoinAs(ctx, "botty", "#dev"))

	joinEvt := drainEvent[domain.JoinEvent](t, sess)
	require.Equal(t, domain.Nick("botty"), joinEvt.Nick)

	// Drain dispatch events triggered by the join.
	drainDispatchEvents(t, sess)

	select {
	case evt := <-sess.Events():
		t.Fatalf("unexpected event after model join: %T %+v", evt, evt)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestJoinAs_user_updates_autojoin(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.JoinAs(ctx, "testuser", "#general"))
	drainEventSkipping[domain.JoinEvent](t, sess)
	drainEventSkipping[domain.ModeChangeEvent](t, sess)

	require.NoError(t, sess.JoinAs(ctx, "testuser", "#dev"))
	drainEventSkipping[domain.JoinEvent](t, sess)
	drainEventSkipping[domain.ModeChangeEvent](t, sess)

	got, err := s.ListAutojoinChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.ChannelName{"#dev", "#general"}, got)
}

func TestPartAs_user_updates_autojoin(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.JoinAs(ctx, "testuser", "#general"))
	drainEventSkipping[domain.JoinEvent](t, sess)
	drainEventSkipping[domain.ModeChangeEvent](t, sess)

	require.NoError(t, sess.JoinAs(ctx, "testuser", "#dev"))
	drainEventSkipping[domain.JoinEvent](t, sess)
	drainEventSkipping[domain.ModeChangeEvent](t, sess)

	require.NoError(t, sess.PartAs(ctx, "testuser", "#general", "bye"))
	drainEventSkipping[domain.PartEvent](t, sess)

	got, err := s.ListAutojoinChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.ChannelName{"#dev"}, got)
}
