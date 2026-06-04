package session

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// channelsAt builds a one-channel membership map with an explicit
// join time, for tests that need a model whose join post-dates some
// of the channel's history.
func channelsAt(ch domain.ChannelName, joinedAt time.Time) *orderedmap.OrderedMap[domain.ChannelName, time.Time] {
	m := orderedmap.New[domain.ChannelName, time.Time]()
	m.Set(ch, joinedAt)
	return m
}

// capturedHistory captures each dispatch turn's prompt history under a
// mutex so the assertion runs after synctest has drained the loop.
type capturedHistory struct {
	mu      sync.Mutex
	history [][]protocol.IRCMessage
}

func (c *capturedHistory) record(history []protocol.IRCMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.history = append(c.history, append([]protocol.IRCMessage(nil), history...))
}

func (c *capturedHistory) snapshot() [][]protocol.IRCMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.history
}

// TestModelClient_load_is_join_scoped proves a model attached to a
// channel that had traffic BEFORE it joined sees only post-join
// events in its prompt transcript. Events older than the instance's
// join time are dropped at load.
func TestModelClient_load_is_join_scoped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var captured capturedHistory
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, history []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
				captured.record(history)
				return api.CompletionResult{}, nil
			},
		}

		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		joinedAt := fixedTime
		beforeJoin := joinedAt.Add(-time.Hour)

		// A message that predates the model's join. It is in the
		// channel log but must not reach the model's prompt.
		_, err := s.AppendEvent(ctx, "#room", domain.Message{
			Target: "#room",
			From:   "early-bird",
			Body:   "before you joined",
			At:     beforeJoin,
		})
		require.NoError(t, err)

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: channelsAt("#room", joinedAt),
		})
		seedChannelWithMembers(t, sess, s, "#room", userNick(t, sess), botty.Nick())

		_, err = userSendMessage(ctx, t, sess, "#room", "hello bot")
		require.NoError(t, err)

		synctest.Wait()

		require.Equal(t, [][]protocol.IRCMessage{nil}, captured.snapshot(),
			"the pre-join message must not appear in the prompt history; "+
				"the load is join-scoped")
	})
}

// TestModelClient_load_fails_closed_on_zero_join proves a channel with
// a zero/unknown join time loads nothing: with no known join boundary
// the load surfaces no history at all, never the whole channel.
func TestModelClient_load_fails_closed_on_zero_join(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var captured capturedHistory
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, history []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
				captured.record(history)
				return api.CompletionResult{}, nil
			},
		}

		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		_, err := s.AppendEvent(ctx, "#room", domain.Message{
			Target: "#room",
			From:   "early-bird",
			Body:   "some history",
			At:     fixedTime.Add(-time.Hour),
		})
		require.NoError(t, err)

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: channelsAt("#room", time.Time{}),
		})
		seedChannelWithMembers(t, sess, s, "#room", userNick(t, sess), botty.Nick())

		_, err = userSendMessage(ctx, t, sess, "#room", "hello bot")
		require.NoError(t, err)

		synctest.Wait()

		require.Equal(t, [][]protocol.IRCMessage{nil}, captured.snapshot(),
			"a zero join time loads nothing: the load fails closed")
	})
}

// TestModelClient_private_replies_converge_on_local_ring proves a
// model's own `/whois` reply re-appears in its prompt transcript on a
// later dispatch, sourced from the local replies ring, not a per-turn
// store read. The first turn drives the whois (so it is filed locally
// by Send); the second turn must surface it.
func TestModelClient_private_replies_converge_on_local_ring(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var (
			captured capturedHistory
			turn     int
			turnMu   sync.Mutex
		)

		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, history []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				captured.record(history)

				turnMu.Lock()
				n := turn
				turn++
				turnMu.Unlock()

				if n == 0 {
					return whoisToolCall(t, "target"), nil
				}
				return msgToolCalls(t, domain.ChannelName(events[0].Target), "ok"), nil
			},
		}

		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedInstance(t, sess, s, instanceSpec{Nick: "target", ModelID: "test/model"})

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", userNick(t, sess), botty.Nick())

		// Turn 1: drive botty's whois of "target".
		_, err := userSendMessage(ctx, t, sess, "#general", "look up target")
		require.NoError(t, err)
		synctest.Wait()

		// Turn 2: a later message. botty's earlier whois reply must
		// re-appear, sourced from its local replies ring.
		_, err = userSendMessage(ctx, t, sess, "#general", "anything else?")
		require.NoError(t, err)
		synctest.Wait()

		snapshots := captured.snapshot()
		require.NotEmpty(t, snapshots, "the model dispatched at least once")

		var latestReplies []string
		for _, m := range snapshots[len(snapshots)-1] {
			if m.Kind == protocol.KindServerReply {
				latestReplies = append(latestReplies, m.Body)
			}
		}

		require.Contains(t, latestReplies, "whois target: test/model",
			"botty's own whois reply must re-appear in the latest dispatch's "+
				"prompt, sourced from the local replies ring")
	})
}
