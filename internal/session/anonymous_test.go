package session

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// TestSession_quit_on_an_anonymous_channel_arrives_as_a_part covers
// RFC 2811 §4.2.1: on a `+a` channel the server sends a PART where
// it would otherwise send a QUIT, so a member sees somebody leave
// the channel and cannot tell that they left the server. The
// departing nick is the `+a` mask, the same origin every message in
// the channel already carried.
func TestSession_quit_on_an_anonymous_channel_arrives_as_a_part(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#anon"))
		setChannelModes(t, sess, "#anon", domain.ChannelModes{Anonymous: true})

		botty, _ := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#anon", ""))

		collectEmittedEvents(t, sess)

		require.NoError(t, sess.quitAs(ctx, botty, "gone"))
		synctest.Wait()

		require.Equal(t, []domain.Event{domain.Part{Source: domain.AnonymousSource(),

			Target: "#anon", Message: "gone", At: fixedTime}}, collectEmittedEvents(t, sess),
			"the channel is told somebody left it, and not who or that they left the server")
	})
}

func TestSubscription_anonymous_scrollback_keeps_the_wire_projection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, store := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#anon"))
		setChannelModes(t, sess, "#anon", domain.ChannelModes{Anonymous: true})

		viewer := seedInstanceRow(t, store, instanceSpec{Nick: "viewer", ModelID: "test/viewer"})
		client := &subscribeFakeClient{id: protocol.ClientID(viewer.ID())}
		sub, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{
			ReplayHistory: true,
		})
		require.NoError(t, err)
		response, err := sess.Handle(ctx, client, protocol.Join{Channels: []domain.ChannelName{"#anon"}})
		require.NoError(t, err)
		require.NoError(t, response.Err)
		sub.Activate()
		for range 3 {
			<-sub.Events()
		}

		speaker, _ := seedPassiveInstance(t, sess, "speaker", "test/speaker")
		require.NoError(t, joinAs(ctx, sess, speaker, "#anon", ""))
		require.NoError(t, sess.changeNickAs(ctx, speaker, "renamed"))
		_, err = sess.sendMessageAs(ctx, speaker, "#anon", "secret origin")
		require.NoError(t, err)
		require.NoError(t, sess.quitAs(ctx, speaker, "gone"))

		entries, err := sub.Scrollback(ctx, protocol.ChannelWindowTarget("#anon"), 100)
		require.NoError(t, err)

		var join domain.Join
		var message domain.Message
		var part domain.Part
		for _, entry := range entries {
			switch event := entry.Event.(type) {
			case domain.Join:
				if id, ok := event.Source.InstanceID(); !ok || id != viewer.ID() {
					join = event
				}
			case domain.Message:
				if event.Body == "secret origin" {
					message = event
				}
			case domain.Part:
				if event.Message == "gone" {
					part = event
				}
			case domain.Quit:
				t.Fatalf("anonymous scrollback exposed QUIT: %#v", event)
			case domain.NickChange:
				t.Fatalf("anonymous scrollback exposed NICK: %#v", event)
			}
		}

		require.Equal(t, domain.Join{Source: domain.AnonymousSource(),

			Target: "#anon", At: fixedTime},

			join)
		require.Equal(t, domain.Message{Source: domain.AnonymousSource(),

			Target: "#anon", Body: "secret origin", At: fixedTime},

			message)
		require.Equal(t, domain.Part{Source: domain.AnonymousSource(),

			Target: "#anon", Message: "gone", At: fixedTime},

			part)
	})
}

