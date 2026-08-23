package session

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

type cancelAfterJoinStore struct {
	Store

	actor  domain.InstanceID
	cancel context.CancelFunc
}

type joinProjectionFailure struct{}

func (*joinProjectionFailure) Error() string { return "join projection failed" }

type failingJoinProjectionStore struct {
	Store
}

type windowInterruption struct {
	Client protocol.ClientID
	Window domain.ChannelName
}

func (s *failingJoinProjectionStore) CommitChannelJoin(
	ctx context.Context,
	join storemod.ChannelJoin,
) (storemod.CommittedChannelEvent, error) {
	if len(join.Scrollback) > 0 {
		return storemod.CommittedChannelEvent{}, &joinProjectionFailure{}
	}

	return s.Store.CommitChannelJoin(ctx, join)
}

func (s *cancelAfterJoinStore) CommitChannelJoin(
	ctx context.Context,
	join storemod.ChannelJoin,
) (storemod.CommittedChannelEvent, error) {
	committed, err := s.Store.CommitChannelJoin(ctx, join)
	if err == nil && join.Instance.ID() == s.actor {
		s.cancel()
	}

	return committed, err
}

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

		require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))
		synctest.Wait()

		ch, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		modelOnlyMembers := domain.NewMemberList()
		modelOnlyMembers.Add(botty)
		modelOnlyMembers.SetModes(botty, domain.MemberModes{Operator: true})
		requireChannelEqual(t, newTestChannelWindow("#dev", fixedTime, modelOnlyMembers), ch)

		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
		}, collectEmittedEvents(t, sess),
			"the user is not in #dev, so botty's join and dispatch do not reach the user-client")

		require.Equal(t, []string{"join"}, channelEventTypes(t, s, "#dev"),
			"the join is broadcast and persisted to the channel")

		inst, err := s.ResolveNick(ctx, "botty")
		require.NoError(t, err)
		requireInstanceEqual(t, domain.NewModelInstance(
			testMemberID("botty"), "botty", "test/model", "", testChannels("#dev"),
		), inst)

		// The last window is a UI-owned write; neither user nor model
		// joins touch it from the session.
		last, err := s.GetLastWindow(ctx)
		require.NoError(t, err)
		require.Nil(t, last)
	})
}

