package session

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/domain"
)

func TestJoinAs_model_actor(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: orderedmap.New[domain.ChannelName, time.Time](),
	})

	require.NoError(t, sess.JoinAs(ctx, botty, "#dev"))

	evt := drainEventSkipping[domain.Join](t, sess)
	require.Equal(t, domain.Join{
		Target:     "#dev",
		Nick:       "botty",
		InstanceID: botty.ID(),
		Created:    true,
		At:         fixedTime,
		Instance:   botty,
	}, evt)

	mode := drainEventSkipping[domain.ModeChange](t, sess)
	require.Equal(t, domain.ModeChange{
		Target:     "#dev",
		Nick:       "botty",
		InstanceID: botty.ID(),
		Mode:       domain.ModeVoice,
		By:         "ChanServ",
		At:         fixedTime,
		Instance:   botty,
		Actor:      "ChanServ",
	}, mode)

	ch, err := sess.loadChannelWindow(ctx, "#dev")
	require.NoError(t, err)
	modelOnlyMembers := domain.NewMemberList()
	modelOnlyMembers.Add(botty)
	modelOnlyMembers.SetMode(botty, domain.ModeVoice)
	requireChannelEqual(t, newTestChannelWindow("#dev", fixedTime, modelOnlyMembers), ch)

	inst, err := s.ResolveNick(ctx, "botty")
	require.NoError(t, err)
	requireInstanceEqual(t, domain.NewModelInstance(
		testMemberID("botty"), "botty", "test/model", "", testChannels("#dev"),
	), inst)

	// `last_channel` is a UI-owned write — neither user nor model
	// joins touch it from the session.
	last, err := s.GetLastChannel(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.ChannelName(""), last)
}

func TestPartAs_model_actor(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#dev"),
	})
	seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")

	require.NoError(t, sess.PartAs(ctx, botty, "#dev", "goodbye"))

	evt := drainEvent[domain.Part](t, sess)
	require.Equal(t, domain.Part{
		Target:     "#dev",
		Nick:       "botty",
		InstanceID: botty.ID(),
		Message:    "goodbye",
		At:         fixedTime,
		Instance:   botty,
	}, evt)

	ch, err := sess.loadChannelWindow(ctx, "#dev")
	require.NoError(t, err)
	requireChannelEqual(t, newTestChannelWindow("#dev", fixedTime, testMembers(t, sess, s, "testuser")), ch)

	inst, err := s.ResolveNick(ctx, "botty")
	require.NoError(t, err)
	requireInstanceEqual(t, domain.NewModelInstance(
		testMemberID("botty"), "botty", "test/model", "", testChannels(),
	), inst)
}

func TestPartAs_unknown_actor_is_noop(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	saveTestChannel(t, sess, s, newTestChannelWindow("#dev", fixedTime, testMembers(t, sess, s, "testuser")))

	// PartAs for an instance that isn't in the channel must be a
	// no-op: no PartEvent emission (the empty-id fallback would
	// otherwise ask the UI to drop the human's channel), no stored
	// membership mutation, no instance-channels mutation.
	ghost := domain.NewModelInstance("ghost-id", "ghost", "test/model", "", nil)
	require.NoError(t, sess.PartAs(ctx, ghost, "#dev", "bye"))

	select {
	case evt := <-sess.Events():
		t.Fatalf("unexpected event for unknown-actor part: %T %+v", evt, evt)
	case evt := <-sess.User().Events():
		t.Fatalf("unexpected event for unknown-actor part: %T %+v", evt, evt)
	case <-time.After(50 * time.Millisecond):
	}

	updated, err := sess.loadChannelWindow(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, slices.Collect(testMembers(t, sess, s, "testuser").All()), slices.Collect(updated.Members.All()))
}

