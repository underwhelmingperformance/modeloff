package session

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

func TestJoinAs_model_actor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: orderedmap.New[domain.ChannelName, time.Time](),
		})

		require.NoError(t, sess.joinAs(ctx, botty, "#dev", ""))
		synctest.Wait()

		ch, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		modelOnlyMembers := domain.NewMemberList()
		modelOnlyMembers.Add(botty)
		modelOnlyMembers.SetMode(botty, domain.ModeOp)
		requireChannelEqual(t, newTestChannelWindow("#dev", fixedTime, modelOnlyMembers), ch)

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target:     "#dev",
				Nick:       "botty",
				InstanceID: botty.ID(),
				Created:    true,
				At:         fixedTime,
				Instance:   botty,
			},
			domain.NamesReplyEvent{
				Channel: "#dev",
				Members: ch.Members,
				At:      fixedTime,
			},
			domain.ModelDispatchStarted{Instance: botty, At: fixedTime},
			domain.ModelDispatchDone{Instance: botty, At: fixedTime},
		}, collectEmittedEvents(t, sess))

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
	})
}

// TestJoinAs_model_joining_existing_channel_gets_RPL_topic_and_names
// pins that a model joining a populated channel receives the
// RFC-equivalent of RPL_TOPIC and RPL_NAMREPLY — the
// [domain.TopicInfo] and [domain.NamesReplyEvent] envelopes. Without
// these, the bot's first dispatch turn has no record of who set the
// topic or who else is in the room beyond the system-prompt topic
// line.
func TestJoinAs_model_joining_existing_channel_gets_RPL_topic_and_names(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#dev"))
		require.NoError(t, sess.setTopicAs(ctx, userInstance(t, sess), "#dev", "ongoing work"))

		// Drain everything emitted up to this point so the assertion
		// scopes to the bot's join sequence.
		collectEmittedEvents(t, sess)

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: orderedmap.New[domain.ChannelName, time.Time](),
		})

		require.NoError(t, sess.joinAs(ctx, botty, "#dev", ""))
		synctest.Wait()

		ch, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)

		require.ElementsMatch(t, []domain.Event{
			domain.Join{
				Target:     "#dev",
				Nick:       "botty",
				InstanceID: botty.ID(),
				At:         fixedTime,
				Instance:   botty,
			},
			domain.NamesReplyEvent{
				Channel: "#dev",
				Members: ch.Members,
				At:      fixedTime,
			},
			domain.TopicInfo{
				Target:     "#dev",
				Topic:      "ongoing work",
				TopicSetBy: userInstance(t, sess).Nick(),
				TopicSetAt: fixedTime,
				At:         fixedTime,
			},
			domain.ModelDispatchStarted{Instance: botty, At: fixedTime},
			domain.ModelDispatchDone{Instance: botty, At: fixedTime},
		}, collectEmittedEvents(t, sess))
	})
}

func TestPartAs_model_actor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#dev"),
		})
		seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")

		require.NoError(t, sess.partAs(ctx, botty, "#dev", "goodbye"))
		synctest.Wait()

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Part{
				Target:     "#dev",
				Nick:       "botty",
				InstanceID: botty.ID(),
				Message:    "goodbye",
				At:         fixedTime,
				Instance:   botty,
			},
		}, collectEmittedEvents(t, sess))

		ch, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		requireChannelEqual(t, newTestChannelWindow("#dev", fixedTime, testMembers(t, sess, s, "testuser")), ch)

		inst, err := s.ResolveNick(ctx, "botty")
		require.NoError(t, err)
		requireInstanceEqual(t, domain.NewModelInstance(
			testMemberID("botty"), "botty", "test/model", "", testChannels(),
		), inst)
	})
}

func TestPartAs_unknown_actor_is_noop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		saveTestChannel(t, sess, s, newTestChannelWindow("#dev", fixedTime, testMembers(t, sess, s, "testuser")))

		// partAs for an instance that isn't in the channel must be a
		// no-op: no PartEvent emission (the empty-id fallback would
		// otherwise ask the UI to drop the human's channel), no stored
		// membership mutation, no instance-channels mutation.
		ghost := domain.NewModelInstance("ghost-id", "ghost", "test/model", "", nil)
		require.NoError(t, sess.partAs(ctx, ghost, "#dev", "bye"))
		synctest.Wait()

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
		}, collectEmittedEvents(t, sess))

		updated, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		require.Equal(t, slices.Collect(testMembers(t, sess, s, "testuser").All()), slices.Collect(updated.Members.All()))
	})
}

