package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

type blockingLifecycleDeletionStore struct {
	Store

	entered chan struct{}
	release chan struct{}
}

func (s *blockingLifecycleDeletionStore) CommitInstanceDeletion(
	ctx context.Context,
	deletion storemod.InstanceDeletion,
) (storemod.CommittedInstanceDeletion, error) {
	close(s.entered)

	select {
	case <-s.release:
	case <-ctx.Done():
		return storemod.CommittedInstanceDeletion{}, ctx.Err()
	}

	return s.Store.CommitInstanceDeletion(ctx, deletion)
}

// quitToolCall builds a [api.CompletionResult] whose PendingToolCalls
// invoke the `quit` tool with the given farewell. A model that runs
// this ends its own connection from inside its dispatch turn.
func quitToolCall(t testing.TB, message string) api.CompletionResult {
	t.Helper()

	args, err := json.Marshal(map[string]any{"message": message})
	require.NoError(t, err)

	return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{
		{ID: "call_quit_0", Name: "quit", Args: args},
	}}
}

// TestSession_model_quit_tool_ends_its_own_connection covers the
// case where the client asking to be disconnected is the one running
// the request. A model's `quit` tool call is issued from its dispatch
// goroutine, so the QUIT handler reaches that model's own client; a
// teardown that joined the goroutine there would be the goroutine
// waiting on itself, and the turn would never return.
//
// The turn ends where the model left it: the QUIT reaches the
// channel, the instance is gone from the member list, and no further
// upstream round-trip is made — `quit` ends the turn the way `pass`
// does.
func TestSession_model_quit_tool_ends_its_own_connection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		continues := 0
		fake := &apitest.Fake{
			SendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, api.SystemPrompt, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
				return quitToolCall(t, "signing off"), nil
			},
			ContinueWithToolResultsFn: func(context.Context, *api.Conversation, []api.ToolResult) (api.CompletionResult, error) {
				continues++
				return api.CompletionResult{}, nil
			},
		}

		bootAt := time.Now()
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

		dispatchUserMessage(ctx, t, sess, "#general", "still there?")

		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Message{Source: domain.ClientSource(protocol.UserClientID, "testuser"), Target: "#general", Body: "still there?", At: fixedTime},
			domain.ModelDispatchStarted{Source: domain.ClientSource(botty.ID(), "botty"), At: fixedTime},
			domain.Quit{Source: domain.ClientSource(botty.ID(), "botty"), Message: "signing off", At: fixedTime},
			domain.ModelDispatchDone{Source: domain.ClientSource(botty.ID(), "botty"), At: fixedTime},
		}, collectEmittedEvents(t, sess))

		require.Equal(t, 0, continues, "quit ends the turn: there is no client left to ask for another round")

		window, err := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		require.False(t, window.Members.HasInstance(botty))

		require.False(t, sess.ClientConnected(protocol.ClientID(botty.ID())))
	})
}

func TestSession_model_quit_drains_terminal_events_before_closing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()
		model, client := seedPassiveInstance(t, sess, "botty", "test/model")
		drainDeliveries(client)

		resp, err := sess.Handle(ctx, client, protocol.Quit{Reason: "goodnight"})
		require.NoError(t, err)
		require.Equal(t, protocol.Response{}, resp)

		var deliveries []protocol.Delivery
		for range 2 {
			deliveries = append(deliveries, <-client.Events())
		}
		require.Equal(t, []protocol.Delivery{
			{Event: domain.Quit{
				Source:  domain.ClientSource(model.ID(), "botty"),
				Message: "goodnight", At: fixedTime,
			}},
			{Event: domain.ConnectionError{Reason: "Connection closed", At: fixedTime}},
		}, deliveries)
		<-client.sub.Done()
	})
}

func TestSession_terminal_error_ends_the_departing_clients_delivery_prefix(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backing := storetest.NewMemoryStore(t)
		factory := newTestModelClientFactory(t, &apitest.Fake{})
		sess := New(t.Context(), backing, factory, nil)
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }

		require.NoError(t, userJoin(t.Context(), t, sess, "#general"))
		botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(t.Context(), sess, botty, "#general", ""))
		user := userClient(t, sess)
		drainDeliveries(user)
		drainDeliveries(client)

		target := protocol.ChannelWindowTarget("#general")
		dispatch := sess.BeginModelDispatch(
			t.Context(), mustWindowGuard(t, client, target), target,
			domain.ModelDispatchStarted{
				Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
			},
		)
		factory.interruptTurnFn = func(id protocol.ClientID) {
			if id == protocol.ClientID(botty.ID()) {
				dispatch.Done(t.Context(), domain.ModelDispatchDone{
					Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
				})
			}
		}

		response, err := user.Send(t.Context(), protocol.Kill{
			Nick: botty.Nick(), Reason: "enough",
		})
		synctest.Wait()

		type assertionSnapshot struct {
			Response protocol.Response
			Error    error
			User     []domain.Event
			Model    []domain.Event
		}

		require.Equal(t, assertionSnapshot{
			User: []domain.Event{
				domain.ModelDispatchStarted{
					Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
				},
				domain.Quit{
					Source:  domain.ClientSource(botty.ID(), botty.Nick()),
					Message: "Killed by testuser (enough)", At: fixedTime,
				},
				domain.ModelDispatchDone{
					Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
				},
			},
			Model: []domain.Event{
				domain.ModelDispatchStarted{
					Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
				},
				domain.KillNotice{
					Source:  domain.ClientSource(protocol.UserClientID, "testuser"),
					Subject: botty.Nick(), Reason: "enough", At: fixedTime,
				},
				domain.Quit{
					Source:  domain.ClientSource(botty.ID(), botty.Nick()),
					Message: "Killed by testuser (enough)", At: fixedTime,
				},
				domain.ConnectionError{
					Reason: "Killed by testuser (enough)", At: fixedTime,
				},
			},
		}, assertionSnapshot{
			Response: response,
			Error:    err,
			User:     drainDeliveries(user),
			Model:    drainDeliveries(client),
		})
	})
}

