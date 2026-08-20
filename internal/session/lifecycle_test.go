package session

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
)

// quitToolCall builds a [api.CompletionResult] whose PendingToolCalls
// invoke the `quit` tool with the given farewell. A model that runs
// this ends its own connection from inside its dispatch turn.
func quitToolCall(t testing.TB, message string) api.CompletionResult {
	t.Helper()

	args, err := json.Marshal(map[string]any{"message": []string{message}})
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
			SendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
				return quitToolCall(t, "signing off"), nil
			},
			ContinueWithToolResultsFn: func(context.Context, *api.Conversation, []api.ToolResult) (api.CompletionResult, error) {
				continues++
				return api.CompletionResult{}, nil
			},
		}

		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

		dispatchUserMessage(ctx, t, sess, "#general", "still there?")

		require.Contains(t, collectEmittedEvents(t, sess), domain.Event(domain.Quit{
			Nick:       "botty",
			InstanceID: testMemberID("botty"),
			Message:    "signing off",
			At:         fixedTime,
			Instance:   botty,
		}))

		require.Equal(t, 0, continues, "quit ends the turn: there is no client left to ask for another round")

		window, err := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		require.False(t, window.Members.HasInstance(botty))

		require.Nil(t, sess.LookupClient(protocol.ClientID(botty.ID())))
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
		fake := &apitest.Fake{
			SendEventsFn: func(ctx context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
				<-ctx.Done()
				return api.CompletionResult{}, ctx.Err()
			},
		}

		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

		dispatchUserMessage(ctx, t, sess, "#general", "are you there?")

		sub := sess.lookupClientHandle(protocol.ClientID(botty.ID()))
		require.NotNil(t, sub)

		// Fill the queue past its allowance. The channel buffer takes
		// the head of the flood before the queue starts holding, so
		// the trip point is one past both.
		for range eventBufSize + sendQAllowance + 1 {
			sub.enqueue(protocol.Delivery{Event: domain.Message{
				Target: "#general",
				From:   "testuser",
				Body:   "flood",
				At:     fixedTime,
			}})
		}

		synctest.Wait()

		require.Contains(t, collectEmittedEvents(t, sess), domain.Event(domain.Quit{
			Nick:       "botty",
			InstanceID: testMemberID("botty"),
			Message:    sendQExceededReason,
			At:         fixedTime,
			Instance:   botty,
		}))

		require.Nil(t, sess.LookupClient(protocol.ClientID(botty.ID())))

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
			sub.enqueue(protocol.Delivery{Event: domain.Message{
				Target: "#general",
				From:   "testuser",
				Body:   "flood",
				At:     fixedTime,
			}})
		}

		synctest.Wait()

		// Nothing was skipped: what the channel buffer could not take
		// is all still queued.
		require.Equal(t, flood-eventBufSize, queuedDeliveries(sub))

		require.NotNil(t, sess.LookupClient(protocol.UserClientID))
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

// TestSession_dispatch_started_and_done_are_paired covers the
// bracket a consumer scopes its "thinking" indicator to. Started and
// Done come as a pair for every turn, including one that fails
// before it can even resolve its window: a consumer that lowers on
// Done must never see one it has no Started for, and one that raises
// on Started must always get the Done that lowers it again.
func TestSession_dispatch_started_and_done_are_paired(t *testing.T) {
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
		registerUserMembership(t, sess, "#ghost", []domain.Nick{userNick(t, sess)})

		sess.Emit(ctx, domain.PokeEvent{Channel: "#ghost", At: fixedTime})
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.ModelDispatchStarted{Instance: botty, At: fixedTime},
			domain.ModelUnavailableError{Channel: "#ghost", Nick: "botty", At: fixedTime},
			domain.ModelDispatchDone{Instance: botty, At: fixedTime},
		}, dispatchLifecycleEvents(collectEmittedEvents(t, sess)))
	})
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
			domain.Quit{
				Nick:       "testuser",
				InstanceID: user.ID(),
				Message:    "goodnight",
				At:         fixedTime,
				Instance:   user,
			},
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

// TestSession_Kill_can_name_the_issuing_client covers KILL against
// the client that sent it. RFC 2812 §3.7.1 describes a command that
// names a nick; nothing in it exempts the operator's own. The
// teardown is the ordinary one: the QUIT carries the kill reason,
// membership goes, and a channel the departure empties is destroyed.
// The only thing the server does not do afterwards is close a
// connection it does not have.
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
			domain.Quit{
				Nick:       "testuser",
				InstanceID: user.ID(),
				Message:    "Killed by testuser (enough)",
				At:         fixedTime,
				Instance:   user,
			},
		}, collectEmittedEvents(t, sess))

		_, err = sess.loadChannelWindow(ctx, "#general")
		require.ErrorIs(t, err, storemod.ErrNoSuchChannel)

		requireChannels(t, user.Channels())
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

				user := userInstance(t, sess)
				resp, err := userClient(t, sess).Send(ctx, protocol.Kill{Nick: "testuser", Reason: "enough"})
				require.NoError(t, err)
				require.NoError(t, resp.Err)
				synctest.Wait()

				require.Contains(t, collectEmittedEvents(t, sess), domain.Quit{
					Nick:       "testuser",
					InstanceID: user.ID(),
					Message:    "Killed by testuser (enough)",
					At:         fixedTime,
					Instance:   user,
				})
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

				return userClient(t, sess).Send(ctx, protocol.Whois{Nick: "testuser", Channel: "#general"})
			},
			assert: func(t *testing.T, _ *Session, resp protocol.Response) {
				t.Helper()

				require.NoError(t, resp.Err)
				require.Equal(t, []protocol.Event{domain.Whois{
					Target:   "#general",
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