func TestQuitAs_model_actor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#dev", "#general"),
		})
		seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

		require.NoError(t, modelQuitViaWire(ctx, t, sess, botty, "farewell"))
		synctest.Wait()

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Quit{
				Nick:       "botty",
				InstanceID: botty.ID(),
				Message:    "farewell",
				At:         fixedTime,
				Instance:   botty,
			},
		}, collectEmittedEvents(t, sess))

		_, err := s.ResolveNick(ctx, "botty")
		require.Error(t, err)

		// handleQuit reaps the model-client's subscription so
		// the dispatch goroutine exits and no future fan-out
		// targets it. The user-client is never reaped.
		sess.subsMu.RLock()
		_, reaped := sess.subscribers[protocol.ClientID(botty.ID())]
		sess.subsMu.RUnlock()
		require.False(t, reaped, "model-client subscription should be reaped after QUIT")

		ch1, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		requireChannelEqual(t, newTestChannelWindow("#dev", fixedTime, testMembers(t, sess, s, "testuser")), ch1)

		ch2, err := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		requireChannelEqual(t, newTestChannelWindow("#general", fixedTime, testMembers(t, sess, s, "testuser")), ch2)

		types1 := channelEventTypes(t, s, "#dev")
		require.Equal(t, []string{"quit"}, types1)

		types2 := channelEventTypes(t, s, "#general")
		require.Equal(t, []string{"quit"}, types2)
	})
}

// TestQuitAs_delivery_targets_intersect_per_recipient pins the
// privacy property of the actor-scoped fan-out. Actor `alpha` is
// in #shared and #private; recipient `beta` is in #shared and
// nowhere else. When alpha quits, the [protocol.Delivery] beta
// receives lists only #shared in `Targets` — the wire payload
// never reveals #private, which beta has no business knowing
// about.
//
// The user-client by contrast sees the full intersection (its own
// projection of "every channel" makes the intersection equal to
// the actor's whole channel set), which is what the chat-screen
// renders against. The two recipients in a single fan-out test
// pin both behaviours simultaneously.
func TestQuitAs_delivery_targets_intersect_per_recipient(t *testing.T) {
	alphaChannels := testChannels("#shared", "#private")
	betaChannels := testChannels("#shared")

	alpha := domain.NewModelInstance("inst-alpha", "alpha", "test/model", "", alphaChannels)
	beta := domain.NewModelInstance("inst-beta", "beta", "test/model", "", betaChannels)

	gotAlpha := intersectActorTargets(
		fakeServerClient(t, alpha),
		channelNames(alphaChannels),
	)
	require.Equal(t, []domain.ChannelName{"#shared", "#private"}, gotAlpha,
		"alpha is the actor; alpha sees the full set as receiver")

	gotBeta := intersectActorTargets(
		fakeServerClient(t, beta),
		channelNames(alphaChannels),
	)
	require.Equal(t, []domain.ChannelName{"#shared"}, gotBeta,
		"beta receives only the channel it shares with alpha; #private is not on the wire")

	gotUser := intersectActorTargets(
		fakeUserServerClient(t),
		channelNames(alphaChannels),
	)
	require.Equal(t, []domain.ChannelName{"#shared", "#private"}, gotUser,
		"user-client receives the full actor channel list")
}

// TestActorChannelSnapshot_only_for_actor_scoped pins the
// helper's gating: window-scoped events return nil so the
// per-sub loop in [Session.fanOutProtocol] does not produce a
// `Targets` slice for them.
func TestActorChannelSnapshot_only_for_actor_scoped(t *testing.T) {
	actor := domain.NewModelInstance("inst-a", "alpha", "test/model", "", testChannels("#one"))

	cases := []struct {
		name string
		ev   domain.ProtocolEvent
		want []domain.ChannelName
	}{
		{"quit returns actor channels", domain.Quit{Instance: actor}, []domain.ChannelName{"#one"}},
		{"nick_change returns actor channels", domain.NickChange{Instance: actor}, []domain.ChannelName{"#one"}},
		{"join returns nil", domain.Join{Target: "#one"}, nil},
		{"part returns nil", domain.Part{Target: "#one"}, nil},
		{"message returns nil", domain.Message{Target: "#one"}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, actorChannelSnapshot(tc.ev))
		})
	}
}