func TestSession_join_finishes_persistence_and_delivery_after_cancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		backing := storetest.NewMemoryStore(t)
		commandCtx, cancelCommand := context.WithCancel(ctx)
		store := &cancelAfterJoinStore{
			Store: backing, actor: protocol.UserClientID, cancel: cancelCommand,
		}
		factory := newTestModelClientFactory(t, &apitest.Fake{})
		sess := New(ctx, store, factory, nil)
		t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }

		peer := domain.NewModelInstance("inst-peer", "peer", "test/model", "", nil)
		require.NoError(t, store.SaveInstance(ctx, peer))
		peerClient := &passiveClient{id: protocol.ClientID(peer.ID())}
		peerSub, err := subscribeTestClient(ctx, t, sess, peerClient,
			protocol.SubscribeOptions{ReplayHistory: true})
		require.NoError(t, err)
		peerClient.sub = peerSub
		peerSub.Activate()
		require.NoError(t, joinAs(ctx, sess, peer, "#dev", ""))
		synctest.Wait()
		drainDeliveries(peerClient)

		resp, sendErr := userClient(t, sess).Send(commandCtx, protocol.Join{
			Channels: []domain.ChannelName{"#dev"},
		})
		synctest.Wait()

		auditEvents, err := backing.EventsBefore(ctx, "#dev", nil, 10)
		require.NoError(t, err)
		scrollback, err := backing.ChannelScrollback(ctx, peer.ID(), "#dev", 10)
		require.NoError(t, err)
		storedUser, err := backing.GetInstanceByID(ctx, protocol.UserClientID)
		require.NoError(t, err)

		require.Equal(t, struct {
			Response             protocol.Response
			SendError            error
			CommandContextError  error
			AuditEvents          []domain.StoredEvent
			Scrollback           []domain.StoredEvent
			PeerDeliveries       []domain.Event
			LiveUserMembership   bool
			StoredUserMembership bool
		}{
			Response: protocol.Response{Events: []protocol.Event{
				domain.JoinedChannel{Channel: "#dev"},
			}},
			CommandContextError: context.Canceled,
			AuditEvents: []domain.StoredEvent{
				{ID: 1, Event: domain.Join{
					Source: domain.ClientSource(peer.ID(), peer.Nick()),
					Target: "#dev", Created: true, At: fixedTime,
				}},
				{ID: 2, Event: domain.Join{
					Source: domain.ClientSource(protocol.UserClientID, "testuser"),
					Target: "#dev", At: fixedTime,
				}},
			},
			Scrollback: []domain.StoredEvent{
				{ID: 1, Event: domain.Join{
					Source: domain.ClientSource(peer.ID(), peer.Nick()),
					Target: "#dev", Created: true, At: fixedTime,
				}},
				{ID: 2, Event: domain.Join{
					Source: domain.ClientSource(protocol.UserClientID, "testuser"),
					Target: "#dev", At: fixedTime,
				}},
			},
			PeerDeliveries: []domain.Event{
				domain.Join{
					Source: domain.ClientSource(protocol.UserClientID, "testuser"),
					Target: "#dev", At: fixedTime,
				},
			},
			LiveUserMembership:   true,
			StoredUserMembership: true,
		}, struct {
			Response             protocol.Response
			SendError            error
			CommandContextError  error
			AuditEvents          []domain.StoredEvent
			Scrollback           []domain.StoredEvent
			PeerDeliveries       []domain.Event
			LiveUserMembership   bool
			StoredUserMembership bool
		}{
			Response:             resp,
			SendError:            sendErr,
			CommandContextError:  commandCtx.Err(),
			AuditEvents:          auditEvents,
			Scrollback:           scrollback,
			PeerDeliveries:       drainDeliveries(peerClient),
			LiveUserMembership:   userInstance(t, sess).InChannel("#dev"),
			StoredUserMembership: storedUser.InChannel("#dev"),
		})
	})
}