func TestSession_anonymous_membership_events_hide_other_members(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, store := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#anon"))
		setChannelModes(t, sess, "#anon", domain.ChannelModes{Anonymous: true})

		viewer := seedInstanceRow(t, store, instanceSpec{Nick: "viewer", ModelID: "test/viewer"})
		client := &subscribeFakeClient{id: protocol.ClientID(viewer.ID())}
		sub, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{})
		require.NoError(t, err)
		response, err := sess.Handle(ctx, client, protocol.Join{
			Channels: []domain.ChannelName{"#anon"},
		})
		require.NoError(t, err)
		require.NoError(t, response.Err)
		synctest.Wait()
		drainSubscriptionEvents(sub)

		speaker, _ := seedPassiveInstance(t, sess, "speaker", "test/speaker")
		require.NoError(t, joinAs(ctx, sess, speaker, "#anon", ""))
		synctest.Wait()
		require.Equal(t, []domain.Event{domain.Join{Source: domain.AnonymousSource(),

			Target: "#anon", At: fixedTime}}, drainSubscriptionEvents(sub))

		require.NoError(t, sess.changeNickAs(ctx, speaker, "renamed"))
		synctest.Wait()
		require.Empty(t, drainSubscriptionEvents(sub))

		require.NoError(t, sess.partAs(ctx, speaker, "#anon", "left"))
		synctest.Wait()
		require.Equal(t, []domain.Event{domain.Part{Source: domain.AnonymousSource(),

			Target: "#anon", Message: "left", At: fixedTime}}, drainSubscriptionEvents(sub))

		require.NoError(t, joinAs(ctx, sess, speaker, "#anon", ""))
		synctest.Wait()
		drainSubscriptionEvents(sub)

		require.NoError(t, sess.kickAs(ctx, userInstance(t, sess), speaker, "#anon"))
		synctest.Wait()
		require.Equal(t, []domain.Event{domain.Kicked{Source: domain.AnonymousSource(),

			Target: "#anon", Subject: domain.AnonymousNick, At: fixedTime}}, drainSubscriptionEvents(sub))
	})
}

func TestSession_anonymous_member_mode_hides_the_user_subject(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, store := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#anon"))
		setChannelModes(t, sess, "#anon", domain.ChannelModes{Anonymous: true})

		viewer := seedInstanceRow(t, store, instanceSpec{Nick: "viewer", ModelID: "test/viewer"})
		client := &subscribeFakeClient{id: protocol.ClientID(viewer.ID())}
		sub, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{ReplayHistory: true})
		require.NoError(t, err)
		response, err := sess.Handle(ctx, client, protocol.Join{Channels: []domain.ChannelName{"#anon"}})
		require.NoError(t, err)
		require.NoError(t, response.Err)
		sub.Activate()
		synctest.Wait()
		drainSubscriptionEvents(sub)

		response, err = userClient(t, sess).Send(ctx, protocol.ChannelMode{
			Channel: "#anon",
			Changes: []protocol.ChannelModeChange{{
				Flag:   domain.ModeChannelVoice,
				Add:    true,
				Target: "testuser",
			}},
		})
		require.NoError(t, err)
		require.NoError(t, response.Err)
		synctest.Wait()

		masked := domain.ChannelModeChange{Source: domain.AnonymousSource(),

			Target: "#anon", Subject: domain.AnonymousNick, Flag: domain.ModeChannelVoice, Add: true, At: fixedTime}

		require.Equal(t, []domain.Event{masked}, drainSubscriptionEvents(sub))

		entries, err := sub.Scrollback(ctx, protocol.ChannelWindowTarget("#anon"), 100)
		require.NoError(t, err)
		var modeChanges []protocol.ScrollbackEntry
		for _, entry := range entries {
			if _, ok := entry.Event.(domain.ChannelModeChange); ok {
				modeChanges = append(modeChanges, entry)
			}
		}
		require.Equal(t, []protocol.ScrollbackEntry{{Event: masked}}, modeChanges)
	})
}

func drainSubscriptionEvents(sub protocol.Subscription) []domain.Event {
	var events []domain.Event
	for {
		select {
		case delivery := <-sub.Events():
			events = append(events, delivery.Event)
		default:
			return events
		}
	}
}

// TestSession_quit_reaches_named_channels_unmasked pins the other
// half of the split: a client that shares a `+a` channel and an
// ordinary one with the quitter already knows who it is from the
// ordinary channel, so it receives the QUIT there as well as the
// masked PART on the anonymous one.
func TestSession_quit_reaches_named_channels_unmasked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#anon"))
		require.NoError(t, userJoin(ctx, t, sess, "#open"))
		setChannelModes(t, sess, "#anon", domain.ChannelModes{Anonymous: true})

		botty, _ := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#anon", ""))
		require.NoError(t, joinAs(ctx, sess, botty, "#open", ""))

		collectEmittedEvents(t, sess)

		require.NoError(t, sess.quitAs(ctx, botty, "gone"))
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.Quit{
				Source:  domain.ClientSource(botty.ID(), "botty"),
				Message: "gone",
				At:      fixedTime,
			},
			domain.Part{Source: domain.AnonymousSource(),

				Target: "#anon", Message: "gone", At: fixedTime},
		}, collectEmittedEvents(t, sess))
	})
}