// fakeServerClient builds a minimal model-client subscription for
// unit-testing the per-recipient intersection helpers. The dispatch
// goroutine is not started — the helpers under test are pure
// functions over the client's identity and channel membership.
func fakeServerClient(t *testing.T, inst *domain.Instance) *serverClient {
	t.Helper()

	return &serverClient{
		id:       protocol.ClientID(inst.ID()),
		instance: inst,
	}
}

// fakeUserServerClient builds a minimal user-client subscription
// for the intersection helpers. Identity is [protocol.UserClientID];
// no instance pointer is needed because the user-client branch of
// the helpers reads only the id.
func fakeUserServerClient(t *testing.T) *serverClient {
	t.Helper()

	return &serverClient{
		id: protocol.UserClientID,
	}
}

// channelNames flattens an ordered channel map into a slice in
// insertion order.
func channelNames(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) []domain.ChannelName {
	if m == nil {
		return nil
	}

	out := make([]domain.ChannelName, 0, m.Len())
	for pair := m.Oldest(); pair != nil; pair = pair.Next() {
		out = append(out, pair.Key)
	}

	return out
}

func TestSendMessageAs_model_actor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:    "botty",
			ModelID: "test/model",
		})
		seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")

		_, err := sess.sendMessageAs(ctx, botty, "#dev", "hello world")
		require.NoError(t, err)
		synctest.Wait()

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Message{
				Target:     "#dev",
				From:       "botty",
				InstanceID: testMemberID("botty"),
				Body:       "hello world",
				At:         fixedTime,
			},
		}, collectEmittedEvents(t, sess))

		msgs := channelMessages(t, s, "#dev")
		require.Equal(t, []domain.Message{
			{Target: "#dev", From: "botty", InstanceID: testMemberID("botty"), Body: "hello world", At: fixedTime},
		}, msgs)
	})
}

// TestSendMessageAs_user_actor_does_not_echo_to_originator pins the
// echo gate's structural property: a PRIVMSG sent by the user to a
// channel they're in does not arrive on their own subscription's
// events channel. The session's chat-traffic suppression rule is
// applied uniformly at fan-out (RFC 2812 §3.3.1); the user-actor
// branch in [Session.sendMessageAs] no longer carries it.
func TestSendMessageAs_user_actor_does_not_echo_to_originator(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, sess.joinAs(ctx, userInstance(t, sess), "#dev", ""))
		synctest.Wait()

		_, err := sess.sendMessageAs(ctx, userInstance(t, sess), "#dev", "hello")
		require.NoError(t, err)
		synctest.Wait()

		user := userInstance(t, sess)
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target:     "#dev",
				Nick:       "testuser",
				InstanceID: user.ID(),
				Created:    true,
				At:         fixedTime,
				Instance:   user,
			},
			domain.NamesReplyEvent{
				Channel: "#dev",
				Members: testMembers(t, sess, s, "testuser"),
				At:      fixedTime,
			},
		}, collectEmittedEvents(t, sess))
	})
}

func TestSetTopicAs_model_actor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
		seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")

		require.NoError(t, sess.setTopicAs(ctx, botty, "#dev", "new topic"))
		synctest.Wait()

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.TopicChange{
				Target:     "#dev",
				Topic:      "new topic",
				By:         "botty",
				InstanceID: botty.ID(),
				At:         fixedTime,
				ByInstance: botty,
			},
		}, collectEmittedEvents(t, sess))

		ch, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)

		expected := newTestChannelWindow("#dev", fixedTime, testMembers(t, sess, s, "testuser", "botty"))
		expected.Topic = "new topic"
		expected.TopicSetBy = "botty"
		expected.TopicSetAt = fixedTime
		requireChannelEqual(t, expected, ch)
	})
}

// TestSetTopicAs_no_op_suppresses_event pins the convention that
// setting a topic to a string equal to the current one neither
// persists nor emits a [domain.TopicChange]. Models tend to call
// /topic redundantly; without this guard each call narrates as a
// fresh topic-set event the channel has to react to.
func TestSetTopicAs_no_op_suppresses_event(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
		seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")

		require.NoError(t, sess.setTopicAs(ctx, userInstance(t, sess), "#dev", "stable topic"))
		require.NoError(t, sess.setTopicAs(ctx, botty, "#dev", "stable topic"))
		synctest.Wait()

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.TopicChange{
				Target:     "#dev",
				Topic:      "stable topic",
				By:         userInstance(t, sess).Nick(),
				At:         fixedTime,
				ByInstance: userInstance(t, sess),
			},
		}, collectEmittedEvents(t, sess))

		events, err := s.EventsBefore(ctx, "#dev", nil, 10)
		require.NoError(t, err)

		var topicChanges int
		for _, se := range events {
			if _, ok := se.Event.(domain.TopicChange); ok {
				topicChanges++
			}
		}
		require.Equal(t, 1, topicChanges,
			"the no-op second call did not persist a TopicChange")
	})
}