func TestSession_failed_join_projection_rolls_back_state_and_delivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		sess, backing := newTestSession(t)
		peer := domain.NewModelInstance("inst-peer", "peer", "test/model", "", nil)
		require.NoError(t, backing.SaveInstance(ctx, peer))
		peerClient := &passiveClient{id: protocol.ClientID(peer.ID())}
		peerSub, err := subscribeTestClient(ctx, t, sess, peerClient,
			protocol.SubscribeOptions{ReplayHistory: true})
		require.NoError(t, err)
		peerClient.sub = peerSub
		peerSub.Activate()
		require.NoError(t, joinAs(ctx, sess, peer, "#dev", ""))
		synctest.Wait()
		collectEmittedEvents(t, sess)
		drainDeliveries(peerClient)

		beforeLive, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		beforeStored, err := backing.GetWindow(ctx, "#dev")
		require.NoError(t, err)
		beforeUser, err := backing.GetInstanceByID(ctx, protocol.UserClientID)
		require.NoError(t, err)
		beforePeer, err := backing.GetInstanceByID(ctx, peer.ID())
		require.NoError(t, err)
		beforeAudit, err := backing.EventsBefore(ctx, "#dev", nil, 10)
		require.NoError(t, err)
		beforeScrollback, err := backing.ChannelScrollback(ctx, peer.ID(), "#dev", 10)
		require.NoError(t, err)

		sess.store = &failingJoinProjectionStore{Store: backing}
		response, sendErr := userClient(t, sess).Send(ctx, protocol.Join{
			Channels: []domain.ChannelName{"#dev"},
		})
		synctest.Wait()

		var executionErr protocol.JoinExecutionError
		require.ErrorAs(t, sendErr, &executionErr)
		require.Equal(t, domain.ChannelName("#dev"), executionErr.Channel)
		var projectionErr *joinProjectionFailure
		require.ErrorAs(t, sendErr, &projectionErr)

		afterLive, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		afterStored, err := backing.GetWindow(ctx, "#dev")
		require.NoError(t, err)
		afterUser, err := backing.GetInstanceByID(ctx, protocol.UserClientID)
		require.NoError(t, err)
		afterPeer, err := backing.GetInstanceByID(ctx, peer.ID())
		require.NoError(t, err)
		afterAudit, err := backing.EventsBefore(ctx, "#dev", nil, 10)
		require.NoError(t, err)
		afterScrollback, err := backing.ChannelScrollback(ctx, peer.ID(), "#dev", 10)
		require.NoError(t, err)

		type joinState struct {
			Response       protocol.Response
			LiveWindow     *domain.ChannelWindow
			StoredWindow   domain.Window
			User           *domain.Instance
			Peer           *domain.Instance
			Audit          []domain.StoredEvent
			PeerScrollback []domain.StoredEvent
			UserEvents     []domain.Event
			PeerEvents     []domain.Event
			PeerConnected  bool
		}
		require.Equal(t, joinState{
			LiveWindow:     beforeLive,
			StoredWindow:   beforeStored,
			User:           beforeUser,
			Peer:           beforePeer,
			Audit:          beforeAudit,
			PeerScrollback: beforeScrollback,
			PeerConnected:  true,
		}, joinState{
			Response:       response,
			LiveWindow:     afterLive,
			StoredWindow:   afterStored,
			User:           afterUser,
			Peer:           afterPeer,
			Audit:          afterAudit,
			PeerScrollback: afterScrollback,
			UserEvents:     collectEmittedEvents(t, sess),
			PeerEvents:     drainDeliveries(peerClient),
			PeerConnected:  sess.activeClientHandle(peerClient.Identity()) != nil,
		})
	})
}

func TestJoinAs_non_oper_creator_receives_channel_op_in_names(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		creator, client := seedPassiveInstance(t, sess, "creator", "test/model")

		require.False(t, sess.ClientCaps(client.Identity()).Has(protocol.CapOperator))

		resp, err := sess.Handle(t.Context(), client, protocol.Join{
			Channels: []domain.ChannelName{"#new"},
		})
		require.NoError(t, err)
		require.Equal(t, protocol.Response{Events: []protocol.Event{
			domain.JoinedChannel{Channel: "#new"},
		}}, resp)
		synctest.Wait()

		members := domain.NewMemberList()
		members.AddIdentity(creator.ID(), creator.Nick())
		members.SetModesID(creator.ID(), domain.MemberModes{Operator: true})

		require.Equal(t, []domain.Event{
			domain.Join{
				Source: domain.ClientSource(creator.ID(), "creator"),
				Target: "#new", Created: true, At: fixedTime,
			},
			domain.NamesReplyEvent{Channel: "#new", Members: members, At: fixedTime},
			domain.NamesEnd{Channel: "#new", At: fixedTime},
		}, drainDeliveries(client))
		require.False(t, sess.ClientCaps(client.Identity()).Has(protocol.CapOperator))
	})
}