func TestQuitAs_model_actor(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#dev", "#general"),
	})
	seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	require.NoError(t, sess.QuitAs(ctx, botty, "farewell"))

	evt := drainEvent[domain.Quit](t, sess)
	require.Equal(t, domain.Quit{
		Channels:   []domain.ChannelName{"#dev", "#general"},
		Nick:       "botty",
		InstanceID: botty.ID(),
		Message:    "farewell",
		At:         fixedTime,
		Instance:   botty,
	}, evt)

	// Instance should be deleted.
	_, err := s.ResolveNick(ctx, "botty")
	require.Error(t, err)

	// Model should be removed from both channels.
	ch1, err := sess.loadChannelWindow(ctx, "#dev")
	require.NoError(t, err)
	requireChannelEqual(t, newTestChannelWindow("#dev", fixedTime, testMembers(t, sess, s, "testuser")), ch1)

	ch2, err := sess.loadChannelWindow(ctx, "#general")
	require.NoError(t, err)
	requireChannelEqual(t, newTestChannelWindow("#general", fixedTime, testMembers(t, sess, s, "testuser")), ch2)

	// Quit events should be appended to both channels.
	types1 := channelEventTypes(t, s, "#dev")
	require.Equal(t, []string{"quit"}, types1)

	types2 := channelEventTypes(t, s, "#general")
	require.Equal(t, []string{"quit"}, types2)
}

func TestSendMessageAs_model_actor(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:    "botty",
		ModelID: "test/model",
	})
	seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")

	_, err := sess.SendMessageAs(ctx, botty, "#dev", "hello world")
	require.NoError(t, err)

	evt := drainEvent[domain.Message](t, sess)
	require.Equal(t, domain.Message{
		Target:     "#dev",
		From:       "botty",
		InstanceID: testMemberID("botty"),
		Body:       "hello world",
		At:         fixedTime,
	}, evt)

	msgs := channelMessages(t, s, "#dev")
	require.Equal(t, []domain.Message{
		{Target: "#dev", From: "botty", InstanceID: testMemberID("botty"), Body: "hello world", At: fixedTime},
	}, msgs)
}

// TestSendMessageAs_user_actor_does_not_echo_to_originator pins the
// echo gate's structural property: a PRIVMSG sent by the user to a
// channel they're in does not arrive on their own subscription's
// events channel. The session's chat-traffic suppression rule is
// applied uniformly at fan-out (RFC 2812 §3.3.1); the user-actor
// branch in [Session.SendMessageAs] no longer carries it.
func TestSendMessageAs_user_actor_does_not_echo_to_originator(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.JoinAs(ctx, sess.user, "#dev"))
	drainEvent[domain.Join](t, sess)
	drainEventSkipping[domain.ModeChange](t, sess)
	drainDispatchEvents(t, sess)

	_, err := sess.SendMessageAs(ctx, sess.user, "#dev", "hello")
	require.NoError(t, err)

	drainDispatchEvents(t, sess)

	if evt, ok := peekEvent(sess); ok {
		t.Fatalf("expected the user's own message to be suppressed, got %T %+v", evt, evt)
	}
}

func TestSetTopicAs_model_actor(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
	seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")

	require.NoError(t, sess.SetTopicAs(ctx, botty, "#dev", "new topic"))

	evt := drainEvent[domain.TopicChange](t, sess)
	require.Equal(t, domain.TopicChange{
		Target:     "#dev",
		Topic:      "new topic",
		By:         "botty",
		At:         fixedTime,
		ByInstance: botty,
	}, evt)

	ch, err := sess.loadChannelWindow(ctx, "#dev")
	require.NoError(t, err)

	expected := newTestChannelWindow("#dev", fixedTime, testMembers(t, sess, s, "testuser", "botty"))
	expected.Topic = "new topic"
	expected.TopicSetBy = "botty"
	expected.TopicSetAt = fixedTime
	requireChannelEqual(t, expected, ch)
}

