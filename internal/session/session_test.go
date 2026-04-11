package session

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	chromem "github.com/philippgille/chromem-go"
	"github.com/stretchr/testify/require"
	orderedmap "github.com/wk8/go-ordered-map/v2"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/observability/oteltest"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

var fixedTime = time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

// testChannels builds an ordered map of channel names to a fixed
// join time for use in test instance construction.
func testChannels(names ...domain.ChannelName) *orderedmap.OrderedMap[domain.ChannelName, time.Time] {
	m := orderedmap.New[domain.ChannelName, time.Time]()
	for _, n := range names {
		m.Set(n, fixedTime)
	}

	return m
}

// requireChannels asserts that the given ordered map contains exactly
// the expected channel names, in order.
func requireChannels(t *testing.T, channels *orderedmap.OrderedMap[domain.ChannelName, time.Time], expected ...domain.ChannelName) {
	t.Helper()

	var got []domain.ChannelName
	for pair := channels.Oldest(); pair != nil; pair = pair.Next() {
		got = append(got, pair.Key)
	}

	require.Equal(t, []domain.ChannelName(expected), got)
}

func drainEvent[T domain.SessionEvent](t *testing.T, sess *Session) T {
	t.Helper()

	select {
	case evt := <-sess.Events():
		got, ok := evt.(T)
		require.True(t, ok, "expected %T, got %T", *new(T), evt)
		return got
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %T", *new(T))
		return *new(T)
	}
}

func testMembers(nicks ...domain.Nick) domain.MemberList {
	ml := domain.NewMemberList()
	for _, nick := range nicks {
		ml.Add(nick)
		if nick == "testuser" {
			ml.SetMode(nick, domain.ModeOp)
		} else {
			ml.SetMode(nick, domain.ModeVoice)
		}
	}
	return ml
}

func requireChannelEqual(t *testing.T, expected, actual domain.Channel) {
	t.Helper()
	require.Equal(t, expected.Name, actual.Name, "channel name")
	require.Equal(t, expected.Kind, actual.Kind, "channel kind")
	require.Equal(t, expected.Topic, actual.Topic, "channel topic")
	require.Equal(t, expected.TopicSetBy, actual.TopicSetBy, "channel topic set by")
	require.Equal(t, expected.TopicSetAt, actual.TopicSetAt, "channel topic set at")
	require.Equal(t, expected.Members.Slice(), actual.Members.Slice(), "channel members")
	require.Equal(t, expected.Created, actual.Created, "channel created")
}

func newTestSession(t *testing.T) (*Session, *storemod.SQLiteStore) {
	t.Helper()

	return newTestSessionWithAPI(t, &fakeAPIClient{})
}

func newTestSessionWithAPI(t *testing.T, apiClient api.Client) (*Session, *storemod.SQLiteStore) {
	t.Helper()

	s := storetest.NewMemoryStore(t)

	sess := New(s, nil, apiClient, "testuser", "", "")
	sess.now = func() time.Time { return fixedTime }

	return sess, s
}

func TestSession_Join(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.Join(ctx, "#general"))
	evt := drainEvent[domain.JoinEvent](t, sess)
	require.Equal(t, domain.JoinEvent{
		Channel: "#general",
		Nick:    "testuser",
		Created: true,
		At:      fixedTime,
	}, evt)

	// Channel should be persisted.
	ch, err := s.GetChannel(ctx, "#general")
	require.NoError(t, err)
	requireChannelEqual(t, domain.Channel{
		Name:    "#general",
		Kind:    domain.KindChannel,
		Members: testMembers("testuser"),
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
		Members: testMembers("testuser"),
		Created: fixedTime.Add(-time.Hour),
	}
	require.NoError(t, s.SaveChannel(ctx, existing))

	require.NoError(t, sess.Join(ctx, "#existing"))
	evt := drainEvent[domain.JoinEvent](t, sess)
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
		Members: testMembers("testuser", "botty"),
		Created: fixedTime,
	}
	require.NoError(t, s.SaveChannel(ctx, ch))

	require.NoError(t, sess.Part(ctx, "#leaving", ""))
	evt := drainEvent[domain.PartEvent](t, sess)
	require.Equal(t, domain.PartEvent{
		Channel: "#leaving",
		Nick:    "testuser",
		At:      fixedTime,
	}, evt)

	updated, err := s.GetChannel(ctx, "#leaving")
	require.NoError(t, err)
	requireChannelEqual(t, domain.Channel{
		Name:    "#leaving",
		Kind:    domain.KindChannel,
		Members: testMembers("botty"),
		Created: fixedTime,
	}, updated)
}

func TestSession_LeaveNonexistent(t *testing.T) {
	sess, _ := newTestSession(t)

	require.Error(t, sess.Part(t.Context(), "#ghost", ""))
}

func TestSession_Part_carries_message(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	ch := domain.Channel{
		Name:    "#farewell",
		Kind:    domain.KindChannel,
		Members: testMembers("testuser"),
		Created: fixedTime,
	}
	require.NoError(t, s.SaveChannel(ctx, ch))

	require.NoError(t, sess.Part(ctx, "#farewell", "see ya later"))
	evt := drainEvent[domain.PartEvent](t, sess)
	require.Equal(t, domain.PartEvent{
		Channel: "#farewell",
		Nick:    "testuser",
		Message: "see ya later",
		At:      fixedTime,
	}, evt)
}

func TestSession_model_ResponsePart_removes_from_channel(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.ModelResponse{
				Kind:            protocol.ResponsePart,
				Messages:        []protocol.ReplyPart{{Kind: protocol.ReplyMessage, Body: "goodbye friends"}},
				FarewellMessage: "off to explore",
			}, nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	require.NoError(t, sess.SendMessage(ctx, "#general", "hello"))

	// Drain MessageEvent first.
	drainEvent[domain.MessageEvent](t, sess)
	events := drainEvents(t, sess, 1)

	// PartEvent is emitted from within dispatchToInstance before
	// replies are returned, so the order is: DispatchStarted,
	// PartEvent, ModelReply, DispatchDone.
	require.Equal(t, domain.DispatchStartedEvent{
		Channel: "#general",
		Nicks:   []domain.Nick{"botty"},
	}, events[0])

	require.Equal(t, domain.PartEvent{
		Channel: "#general",
		Nick:    "botty",
		Message: "off to explore",
		At:      fixedTime,
	}, events[1])

	require.IsType(t, domain.ModelReplyEvent{}, events[2])
	reply := events[2].(domain.ModelReplyEvent)
	require.Equal(t, "goodbye friends", reply.Event.Body)

	require.Equal(t, domain.DispatchDoneEvent{Channel: "#general"}, events[3])

	// Verify model is removed from the channel.
	ch, err := s.GetChannel(ctx, "#general")
	require.NoError(t, err)
	require.False(t, ch.Members.Has("botty"))

	// Instance channels should be updated.
	inst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)

	_, ok := inst.Channels.Get("#general")
	require.False(t, ok)
}