// TestJoinAs_model_joining_existing_channel_gets_RPL_topic_and_names
// pins that a model joining a populated channel receives the
// RFC 2812 §3.2.1 / §3.2.4 equivalents of RPL_NAMREPLY and
// RPL_TOPIC ([domain.NamesReplyEvent] and [domain.TopicInfo]) on
// the joiner's own subscription — not as channel-wide broadcasts.
//
// The test captures the user-client bus (which is everyone else
// in this fixture) and asserts the RPL envelopes are NOT on it:
// the user did not join, so per RFC they get nothing. The Join
// itself is still broadcast (every member sees a JOIN) and the
// model's own dispatch lifecycle fires as usual.
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

		require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.Join{
				Source: domain.ClientSource(botty.ID(), "botty"),
				Target: "#dev", At: fixedTime,
			},
		}, collectEmittedEvents(t, sess),
			"the user-client bus carries only the JOIN broadcast; the bot's own "+
				"JOIN raises no dispatch turn, and NamesReplyEvent + TopicInfo are "+
				"scoped to the joiner (the bot) and never reach the user")

		replies, err := s.InstanceRepliesBefore(ctx, botty.ID(), nil, 10)
		require.NoError(t, err)
		require.Equal(t, []domain.PersistableEvent{
			domain.TopicInfo{
				Target:     "#dev",
				Topic:      "ongoing work",
				TopicSetBy: "testuser",
				TopicSetAt: fixedTime,
				At:         fixedTime,
			},
		}, storedReplyEvents(replies))
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
		turnID, err := s.BeginModelTurn(ctx, storemod.ModelTurn{
			InstanceID: botty.ID(),
			Window:     protocol.ChannelWindowTarget("#dev"),
			ModelID:    botty.ModelID,
			StartedAt:  fixedTime,
		}, storemod.ModelTurnEntry{
			Kind: storemod.ModelTurnInput,
			Data: []byte(`{"input":"channel context"}`),
			At:   fixedTime,
		})
		require.NoError(t, err)

		require.NoError(t, sess.partAs(ctx, botty, "#dev", "goodbye"))
		synctest.Wait()

		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Part{
				Source: domain.ClientSource(botty.ID(), "botty"),
				Target: "#dev", Message: "goodbye", At: fixedTime,
			},
		}, collectEmittedEvents(t, sess))

		ch, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		requireChannelEqual(t, newTestChannelWindow("#dev", fixedTime, testMembers(t, sess, s, "testuser")), ch)

		entries, err := s.ModelTurnEntries(ctx, turnID)
		require.NoError(t, err)
		require.Equal(t, []storemod.ModelTurnEntry{{
			Kind: storemod.ModelTurnInput,
			Data: []byte(`{"input":"channel context"}`),
			At:   fixedTime,
		}}, entries)

		inst, err := s.ResolveNick(ctx, "botty")
		require.NoError(t, err)
		requireInstanceEqual(t, domain.NewModelInstance(
			testMemberID("botty"), "botty", "test/model", "", testChannels(),
		), inst)
	})
}