func TestKickAs_model_actor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
		helper := seedInstance(t, sess, s, instanceSpec{
			Nick:     "helper",
			ModelID:  "test/model-b",
			Channels: testChannels("#dev"),
		})
		seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty", "helper")

		// KICK is channel-op gated (RFC 2812 §3.2.8); give botty `@`
		// before exercising the kick path.
		w, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		w.Members.SetMode(botty, domain.ModeOp)
		require.NoError(t, sess.persistChannelWindow(ctx, w))

		require.NoError(t, sess.kickAs(ctx, botty, helper, "#dev"))
		synctest.Wait()

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.ModelKicked{
				Target:       "#dev",
				Nick:         "helper",
				InstanceID:   helper.ID(),
				By:           "botty",
				ByInstanceID: botty.ID(),
				At:           fixedTime,
				Instance:     helper,
			},
		}, collectEmittedEvents(t, sess))

		ch, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		expectedMembers := testMembers(t, sess, s, "testuser", "botty")
		expectedMembers.SetMode(botty, domain.ModeOp)
		requireChannelEqual(t, newTestChannelWindow("#dev", fixedTime, expectedMembers), ch)

		inst, err := s.ResolveNick(ctx, "helper")
		require.NoError(t, err)
		requireInstanceEqual(t, domain.NewModelInstance(
			testMemberID("helper"), "helper", "test/model-b", "", testChannels(),
		), inst)
	})
}

// TestInviteAs_unknown_nick_returns_notice pins the unknown-nick
// path of `inviteAs`: there is no subscription to deliver the
// invite to, so the call returns a [domain.SystemNotice] for the
// inviter and leaves the channel event log untouched.
func TestInviteAs_unknown_nick_returns_notice(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
	seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")

	event, err := sess.inviteAs(ctx, botty, "helper", "#dev")
	require.NoError(t, err)
	require.Equal(t, domain.SystemNotice{
		Target: "#dev",
		Text:   "no such nick: helper",
		At:     fixedTime,
	}, event)

	stored, err := s.EventsBefore(ctx, "#dev", nil, 100)
	require.NoError(t, err)
	for _, se := range stored {
		_, isNotice := se.Event.(domain.SystemNotice)
		require.False(t, isNotice,
			"channel log carries no SystemNotice from invite — the unknown-nick "+
				"notice is the inviter's RPL_NOSUCHNICK-equivalent, not channel chat")
	}
}

func TestSetTopicAs_rejects_DM(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, sess, s, instanceSpec{
		Nick:    "botty",
		ModelID: "test/model",
	})

	err := userSetTopic(ctx, t, sess, domain.ChannelName(botty.ID()), "some topic")
	require.EqualError(t, err, "cannot set topic on a direct message")
}

func TestKickAs_rejects_DM(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, sess, s, instanceSpec{
		Nick:    "botty",
		ModelID: "test/model",
	})

	err := kickViaWire(ctx, t, sess, domain.ChannelName(botty.ID()), "botty")
	require.EqualError(t, err, "cannot kick from a direct message")
}

// TestSendMessageAs_model_to_model_dispatches verifies that the
// nick-targeted dispatch path fires when one model messages
// another. The wire-form target is the recipient's
// `InstanceID`; the addressed model's dispatch goroutine
// receives the resulting [domain.Message] and runs an LLM turn.
func TestSendMessageAs_model_to_model_dispatches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model-a",
			Channels: orderedmap.New[domain.ChannelName, time.Time](),
		})
		helper := seedInstance(t, sess, s, instanceSpec{
			Nick:     "helper",
			ModelID:  "test/model-b",
			Channels: orderedmap.New[domain.ChannelName, time.Time](),
		})

		attachModelClient(t, sess, botty)
		attachModelClient(t, sess, helper)

		target := domain.ChannelName(helper.ID())

		_, err := sess.sendMessageAs(ctx, botty, target, "hey there")
		require.NoError(t, err)
		synctest.Wait()

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Message{
				Target:     target,
				From:       "botty",
				InstanceID: testMemberID("botty"),
				Body:       "hey there",
				At:         fixedTime,
			},
			domain.ModelDispatchStarted{Instance: helper, At: fixedTime},
			domain.ModelDispatchDone{Instance: helper, At: fixedTime},
		}, collectEmittedEvents(t, sess))

		msgs := channelMessages(t, s, target)
		require.Equal(t, []domain.Message{
			{Target: target, From: "botty", InstanceID: testMemberID("botty"), Body: "hey there", At: fixedTime},
		}, msgs)
	})
}