func TestSession_model_ResponseQuit_removes_from_all_channels(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.ModelResponse{
				Kind:            protocol.ResponseQuit,
				Messages:        []protocol.ReplyPart{{Kind: protocol.ReplyMessage, Body: "farewell"}},
				FarewellMessage: "shutting down",
			}, nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedChannelWithMembers(t, s, "#random", "testuser", "botty")
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general", "#random"),
	})

	require.NoError(t, sess.SendMessage(ctx, "#general", "hello"))

	drainEvent[domain.MessageEvent](t, sess)
	events := drainEvents(t, sess, 1)

	// QuitEvent is emitted from within dispatchToInstance before
	// replies are returned: DispatchStarted, QuitEvent, ModelReply,
	// DispatchDone.
	require.Equal(t, domain.DispatchStartedEvent{
		Channel: "#general",
		Nicks:   []domain.Nick{"botty"},
	}, events[0])

	require.Equal(t, domain.QuitEvent{
		Nick:    "botty",
		Message: "shutting down",
		At:      fixedTime,
	}, events[1])

	require.IsType(t, domain.ModelReplyEvent{}, events[2])

	require.Equal(t, domain.DispatchDoneEvent{Channel: "#general"}, events[3])

	// Verify model is removed from both channels.
	for _, chName := range []domain.ChannelName{"#general", "#random"} {
		ch, err := s.GetChannel(ctx, chName)
		require.NoError(t, err)
		require.False(t, ch.Members.Has("botty"), "botty should be removed from %s", chName)
	}

	inst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)
	require.Equal(t, 0, inst.Channels.Len())
}

func TestSession_Quit_saves_pending_quit(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.Join(ctx, "#general"))
	drainNEvents(t, sess, 3) // JoinEvent + ModeChangeEvent + DispatchDoneEvent

	require.NoError(t, sess.Join(ctx, "#random"))
	drainNEvents(t, sess, 3)

	require.NoError(t, sess.Quit(ctx, "goodnight"))

	pq, err := s.GetPendingQuit(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.Nick("testuser"), pq.Nick)
	require.Equal(t, "goodnight", pq.Message)
	require.Equal(t, []domain.ChannelName{"#general", "#random"}, pq.Channels)
}

func TestSession_Quit_no_channels_is_noop(t *testing.T) {
	sess, s := newTestSession(t)

	require.NoError(t, sess.Quit(t.Context(), "bye"))

	pq, err := s.GetPendingQuit(t.Context())
	require.NoError(t, err)
	require.Nil(t, pq)
}

func TestSession_ProcessPendingQuit_dispatches_and_clears(t *testing.T) {
	var dispatched []string

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, _ domain.ModelID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (protocol.ModelResponse, error) {
			for _, ev := range events {
				if ev.Kind == protocol.KindQuit {
					dispatched = append(dispatched, ev.From)
				}
			}

			return protocol.Reply("goodbye"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	pq := domain.PendingQuit{
		Nick:     "testuser",
		Message:  "goodnight",
		At:       fixedTime,
		Channels: []domain.ChannelName{"#general"},
	}
	require.NoError(t, s.SavePendingQuit(ctx, pq))

	require.NoError(t, sess.ProcessPendingQuit(ctx))

	// Model should have been dispatched a QUIT event.
	require.Equal(t, []string{"testuser"}, dispatched)

	// Pending quit should be cleared.
	got, err := s.GetPendingQuit(ctx)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestSession_ProcessPendingQuit_multi_channel(t *testing.T) {
	var mu sync.Mutex
	var dispatchedModels []string

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (protocol.ModelResponse, error) {
			for _, ev := range events {
				if ev.Kind == protocol.KindQuit {
					mu.Lock()
					dispatchedModels = append(dispatchedModels, string(modelID))
					mu.Unlock()
				}
			}

			return protocol.Reply("bye"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "alpha", "beta")
	seedChannelWithMembers(t, s, "#random", "testuser", "gamma")

	seedInstance(t, s, domain.Instance{
		Nick:     "alpha",
		ModelID:  "test/alpha",
		Channels: testChannels("#general"),
	})
	seedInstance(t, s, domain.Instance{
		Nick:     "beta",
		ModelID:  "test/beta",
		Channels: testChannels("#general"),
	})
	seedInstance(t, s, domain.Instance{
		Nick:     "gamma",
		ModelID:  "test/gamma",
		Channels: testChannels("#random"),
	})

	pq := domain.PendingQuit{
		Nick:     "testuser",
		Message:  "goodnight all",
		At:       fixedTime,
		Channels: []domain.ChannelName{"#general", "#random"},
	}
	require.NoError(t, s.SavePendingQuit(ctx, pq))

	require.NoError(t, sess.ProcessPendingQuit(ctx))

	// Quit events should be appended to each channel.
	generalEvents := channelEventTypes(t, s, "#general")
	require.Contains(t, generalEvents, "quit")

	randomEvents := channelEventTypes(t, s, "#random")
	require.Contains(t, randomEvents, "quit")

	// All models across both channels should have been dispatched.
	mu.Lock()
	defer mu.Unlock()

	require.ElementsMatch(t, []string{"test/alpha", "test/beta", "test/gamma"}, dispatchedModels)

	// Pending quit should be cleared.
	got, err := s.GetPendingQuit(ctx)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestSession_ProcessPendingQuit_no_pending_is_noop(t *testing.T) {
	sess, _ := newTestSession(t)

	require.NoError(t, sess.ProcessPendingQuit(t.Context()))
}

func TestSession_Invite(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	ch := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Members: testMembers("testuser"),
		Created: fixedTime,
	}
	require.NoError(t, s.SaveChannel(ctx, ch))

	require.NoError(t, sess.Invite(ctx, "#dev", "anthropic/claude-3-haiku", ""))
	evt := drainEvent[domain.ModelInvitedEvent](t, sess)
	require.Equal(t, domain.ModelInvitedEvent{
		Channel: "#dev",
		Instance: domain.Instance{
			Nick:     "fakenick",
			ModelID:  "anthropic/claude-3-haiku",
			Channels: testChannels("#dev")},
		At: fixedTime,
	}, evt)

	// Instance should be persisted.
	inst, err := s.GetInstance(ctx, "fakenick")
	require.NoError(t, err)
	require.Equal(t, domain.Instance{
		Nick:     "fakenick",
		ModelID:  "anthropic/claude-3-haiku",
		Channels: testChannels("#dev")}, inst)

	// Channel should have new member.
	updated, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, testMembers("testuser", "fakenick").Slice(), updated.Members.Slice())
}

func TestSession_Kick(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	ch := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Members: testMembers("testuser", "botty"),
		Created: fixedTime,
	}
	require.NoError(t, s.SaveChannel(ctx, ch))
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#dev", "#random"),
	})

	require.NoError(t, sess.Kick(ctx, "#dev", "botty"))
	evt := drainEvent[domain.ModelKickedEvent](t, sess)
	require.Equal(t, domain.ModelKickedEvent{
		Channel: "#dev",
		Nick:    "botty",
		At:      fixedTime,
	}, evt)

	// Channel should no longer have the kicked member.
	updated, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, testMembers("testuser").Slice(), updated.Members.Slice())

	inst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)
	requireChannels(t, inst.Channels, "#random")
}