func TestSession_kill_drains_terminal_events_through_backpressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()
		model, client := seedPassiveInstance(t, sess, "botty", "test/model")
		drainDeliveries(client)

		filler := protocol.Delivery{Event: domain.SystemNotice{Text: "queued", At: fixedTime}}
		sub := client.sub.(*serverClient)
		for range eventBufSize {
			sub.events <- filler
		}

		resp, err := userClient(t, sess).Send(ctx, protocol.Kill{
			Nick: "botty", Reason: "enough",
		})
		require.NoError(t, err)
		require.Equal(t, protocol.Response{}, resp)
		require.False(t, sess.ClientConnected(client.Identity()))

		for range eventBufSize {
			require.Equal(t, filler, <-client.Events())
		}

		var terminal []domain.Event
		for range 3 {
			terminal = append(terminal, (<-client.Events()).Event)
		}
		require.Equal(t, []domain.Event{
			domain.KillNotice{
				Source:  domain.ClientSource(protocol.UserClientID, "testuser"),
				Subject: "botty", Reason: "enough", At: fixedTime,
			},
			domain.Quit{
				Source:  domain.ClientSource(model.ID(), "botty"),
				Message: "Killed by testuser (enough)", At: fixedTime,
			},
			domain.ConnectionError{
				Reason: "Killed by testuser (enough)", At: fixedTime,
			},
		}, terminal)
		<-client.sub.Done()
	})
}

func TestSession_shutdown_releases_a_terminal_reaper_blocked_on_outbound_delivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()
		_, client := seedPassiveInstance(t, sess, "botty", "test/model")
		drainDeliveries(client)

		filler := protocol.Delivery{Event: domain.SystemNotice{Text: "queued", At: fixedTime}}
		sub := client.sub.(*serverClient)
		for range eventBufSize {
			sub.events <- filler
		}

		response, err := userClient(t, sess).Send(ctx, protocol.Kill{
			Nick: "botty", Reason: "enough",
		})
		require.NoError(t, err)
		require.Equal(t, protocol.Response{}, response)
		require.False(t, sess.ClientConnected(client.Identity()))

		shutdownCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		shutdownErr := sess.Shutdown(shutdownCtx)

		type assertionSnapshot struct {
			ShutdownError error
			Queued        int
		}

		require.Equal(t, assertionSnapshot{}, assertionSnapshot{
			ShutdownError: shutdownErr,
			Queued:        queuedDeliveries(sub),
		})
	})
}

// TestSession_sendQ_overflow_disconnects_the_client covers the
// outbound bound. A subscription whose consumer has stopped reading
// is one the server cannot serve, and past `sendQAllowance` it stops
// trying: the client is disconnected through the ordinary QUIT
// teardown, so the channel sees it leave. Dropping deliveries
// instead would leave every reader's transcript with a hole in it
// and nothing to say one was there.
//
// The rule belongs to the subscription, so it is the same rule for
// every kind of client; this fixture drives it with a model, which
// is the kind that stops reading in practice.
func TestSession_sendQ_overflow_disconnects_the_client(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The turn parks until the client's own teardown cancels it,
		// which is what leaves the subscription with nobody reading
		// it while the flood arrives.
		turnStarted := make(chan struct{})
		turnCancelled := make(chan struct{})
		fake := &apitest.Fake{
			SendEventsFn: func(ctx context.Context, _ domain.ModelID, _ domain.InstanceID, _ api.SystemPrompt, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
				close(turnStarted)
				<-ctx.Done()
				close(turnCancelled)
				return api.CompletionResult{}, ctx.Err()
			},
		}

		bootAt := time.Now()
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

		dispatchUserMessage(ctx, t, sess, "#general", "are you there?")
		<-turnStarted

		sub := sess.lookupClientHandle(protocol.ClientID(botty.ID()))
		require.NotNil(t, sub)

		// Fill the queue past its allowance. The channel buffer takes
		// the head of the flood before the queue starts holding, so
		// the trip point is one past both.
		for range eventBufSize + sendQAllowance + 1 {
			sub.enqueue(t.Context(), protocol.Delivery{Event: domain.Message{Source: domain.ClientSource(protocol.UserClientID, "testuser"),

				Target: "#general", Body: "flood", At: fixedTime}})
		}

		<-turnCancelled
		synctest.Wait()
		<-sub.Done()

		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Message{Source: domain.ClientSource(protocol.UserClientID, "testuser"), Target: "#general", Body: "are you there?", At: fixedTime},
			domain.ModelDispatchStarted{Source: domain.ClientSource(botty.ID(), "botty"), At: fixedTime},
			domain.Quit{Source: domain.ClientSource(botty.ID(), "botty"), Message: "Max SendQ exceeded", At: fixedTime},
			domain.ModelDispatchDone{Source: domain.ClientSource(botty.ID(), "botty"), At: fixedTime},
		}, collectEmittedEvents(t, sess))

		require.False(t, sess.ClientConnected(protocol.ClientID(botty.ID())))
		factory := sess.modelClientFactory.(*testModelClientFactory)
		require.Equal(t, []protocol.ClientID{}, factory.attached())
		require.Equal(t, []protocol.ClientID{protocol.ClientID(botty.ID())}, factory.deletedInstances())

		window, err := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		require.False(t, window.Members.HasInstance(botty))
	})
}

func TestSession_sendQ_overflow_seals_the_accepted_prefix_during_teardown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backing := storetest.NewMemoryStore(t)
		blocking := &blockingLifecycleDeletionStore{
			Store: backing, entered: make(chan struct{}), release: make(chan struct{}),
		}
		factory := newTestModelClientFactory(t, &apitest.Fake{})
		sess := New(t.Context(), blocking, factory, nil)
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }

		model, client := seedPassiveInstance(t, sess, "botty", "test/model")
		drainDeliveries(client)
		sub := client.sub.(*serverClient)

		buffered := protocol.Delivery{Event: domain.SystemNotice{
			Text: "already buffered", At: fixedTime,
		}}
		for range eventBufSize {
			sub.events <- buffered
		}

		acceptedDelivery := protocol.Delivery{Event: domain.SystemNotice{
			Text: "accepted before overflow", At: fixedTime,
		}}
		acceptedQueue := make([]queuedDelivery, sendQAllowance)
		for i := range acceptedQueue {
			acceptedQueue[i] = queuedDelivery{delivery: acceptedDelivery}
			sub.enqueue(t.Context(), acceptedDelivery)
		}

		sub.enqueue(t.Context(), protocol.Delivery{Event: domain.SystemNotice{
			Text: "overflow", At: fixedTime,
		}})
		<-blocking.entered
		actual := []protocol.Delivery{<-client.Events()}
		synctest.Wait()

		acceptedAfterDrain := make([]queuedDelivery, sendQAllowance-1)
		for i := range acceptedAfterDrain {
			acceptedAfterDrain[i] = queuedDelivery{delivery: acceptedDelivery}
		}

		lateDelivery := protocol.Delivery{Event: domain.PokeEvent{
			Channel: "#general", At: fixedTime,
		}}
		for range 3 {
			sub.enqueue(t.Context(), lateDelivery)
		}
		queuedDuringTeardown := queuedDeliverySnapshot(sub)

		close(blocking.release)
		for range eventBufSize + len(queuedDuringTeardown) + 2 {
			actual = append(actual, <-client.Events())
		}
		<-sub.Done()
		require.NoError(t, sess.Shutdown(t.Context()))

		expected := make([]protocol.Delivery, 0, eventBufSize+sendQAllowance+2)
		for range eventBufSize {
			expected = append(expected, buffered)
		}
		for range sendQAllowance {
			expected = append(expected, acceptedDelivery)
		}
		expected = append(expected,
			protocol.Delivery{Event: domain.Quit{
				Source:  domain.ClientSource(model.ID(), "botty"),
				Message: sendQExceededReason, At: fixedTime,
			}},
			protocol.Delivery{Event: domain.ConnectionError{
				Reason: sendQExceededReason, At: fixedTime,
			}},
		)

		type assertionSnapshot struct {
			Queued  []queuedDelivery
			Drained []protocol.Delivery
		}

		require.Equal(t, assertionSnapshot{
			Queued:  acceptedAfterDrain,
			Drained: expected,
		}, assertionSnapshot{
			Queued:  queuedDuringTeardown,
			Drained: actual,
		})
	})
}