func TestSession_failed_departure_preserves_membership_and_publishes_nothing(t *testing.T) {
	for _, tc := range []struct {
		Name string
		Run  func(context.Context, *Session, *domain.Instance) error
	}{
		{
			Name: "part",
			Run: func(ctx context.Context, sess *Session, actor *domain.Instance) error {
				return sess.partAs(ctx, actor, "#dev", "bye")
			},
		},
		{
			Name: "kick",
			Run: func(ctx context.Context, sess *Session, actor *domain.Instance) error {
				return sess.kickAs(ctx, sess.userInstance(), actor, "#dev")
			},
		},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sess, backing := newTestSession(t)
				ctx := t.Context()
				var interruptions []windowInterruption
				factory := sess.modelClientFactory.(*testModelClientFactory)
				factory.interruptWindowFn = func(id protocol.ClientID, window domain.ChannelName) {
					interruptions = append(interruptions, windowInterruption{Client: id, Window: window})
				}
				require.NoError(t, userJoin(ctx, t, sess, "#dev"))

				botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
				require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))
				turnEntry := storemod.ModelTurnEntry{
					Kind: storemod.ModelTurnInput,
					Data: []byte(`{"input":"channel context"}`),
					At:   fixedTime,
				}
				turnID, err := backing.BeginModelTurn(ctx, storemod.ModelTurn{
					InstanceID: botty.ID(),
					Window:     protocol.ChannelWindowTarget("#dev"),
					ModelID:    botty.ModelID,
					StartedAt:  fixedTime,
				}, turnEntry)
				require.NoError(t, err)

				synctest.Wait()
				collectEmittedEvents(t, sess)
				drainDeliveries(client)

				sentinel := errors.New("departure failed")
				sess.store = &failingChannelDepartureStore{Store: backing, err: sentinel}
				departureErr := tc.Run(ctx, sess, botty)
				synctest.Wait()

				liveWindow, liveWindowErr := sess.loadChannelWindow(ctx, "#dev")
				storedWindow, storedWindowErr := backing.GetWindow(ctx, "#dev")
				storedChannel, storedIsChannel := storedWindow.(*domain.ChannelWindow)
				storedActor, storedActorErr := backing.GetInstanceByID(ctx, botty.ID())
				auditEvents, auditErr := backing.EventsBefore(ctx, "#dev", nil, 10)
				turnEntries, turnEntriesErr := backing.ModelTurnEntries(ctx, turnID)

				type departureState struct {
					Failed             bool
					LiveWindowError    error
					LiveWindowHasActor bool
					LiveActorInChannel bool
					StoredWindowError  error
					StoredIsChannel    bool
					StoredHasActor     bool
					StoredActorError   error
					StoredActorInRoom  bool
					AuditError         error
					AuditEvents        []domain.StoredEvent
					TurnEntriesError   error
					TurnEntries        []storemod.ModelTurnEntry
					UserEvents         []domain.Event
					ActorEvents        []domain.Event
					Interruptions      []windowInterruption
				}
				require.Equal(t, departureState{
					Failed:             true,
					LiveWindowHasActor: true,
					LiveActorInChannel: true,
					StoredIsChannel:    true,
					StoredHasActor:     true,
					StoredActorInRoom:  true,
					AuditEvents: []domain.StoredEvent{
						{ID: 1, Event: domain.Join{
							Source: domain.ClientSource(protocol.UserClientID, "testuser"),
							Target: "#dev", Created: true, At: fixedTime,
						}},
						{ID: 2, Event: domain.Join{
							Source: domain.ClientSource(botty.ID(), botty.Nick()),
							Target: "#dev", At: fixedTime,
						}},
					},
					TurnEntries: []storemod.ModelTurnEntry{turnEntry},
				}, departureState{
					Failed:             errors.Is(departureErr, sentinel),
					LiveWindowError:    liveWindowErr,
					LiveWindowHasActor: liveWindow.Members.HasInstance(botty),
					LiveActorInChannel: botty.InChannel("#dev"),
					StoredWindowError:  storedWindowErr,
					StoredIsChannel:    storedIsChannel,
					StoredHasActor:     storedIsChannel && storedChannel.Members.HasInstance(botty),
					StoredActorError:   storedActorErr,
					StoredActorInRoom:  storedActor.InChannel("#dev"),
					AuditError:         auditErr,
					AuditEvents:        auditEvents,
					TurnEntriesError:   turnEntriesErr,
					TurnEntries:        turnEntries,
					UserEvents:         collectEmittedEvents(t, sess),
					ActorEvents:        drainDeliveries(client),
					Interruptions:      interruptions,
				})
			})
		})
	}
}