// TestSession_dispatch_events_are_withheld_from_anonymous_peers
// covers the thinking indicator: only the actor may observe its own
// dispatch lifecycle when the turn runs in an anonymous channel.
func TestSession_dispatch_events_are_withheld_from_anonymous_peers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#anon"))
		setChannelModes(t, sess, "#anon", domain.ChannelModes{Anonymous: true})

		botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#anon", ""))

		synctest.Wait()
		collectProtocolDeliveries(userClient(t, sess))
		collectProtocolDeliveries(client)

		target := protocol.ChannelWindowTarget("#anon")
		source := domain.ClientSource(botty.ID(), botty.Nick())
		dispatch := sess.BeginModelDispatch(ctx, mustWindowGuard(t, client, target), target,
			domain.ModelDispatchStarted{Source: source, At: fixedTime})
		dispatch.Done(ctx, domain.ModelDispatchDone{Source: source, At: fixedTime})
		synctest.Wait()

		type observedDeliveries struct {
			Peer  []protocol.Delivery
			Actor []protocol.Delivery
		}
		require.Equal(t, observedDeliveries{
			Peer: []protocol.Delivery{},
			Actor: []protocol.Delivery{
				{
					Event:   domain.ModelDispatchStarted{Source: source, At: fixedTime},
					Targets: []domain.ChannelName{"#anon"},
					Window:  target,
				},
				{
					Event:   domain.ModelDispatchDone{Source: source, At: fixedTime},
					Targets: []domain.ChannelName{"#anon"},
					Window:  target,
				},
			},
		}, observedDeliveries{
			Peer:  collectProtocolDeliveries(userClient(t, sess)),
			Actor: collectProtocolDeliveries(client),
		})
	})
}

// TestSession_dispatch_events_name_the_actor_on_a_named_channel
// pins that the masking is scoped to `+a`: an ordinary channel sees
// the handle, which is what the thinking indicator renders from.
func TestSession_dispatch_events_name_the_actor_on_a_named_channel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#open"))

		botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#open", ""))

		collectEmittedEvents(t, sess)

		target := protocol.ChannelWindowTarget("#open")
		sess.BeginModelDispatch(ctx, mustWindowGuard(t, client, target), target, domain.ModelDispatchStarted{Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime})
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.ModelDispatchStarted{Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime},
		}, collectEmittedEvents(t, sess))
	})
}

func TestSession_dispatch_done_reaches_the_start_audience_after_anonymous_mode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#general"))
		botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#general", ""))
		collectEmittedEvents(t, sess)

		target := protocol.ChannelWindowTarget("#general")
		dispatch := sess.BeginModelDispatch(ctx, mustWindowGuard(t, client, target), target, domain.ModelDispatchStarted{
			Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
		})
		setChannelModes(t, sess, "#general", domain.ChannelModes{Anonymous: true})
		dispatch.Done(ctx, domain.ModelDispatchDone{
			Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
		})
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.ModelDispatchStarted{
				Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
			},
			domain.ModelDispatchDone{
				Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
			},
		}, collectEmittedEvents(t, sess))
	})
}

func TestSession_dispatch_events_do_not_name_an_anonymous_target(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#open"))
		require.NoError(t, userJoin(ctx, t, sess, "#anon"))
		setChannelModes(t, sess, "#anon", domain.ChannelModes{Anonymous: true})

		botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#open", ""))
		require.NoError(t, joinAs(ctx, sess, botty, "#anon", ""))
		collectEmittedEvents(t, sess)

		target := protocol.ChannelWindowTarget("#open")
		sess.BeginModelDispatch(ctx, mustWindowGuard(t, client, target), target, domain.ModelDispatchStarted{Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime})
		synctest.Wait()

		require.Equal(t, protocol.Delivery{
			Event:   domain.ModelDispatchStarted{Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime},
			Targets: []domain.ChannelName{"#open"},
			Window:  protocol.ChannelWindowTarget("#open"),
		}, <-userClient(t, sess).Events())
	})
}