func TestKickAs_model_actor(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
	helper := seedInstance(t, s, instanceSpec{
		Nick:     "helper",
		ModelID:  "test/model-b",
		Channels: testChannels("#dev"),
	})
	seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty", "helper")

	require.NoError(t, sess.KickAs(ctx, botty, helper, "#dev"))

	evt := drainEvent[domain.ModelKicked](t, sess)
	require.Equal(t, domain.ModelKicked{
		Target:     "#dev",
		Nick:       "helper",
		InstanceID: helper.ID(),
		By:         "botty",
		At:         fixedTime,
		Instance:   helper,
	}, evt)

	ch, err := sess.loadChannelWindow(ctx, "#dev")
	require.NoError(t, err)
	requireChannelEqual(t, newTestChannelWindow("#dev", fixedTime, testMembers(t, sess, s, "testuser", "botty")), ch)

	inst, err := s.ResolveNick(ctx, "helper")
	require.NoError(t, err)
	requireInstanceEqual(t, domain.NewModelInstance(
		testMemberID("helper"), "helper", "test/model-b", "", testChannels(),
	), inst)
}

func TestInviteAs_model_actor(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
	seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")

	require.NoError(t, sess.InviteAs(ctx, botty, "helper", "#dev"))

	// Model invites produce a system notice, not a real invite.
	events, err := s.EventsBefore(ctx, "#dev", nil, 100)
	require.NoError(t, err)

	var notices []domain.SystemNotice
	for _, se := range events {
		if n, ok := se.Event.(domain.SystemNotice); ok {
			notices = append(notices, n)
		}
	}

	require.Equal(t, []domain.SystemNotice{
		{Target: "#dev", Text: "botty invited helper to #dev", At: fixedTime},
	}, notices)
}

func TestSetTopicAs_rejects_DM(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:    "botty",
		ModelID: "test/model",
	})

	err := sess.SetTopic(ctx, domain.ChannelName(botty.ID()), "some topic")
	require.EqualError(t, err, "cannot set topic on a direct message")
}

func TestKickAs_rejects_DM(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:    "botty",
		ModelID: "test/model",
	})

	err := sess.Kick(ctx, domain.ChannelName(botty.ID()), "botty")
	require.EqualError(t, err, "cannot kick from a direct message")
}

func TestSendMessageAs_rejects_status_channel(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()

	_, err := sess.SendMessageAs(ctx, sess.UserInstance(), domain.StatusChannelName, "hello")

	var guard domain.StatusChannelGuardError
	require.ErrorAs(t, err, &guard)
	require.Equal(t, domain.StatusChannelGuardError{
		Command: "send",
		Hint:    "the status channel doesn't take messages — try /msg <nick-or-#channel> instead",
	}, guard)

	if evt, ok := peekEvent(sess); ok {
		t.Fatalf("expected no event, got %T", evt)
	}
}

func TestSendActionAs_rejects_status_channel(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()

	_, err := sess.SendActionAs(ctx, sess.UserInstance(), domain.StatusChannelName, "waves")

	var guard domain.StatusChannelGuardError
	require.ErrorAs(t, err, &guard)
	require.Equal(t, domain.StatusChannelGuardError{
		Command: "me",
		Hint:    "the status channel doesn't take messages — try /msg <nick-or-#channel> instead",
	}, guard)

	if evt, ok := peekEvent(sess); ok {
		t.Fatalf("expected no event, got %T", evt)
	}
}