func TestSession_mutationOperations_recordSpans(t *testing.T) {
	recorder := oteltest.InstallSpanRecorder(t)
	store := storetest.NewMemoryStore(t)
	sess := New(store, nil, &fakeAPIClient{}, "testuser", "", "")
	sess.now = func() time.Time { return fixedTime }
	ctx := t.Context()

	require.NoError(t, sess.Join(ctx, "#general"))

	seedChannelWithMembers(t, store, "#leave", "testuser")
	require.NoError(t, sess.Part(ctx, "#leave", ""))

	seedInstance(t, store, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	channel, err := store.GetChannel(ctx, "#general")
	require.NoError(t, err)
	channel.Members.Add("botty")
	require.NoError(t, store.SaveChannel(ctx, channel))
	require.NoError(t, sess.Kick(ctx, "#general", "botty"))

	require.NoError(t, sess.SetTopic(ctx, "#general", "observability"))
	require.NoError(t, sess.ChangeNick(ctx, "renamed"))

	seedInstance(t, store, domain.Instance{
		Nick:     "dm-bot",
		ModelID:  "test/dm-model",
		Channels: testChannels(),
	})
	_, _, err = sess.OpenDM(ctx, "dm-bot")
	require.NoError(t, err)

	require.NoError(t, sess.Reset(ctx))

	ended := make(map[string]sdktrace.ReadOnlySpan)
	for _, span := range recorder.Ended() {
		ended[span.Name()] = span
	}

	require.Contains(t, ended, "session.join")
	require.Contains(t, ended, "session.part")
	require.Contains(t, ended, "session.kick")
	require.Contains(t, ended, "session.set_topic")
	require.Contains(t, ended, "session.change_nick")
	require.Contains(t, ended, "session.open_dm")
	require.Contains(t, ended, "session.reset")
}

func TestSession_dispatchToInstance_recordsPassReasonAndToolTurns(t *testing.T) {
	recorder := oteltest.InstallSpanRecorder(t)
	dataStore := storetest.NewMemoryStore(t)
	memStore := memory.NewStoreAdapter(storetest.NewMemoryStore(t))
	fake := &fakeAPIClient{
		sendEventsFullFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				PendingToolCalls: []api.PendingToolCall{{
					ID:   "call-1",
					Kind: api.ToolCallWriteMemory,
					Key:  "topic",
					Body: "observability",
				}},
			}, nil
		},
		continueWithToolResultsFn: func(context.Context, *api.Conversation, []api.ToolResult) (api.CompletionResult, error) {
			return api.CompletionResult{
				Response: protocol.ModelResponse{
					Kind:   protocol.ResponseSilence,
					Reason: "nothing to say",
				},
			}, nil
		},
	}
	sess := New(dataStore, memStore, fake, "testuser", "", "")
	sess.now = func() time.Time { return fixedTime }
	ctx := t.Context()

	seedChannelWithMembers(t, dataStore, "#general", "testuser", "botty")
	channel, err := dataStore.GetChannel(ctx, "#general")
	require.NoError(t, err)
	inst := domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	}

	replies, err := sess.dispatchToInstance(ctx, channel, inst, "#general", nil, nil)
	require.NoError(t, err)
	require.Empty(t, replies)

	span := oteltest.FindSpan(t, recorder, "session.dispatch_to_instance")
	require.Equal(t, observability.ResultPass, oteltest.AttrValue(span.Attributes(), observability.AttrResult))
	require.Equal(t, observability.PassReasonModelPass, oteltest.AttrValue(span.Attributes(), observability.AttrPassReason))
	require.Equal(t, "0", oteltest.AttrValue(span.Attributes(), observability.AttrRetryCount))
	require.Equal(t, "1", oteltest.AttrValue(span.Attributes(), observability.AttrToolTurnCount))
}

func TestSession_dispatchInBackground_recordsSpan(t *testing.T) {
	recorder := oteltest.InstallSpanRecorder(t)
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser")
	sess.dispatchInBackground(ctx, "#general", nil)

	drainEvent[domain.DispatchDoneEvent](t, sess)

	span := oteltest.FindSpan(t, recorder, "session.dispatch_background")
	require.Equal(t, "#general", oteltest.AttrValue(span.Attributes(), observability.AttrChannel))
	require.Equal(t, observability.ResultOK, oteltest.AttrValue(span.Attributes(), observability.AttrResult))
}

func TestSession_SendMessage(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser")

	require.NoError(t, sess.SendMessage(ctx, "#general", "hello world"))
	evt := drainEvent[domain.MessageEvent](t, sess)
	require.Equal(t, domain.MessageEvent{
		Event: domain.ChannelMessage{
			Channel: "#general",
			From:    "testuser",
			Body:    "hello world",
			At:      fixedTime,
		},
	}, evt)

	// Message should be persisted as a ChannelMessage event.
	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
	}, msgs)

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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	require.NoError(t, sess.SendMessage(ctx, "#general", "hello"))

	// Drain the MessageEvent first, then the dispatch events.
	drainEvent[domain.MessageEvent](t, sess)
	events := drainEvents(t, sess, 1)

	require.Equal(t, []domain.SessionEvent{
		domain.DispatchStartedEvent{Channel: "#general", Nicks: []domain.Nick{"botty"}},
		domain.ModelReplyEvent{
			Channel:  "#general",
			Instance: "botty",
			Event: domain.ChannelMessage{
				Channel: "#general",
				From:    "botty",
				Body:    "got it",
				At:      fixedTime,
			},
			At: fixedTime,
		},
		domain.DispatchDoneEvent{Channel: "#general"},
	}, events)
}