func TestSession_dispatch_failure_does_not_name_an_anonymous_target(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#anon"))
		setChannelModes(t, sess, "#anon", domain.ChannelModes{Anonymous: true})

		botty, _ := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#anon", ""))
		collectEmittedEvents(t, sess)

		sess.EmitModelFailure(ctx, protocol.ChannelWindowTarget("#anon"), domain.ModelUnavailableError{Source: domain.ClientSource(botty.ID(), "botty"), At: fixedTime})
		synctest.Wait()

		require.Equal(t, protocol.Delivery{
			Event: domain.ModelUnavailableError{
				Source: domain.AnonymousSource(), At: fixedTime,
			},
			Targets: []domain.ChannelName{"#anon"},
			Window:  protocol.ChannelWindowTarget("#anon"),
		}, <-userClient(t, sess).Events())
	})
}

func TestSession_invite_masks_the_anonymous_inviter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#anon"))
		setChannelModes(t, sess, "#anon", domain.ChannelModes{Anonymous: true})

		_, invitee := seedPassiveInstance(t, sess, "botty", "test/model")
		drainDeliveries(invitee)

		response, err := userClient(t, sess).Send(ctx, protocol.Invite{
			Nick: "botty", Channel: "#anon",
		})
		require.NoError(t, err)
		require.NoError(t, response.Err)
		require.Equal(t, []protocol.Event{domain.Inviting{
			Target:  "#anon",
			Invitee: "botty",
			At:      fixedTime,
		}}, response.Events)
		synctest.Wait()

		require.Equal(t, []domain.Event{domain.Invited{
			Source:  domain.AnonymousSource(),
			Target:  "#anon",
			Invitee: "botty",
			At:      fixedTime,
		}}, drainDeliveries(invitee))
	})
}

// TestSession_join_replay_masks_names_on_an_anonymous_channel
// covers the NAMES half of `+a`. The replay a joiner receives is the
// one path that would hand over the membership verbatim, and it is
// reachable by anyone, because an anonymous channel is joinable: a
// client that wanted the member list would join and read the reply.
// RFC 2811 §4.2.1 answers a NAMES on such a channel with the mask
// alone, privileges included.
func TestSession_join_replay_masks_names_on_an_anonymous_channel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#anon"))
		setChannelModes(t, sess, "#anon", domain.ChannelModes{Anonymous: true})
		require.NoError(t, sess.setTopicAs(ctx, userInstance(t, sess), "#anon", "quiet room"))

		botty, joiner := seedPassiveInstance(t, sess, "botty", "test/model")
		drainDeliveries(joiner)

		require.NoError(t, joinAs(ctx, sess, botty, "#anon", ""))
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.Join{
				Source: domain.ClientSource(botty.ID(), "botty"),
				Target: "#anon",
				At:     fixedTime,
			},
			domain.TopicInfo{
				Target:     "#anon",
				Topic:      "quiet room",
				TopicSetBy: domain.AnonymousNick,
				TopicSetAt: fixedTime,
				At:         fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#anon",
				Members: domain.AnonymousMembers(),
				At:      fixedTime,
			},
			domain.NamesEnd{Channel: "#anon", At: fixedTime},
		}, drainDeliveries(joiner))
	})
}