// TestSendMessageAs_model_to_model_dispatches verifies that the
// nick-targeted dispatch path fires when one model messages
// another. The wire-form target is the recipient's
// `InstanceID`; the recipient resolution in
// `dispatchInBackground` picks up the DM-shaped target and
// dispatches a turn to that addressed model directly.
func TestSendMessageAs_model_to_model_dispatches(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model-a",
		Channels: orderedmap.New[domain.ChannelName, time.Time](),
	})
	helper := seedInstance(t, s, instanceSpec{
		Nick:     "helper",
		ModelID:  "test/model-b",
		Channels: orderedmap.New[domain.ChannelName, time.Time](),
	})

	target := domain.ChannelName(helper.ID())

	_, err := sess.SendMessageAs(ctx, botty, target, "hey there")
	require.NoError(t, err)

	evt := drainEvent[domain.Message](t, sess)
	require.Equal(t, domain.Message{
		Target:     target,
		From:       "botty",
		InstanceID: testMemberID("botty"),
		Body:       "hey there",
		At:         fixedTime,
	}, evt)

	started := drainEvent[domain.DispatchStartedEvent](t, sess)
	require.Equal(t, target, started.Channel)
	require.Equal(t, []domain.Nick{"helper"}, started.Nicks)

	done := drainEvent[domain.DispatchDoneEvent](t, sess)
	require.Equal(t, target, done.Channel)

	msgs := channelMessages(t, s, target)
	require.Equal(t, []domain.Message{
		{Target: target, From: "botty", InstanceID: testMemberID("botty"), Body: "hey there", At: fixedTime},
	}, msgs)
}

func TestJoinAs_normalises_channel_prefix(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: orderedmap.New[domain.ChannelName, time.Time](),
	})

	// Model joins with bare name (no # prefix).
	require.NoError(t, sess.JoinAs(ctx, botty, "modeloff"))

	evt := drainEvent[domain.Join](t, sess)
	require.Equal(t, domain.ChannelName("#modeloff"), evt.Target)

	// Channel should exist with the normalised name.
	ch, err := sess.loadChannelWindow(ctx, "#modeloff")
	require.NoError(t, err)
	require.True(t, ch.Members.HasNick("botty"))

	// The bare name should not exist.
	_, err = sess.loadChannelWindow(ctx, "modeloff")
	require.Error(t, err)
}

func TestJoinAs_user_rejoin_preserves_join_time(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.Join(ctx, "#general"))
	drainEvent[domain.Join](t, sess)

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
	sess, _ := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.JoinAs(ctx, sess.UserInstance(), "#dev"))

	joinEvt := drainEvent[domain.Join](t, sess)
	require.Equal(t, domain.Join{
		Target: "#dev", Nick: "testuser", InstanceID: sess.UserInstance().ID(),
		Instance: sess.UserInstance(), Created: true, At: fixedTime,
	}, joinEvt)

	modeEvt := drainEventSkipping[domain.ModeChange](t, sess)
	require.Equal(t, domain.ModeChange{
		Target: "#dev", Nick: "testuser", InstanceID: sess.UserInstance().ID(),
		Instance: sess.UserInstance(), Mode: domain.ModeOp, By: "ChanServ", Actor: "ChanServ", At: fixedTime,
	}, modeEvt)

	ch, err := sess.loadChannelWindow(ctx, "#dev")
	require.NoError(t, err)

	m, ok := ch.Members.GetByInstance(sess.UserInstance())
	require.True(t, ok)
	require.Equal(t, domain.ModeOp, m.Mode)
}

func TestJoinAs_user_existing_channel_with_topic(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, s, instanceSpec{Nick: "alice", ModelID: "test/model"})

	withAlice := newTestChannelWindow("#dev", fixedTime.Add(-time.Hour), testMembers(t, sess, s, "alice"))
	withAlice.Topic = "Go development"
	withAlice.TopicSetBy = "alice"
	withAlice.TopicSetAt = fixedTime.Add(-time.Hour)
	saveTestChannel(t, sess, s, withAlice)

	require.NoError(t, sess.JoinAs(ctx, sess.UserInstance(), "#dev"))

	joinEvt := drainEvent[domain.Join](t, sess)
	require.Equal(t, domain.Join{
		Target: "#dev", Nick: "testuser", InstanceID: sess.UserInstance().ID(),
		Instance: sess.UserInstance(), Created: false, At: fixedTime,
	}, joinEvt)

	modeEvt := drainEventSkipping[domain.ModeChange](t, sess)
	require.Equal(t, domain.ModeChange{
		Target: "#dev", Nick: "testuser", InstanceID: sess.UserInstance().ID(),
		Instance: sess.UserInstance(), Mode: domain.ModeOp, By: "ChanServ", Actor: "ChanServ", At: fixedTime,
	}, modeEvt)

	topicEvt := drainEventSkipping[domain.TopicInfo](t, sess)
	require.Equal(t, domain.TopicInfo{
		Target:     "#dev",
		Topic:      "Go development",
		TopicSetBy: "alice",
		TopicSetAt: fixedTime.Add(-time.Hour),
		At:         fixedTime,
	}, topicEvt)
}