func TestSession_JoinEvent_triggers_dispatch(t *testing.T) {
	var receivedEvents []protocol.IRCMessage

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, _ domain.ModelID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (protocol.ModelResponse, error) {
			receivedEvents = events
			return protocol.Reply("welcome"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	// Seed a channel with a model already present so join dispatch
	// has someone to notify.
	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	// Join an existing channel — the reactive dispatch should fire.
	require.NoError(t, sess.Join(ctx, "#general"))

	drainEvent[domain.JoinEvent](t, sess)
	events := drainEvents(t, sess, 1)

	require.Equal(t, []domain.SessionEvent{
		domain.DispatchStartedEvent{Channel: "#general", Nicks: []domain.Nick{"botty"}},
		domain.ModelReplyEvent{
			Channel:  "#general",
			Instance: "botty",
			Event: domain.ChannelMessage{
				Channel: "#general",
				From:    "botty",
				Body:    "welcome",
				At:      fixedTime,
			},
			At: fixedTime,
		},
		domain.DispatchDoneEvent{Channel: "#general"},
	}, events)

	// The trigger event sent to the model should be a JOIN message.
	require.Equal(t, []protocol.IRCMessage{{
		Kind:   protocol.KindJoin,
		From:   "testuser",
		Target: "#general",
		At:     fixedTime,
	}}, receivedEvents)
}

func TestSession_model_reply_does_not_retrigger_dispatch(t *testing.T) {
	var dispatchCount int

	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			dispatchCount++
			return protocol.Reply("got it"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	require.NoError(t, sess.SendMessage(ctx, "#general", "hello"))

	// Drain the MessageEvent and all dispatch events.
	drainEvent[domain.MessageEvent](t, sess)
	drainEvents(t, sess, 1)

	// Only one dispatch should have occurred — the ModelReplyEvent
	// emitted by the dispatch goroutine must not trigger another
	// dispatch.
	require.Equal(t, 1, dispatchCount)
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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
		{Channel: "#general", From: "botty", Body: "got it", At: fixedTime},
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

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
	}, msgs)
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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
	}, msgs)
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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
		{Channel: "#general", From: "botty", Body: "hello back", At: fixedTime},
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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model-a",
		Channels: testChannels("#general"),
	})
	seedInstance(t, s, domain.Instance{
		Nick:     "otherbot",
		ModelID:  "test/model-b",
		Channels: testChannels("#random"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	generalMsgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
		{Channel: "#general", From: "botty", Body: "reply from test/model-a", At: fixedTime},
	}, generalMsgs)

	randomMsgs := channelMessages(t, s, "#random")
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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
		{Channel: "#general", From: "botty", Body: "reply once", At: fixedTime},
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
	seedInstance(t, s, domain.Instance{
		Nick:     "bot-a",
		ModelID:  "test/model-a",
		Channels: testChannels("#general"),
	})
	seedInstance(t, s, domain.Instance{
		Nick:     "bot-b",
		ModelID:  "test/model-b",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs := channelMessages(t, s, "#general")

	require.Equal(t, domain.ChannelMessage{
		Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime,
	}, msgs[0])
	require.ElementsMatch(t, []domain.ChannelMessage{
		{Channel: "#general", From: "bot-a", Body: "reply from test/model-a", At: fixedTime},
		{Channel: "#general", From: "bot-b", Body: "reply from test/model-b", At: fixedTime},
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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
	}, msgs)
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
	seedInstance(t, s, domain.Instance{
		Nick:     "bot-a",
		ModelID:  "test/model-a",
		Channels: testChannels("#general"),
	})
	seedInstance(t, s, domain.Instance{
		Nick:     "bot-b",
		ModelID:  "test/model-b",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.Error(t, err, "should surface the API error")
	require.ErrorContains(t, err, "network timeout")

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
		{Channel: "#general", From: "bot-b", Body: "reply from bot-b", At: fixedTime},
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
	seedInstance(t, s, domain.Instance{
		Nick:     "bot-a",
		ModelID:  "test/model-a",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, s, "#random", "testuser", "bot-b")
	seedInstance(t, s, domain.Instance{
		Nick:     "bot-b",
		ModelID:  "test/model-b",
		Channels: testChannels("#random"),
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

	msgs := channelMessages(t, s, "#random")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#random", From: "bot-b", Body: "still here", At: fixedTime},
	}, msgs)
}

func TestSession_SetTopic(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	ch := domain.Channel{Name: "#dev", Kind: domain.KindChannel, Created: fixedTime}
	require.NoError(t, s.SaveChannel(ctx, ch))

	require.NoError(t, sess.SetTopic(ctx, "#dev", "Development Chat"))
	evt := drainEvent[domain.TopicChangeEvent](t, sess)
	require.Equal(t, domain.TopicChangeEvent{
		Channel: "#dev",
		Topic:   "Development Chat",
		By:      "testuser",
		At:      fixedTime,
	}, evt)

	// Channel topic and metadata should be updated.
	updated, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	requireChannelEqual(t, domain.Channel{
		Name:       "#dev",
		Kind:       domain.KindChannel,
		Topic:      "Development Chat",
		TopicSetBy: "testuser",
		TopicSetAt: fixedTime,
		Created:    fixedTime,
	}, updated)
}

func TestSession_ChangeNick(t *testing.T) {
	s := storetest.NewMemoryStore(t)
	sess := New(s, nil, &fakeAPIClient{}, "testuser", "", "")
	sess.now = func() time.Time { return fixedTime }

	// Join a channel so the nick change emits per-channel events.
	// Creating a channel emits JoinEvent, ModeChangeEvent, and a
	// DispatchDoneEvent from reactive dispatch (no models → immediate
	// done). The ModeChangeEvent and DispatchDoneEvent race.
	require.NoError(t, sess.Join(t.Context(), "#general"))
	drainNEvents(t, sess, 3)

	require.NoError(t, sess.ChangeNick(t.Context(), "newname"))
	evt := drainEvent[domain.NickChangeEvent](t, sess)
	require.Equal(t, domain.NickChangeEvent{
		Channel: "#general",
		OldNick: "testuser",
		NewNick: "newname",
		At:      fixedTime,
	}, evt)

	require.Equal(t, domain.Nick("newname"), sess.UserNick())
}

func TestSession_Whois(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	inst := domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Persona:  "A test bot",
		Channels: testChannels("#dev"),
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

	require.Error(t, sess.Invite(t.Context(), "#ghost", "anthropic/claude-3-haiku", ""))
}

func TestSession_Invite_existing_instance_to_nonexistent_channel_does_not_corrupt(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	require.Error(t, sess.Invite(ctx, "#ghost", "botty", ""))

	// Instance should not have the phantom channel in its set.
	inst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)
	requireChannels(t, inst.Channels, "#general")
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
		Members: testMembers("testuser"),
		Created: fixedTime,
	}
	require.NoError(t, s.SaveChannel(ctx, ch))

	require.Error(t, sess.Invite(ctx, "#dev", "anthropic/claude-3-haiku", ""))
}

func TestSession_Invite_persists_persona(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser")

	require.NoError(t, sess.Invite(ctx, "#general", "anthropic/claude-3-haiku", "Helpful assistant"))
	evt := drainEvent[domain.ModelInvitedEvent](t, sess)
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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Persona:  "Helpful assistant",
		Channels: testChannels("#general"),
	})

	require.NoError(t, sess.Invite(ctx, "#random", "botty", ""))
	evt := drainEvent[domain.ModelInvitedEvent](t, sess)
	require.Equal(t, domain.ModelInvitedEvent{
		Channel: "#random",
		Instance: domain.Instance{
			Nick:     "botty",
			ModelID:  "test/model",
			Persona:  "Helpful assistant",
			Channels: testChannels("#general", "#random")},
		At: fixedTime,
	}, evt)

	inst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)
	require.Equal(t, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Persona:  "Helpful assistant",
		Channels: testChannels("#general", "#random")}, inst)

	channel, err := s.GetChannel(ctx, "#random")
	require.NoError(t, err)
	require.Equal(t, testMembers("testuser", "botty").Slice(), channel.Members.Slice())
}