func TestPartAs_non_member_returns_442(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		saveTestChannel(t, sess, s, newTestChannelWindow("#dev", fixedTime, testMembers(t, sess, s, "testuser")))

		// partAs for an instance that isn't in the channel refuses
		// with RFC 2812 numeric 442 (ERR_NOTONCHANNEL) and otherwise
		// changes nothing: no PartEvent emission (the empty-id
		// fallback would otherwise ask the UI to drop the human's
		// channel), no stored membership mutation, no instance-
		// channels mutation.
		ghost := domain.NewModelInstance("ghost-id", "ghost", "test/model", "", nil)
		require.Equal(t,
			domain.NotOnChannelError{Channel: "#dev", Command: "PART", At: fixedTime},
			sess.partAs(ctx, ghost, "#dev", "bye"))
		synctest.Wait()

		require.Equal(t, []domain.Event{
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

		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Quit{
				Source:  domain.ClientSource(botty.ID(), "botty"),
				Message: "farewell", At: fixedTime,
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

func TestKillAs_sends_the_target_KILL_QUIT_ERROR_and_peers_only_QUIT(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#dev"))
		botty, target := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))
		drainDeliveries(target)
		collectEmittedEvents(t, sess)

		resp, err := sess.Handle(ctx, userClient(t, sess), protocol.Kill{Nick: "botty", Reason: "spam"})
		require.NoError(t, err)
		require.NoError(t, resp.Err)
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.KillNotice{
				Source:  domain.ClientSource("", "testuser"),
				Subject: "botty", Reason: "spam", At: fixedTime,
			},
			domain.Quit{
				Source:  domain.ClientSource(botty.ID(), "botty"),
				Message: "Killed by testuser (spam)", At: fixedTime,
			},
			domain.ConnectionError{Reason: "Killed by testuser (spam)", At: fixedTime},
		}, drainDeliveries(target))
		require.Equal(t, []domain.Event{domain.Quit{
			Source:  domain.ClientSource(botty.ID(), "botty"),
			Message: "Killed by testuser (spam)", At: fixedTime,
		}}, collectEmittedEvents(t, sess))

		_, err = s.ResolveNick(ctx, "botty")
		require.Error(t, err)
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

	user := domain.NewModelInstance("", "testuser", "", "", testChannels("#shared"))
	gotUser := intersectActorTargets(
		fakeUserServerClient(t, user),
		channelNames(alphaChannels),
	)
	require.Equal(t, []domain.ChannelName{"#shared"}, gotUser,
		"user-client receives only the channels it shares with the actor")
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
// the user rides the same membership filter as a model, so the
// instance carries the channels the user has joined.
func fakeUserServerClient(t *testing.T, inst *domain.Instance) *serverClient {
	t.Helper()

	return &serverClient{
		id:       protocol.UserClientID,
		instance: inst,
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

		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Message{Source: domain.ClientSource(

				testMemberID("botty"), "botty"), Target: "#dev", Body: "hello world", At: fixedTime},
		}, collectEmittedEvents(t, sess))

		msgs := channelMessages(t, s, "#dev")
		require.Equal(t, []domain.Message{
			{
				Source: domain.ClientSource(testMemberID("botty"), "botty"),
				Target: "#dev",
				Body:   "hello world",
				At:     fixedTime,
			},
		}, msgs)
	})
}