func TestJoinAs_normalises_channel_prefix(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: orderedmap.New[domain.ChannelName, time.Time](),
		})

		require.NoError(t, sess.joinAs(ctx, botty, "modeloff", ""))
		synctest.Wait()

		ch, err := sess.loadChannelWindow(ctx, "#modeloff")
		require.NoError(t, err)
		require.True(t, ch.Members.HasNick("botty"))

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target:     "#modeloff",
				Nick:       "botty",
				InstanceID: botty.ID(),
				Created:    true,
				At:         fixedTime,
				Instance:   botty,
			},
			domain.NamesReplyEvent{
				Channel: "#modeloff",
				Members: ch.Members,
				At:      fixedTime,
			},
			domain.ModelDispatchStarted{Instance: botty, At: fixedTime},
			domain.ModelDispatchDone{Instance: botty, At: fixedTime},
		}, collectEmittedEvents(t, sess))

		_, err = sess.loadChannelWindow(ctx, "modeloff")
		require.Error(t, err)
	})
}

func TestJoinAs_user_rejoin_preserves_join_time(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#general"))
		synctest.Wait()

		user := userInstance(t, sess)
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target:     "#general",
				Nick:       "testuser",
				InstanceID: user.ID(),
				Created:    true,
				At:         fixedTime,
				Instance:   user,
			},
			domain.NamesReplyEvent{
				Channel: "#general",
				Members: testMembers(t, sess, s, "testuser"),
				At:      fixedTime,
			},
		}, collectEmittedEvents(t, sess))

		originalJoinTime := userJoinedAt(t, sess, "#general")
		require.Equal(t, fixedTime, originalJoinTime)

		sess.now = func() time.Time { return fixedTime.Add(time.Hour) }

		require.NoError(t, userJoin(ctx, t, sess, "#general"))
		synctest.Wait()

		require.Empty(t, collectEmittedEvents(t, sess))
		require.Equal(t, originalJoinTime, userJoinedAt(t, sess, "#general"))
	})
}

func TestJoinAs_user_new_channel_emits_join_and_mode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, sess.joinAs(ctx, userInstance(t, sess), "#dev", ""))
		synctest.Wait()

		user := userInstance(t, sess)
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target:     "#dev",
				Nick:       "testuser",
				InstanceID: user.ID(),
				Instance:   user,
				Created:    true,
				At:         fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#dev",
				Members: testMembers(t, sess, s, "testuser"),
				At:      fixedTime,
			},
		}, collectEmittedEvents(t, sess))

		ch, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)

		m, ok := ch.Members.GetByInstance(user)
		require.True(t, ok)
		require.Equal(t, domain.ModeOp, m.Mode)
	})
}

func TestJoinAs_user_existing_channel_with_topic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedInstance(t, sess, s, instanceSpec{Nick: "alice", ModelID: "test/model"})

		withAlice := newTestChannelWindow("#dev", fixedTime.Add(-time.Hour), testMembers(t, sess, s, "alice"))
		withAlice.Topic = "Go development"
		withAlice.TopicSetBy = "alice"
		withAlice.TopicSetAt = fixedTime.Add(-time.Hour)
		saveTestChannel(t, sess, s, withAlice)

		require.NoError(t, sess.joinAs(ctx, userInstance(t, sess), "#dev", ""))
		synctest.Wait()

		user := userInstance(t, sess)
		expectedMembers := testMembers(t, sess, s, "testuser", "alice")
		expectedMembers.SetMode(user, domain.ModeNone)
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target:     "#dev",
				Nick:       "testuser",
				InstanceID: user.ID(),
				Instance:   user,
				Created:    false,
				At:         fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#dev",
				Members: expectedMembers,
				At:      fixedTime,
			},
			domain.TopicInfo{
				Target:     "#dev",
				Topic:      "Go development",
				TopicSetBy: "alice",
				TopicSetAt: fixedTime.Add(-time.Hour),
				At:         fixedTime,
			},
		}, collectEmittedEvents(t, sess))
	})
}