func TestJoinAs_user_existing_channel_no_topic(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, s, instanceSpec{Nick: "alice", ModelID: "test/model"})

	saveTestChannel(t, sess, s, newTestChannelWindow("#dev", fixedTime.Add(-time.Hour), testMembers(t, sess, s, "alice")))

	require.NoError(t, sess.JoinAs(ctx, sess.UserInstance(), "#dev"))

	_ = drainEvent[domain.Join](t, sess)
	_ = drainEventSkipping[domain.ModeChange](t, sess)

	// No topic event should be emitted. Drain dispatch events, then verify.
	drainDispatchEvents(t, sess)
	select {
	case evt := <-sess.Events():
		t.Fatalf("unexpected event: %T %+v", evt, evt)
	case evt := <-sess.User().Events():
		t.Fatalf("unexpected event: %T %+v", evt, evt)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestJoinAs_model_voice_only_no_topic(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#dev", "testuser")
	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: orderedmap.New[domain.ChannelName, time.Time](),
	})

	ch, _ := sess.loadChannelWindow(ctx, "#dev")
	ch.Topic = "some topic"
	saveTestChannel(t, sess, s, ch)

	require.NoError(t, sess.JoinAs(ctx, botty, "#dev"))

	joinEvt := drainEventSkipping[domain.Join](t, sess)
	require.Equal(t, domain.Nick("botty"), joinEvt.Instance.Nick())

	// A model join grants +v via ChanServ, mirroring the +o granted to
	// the user on user joins.
	mode := drainEventSkipping[domain.ModeChange](t, sess)
	require.Equal(t, domain.ModeChange{
		Target:     "#dev",
		Nick:       "botty",
		InstanceID: botty.ID(),
		Mode:       domain.ModeVoice,
		By:         "ChanServ",
		At:         fixedTime,
		Instance:   botty,
		Actor:      "ChanServ",
	}, mode)

	// Drain remaining dispatch events triggered by the join.
	drainDispatchEvents(t, sess)

	// No TopicInfoEvent for a non-user joiner.
	select {
	case evt := <-sess.Events():
		t.Fatalf("unexpected event after model join: %T %+v", evt, evt)
	case evt := <-sess.User().Events():
		t.Fatalf("unexpected event after model join: %T %+v", evt, evt)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestJoinAs_user_updates_autojoin(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.JoinAs(ctx, sess.UserInstance(), "#general"))
	drainEventSkipping[domain.Join](t, sess)
	drainEventSkipping[domain.ModeChange](t, sess)

	require.NoError(t, sess.JoinAs(ctx, sess.UserInstance(), "#dev"))
	drainEventSkipping[domain.Join](t, sess)
	drainEventSkipping[domain.ModeChange](t, sess)

	got, err := s.ListAutojoinChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.ChannelName{"#dev", "#general"}, got)
}

func TestPartAs_user_updates_autojoin(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.JoinAs(ctx, sess.UserInstance(), "#general"))
	drainEventSkipping[domain.Join](t, sess)
	drainEventSkipping[domain.ModeChange](t, sess)

	require.NoError(t, sess.JoinAs(ctx, sess.UserInstance(), "#dev"))
	drainEventSkipping[domain.Join](t, sess)
	drainEventSkipping[domain.ModeChange](t, sess)

	require.NoError(t, sess.PartAs(ctx, sess.UserInstance(), "#general", "bye"))
	drainEventSkipping[domain.Part](t, sess)

	got, err := s.ListAutojoinChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.ChannelName{"#dev"}, got)
}