// TestSendMessageAs_user_actor_echoes_to_originator pins the
// echo-message capability: the user-client holds it, so a PRIVMSG it
// sends to a channel it is in returns on its own subscription's
// events channel (IRCv3 echo-message). A model, without the cap,
// keeps RFC 2812 §3.3.1 no-self-echo — see
// TestSendMessageAs_model_to_model_dispatches, where a self-echoing
// model would wrongly self-dispatch.
func TestSendMessageAs_user_actor_echoes_to_originator(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, joinAs(ctx, sess, userInstance(t, sess), "#dev", ""))
		synctest.Wait()

		_, err := sess.sendMessageAs(ctx, userInstance(t, sess), "#dev", "hello")
		require.NoError(t, err)
		synctest.Wait()

		user := userInstance(t, sess)
		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Source:  domain.ClientSource(user.ID(), "testuser"),
				Target:  "#dev",
				Created: true,
				At:      fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#dev",
				Members: testMembers(t, sess, s, "testuser"),
				At:      fixedTime,
			},
			domain.NamesEnd{
				Channel: "#dev",
				At:      fixedTime,
			},
			domain.Message{Source: domain.ClientSource(

				user.ID(), "testuser"), Target: "#dev", Body: "hello", At: fixedTime},
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

		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.TopicChange{
				Source: domain.ClientSource(botty.ID(), "botty"),
				Target: "#dev",
				Topic:  "new topic",
				At:     fixedTime,
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

		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.TopicChange{
				Source: domain.ClientSource(
					userInstance(t, sess).ID(), userInstance(t, sess).Nick(),
				),
				Target: "#dev",
				Topic:  "stable topic",
				At:     fixedTime,
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

// TestSetTopicAs_rejects_a_topic_over_TopicMaxLen pins TOPICLEN
// enforcement: a topic longer than [domain.TopicMaxLen] is refused
// before it touches channel state, and the channel's topic is left
// as it was.
func TestSetTopicAs_rejects_a_topic_over_TopicMaxLen(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#dev", "testuser")

	tooLong := strings.Repeat("x", domain.TopicMaxLen+1)

	err := sess.setTopicAs(ctx, userInstance(t, sess), "#dev", tooLong)
	require.Equal(t, domain.ErroneousTopicError{
		Channel: "#dev",
		Reason:  domain.TopicTooLong,
		At:      fixedTime,
	}, err)

	window, err := sess.loadChannelWindow(ctx, "#dev")
	require.NoError(t, err)
	require.Empty(t, window.Topic)
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
			Channels: testChannels("#dev", "#other"),
		})
		seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty", "helper")
		var interruptions []windowInterruption
		factory := sess.modelClientFactory.(*testModelClientFactory)
		factory.interruptWindowFn = func(id protocol.ClientID, window domain.ChannelName) {
			interruptions = append(interruptions, windowInterruption{Client: id, Window: window})
		}

		// KICK is channel-op gated (RFC 2812 §3.2.8); give botty `@`
		// before exercising the kick path.
		w, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		w.Members.SetModes(botty, domain.MemberModes{Operator: true})
		require.NoError(t, sess.persistChannelWindow(ctx, w))

		require.NoError(t, sess.kickAs(ctx, botty, helper, "#dev"))
		synctest.Wait()

		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Kicked{
				Source:  domain.ClientSource(botty.ID(), "botty"),
				Target:  "#dev",
				Subject: "helper",
				At:      fixedTime,
			},
		}, collectEmittedEvents(t, sess))

		ch, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		expectedMembers := testMembers(t, sess, s, "testuser", "botty")
		expectedMembers.SetModes(botty, domain.MemberModes{Operator: true})
		requireChannelEqual(t, newTestChannelWindow("#dev", fixedTime, expectedMembers), ch)

		inst, err := s.ResolveNick(ctx, "helper")
		require.NoError(t, err)
		requireInstanceEqual(t, domain.NewModelInstance(
			testMemberID("helper"), "helper", "test/model-b", "", testChannels("#other"),
		), inst)
		require.Equal(t, []windowInterruption{{
			Client: protocol.ClientID(helper.ID()), Window: "#dev",
		}}, interruptions)
		require.NotNil(t, sess.activeClientHandle(protocol.ClientID(helper.ID())))
	})
}