func TestSession_Invite_existing_instance_is_idempotent(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	require.NoError(t, sess.Invite(ctx, "#general", "botty", ""))

	inst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)
	requireChannels(t, inst.Channels, "#general")

	channel, err := s.GetChannel(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, testMembers("testuser", "botty").Slice(), channel.Members.Slice())
}

func TestSession_Invite_existing_instance_preserves_persona(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser")
	seedChannelWithMembers(t, s, "#random", "testuser")
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Persona:  "Existing persona",
		Channels: testChannels("#general"),
	})

	require.NoError(t, sess.Invite(ctx, "#random", "botty", "New persona"))
	evt := drainEvent[domain.ModelInvitedEvent](t, sess)
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

	require.NoError(t, sess.Invite(ctx, "#general", "test/model", "Helpful assistant"))
	evt1 := drainEvent[domain.ModelInvitedEvent](t, sess)
	require.Equal(t, domain.Nick("fakenick"), evt1.Instance.Nick)
	// New member also emits a ModeChangeEvent; drain it.
	drainEvent[domain.ModeChangeEvent](t, sess)

	require.NoError(t, sess.Invite(ctx, "#random", "test/model", ""))
	evt2 := drainEvent[domain.ModelInvitedEvent](t, sess)
	require.Equal(t, domain.ModelInvitedEvent{
		Channel: "#random",
		Instance: domain.Instance{
			Nick:     "fakenick",
			ModelID:  "test/model",
			Persona:  "Helpful assistant",
			Channels: testChannels("#general", "#random")},
		At: fixedTime,
	}, evt2)

	instances, err := s.ListInstances(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.Instance{
		{
			Nick:     "fakenick",
			ModelID:  "test/model",
			Persona:  "Helpful assistant",
			Channels: testChannels("#general", "#random")},
	}, instances)
}

func TestSession_KickNonexistentChannel(t *testing.T) {
	sess, _ := newTestSession(t)

	require.Error(t, sess.Kick(t.Context(), "#ghost", "botty"))
}

func TestSession_KickNonMember(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	ch := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Members: testMembers("testuser"),
		Created: fixedTime,
	}
	require.NoError(t, s.SaveChannel(ctx, ch))

	require.NoError(t, sess.Kick(ctx, "#dev", "nobody"))
	evt := drainEvent[domain.ModelKickedEvent](t, sess)
	require.Equal(t, domain.ModelKickedEvent{
		Channel: "#dev",
		Nick:    "nobody",
		At:      fixedTime,
	}, evt)

	// Members should be unchanged.
	updated, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, testMembers("testuser").Slice(), updated.Members.Slice())
}

func TestSession_SetTopicNonexistentChannel(t *testing.T) {
	sess, _ := newTestSession(t)

	require.Error(t, sess.SetTopic(t.Context(), "#ghost", "topic"))
}

func TestSession_DispatchToChannel_includes_memory_in_prompt(t *testing.T) {
	memStore := memory.NewStoreAdapter(storetest.NewMemoryStore(t))
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
	s := storetest.NewMemoryStore(t)
	sess := New(s, memStore, fake, "testuser", "", "")
	sess.now = func() time.Time { return fixedTime }

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Persona:  "Helpful assistant",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(t.Context(), "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
		{Channel: "#general", From: "botty", Body: "memory and persona received", At: fixedTime},
	}, msgs)
}

func TestBuildSystemPrompt(t *testing.T) {
	ch := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Topic:   "go stuff",
		Members: testMembers("testuser", "botty"),
	}
	inst := domain.Instance{
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
	inst := domain.Instance{
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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	require.NoError(t, sess.Poke(ctx))

	// PokeEvent is emitted via emit(), then the reactive dispatch
	// runs in the background.
	pokeEvt := drainEvent[domain.PokeEvent](t, sess)
	require.Equal(t, domain.PokeEvent{Channel: "#general", At: fixedTime}, pokeEvt)

	events := drainEvents(t, sess, 1)

	require.Equal(t, []domain.SessionEvent{
		domain.DispatchStartedEvent{Channel: "#general", Nicks: []domain.Nick{"botty"}},
		domain.ModelReplyEvent{
			Channel:  "#general",
			Instance: "botty",
			Event: domain.ChannelMessage{
				Channel: "#general",
				From:    "botty",
				Body:    "poke received",
				At:      fixedTime,
			},
			At: fixedTime,
		},
		domain.DispatchDoneEvent{Channel: "#general"},
	}, events)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "botty", Body: "poke received", At: fixedTime},
	}, msgs)
}

func TestSession_OpenDM_creates_dm_channel(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, s, domain.Instance{
		Nick:    "botty",
		ModelID: "test/model",
	})

	ch, created, err := sess.OpenDM(ctx, "botty")
	require.NoError(t, err)
	require.True(t, created)
	requireChannelEqual(t, domain.Channel{
		Name:    "botty",
		Kind:    domain.KindDM,
		Members: testMembers("testuser", "botty"),
		Created: fixedTime,
	}, ch)

	got, err := s.GetChannel(ctx, "botty")
	require.NoError(t, err)
	requireChannelEqual(t, ch, got)

	inst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)
	requireChannels(t, inst.Channels, "botty")
}