func TestSession_channel_event_failure_keeps_the_recipient_connected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, backing := newTestSession(t)
		ctx := t.Context()
		require.NoError(t, userJoin(ctx, t, sess, "#general"))

		model := seedInstanceRow(t, backing, instanceSpec{
			Nick: "botty", ModelID: "test/model",
		})
		client := &passiveClient{id: protocol.ClientID(model.ID())}
		subscription, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{
			ReplayHistory: true,
		})
		require.NoError(t, err)
		client.sub = subscription
		joinResponse, err := sess.Handle(ctx, client, protocol.Join{
			Channels: []domain.ChannelName{"#general"},
		})
		require.NoError(t, err)
		require.Equal(t, protocol.Response{Events: []protocol.Event{
			domain.JoinedChannel{Channel: "#general"},
		}}, joinResponse)
		subscription.Activate()
		synctest.Wait()
		collectSubscriptionDeliveries(subscription)
		auditBefore, err := backing.EventsBefore(ctx, "#general", nil, 10)
		require.NoError(t, err)
		projectedBefore, err := backing.ChannelScrollback(
			ctx, model.ID(), "#general", 10,
		)
		require.NoError(t, err)

		sess.store = &failingProjectionStore{Store: backing}
		response, sendErr := userClient(t, sess).Send(ctx, protocol.PrivMsg{
			Target: protocol.ChannelTarget("#general"), Body: "projection fails",
		})
		var persistenceErr *MessagePersistenceError
		require.ErrorAs(t, sendErr, &persistenceErr)

		serverSub := subscription.(*serverClient)
		serverSub.enqueue(ctx, protocol.Delivery{Event: domain.PokeEvent{
			Channel: "#general", At: fixedTime,
		}})
		synctest.Wait()
		audit, auditErr := backing.EventsBefore(ctx, "#general", nil, 10)
		projected, projectedErr := backing.ChannelScrollback(
			ctx, model.ID(), "#general", 10,
		)
		window, windowErr := sess.loadChannelWindow(ctx, "#general")

		type assertionSnapshot struct {
			Response          protocol.Response
			PersistenceFailed bool
			Connected         bool
			Member            bool
			AuditError        error
			Audit             []domain.StoredEvent
			ProjectedError    error
			Projected         []domain.StoredEvent
			Deliveries        []protocol.Delivery
		}

		require.Equal(t, assertionSnapshot{
			PersistenceFailed: true,
			Connected:         true,
			Member:            true,
			Audit:             auditBefore,
			Projected:         projectedBefore,
			Deliveries: []protocol.Delivery{{Event: domain.PokeEvent{
				Channel: "#general", At: fixedTime,
			}}},
		}, assertionSnapshot{
			Response:          response,
			PersistenceFailed: persistenceErr != nil,
			Connected:         sess.ClientConnected(protocol.ClientID(model.ID())),
			Member:            windowErr == nil && window.Members.HasInstance(model),
			AuditError:        auditErr,
			Audit:             audit,
			ProjectedError:    projectedErr,
			Projected:         projected,
			Deliveries:        collectSubscriptionDeliveries(subscription),
		})
	})
}

func TestSession_sendQ_overflow_interrupts_turn_when_instance_deletion_fails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		turnStarted := make(chan struct{})
		turnCancelled := make(chan struct{})
		fake := &apitest.Fake{
			SendEventsFn: func(ctx context.Context, _ domain.ModelID, _ domain.InstanceID, _ api.SystemPrompt, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
				close(turnStarted)
				<-ctx.Done()
				close(turnCancelled)

				return api.CompletionResult{}, ctx.Err()
			},
		}

		backing := storetest.NewMemoryStore(t)
		failing := &teardownFailureStore{
			Store:             backing,
			deleteInstanceErr: errors.New("delete failed"),
		}
		factory := newTestModelClientFactory(t, fake)
		sess := New(t.Context(), failing, factory, nil)
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }

		botty := seedInstance(t, sess, backing, instanceSpec{
			Nick: "botty", ModelID: "test/model", Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, backing, "#general", "testuser", "botty")
		collectEmittedEvents(t, sess)

		failing.instanceID = botty.ID()
		failing.armed.Store(true)

		dispatchUserMessage(t.Context(), t, sess, "#general", "are you there?")
		<-turnStarted

		sub := sess.lookupClientHandle(protocol.ClientID(botty.ID()))
		require.NotNil(t, sub)
		for range eventBufSize + sendQAllowance + 1 {
			sub.enqueue(t.Context(), protocol.Delivery{Event: domain.Message{
				Source: domain.ClientSource(protocol.UserClientID, "testuser"),
				Target: "#general", Body: "flood", At: fixedTime,
			}})
		}

		synctest.Wait()
		type assertionSnapshot struct {
			TurnCancelled bool
			Connected     bool
			Member        bool
		}

		cancelledBeforeDrain := false
		select {
		case <-turnCancelled:
			cancelledBeforeDrain = true
		default:
		}
		window, err := sess.loadChannelWindow(t.Context(), "#general")
		require.NoError(t, err)
		stateBeforeDrain := assertionSnapshot{
			TurnCancelled: cancelledBeforeDrain,
			Connected:     sess.ClientConnected(protocol.ClientID(botty.ID())),
			Member:        window.Members.HasInstance(botty),
		}

		go func() {
			for {
				select {
				case <-sub.events:
				case <-sub.Done():
					return
				}
			}
		}()
		<-sub.Done()
		require.NoError(t, sess.Shutdown(t.Context()))

		require.Equal(t, assertionSnapshot{
			TurnCancelled: true,
		}, stateBeforeDrain)
	})
}