// TestInviteAs_unknown_nick_returns_notice pins the unknown-nick
// path of `inviteAs`: there is no subscription to deliver the
// invite to, so the call returns the typed refusal and a private
// [domain.SystemNotice], and leaves the channel event log untouched.
func TestInviteAs_unknown_nick_returns_notice(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
	seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")

	event, err := sess.inviteAs(ctx, botty, "helper", "#dev")
	require.Equal(t, domain.UnknownNickError{Nick: "helper", At: fixedTime}, err)
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

		var dispatched []domain.ModelID
		fake := &apitest.Fake{
			SendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ api.SystemPrompt, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
				dispatched = append(dispatched, modelID)
				return api.CompletionResult{}, nil
			},
		}

		sess, s := newTestSessionWithAPI(t, fake)
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

		// helper takes a dispatch turn on the incoming DM; botty, the
		// sender, is echo-gated and does not.
		require.Equal(t, []domain.ModelID{"test/model-b"}, dispatched)

		// The user is not party to the model↔model DM, so it sees only
		// its bootstrap OPER promotion.
		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
		}, collectEmittedEvents(t, sess))

		msgs := channelMessages(t, s, target)
		require.Equal(t, []domain.Message{
			{
				Source: domain.ClientSource(testMemberID("botty"), "botty"),
				Target: target,
				Body:   "hey there",
				At:     fixedTime,
			},
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

		require.NoError(t, joinAs(ctx, sess, botty, "modeloff", ""))
		synctest.Wait()

		ch, err := sess.loadChannelWindow(ctx, "#modeloff")
		require.NoError(t, err)
		require.True(t, ch.Members.HasNick("botty"))

		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
		}, collectEmittedEvents(t, sess),
			"the user is not in #modeloff, so botty's join does not reach the user-client")

		require.Equal(t, []string{"join"}, channelEventTypes(t, s, "#modeloff"),
			"the normalised join is broadcast and persisted to #modeloff")

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
		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Source:  domain.ClientSource(user.ID(), "testuser"),
				Target:  "#general",
				Created: true,
				At:      fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#general",
				Members: testMembers(t, sess, s, "testuser"),
				At:      fixedTime,
			},
			domain.NamesEnd{
				Channel: "#general",
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

		require.NoError(t, joinAs(ctx, sess, userInstance(t, sess), "#dev", ""))
		synctest.Wait()

		user := userInstance(t, sess)
		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Source:  domain.ClientSource(user.ID(), "testuser"),
				Target:  "#dev",
				Created: true,
				At:      fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#dev",
				Members: testMembers(t, sess, s, "testuser"),
				At:      fixedTime,
			},
			domain.NamesEnd{
				Channel: "#dev",
				At:      fixedTime,
			},
		}, collectEmittedEvents(t, sess))

		ch, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)

		m, ok := ch.Members.GetByInstance(user)
		require.True(t, ok)
		require.Equal(t, domain.MemberModes{Operator: true}, m.Modes)
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

		require.NoError(t, joinAs(ctx, sess, userInstance(t, sess), "#dev", ""))
		synctest.Wait()

		user := userInstance(t, sess)
		expectedMembers := testMembers(t, sess, s, "testuser", "alice")
		expectedMembers.SetModes(user, domain.MemberModes{})
		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Source:  domain.ClientSource(user.ID(), "testuser"),
				Target:  "#dev",
				Created: false,
				At:      fixedTime,
			},
			domain.TopicInfo{
				Target:     "#dev",
				Topic:      "Go development",
				TopicSetBy: "alice",
				TopicSetAt: fixedTime.Add(-time.Hour),
				At:         fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#dev",
				Members: expectedMembers,
				At:      fixedTime,
			},
			domain.NamesEnd{
				Channel: "#dev",
				At:      fixedTime,
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

		require.NoError(t, joinAs(ctx, sess, userInstance(t, sess), "#dev", ""))
		synctest.Wait()

		user := userInstance(t, sess)
		expectedMembers := testMembers(t, sess, s, "testuser", "alice")
		expectedMembers.SetModes(user, domain.MemberModes{})
		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Source:  domain.ClientSource(user.ID(), "testuser"),
				Target:  "#dev",
				Created: false,
				At:      fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#dev",
				Members: expectedMembers,
				At:      fixedTime,
			},
			domain.NamesEnd{
				Channel: "#dev",
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

		require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))
		synctest.Wait()

		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Source:  domain.ClientSource(botty.ID(), "botty"),
				Target:  "#dev",
				Created: false,
				At:      fixedTime,
			},
		}, collectEmittedEvents(t, sess),
			"NamesReplyEvent and TopicInfo are scoped to the joiner (the bot) and "+
				"the user-client bus does not carry them; the bot's own JOIN also "+
				"raises no dispatch turn")
	})
}