func TestSession_OpenDM_reuses_existing_dm_channel(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	existing := domain.Channel{
		Name:    "botty",
		Kind:    domain.KindDM,
		Members: testMembers("testuser", "botty"),
		Created: fixedTime.Add(-time.Hour),
	}
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("botty"),
	})
	require.NoError(t, s.SaveChannel(ctx, existing))

	ch, created, err := sess.OpenDM(ctx, "botty")
	require.NoError(t, err)
	require.False(t, created)
	requireChannelEqual(t, existing, ch)

	inst, err := s.GetInstance(ctx, "botty")
	require.NoError(t, err)
	requireChannels(t, inst.Channels, "botty")
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

	seedInstance(t, s, domain.Instance{
		Nick:    "botty",
		ModelID: "test/model-a",
	})
	seedInstance(t, s, domain.Instance{
		Nick:     "otherbot",
		ModelID:  "test/model-b",
		Channels: testChannels("#general"),
	})

	_, _, err := sess.OpenDM(ctx, "botty")
	require.NoError(t, err)

	_, ircMsg := seedUserMessage(t, s, "botty", "hello in dm")

	_, err = sess.DispatchToChannel(ctx, "botty", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs := channelMessages(t, s, "botty")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "botty", From: "testuser", Body: "hello in dm", At: fixedTime},
		{Channel: "botty", From: "botty", Body: "dm reply", At: fixedTime},
	}, msgs)
}

func TestSession_MarkRead_and_UnreadCount(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser")

	_, err := s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general", From: "testuser", Body: "first", At: fixedTime,
	})
	require.NoError(t, err)
	_, err = s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general", From: "testuser", Body: "second", At: fixedTime,
	})
	require.NoError(t, err)

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

	_, err := s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general", From: "testuser", Body: "first", At: fixedTime,
	})
	require.NoError(t, err)

	require.NoError(t, sess.MarkRead(ctx, "#general"))

	_, err = s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general", From: "testuser", Body: "second", At: fixedTime,
	})
	require.NoError(t, err)
	_, err = s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general", From: "testuser", Body: "third", At: fixedTime,
	})
	require.NoError(t, err)

	count, err := sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 2, count)
}

func TestSession_Join_marks_channel_as_read(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser")
	_, err := s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general", From: "testuser", Body: "old", At: fixedTime,
	})
	require.NoError(t, err)

	require.NoError(t, sess.Join(ctx, "#general"))

	// MarkRead is called before the JoinEvent is appended, so the
	// JoinEvent itself is the one unread event.
	count, err := sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestSession_SetAPIKey(t *testing.T) {
	s := storetest.NewMemoryStore(t)
	initial := &fakeAPIClient{}
	replacement := &fakeAPIClient{}
	sess := New(s, nil, initial, "testuser", "", "")
	sess.SetAPIFactory(func(apiKey, baseURL string) (api.Client, error) {
		require.Equal(t, "test-key", apiKey)
		return replacement, nil
	})

	require.NoError(t, sess.SetAPIKey("test-key", ""))
	require.Equal(t, "test-key", sess.apiKey)
	require.Same(t, replacement, sess.api)
}

func TestSession_SetAPIKey_factory_failure_keeps_existing_client(t *testing.T) {
	s := storetest.NewMemoryStore(t)
	initial := &fakeAPIClient{}
	sess := New(s, nil, initial, "testuser", "", "")
	sess.SetAPIFactory(func(string, string) (api.Client, error) {
		return nil, fmt.Errorf("boom")
	})

	err := sess.SetAPIKey("test-key", "")
	require.Error(t, err)
	require.Same(t, initial, sess.api)
	require.Empty(t, sess.apiKey)
}

func TestSession_SetBaseURL(t *testing.T) {
	s := storetest.NewMemoryStore(t)

	var factoryBaseURL string
	factoryCalls := 0
	newClient := &fakeAPIClient{}

	sess := New(s, nil, &fakeAPIClient{}, "testuser", "test-key", "")
	sess.SetAPIFactory(func(apiKey, baseURL string) (api.Client, error) {
		factoryCalls++
		factoryBaseURL = baseURL
		return newClient, nil
	})

	require.NoError(t, sess.SetBaseURL("https://custom.example.com"))
	require.Equal(t, 1, factoryCalls)
	require.Equal(t, "https://custom.example.com", factoryBaseURL)
}

func TestSession_DispatchToChannel_filters_history_before_join(t *testing.T) {
	beforeJoin := fixedTime.Add(-10 * time.Minute)
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

	// Append a message event from before the model joined.
	_, err := s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general",
		From:    "testuser",
		Body:    "old message",
		At:      beforeJoin,
	})
	require.NoError(t, err)

	// Append a message event from after the model joined.
	_, err = s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general",
		From:    "testuser",
		Body:    "new message",
		At:      afterJoin,
	})
	require.NoError(t, err)

	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general")})

	newEvent := protocol.IRCMessage{
		Kind:   protocol.KindPrivMsg,
		From:   "testuser",
		Target: "#general",
		Body:   "ping",
	}
	_, err = sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{newEvent})
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
	seedInstance(t, s, domain.Instance{
		Nick:     "alpha",
		ModelID:  "test/alpha",
		Channels: testChannels("#general")})
	seedInstance(t, s, domain.Instance{
		Nick:     "beta",
		ModelID:  "test/beta",
		Channels: testChannels("#general")})

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

func (f *fakeAPIClient) GeneratePersonas(context.Context, domain.ModelID) ([]domain.Persona, error) {
	return nil, nil
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

func TestSession_Reset(t *testing.T) {
	s := storetest.NewMemoryStore(t)
	memStore := memory.NewStoreAdapter(storetest.NewMemoryStore(t))
	sess := New(s, memStore, &fakeAPIClient{}, "testuser", "", "")
	sess.now = func() time.Time { return fixedTime }
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	_, err := s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general", From: "testuser", Body: "hello", At: fixedTime,
	})
	require.NoError(t, err)
	require.NoError(t, memStore.Write(ctx, "botty", memory.Entry{Key: "mood", Content: "happy"}))

	require.NoError(t, sess.Reset(ctx))

	channels, err := s.ListChannels(ctx)
	require.NoError(t, err)
	require.Empty(t, channels)

	instances, err := s.ListInstances(ctx)
	require.NoError(t, err)
	require.Empty(t, instances)

	msgs := channelMessages(t, s, "#general")
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
	inst := domain.Instance{Nick: "botty", ModelID: "test/model"}

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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, 2, calls)
	require.Equal(t, []domain.ModelReplyEvent{
		{
			Channel:  "#general",
			Instance: "botty",
			Event: domain.ChannelMessage{
				Channel: "#general",
				From:    "botty",
				Body:    "clean reply",
				At:      fixedTime,
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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, 3, calls)
	require.Empty(t, replies)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello", At: fixedTime},
	}, msgs)
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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, []domain.ModelReplyEvent{
		{
			Channel:  "#general",
			Instance: "botty",
			Event: domain.ChannelMessage{
				Channel: "#general",
				From:    "botty",
				Body:    "no newlines here",
				At:      fixedTime,
			},
			At: fixedTime,
		},
	}, replies)
}