func TestSession_anonymous_topic_does_not_trust_a_reused_setter_nick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		realSetter := userInstance(t, sess)
		require.NoError(t, userJoin(ctx, t, sess, "#anon"))
		setChannelModes(t, sess, "#anon", domain.ChannelModes{Anonymous: true})
		require.NoError(t, sess.setTopicAs(ctx, realSetter, "#anon", "quiet room"))
		require.NoError(t, sess.changeNickAs(ctx, realSetter, "renamed-user"))

		joiner := domain.NewModelInstance("inst-reused-nick", "testuser", "test/model", "", nil)
		require.NoError(t, sess.store.SaveInstance(ctx, joiner))
		subscription := &passiveClient{id: protocol.ClientID(joiner.ID())}
		sub, err := subscribeTestClient(t.Context(), t, sess, subscription, protocol.SubscribeOptions{})
		require.NoError(t, err)
		subscription.sub = sub
		drainDeliveries(subscription)

		require.NoError(t, joinAs(ctx, sess, joiner, "#anon", ""))
		synctest.Wait()

		var topicInfo domain.TopicInfo
		for _, event := range drainDeliveries(subscription) {
			if topic, ok := event.(domain.TopicInfo); ok {
				topicInfo = topic
			}
		}

		require.Equal(t, domain.TopicInfo{
			Target:     "#anon",
			Topic:      "quiet room",
			TopicSetBy: domain.AnonymousNick,
			TopicSetAt: fixedTime,
			At:         fixedTime,
		}, topicInfo)

		response, err := sess.Handle(ctx, subscription, protocol.TopicQuery{Channel: "#anon"})
		require.NoError(t, err)
		require.NoError(t, response.Err)
		require.Equal(t, []protocol.Event{domain.TopicInfo{
			Target:     "#anon",
			Topic:      "quiet room",
			TopicSetBy: domain.AnonymousNick,
			TopicSetAt: fixedTime,
			At:         fixedTime,
		}}, response.Events)
	})
}

// TestSession_join_replay_names_the_members_on_a_named_channel pins
// that the masking is scoped to `+a`: an ordinary channel answers
// with its real membership, which is what the joiner's nick list is
// built from.
func TestSession_join_replay_names_the_members_on_a_named_channel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#open"))

		botty, joiner := seedPassiveInstance(t, sess, "botty", "test/model")
		drainDeliveries(joiner)

		require.NoError(t, joinAs(ctx, sess, botty, "#open", ""))
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.Join{
				Source: domain.ClientSource(botty.ID(), "botty"),
				Target: "#open",
				At:     fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#open",
				Members: testMembers(t, sess, s, "testuser", "botty"),
				At:      fixedTime,
			},
			domain.NamesEnd{Channel: "#open", At: fixedTime},
		}, drainDeliveries(joiner))
	})
}

// TestSession_nick_reaches_a_client_in_no_channels covers RFC 2812
// §3.1.2: a client is always told its own NICK succeeded. The
// broadcast carries it back through the membership filter for a
// client that is on a channel; a client on none has no channel to
// reach it through, so the session delivers it point-to-point.
func TestSession_nick_reaches_a_client_in_no_channels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
		drainDeliveries(client)

		require.NoError(t, sess.changeNickAs(ctx, botty, "renamed"))
		synctest.Wait()

		require.Equal(t, []domain.Event{domain.NickChange{
			Source:  domain.ClientSource(botty.ID(), "botty"),
			NewNick: "renamed",
			At:      fixedTime,
		}}, drainDeliveries(client))
	})
}

// TestSession_channel_mode_change_names_its_issuer_by_id covers the
// identity half of a MODE broadcast. A nick is display state a
// client may change, so a recipient reading `By` alone cannot tell
// its own MODE from a peer's after either of them renames;
// `ByInstanceID` is the stable answer, as it already is on KICK and
// INVITE.
func TestSession_channel_mode_change_names_its_issuer_by_id(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#dev"))

		botty, _ := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))

		// Give botty `@` so it can issue a MODE of its own.
		resp, err := userClient(t, sess).Send(ctx, protocol.ChannelMode{
			Channel: "#dev",
			Changes: []protocol.ChannelModeChange{{Flag: domain.ModeOperator, Add: true, Target: "botty"}},
		})
		require.NoError(t, err)
		require.NoError(t, resp.Err)

		collectEmittedEvents(t, sess)

		resp, err = sess.Handle(ctx, sess.clientOwner(protocol.ClientID(botty.ID())), protocol.ChannelMode{
			Channel: "#dev",
			Changes: []protocol.ChannelModeChange{{Flag: domain.ModeModerated, Add: true}},
		})
		require.NoError(t, err)
		require.NoError(t, resp.Err)
		synctest.Wait()

		require.Equal(t, []domain.Event{domain.ChannelModeChange{
			Source: domain.ClientSource(botty.ID(), "botty"),
			Target: "#dev",
			Flag:   domain.ModeModerated,
			Add:    true,
			At:     fixedTime,
		}}, collectEmittedEvents(t, sess))
	})
}