func TestJoinAs_user_existing_channel_no_topic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedInstance(t, sess, s, instanceSpec{Nick: "alice", ModelID: "test/model"})

		saveTestChannel(t, sess, s, newTestChannelWindow("#dev", fixedTime.Add(-time.Hour), testMembers(t, sess, s, "alice")))

		require.NoError(t, sess.joinAs(ctx, userInstance(t, sess), "#dev", ""))
		synctest.Wait()

		user := userInstance(t, sess)
		expectedMembers := testMembers(t, sess, s, "testuser", "alice")
		expectedMembers.SetMode(user, domain.ModeNone)
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target:     "#dev",
				Nick:       "testuser",
				InstanceID: user.ID(),
				Instance:   user,
				Created:    false,
				At:         fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#dev",
				Members: expectedMembers,
				At:      fixedTime,
			},
		}, collectEmittedEvents(t, sess))
	})
}

func TestJoinAs_model_voice_only_no_topic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#dev", "testuser")
		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: orderedmap.New[domain.ChannelName, time.Time](),
		})

		ch, _ := sess.loadChannelWindow(ctx, "#dev")
		ch.Topic = "some topic"
		saveTestChannel(t, sess, s, ch)

		require.NoError(t, sess.joinAs(ctx, botty, "#dev", ""))
		synctest.Wait()

		ch, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target:     "#dev",
				Nick:       "botty",
				InstanceID: botty.ID(),
				Created:    false,
				At:         fixedTime,
				Instance:   botty,
			},
			domain.NamesReplyEvent{
				Channel: "#dev",
				Members: ch.Members,
				At:      fixedTime,
			},
			domain.TopicInfo{
				Target:     "#dev",
				Topic:      "some topic",
				TopicSetBy: ch.TopicSetBy,
				TopicSetAt: ch.TopicSetAt,
				At:         fixedTime,
			},
			domain.ModelDispatchStarted{Instance: botty, At: fixedTime},
			domain.ModelDispatchDone{Instance: botty, At: fixedTime},
		}, collectEmittedEvents(t, sess))
	})
}

func TestJoinAs_user_updates_autojoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, sess.joinAs(ctx, userInstance(t, sess), "#general", ""))
		require.NoError(t, sess.joinAs(ctx, userInstance(t, sess), "#dev", ""))
		synctest.Wait()

		user := userInstance(t, sess)
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target:     "#general",
				Nick:       "testuser",
				InstanceID: user.ID(),
				Instance:   user,
				Created:    true,
				At:         fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#general",
				Members: testMembers(t, sess, s, "testuser"),
				At:      fixedTime,
			},
			domain.Join{
				Target:     "#dev",
				Nick:       "testuser",
				InstanceID: user.ID(),
				Instance:   user,
				Created:    true,
				At:         fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#dev",
				Members: testMembers(t, sess, s, "testuser"),
				At:      fixedTime,
			},
		}, collectEmittedEvents(t, sess))

		got, err := s.ListAutojoinChannels(ctx)
		require.NoError(t, err)
		require.Equal(t, []domain.ChannelName{"#dev", "#general"}, got)
	})
}

func TestPartAs_user_updates_autojoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, sess.joinAs(ctx, userInstance(t, sess), "#general", ""))
		require.NoError(t, sess.joinAs(ctx, userInstance(t, sess), "#dev", ""))
		require.NoError(t, sess.partAs(ctx, userInstance(t, sess), "#general", "bye"))
		synctest.Wait()

		user := userInstance(t, sess)
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target:     "#general",
				Nick:       "testuser",
				InstanceID: user.ID(),
				Instance:   user,
				Created:    true,
				At:         fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#general",
				Members: testMembers(t, sess, s, "testuser"),
				At:      fixedTime,
			},
			domain.Join{
				Target:     "#dev",
				Nick:       "testuser",
				InstanceID: user.ID(),
				Instance:   user,
				Created:    true,
				At:         fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#dev",
				Members: testMembers(t, sess, s, "testuser"),
				At:      fixedTime,
			},
			domain.Part{
				Target:     "#general",
				Nick:       "testuser",
				InstanceID: user.ID(),
				Message:    "bye",
				At:         fixedTime,
				Instance:   user,
			},
		}, collectEmittedEvents(t, sess))

		got, err := s.ListAutojoinChannels(ctx)
		require.NoError(t, err)
		require.Equal(t, []domain.ChannelName{"#dev"}, got)
	})
}