func TestSession_dispatch_panic_reaps_a_backpressured_client(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		turnStarted := make(chan struct{})
		panicNow := make(chan struct{})
		fake := &apitest.Fake{
			SendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, api.SystemPrompt, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
				close(turnStarted)
				<-panicNow
				panic("dispatch failed")
			},
		}

		bootAt := time.Now()
		sess, store := newTestSessionWithAPI(t, fake)
		ctx := t.Context()
		botty := seedInstance(t, sess, store, instanceSpec{
			Nick: "botty", ModelID: "test/model", Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, store, "#general", "testuser", "botty")

		dispatchUserMessage(ctx, t, sess, "#general", "break now")
		<-turnStarted

		sub := sess.lookupClientHandle(protocol.ClientID(botty.ID()))
		require.NotNil(t, sub)
		for range eventBufSize + 5 {
			sub.enqueue(ctx, protocol.Delivery{Event: domain.Message{
				Source: domain.ClientSource(protocol.UserClientID, "testuser"),
				Target: "#general", Body: "backpressure", At: fixedTime,
			}})
		}
		require.Equal(t, 6, queuedDeliveries(sub))

		close(panicNow)
		synctest.Wait()
		<-sub.Done()

		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Message{
				Source: domain.ClientSource(protocol.UserClientID, "testuser"),
				Target: "#general", Body: "break now", At: fixedTime,
			},
			domain.ModelDispatchStarted{
				Source: domain.ClientSource(botty.ID(), "botty"), At: fixedTime,
			},
			domain.ModelDispatchDone{
				Source: domain.ClientSource(botty.ID(), "botty"), At: fixedTime,
			},
			domain.Quit{
				Source:  domain.ClientSource(botty.ID(), "botty"),
				Message: "Internal error", At: fixedTime,
			},
		}, collectEmittedEvents(t, sess))
		require.False(t, sess.ClientConnected(protocol.ClientID(botty.ID())))
		factory := sess.modelClientFactory.(*testModelClientFactory)
		require.Equal(t, []protocol.ClientID{}, factory.attached())
		require.Equal(t, []protocol.ClientID{protocol.ClientID(botty.ID())}, factory.deletedInstances())

		window, err := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		require.False(t, window.Members.HasInstance(botty))
	})
}

// TestSession_sendQ_overflow_spares_the_session_lifetime_client
// covers the one subscription the allowance does not reach. The
// server cannot close the connection that is the process hosting it,
// so there is nothing to trade a bounded queue for: the queue keeps
// growing, and the client stays exactly where it was.
//
// The alternative endings are both worse than unbounded buffering.
// Skipping deliveries would put a hole in the one transcript a
// person is reading. Running the teardown would drop every
// membership and put a QUIT in each channel underneath a process
// still running on the far side of it.
func TestSession_sendQ_overflow_spares_the_session_lifetime_client(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSessionWithAPI(t, &apitest.Fake{})
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", "testuser")

		sub := sess.lookupClientHandle(protocol.UserClientID)
		require.NotNil(t, sub)

		// Start from an empty bus so the arithmetic below is exact:
		// the channel buffer takes the head of the flood and the queue
		// holds the rest.
		collectEmittedEvents(t, sess)

		const flood = eventBufSize + sendQAllowance + 1

		for range flood {
			sub.enqueue(t.Context(), protocol.Delivery{Event: domain.Message{Source: domain.ClientSource(protocol.UserClientID, "testuser"),

				Target: "#general", Body: "flood", At: fixedTime}})
		}

		synctest.Wait()

		// Nothing was skipped: what the channel buffer could not take
		// is all still queued.
		require.Equal(t, flood-eventBufSize, queuedDeliveries(sub))

		require.True(t, sess.ClientConnected(protocol.UserClientID))
		require.True(t, userInstance(t, sess).InChannel("#general"))

		window, err := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		require.True(t, window.Members.HasInstance(userInstance(t, sess)))

		active, err := s.GetSessionActive(ctx)
		require.NoError(t, err)
		require.Empty(t, active, "the crash marker is the session's to write, and nothing here has ended it")
	})
}

// queuedDeliveries reports how many deliveries the subscription is
// still holding for its consumer.
func queuedDeliveries(c *serverClient) int {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	return len(c.outbox)
}

func queuedDeliverySnapshot(c *serverClient) []queuedDelivery {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	return append([]queuedDelivery(nil), c.outbox...)
}

// TestSession_dispatch_lifecycle_starts_with_the_upstream_turn covers
// the boundary around the work a consumer displays as "thinking".
// A failure before the model has a valid window does not start that
// lifecycle.
func TestSession_dispatch_lifecycle_starts_with_the_upstream_turn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSessionWithAPI(t, &apitest.Fake{})
		ctx := t.Context()

		// Both actors believe they are in #ghost, which has no channel
		// record, so the turn's window load fails. The user's
		// membership is what puts it in range of botty's lifecycle
		// events.
		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#ghost"),
		})
		require.NoError(t, sess.destroyChannel(ctx, "#ghost"))
		registerUserMembership(t, sess, "#ghost", []domain.Nick{userNick(t, sess)})

		sess.emitScoped(ctx, domain.PokeEvent{Channel: "#ghost", At: fixedTime}, channelScope{"#ghost"})
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.ModelUnavailableError{
				Source: domain.ClientSource(botty.ID(), "botty"),
				At:     fixedTime,
			},
		}, dispatchLifecycleEvents(collectEmittedEvents(t, sess)))
	})
}

func TestSession_direct_dispatch_failure_uses_the_recipient_window(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()
		botty, _ := seedPassiveInstance(t, sess, "botty", "test/model")
		user := userClient(t, sess)
		drainDeliveries(user)

		failure := domain.ModelUnavailableError{
			Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
		}
		sess.EmitModelFailure(ctx, protocol.DirectWindowTarget(protocol.UserClientID), failure)
		synctest.Wait()

		require.Equal(t, protocol.Delivery{
			Event: failure, Window: protocol.DirectWindowTarget(botty.ID()),
		}, <-user.Events())
	})
}