func newTestSessionWithMemory(t *testing.T, apiClient api.Client) (*Session, *storemod.SQLiteStore, *memory.StoreAdapter) {
	t.Helper()

	s := storetest.NewMemoryStore(t)

	m := memory.NewStoreAdapter(storetest.NewMemoryStore(t))
	sess := New(s, m, apiClient, "testuser", "", "")
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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
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
			Event: domain.ChannelMessage{
				Channel: "#general",
				From:    "botty",
				Body:    "noted!",
				At:      fixedTime,
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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
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

	s := storetest.NewMemoryStore(t)
	memStore := &failingMemoryStore{writeErr: fmt.Errorf("disk full")}
	sess := New(s, memStore, fake, "testuser", "", "")
	sess.now = func() time.Time { return fixedTime }
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
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
			Event: domain.ChannelMessage{
				Channel: "#general",
				From:    "botty",
				Body:    "ok anyway",
				At:      fixedTime,
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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
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
			Event: domain.ChannelMessage{
				Channel: "#general",
				From:    "botty",
				Body:    "stored both",
				At:      fixedTime,
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

func TestSession_DispatchToChannel_search_memory_then_reply(t *testing.T) {
	var continueResults []api.ToolResult
	fake := &fakeAPIClient{
		sendEventsFullFn: func(_ context.Context, _ domain.ModelID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				Conversation: &api.Conversation{},
				PendingToolCalls: []api.PendingToolCall{
					{ID: "call_1", Kind: api.ToolCallSearchMemory, Body: "favourite colour", Limit: 5},
				},
			}, nil
		},
		continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
			continueResults = results
			return api.CompletionResult{
				Response: protocol.Reply("your favourite colour is blue"),
			}, nil
		},
	}

	sess, s, memStore := newTestSessionWithMemory(t, fake)
	ctx := t.Context()

	require.NoError(t, memStore.Write(ctx, "botty", memory.Entry{Key: "colour", Content: "blue"}))

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "what is my favourite colour?")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	// The search result should be an error because StoreAdapter doesn't
	// implement Searcher.
	require.Equal(t, []api.ToolResult{
		{ToolCallID: "call_1", Content: "semantic search is not configured"},
	}, continueResults)

	require.Equal(t, []domain.ModelReplyEvent{
		{
			Channel:  "#general",
			Instance: "botty",
			Event: domain.ChannelMessage{
				Channel: "#general",
				From:    "botty",
				Body:    "your favourite colour is blue",
				At:      fixedTime,
			},
			At: fixedTime,
		},
	}, replies)
}

// newEmbeddingServer returns an httptest server that responds to
// OpenAI-compatible embedding requests. The topics map assigns each
// keyword a dimension in the embedding vector; matching keywords get a
// unit vector in that dimension, non-matching text gets a uniform
// spread.
func newEmbeddingServer(t *testing.T, dims int, topics map[string]int) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/embeddings", r.URL.Path)

		var req struct {
			Input string `json:"input"`
			Model string `json:"model"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

		vec := make([]float32, dims)

		matched := false
		for keyword, dim := range topics {
			if strings.Contains(req.Input, keyword) {
				vec[dim] = 1.0
				matched = true

				break
			}
		}

		if !matched {
			val := float32(1.0 / math.Sqrt(float64(dims)))
			for i := range vec {
				vec[i] = val
			}
		}

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": vec},
			},
		}))
	}))
	t.Cleanup(srv.Close)

	return srv
}

func newTestSessionWithIndexedMemory(
	t *testing.T,
	apiClient api.Client,
	embeddingURL string,
) (*Session, *storemod.SQLiteStore, *memory.IndexedStore) {
	t.Helper()

	s := storetest.NewMemoryStore(t)

	backing := memory.NewStoreAdapter(storetest.NewMemoryStore(t))

	normalized := true
	embeddingFunc := chromem.NewEmbeddingFuncOpenAICompat(
		embeddingURL, "test-key", "test-model", &normalized,
	)

	m := memory.NewIndexedStoreFromDB(backing, chromem.NewDB(), embeddingFunc)
	sess := New(s, m, apiClient, "testuser", "", "")
	sess.now = func() time.Time { return fixedTime }

	return sess, s, m
}

func TestSession_DispatchToChannel_search_memory_with_vector_store(t *testing.T) {
	// Three topics in 3 dimensions. Querying "cats" produces [1,0,0],
	// giving each entry a distinct cosine similarity:
	//   "cats are great"    → [1,0,0] → 1.0
	//   "no keyword match"  → uniform  → 1/√3 ≈ 0.577
	//   "dogs are loyal"    → [0,1,0] → 0.0
	embSrv := newEmbeddingServer(t, 3, map[string]int{
		"cats": 0,
		"dogs": 1,
		"fish": 2,
	})

	uniformSim := float32(1.0 / math.Sqrt(3))

	var continueResults []api.ToolResult
	fake := &fakeAPIClient{
		sendEventsFullFn: func(_ context.Context, _ domain.ModelID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				Conversation: &api.Conversation{},
				PendingToolCalls: []api.PendingToolCall{
					{ID: "call_1", Kind: api.ToolCallSearchMemory, Body: "cats", Limit: 3},
				},
			}, nil
		},
		continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
			continueResults = results
			return api.CompletionResult{
				Response: protocol.Reply("your favourite is cats"),
			}, nil
		},
	}

	sess, s, memStore := newTestSessionWithIndexedMemory(t, fake, embSrv.URL)
	ctx := t.Context()

	require.NoError(t, memStore.Write(ctx, "botty", memory.Entry{Key: "fav_pet", Content: "cats are great"}))
	require.NoError(t, memStore.Write(ctx, "botty", memory.Entry{Key: "hobby", Content: "no keyword match here"}))
	require.NoError(t, memStore.Write(ctx, "botty", memory.Entry{Key: "other_pet", Content: "dogs are loyal"}))

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "what is my favourite pet?")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	// Unmarshal the JSON content so we can assert the full search
	// results slice, then assert the full tool results wrapper too.
	var searchResults []memory.SearchResult
	require.NoError(t, json.Unmarshal([]byte(continueResults[0].Content), &searchResults))

	require.Equal(t, []api.ToolResult{
		{ToolCallID: "call_1", Content: continueResults[0].Content},
	}, continueResults)

	require.Equal(t, []memory.SearchResult{
		{Entry: memory.Entry{Key: "fav_pet", Content: "cats are great"}, Similarity: 1.0},
		{Entry: memory.Entry{Key: "hobby", Content: "no keyword match here"}, Similarity: uniformSim},
		{Entry: memory.Entry{Key: "other_pet", Content: "dogs are loyal"}, Similarity: 0},
	}, searchResults)

	require.Equal(t, []domain.ModelReplyEvent{
		{
			Channel:  "#general",
			Instance: "botty",
			Event: domain.ChannelMessage{
				Channel: "#general",
				From:    "botty",
				Body:    "your favourite is cats",
				At:      fixedTime,
			},
			At: fixedTime,
		},
	}, replies)
}

func TestSession_DispatchToChannel_write_then_search_memory_with_vector_store(t *testing.T) {
	// Two topics in 2 dimensions. After writing two entries, a search
	// for "cats" returns both with distinct scores:
	//   "cats are wonderful" → [1,0] → 1.0
	//   "dogs are loyal"     → [0,1] → 0.0
	embSrv := newEmbeddingServer(t, 2, map[string]int{
		"cats": 0,
		"dogs": 1,
	})

	var writeResults, searchResults []api.ToolResult
	fake := &fakeAPIClient{
		sendEventsFullFn: func(_ context.Context, _ domain.ModelID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				Conversation: &api.Conversation{},
				PendingToolCalls: []api.PendingToolCall{
					{ID: "call_write_cats", Kind: api.ToolCallWriteMemory, Key: "pet_cats", Body: "cats are wonderful"},
					{ID: "call_write_dogs", Kind: api.ToolCallWriteMemory, Key: "pet_dogs", Body: "dogs are loyal"},
				},
			}, nil
		},
		continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
			// First continuation receives the write results;
			// second receives the search results.
			if writeResults == nil {
				writeResults = results
				return api.CompletionResult{
					Conversation: &api.Conversation{},
					PendingToolCalls: []api.PendingToolCall{
						{ID: "call_search", Kind: api.ToolCallSearchMemory, Body: "cats", Limit: 5},
					},
				}, nil
			}

			searchResults = results
			return api.CompletionResult{
				Response: protocol.Reply("noted"),
			}, nil
		},
	}

	sess, s, _ := newTestSessionWithIndexedMemory(t, fake, embSrv.URL)
	ctx := t.Context()

	seedChannelWithMembers(t, s, "#general", "testuser", "botty")
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, []api.ToolResult{
		{ToolCallID: "call_write_cats", Content: "ok"},
		{ToolCallID: "call_write_dogs", Content: "ok"},
	}, writeResults)

	var parsed []memory.SearchResult
	require.NoError(t, json.Unmarshal([]byte(searchResults[0].Content), &parsed))

	require.Equal(t, []api.ToolResult{
		{ToolCallID: "call_search", Content: searchResults[0].Content},
	}, searchResults)

	require.Equal(t, []memory.SearchResult{
		{Entry: memory.Entry{Key: "pet_cats", Content: "cats are wonderful"}, Similarity: 1.0},
		{Entry: memory.Entry{Key: "pet_dogs", Content: "dogs are loyal"}, Similarity: 0},
	}, parsed)
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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
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
	inst := domain.Instance{Nick: "botty", ModelID: "test/model"}

	prompt := buildSystemPrompt(ch, inst, nil)

	require.Contains(t, prompt, "write_memory")
	require.Contains(t, prompt, "delete_memory")
	require.Contains(t, prompt, "no memories yet")
}

func seedChannelWithMembers(t *testing.T, s *storemod.SQLiteStore, name domain.ChannelName, members ...domain.Nick) {
	t.Helper()

	require.NoError(t, s.SaveChannel(t.Context(), domain.Channel{
		Name:    name,
		Kind:    domain.KindChannel,
		Members: testMembers(members...),
		Created: fixedTime,
	}))
}

func seedInstance(t *testing.T, s *storemod.SQLiteStore, inst domain.Instance) {
	t.Helper()

	require.NoError(t, s.SaveInstance(t.Context(), inst))
}

// seedUserMessage appends a user message as a ChannelMessage event and
// returns the event and its protocol representation. Unlike
// sess.SendMessage, this does not trigger background dispatch.
func seedUserMessage(t *testing.T, s *storemod.SQLiteStore, ch domain.ChannelName, body string) (domain.ChannelMessage, protocol.IRCMessage) {
	t.Helper()

	cm := domain.ChannelMessage{
		Channel: ch,
		From:    "testuser",
		Body:    body,
		At:      fixedTime,
	}

	_, err := s.AppendEvent(t.Context(), ch, cm)
	require.NoError(t, err)

	ircMsg, _ := protocol.FromChannelEvent(cm)

	return cm, ircMsg
}

// channelMessages extracts ChannelMessage events from stored events.
func channelMessages(t *testing.T, s *storemod.SQLiteStore, ch domain.ChannelName) []domain.ChannelMessage {
	t.Helper()

	events, err := s.EventsBefore(t.Context(), ch, nil, 1000)
	require.NoError(t, err)

	var msgs []domain.ChannelMessage

	for _, se := range events {
		if cm, ok := se.Event.(domain.ChannelMessage); ok {
			msgs = append(msgs, cm)
		}
	}

	return msgs
}

func channelEventTypes(t *testing.T, s *storemod.SQLiteStore, ch domain.ChannelName) []string {
	t.Helper()

	events, err := s.EventsBefore(t.Context(), ch, nil, 1000)
	require.NoError(t, err)

	types := make([]string, len(events))

	for i, se := range events {
		types[i] = domain.ChannelEventType(se.Event)
	}

	return types
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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
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
	seedInstance(t, s, domain.Instance{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.ErrorIs(t, err, api.ErrResponseTruncated)
}

// drainNEvents reads exactly n events from the session events channel
// and discards them. Use when you need to clear events without
// inspecting them.
func drainNEvents(t *testing.T, sess *Session, n int) {
	t.Helper()

	for range n {
		select {
		case <-sess.Events():
		case <-time.After(time.Second):
			t.Fatal("timed out draining events")
		}
	}
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