func TestSession_direct_dispatch_failure_has_no_unrelated_operator_window(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()
		botty, _ := seedPassiveInstance(t, sess, "botty", "test/model")
		alice, _ := seedPassiveInstance(t, sess, "alice", "test/model")
		user := userClient(t, sess)
		drainDeliveries(user)

		failure := domain.ModelUnavailableError{
			Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
		}
		sess.EmitModelFailure(ctx, protocol.DirectWindowTarget(alice.ID()), failure)
		synctest.Wait()

		require.Equal(t, protocol.Delivery{Event: failure}, <-user.Events())
	})
}

func TestSession_dispatch_done_reaches_the_start_audience_after_part(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()
		require.NoError(t, userJoin(ctx, t, sess, "#general"))

		botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#general", ""))
		drainDeliveries(client)
		collectEmittedEvents(t, sess)

		target := protocol.ChannelWindowTarget("#general")
		dispatch := sess.BeginModelDispatch(ctx, mustWindowGuard(t, client, target), target, domain.ModelDispatchStarted{
			Source: domain.ClientSource(botty.ID(), "botty"), At: fixedTime,
		})
		resp, err := sess.Handle(ctx, client, protocol.Part{Channel: "#general"})
		require.NoError(t, err)
		require.Equal(t, protocol.Response{}, resp)
		dispatch.Done(ctx, domain.ModelDispatchDone{
			Source: domain.ClientSource(botty.ID(), "botty"), At: fixedTime,
		})
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.ModelDispatchStarted{
				Source: domain.ClientSource(botty.ID(), "botty"), At: fixedTime,
			},
			domain.Part{
				Source: domain.ClientSource(botty.ID(), "botty"),
				Target: "#general", At: fixedTime,
			},
			domain.ModelDispatchDone{
				Source: domain.ClientSource(botty.ID(), "botty"), At: fixedTime,
			},
		}, collectEmittedEvents(t, sess))
	})
}

func TestSession_dispatch_start_rejects_a_guard_from_an_earlier_membership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()
		require.NoError(t, userJoin(ctx, t, sess, "#general"))

		botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#general", ""))
		target := protocol.ChannelWindowTarget("#general")
		guard := mustWindowGuard(t, client, target)
		collectEmittedEvents(t, sess)
		drainDeliveries(client)

		partResponse, partErr := sess.Handle(ctx, client, protocol.Part{Channel: "#general"})
		joinResponse, joinErr := sess.Handle(ctx, client, protocol.Join{
			Channels: []domain.ChannelName{"#general"},
		})
		synctest.Wait()
		collectEmittedEvents(t, sess)
		drainDeliveries(client)

		dispatch := sess.BeginModelDispatch(ctx, guard, target, domain.ModelDispatchStarted{
			Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
		})
		dispatch.Done(ctx, domain.ModelDispatchDone{
			Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
		})
		synctest.Wait()

		type assertionSnapshot struct {
			PartResponse protocol.Response
			PartError    error
			JoinResponse protocol.Response
			JoinError    error
			UserEvents   []domain.Event
			ActorEvents  []domain.Event
		}

		require.Equal(t, assertionSnapshot{
			JoinResponse: protocol.Response{Events: []protocol.Event{
				domain.JoinedChannel{Channel: "#general"},
			}},
		}, assertionSnapshot{
			PartResponse: partResponse,
			PartError:    partErr,
			JoinResponse: joinResponse,
			JoinError:    joinErr,
			UserEvents:   collectEmittedEvents(t, sess),
			ActorEvents:  drainDeliveries(client),
		})
	})
}

func TestSession_dispatch_lifecycle_is_scoped_to_its_channel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()
		require.NoError(t, userJoin(ctx, t, sess, "#public"))

		botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#public", ""))
		require.NoError(t, joinAs(ctx, sess, botty, "#private", ""))
		collectEmittedEvents(t, sess)

		dispatch := sess.BeginModelDispatch(
			ctx,
			mustWindowGuard(t, client, protocol.ChannelWindowTarget("#private")),
			protocol.ChannelWindowTarget("#private"),
			domain.ModelDispatchStarted{
				Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
			},
		)
		dispatch.Done(ctx, domain.ModelDispatchDone{
			Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
		})
		synctest.Wait()

		require.Equal(t, []domain.Event(nil), collectEmittedEvents(t, sess))
	})
}

func TestSession_dispatch_lifecycle_does_not_reveal_a_direct_message(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()
		require.NoError(t, userJoin(ctx, t, sess, "#public"))

		botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
		require.NoError(t, joinAs(ctx, sess, botty, "#public", ""))
		alice, _ := seedPassiveInstance(t, sess, "alice", "test/peer")
		collectEmittedEvents(t, sess)

		dispatch := sess.BeginModelDispatch(
			ctx,
			mustWindowGuard(t, client, protocol.DirectWindowTarget(alice.ID())),
			protocol.DirectWindowTarget(alice.ID()),
			domain.ModelDispatchStarted{
				Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
			},
		)
		dispatch.Done(ctx, domain.ModelDispatchDone{
			Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
		})
		synctest.Wait()

		require.Equal(t, []domain.Event(nil), collectEmittedEvents(t, sess))
	})
}

func TestSession_dispatch_lifecycle_uses_each_recipient_direct_window(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()
		botty, botClient := seedPassiveInstance(t, sess, "botty", "test/model")
		collectEmittedEvents(t, sess)
		drainDeliveries(botClient)

		dispatch := sess.BeginModelDispatch(
			ctx,
			mustWindowGuard(t, botClient, protocol.DirectWindowTarget(protocol.UserClientID)),
			protocol.DirectWindowTarget(protocol.UserClientID),
			domain.ModelDispatchStarted{
				Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
			},
		)
		dispatch.Done(ctx, domain.ModelDispatchDone{
			Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
		})
		synctest.Wait()

		require.Equal(t, []protocol.Delivery{
			{
				Event: domain.ModelDispatchStarted{
					Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
				},
				Window: protocol.DirectWindowTarget(botty.ID()),
			},
			{
				Event: domain.ModelDispatchDone{
					Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
				},
				Window: protocol.DirectWindowTarget(botty.ID()),
			},
		}, collectProtocolDeliveries(userClient(t, sess)))
		require.Equal(t, []protocol.Delivery{
			{
				Event: domain.ModelDispatchStarted{
					Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
				},
				Window: protocol.DirectWindowTarget(protocol.UserClientID),
			},
			{
				Event: domain.ModelDispatchDone{
					Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
				},
				Window: protocol.DirectWindowTarget(protocol.UserClientID),
			},
		}, collectProtocolDeliveries(botClient))
	})
}

func collectProtocolDeliveries(client protocol.Client) []protocol.Delivery {
	deliveries := []protocol.Delivery{}
	for {
		select {
		case delivery := <-client.Events():
			deliveries = append(deliveries, delivery)
		default:
			return deliveries
		}
	}
}

// dispatchLifecycleEvents keeps the per-turn lifecycle events from a
// drained bus, dropping the bootstrap and channel traffic around
// them.
func dispatchLifecycleEvents(events []domain.Event) []domain.Event {
	var kept []domain.Event

	for _, e := range events {
		switch e.(type) {
		case domain.ModelDispatchStarted, domain.ModelDispatchDone, domain.ModelUnavailableError:
			kept = append(kept, e)
		}
	}

	return kept
}

// TestSession_Quit_tears_down_the_same_way_for_every_actor pins the
// one QUIT. Whoever sent it, the channels the actor was on are told,
// its membership is dropped from each of them, and a channel the
// departure leaves empty is destroyed like a last PART (RFC 2811
// §2). The user is not exempt from any of it: the only thing its
// QUIT does differently is that the subscription survives, because
// the process hosting the server is the connection under it.
func TestSession_Quit_tears_down_the_same_way_for_every_actor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
		require.NoError(t, userJoin(ctx, t, sess, "#solo"))

		collectEmittedEvents(t, sess)

		user := userInstance(t, sess)
		require.NoError(t, userQuitViaWire(ctx, t, sess, "goodnight"))
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.Quit{Source: domain.ClientSource(user.ID(), domain.Nick("testuser")), Message: "goodnight",
				At: fixedTime,
			},
			domain.ConnectionError{Reason: "Connection closed", At: fixedTime},
		}, collectEmittedEvents(t, sess),
			"the departing client is on the channels the QUIT reaches, so the "+
				"membership filter carries it its own QUIT")

		general, err := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		require.False(t, general.Members.HasInstance(user),
			"a channel with another occupant survives, without the client that left")

		_, err = sess.loadChannelWindow(ctx, "#solo")
		require.ErrorIs(t, err, storemod.ErrNoSuchChannel,
			"the channel the departure emptied is destroyed")

		requireChannels(t, user.Channels())
	})
}

func TestSession_user_QUIT_revokes_connection_authority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, store := newTestSession(t)
		ctx := t.Context()
		user := userClient(t, sess)
		sub := sess.lookupClientHandle(protocol.UserClientID)
		require.NotNil(t, sub)

		botty := seedInstanceRow(t, store, instanceSpec{
			Nick: "botty", ModelID: "test/model",
		})
		botClient := attachModelClient(t, sess, botty)
		guard, err := sub.GuardWindow(ctx, protocol.DirectWindowTarget(botty.ID()))
		require.NoError(t, err)
		collectEmittedEvents(t, sess)

		resp, err := user.Send(ctx, protocol.Quit{Reason: "goodnight"})
		require.NoError(t, err)
		require.NoError(t, resp.Err)
		synctest.Wait()

		require.False(t, sess.ClientConnected(protocol.UserClientID))
		require.False(t, guard.Valid(ctx))
		terminalEvents := collectEmittedEvents(t, sess)

		failure := domain.ModelUnavailableError{
			Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
		}
		sess.EmitModelFailure(ctx, protocol.DirectWindowTarget(protocol.UserClientID), failure)
		synctest.Wait()
		disconnectedEvents := collectEmittedEvents(t, sess)

		_, err = user.Send(ctx, protocol.Join{Channels: []domain.ChannelName{"#afterlife"}})
		require.ErrorIs(t, err, ErrClientNotConnected)

		_, err = sub.Replies(ctx, nil, 10)
		require.ErrorIs(t, err, protocol.ErrSubscriptionClosed)

		resp, err = botClient.Send(ctx, protocol.PrivMsg{
			Target: protocol.ClientTarget(protocol.UserClientID), Body: "are you there?",
		})
		require.NoError(t, err)
		require.Equal(t, protocol.Response{Err: domain.UnknownNickError{
			At: fixedTime,
		}}, resp)

		require.NoError(t, sess.Connect(ctx))
		synctest.Wait()
		require.True(t, sess.ClientConnected(protocol.UserClientID))
		require.False(t, guard.Valid(ctx), "reconnect must not revive old window authority")
		reconnectedEvents := collectEmittedEvents(t, sess)

		resp, err = user.Send(ctx, protocol.Join{Channels: []domain.ChannelName{"#new-connection"}})
		require.NoError(t, err)
		require.NoError(t, resp.Err)

		type assertionSnapshot struct {
			Terminal     []domain.Event
			Disconnected []domain.Event
			Reconnected  []domain.Event
		}

		require.Equal(t, assertionSnapshot{
			Terminal: []domain.Event{
				domain.Quit{
					Source:  domain.ClientSource(protocol.UserClientID, "testuser"),
					Message: "goodnight", At: fixedTime,
				},
				domain.ConnectionError{Reason: "Connection closed", At: fixedTime},
			},
			Reconnected: []domain.Event{
				domain.Welcome{ServerName: domain.StatusServerName, Nick: "testuser", At: fixedTime},
			},
		}, assertionSnapshot{
			Terminal:     terminalEvents,
			Disconnected: disconnectedEvents,
			Reconnected:  reconnectedEvents,
		})
	})
}

func TestSession_dispatch_done_does_not_cross_a_user_reconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()
		user := userClient(t, sess)
		botty, botClient := seedPassiveInstance(t, sess, "botty", "test/model")
		collectEmittedEvents(t, sess)
		drainDeliveries(botClient)

		target := protocol.DirectWindowTarget(protocol.UserClientID)
		dispatch := sess.BeginModelDispatch(
			ctx,
			mustWindowGuard(t, botClient, target),
			target,
			domain.ModelDispatchStarted{
				Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
			},
		)
		synctest.Wait()
		started := collectEmittedEvents(t, sess)

		response, err := user.Send(ctx, protocol.Quit{Reason: "reconnecting"})
		require.NoError(t, err)
		require.Equal(t, protocol.Response{}, response)
		synctest.Wait()
		terminal := collectEmittedEvents(t, sess)

		dispatch.Done(ctx, domain.ModelDispatchDone{
			Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
		})
		synctest.Wait()
		require.NoError(t, sess.Connect(ctx))
		synctest.Wait()
		reconnected := collectEmittedEvents(t, sess)

		type assertionSnapshot struct {
			Started     []domain.Event
			Terminal    []domain.Event
			Reconnected []domain.Event
		}

		require.Equal(t, assertionSnapshot{
			Started: []domain.Event{domain.ModelDispatchStarted{
				Source: domain.ClientSource(botty.ID(), botty.Nick()), At: fixedTime,
			}},
			Terminal: []domain.Event{
				domain.Quit{
					Source:  domain.ClientSource(protocol.UserClientID, "testuser"),
					Message: "reconnecting", At: fixedTime,
				},
				domain.ConnectionError{Reason: "Connection closed", At: fixedTime},
			},
			Reconnected: []domain.Event{
				domain.Welcome{
					ServerName: domain.StatusServerName, Nick: "testuser", At: fixedTime,
				},
			},
		}, assertionSnapshot{
			Started: started, Terminal: terminal, Reconnected: reconnected,
		})
	})
}

// TestSession_Kill_can_name_the_issuing_client covers KILL against
// the client that sent it. RFC 2812 §3.7.1 describes a command that
// names a nick; nothing in it exempts the operator's own. The
// target receives KILL, then the ordinary QUIT and terminal ERROR.
// Membership goes, and a channel the departure empties is destroyed.
func TestSession_Kill_can_name_the_issuing_client(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#general"))
		collectEmittedEvents(t, sess)

		user := userInstance(t, sess)
		resp, err := userClient(t, sess).Send(ctx, protocol.Kill{Nick: "testuser", Reason: "enough"})
		require.NoError(t, err)
		require.NoError(t, resp.Err)
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.KillNotice{
				Source:  domain.ClientSource(user.ID(), "testuser"),
				Subject: "testuser", Reason: "enough", At: fixedTime,
			},
			domain.Quit{Source: domain.ClientSource(user.ID(), domain.Nick("testuser")), Message: "Killed by testuser (enough)",
				At: fixedTime,
			},
			domain.ConnectionError{Reason: "Killed by testuser (enough)", At: fixedTime},
		}, collectEmittedEvents(t, sess))

		_, err = sess.loadChannelWindow(ctx, "#general")
		require.ErrorIs(t, err, storemod.ErrNoSuchChannel)

		requireChannels(t, user.Channels())
	})
}

func TestSession_committed_self_kill_reports_connection_error_after_cleanup_failure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backing := storetest.NewMemoryStore(t)
		failing := &teardownFailureStore{
			Store:           backing,
			clearSessionErr: errors.New("clear session failed"),
		}
		sess := New(t.Context(), failing, newTestModelClientFactory(t, &apitest.Fake{}), nil)
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }

		seedInstance(t, sess, backing, instanceSpec{
			Nick: "botty", ModelID: "test/model", Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, backing, "#general", "testuser", "botty")
		collectEmittedEvents(t, sess)
		failing.armed.Store(true)

		resp, err := userClient(t, sess).Send(t.Context(), protocol.Kill{
			Nick: "testuser", Reason: "enough",
		})
		require.ErrorIs(t, err, failing.clearSessionErr)
		require.Equal(t, protocol.Response{}, resp)
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.KillNotice{
				Source:  domain.ClientSource("", "testuser"),
				Subject: "testuser", Reason: "enough", At: fixedTime,
			},
			domain.Quit{
				Source:  domain.ClientSource("", "testuser"),
				Message: "Killed by testuser (enough)", At: fixedTime,
			},
			domain.ConnectionError{Reason: "Killed by testuser (enough)", At: fixedTime},
		}, collectEmittedEvents(t, sess))
	})
}

// TestSession_Quit_reaches_a_client_the_broadcast_cannot pins the
// point-to-point fallback. RFC 2812 §3.7.1 has a killed client told
// it was killed, and the broadcast is usually how it hears; two
// cases leave it with no channel the QUIT can arrive through, and
// both fall back to a direct delivery. This is the same fallback
// `changeNickAs` makes for NICK, and for the same reason.
func TestSession_Quit_reaches_a_client_the_broadcast_cannot(t *testing.T) {
	tests := []struct {
		name  string
		setUp func(t *testing.T, sess *Session, ctx context.Context)
	}{
		{
			name:  "on no channels at all",
			setUp: func(*testing.T, *Session, context.Context) {},
		},
		{
			// RFC 2811 §4.2.1 withholds a QUIT from a `+a` channel and
			// puts a masked PART there instead, so a client whose
			// channels all carry `+a` is named nowhere the broadcast
			// reaches.
			name: "on anonymous channels only",
			setUp: func(t *testing.T, sess *Session, ctx context.Context) {
				t.Helper()

				require.NoError(t, userJoin(ctx, t, sess, "#hidden"))

				resp, err := userClient(t, sess).Send(ctx, protocol.ChannelMode{
					Channel: "#hidden",
					Changes: []protocol.ChannelModeChange{{Flag: domain.ModeAnonymous, Add: true}},
				})
				require.NoError(t, err)
				require.NoError(t, resp.Err)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sess, _ := newTestSession(t)
				ctx := t.Context()

				tc.setUp(t, sess, ctx)
				collectEmittedEvents(t, sess)

				resp, err := userClient(t, sess).Send(ctx, protocol.Kill{Nick: "testuser", Reason: "enough"})
				require.NoError(t, err)
				require.NoError(t, resp.Err)
				synctest.Wait()

				want := []domain.Event{
					domain.KillNotice{Source: domain.ClientSource(protocol.UserClientID, "testuser"), Subject: "testuser", Reason: "enough", At: fixedTime},
				}
				if tc.name == "on anonymous channels only" {
					want = append(want, domain.Part{Source: domain.AnonymousSource(), Target: "#hidden", Message: "Killed by testuser (enough)", At: fixedTime})
				}
				want = append(want,
					domain.Quit{Source: domain.ClientSource(protocol.UserClientID, "testuser"), Message: "Killed by testuser (enough)", At: fixedTime},
					domain.ConnectionError{Reason: "Killed by testuser (enough)", At: fixedTime},
				)
				require.Equal(t, want, collectEmittedEvents(t, sess))
			})
		})
	}
}

// TestSession_commands_naming_the_issuing_client answers the question
// this programme is about: does a command mean the same thing when it
// names the client that sent it? Every one of these reads the same
// registry and the same channel records for the issuer as for anybody
// else, so the answers are the RFC's, not a special case.
func TestSession_commands_naming_the_issuing_client(t *testing.T) {
	tests := []struct {
		name   string
		run    func(t *testing.T, sess *Session, ctx context.Context) (protocol.Response, error)
		assert func(t *testing.T, sess *Session, resp protocol.Response)
	}{
		{
			name: "WHOIS answers with the issuer's own snapshot",
			run: func(t *testing.T, sess *Session, ctx context.Context) (protocol.Response, error) {
				t.Helper()

				return userClient(t, sess).Send(ctx, protocol.Whois{Nick: "testuser", Window: protocol.ChannelWindowTarget("#general")})
			},
			assert: func(t *testing.T, _ *Session, resp protocol.Response) {
				t.Helper()

				require.NoError(t, resp.Err)
				require.Equal(t, []protocol.Event{domain.Whois{
					Nick:     "testuser",
					Channels: []domain.ChannelName{"#general"},
					At:       fixedTime,
				}}, resp.Events)
			},
		},
		{
			name: "INVITE of a nick already on the channel is refused with 443",
			run: func(t *testing.T, sess *Session, ctx context.Context) (protocol.Response, error) {
				t.Helper()

				return userClient(t, sess).Send(ctx, protocol.Invite{Nick: "testuser", Channel: "#general"})
			},
			assert: func(t *testing.T, _ *Session, resp protocol.Response) {
				t.Helper()

				require.Equal(t,
					domain.UserOnChannelError{Nick: "testuser", Channel: "#general", At: fixedTime},
					resp.Err)
			},
		},
		{
			name: "MODE +v names the issuer like any other member",
			run: func(t *testing.T, sess *Session, ctx context.Context) (protocol.Response, error) {
				t.Helper()

				return userClient(t, sess).Send(ctx, protocol.ChannelMode{
					Channel: "#general",
					Changes: []protocol.ChannelModeChange{
						{Flag: domain.ModeChannelVoice, Add: true, Target: "testuser"},
					},
				})
			},
			assert: func(t *testing.T, sess *Session, resp protocol.Response) {
				t.Helper()

				require.NoError(t, resp.Err)

				window, err := sess.loadChannelWindow(t.Context(), "#general")
				require.NoError(t, err)

				member, ok := window.Members.GetByNick("testuser")
				require.True(t, ok)
				require.Equal(t, domain.MemberModes{Operator: true, Voice: true}, member.Modes)
			},
		},
		{
			name: "KICK takes the issuer off the channel",
			run: func(t *testing.T, sess *Session, ctx context.Context) (protocol.Response, error) {
				t.Helper()

				return userClient(t, sess).Send(ctx, protocol.Kick{Channel: "#general", Nick: "testuser"})
			},
			assert: func(t *testing.T, sess *Session, resp protocol.Response) {
				t.Helper()

				require.NoError(t, resp.Err)
				requireChannels(t, userInstance(t, sess).Channels())
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sess, _ := newTestSession(t)
				ctx := t.Context()

				require.NoError(t, userJoin(ctx, t, sess, "#general"))
				collectEmittedEvents(t, sess)

				resp, err := tc.run(t, sess, ctx)
				require.NoError(t, err)

				tc.assert(t, sess, resp)
			})
		})
	}
}

// TestSession_the_issuing_client_has_a_connection_record pins the row
// itself. It is written when the client registers, the session
// maintains it exactly as it maintains a model's, and the QUIT
// teardown deletes it. A join stamps the channel onto it, a NICK
// rewrites the nick, and deleting the row is what frees the nick.
func TestSession_the_issuing_client_has_a_connection_record(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#general"))

		row, err := s.GetInstanceByID(ctx, "")
		require.NoError(t, err)
		require.Same(t, userInstance(t, sess), row)
		require.Equal(t, comparableInstance{
			Nick:     "testuser",
			Channels: []channelEntry{{Name: "#general", JoinedAt: fixedTime}},
		}, normaliseInstance(row))

		require.NoError(t, userChangeNick(ctx, t, sess, "renamed"))

		resolved, err := s.ResolveNick(ctx, "renamed")
		require.NoError(t, err)
		require.Same(t, userInstance(t, sess), resolved)

		require.NoError(t, userQuitViaWire(ctx, t, sess, "bye"))

		require.Equal(t, []domain.InstanceID{}, instanceIDs(t, s),
			"the QUIT teardown deletes the connection record, which frees the nick")
	})
}

// TestSession_a_guarded_quit_disconnects_an_overflowed_peer separates
// one client's window authority from another client's teardown. A
// model ending its own connection sends QUIT under the window guard
// its turn holds, and `quitAs` invalidates that guard before the QUIT
// is broadcast. The broadcast is what pushes a peer that has stopped
// reading past its send-queue allowance, and the peer's disconnect is
// the server acting on the peer's own connection. The departing
// model's authority therefore has no say in whether it runs.
//
// A peer that keeps its subscription here is unreachable from then
// on: `beginTermination` has sealed its outbound queue and
// `claimServerDisconnect` has latched, so every later delivery is
// dropped and no second disconnect can be attempted.
func TestSession_a_guarded_quit_disconnects_an_overflowed_peer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		botty := seedInstanceRow(t, s, instanceSpec{
			Nick: "botty", ModelID: "test/model", Channels: testChannels("#general"),
		})
		peer := seedInstanceRow(t, s, instanceSpec{
			Nick: "peer", ModelID: "test/model", Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty", "peer")

		bottyClient := attachBareClient(t, sess, botty)
		peerClient := &passiveClient{id: protocol.ClientID(peer.ID())}
		peerSub, err := subscribeTestClient(ctx, t, sess, peerClient, protocol.SubscribeOptions{})
		require.NoError(t, err)
		peerClient.sub = peerSub

		guard, err := bottyClient.sub.GuardWindow(ctx, protocol.ChannelWindowTarget("#general"))
		require.NoError(t, err)

		peerHandle := sess.lookupClientHandle(protocol.ClientID(peer.ID()))
		require.NotNil(t, peerHandle)
		for range eventBufSize + sendQAllowance {
			peerHandle.enqueue(ctx, protocol.Delivery{Event: domain.SystemNotice{
				Text: "flood", At: fixedTime,
			}})
		}
		synctest.Wait()

		resp, err := guard.Send(ctx, bottyClient, protocol.Quit{Reason: "bye"})
		require.NoError(t, err)
		require.NoError(t, resp.Err)

		synctest.Wait()

		window, windowErr := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, windowErr)

		type assertionSnapshot struct {
			PeerConnected bool
			PeerInChannel bool
			Events        []domain.Event
		}

		require.Equal(t, assertionSnapshot{
			Events: []domain.Event{
				bootstrapModeChange(t, sess, bootAt),
				domain.Quit{
					Source:  domain.ClientSource(botty.ID(), "botty"),
					Message: "bye", At: fixedTime,
				},
				domain.Quit{
					Source:  domain.ClientSource(peer.ID(), "peer"),
					Message: sendQExceededReason, At: fixedTime,
				},
			},
		}, assertionSnapshot{
			PeerConnected: sess.ClientConnected(protocol.ClientID(peer.ID())),
			PeerInChannel: window.Members.HasInstance(peer),
			Events:        collectEmittedEvents(t, sess),
		})
	})
}
