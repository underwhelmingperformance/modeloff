package screens

import (
	"errors"
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/components"
)

// collectMsgs executes a tea.Cmd and flattens any BatchMsg into a
// slice of concrete messages.
func collectMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}

	msg := cmd()

	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}

	var msgs []tea.Msg

	for _, c := range batch {
		msgs = append(msgs, collectMsgs(c)...)
	}

	return msgs
}

func containsMsg[T any](msgs []tea.Msg) (T, bool) {
	for _, msg := range msgs {
		if v, ok := msg.(T); ok {
			return v, true
		}
	}

	var zero T

	return zero, false
}

func TestChatScreen_DispatchStarted_shows_pending(t *testing.T) {
	screen := NewChatScreen(t.Context(), newTestSession(t))
	*screen.active = "#general"

	_, cmd := screen.handleDispatchStarted(domain.DispatchStartedEvent{
		Channel: "#general",
		Nicks:   []domain.Nick{"botty"},
	})

	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)

	pending, ok := containsMsg[components.PendingResponseMsg](msgs)
	require.True(t, ok, "expected PendingResponseMsg in batch")
	require.True(t, pending.Pending)

	thinking, ok := containsMsg[components.NickListThinkingMsg](msgs)
	require.True(t, ok, "expected NickListThinkingMsg in batch")
	require.True(t, thinking.Nicks["botty"])
}

func TestChatScreen_DispatchDone_clears_pending(t *testing.T) {
	screen := NewChatScreen(t.Context(), newTestSession(t))
	*screen.active = "#general"

	_, cmd := screen.handleDispatchDone(domain.DispatchDoneEvent{Channel: "#general"})

	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)

	pending, ok := containsMsg[components.PendingResponseMsg](msgs)
	require.True(t, ok, "expected PendingResponseMsg in batch")
	require.False(t, pending.Pending)
}

func TestChatScreen_DispatchDone_deferred_while_replies_queued(t *testing.T) {
	screen := NewChatScreen(t.Context(), newTestSession(t))
	*screen.active = "#general"
	screen.replyQueue = []domain.ModelReplyEvent{
		{Message: domain.Message{Channel: "#general", From: "botty", Body: "queued"}},
	}

	// With replies still queued, DispatchDone should not clear the
	// pending indicator — the queue drainer handles that.
	_, cmd := screen.handleDispatchDone(domain.DispatchDoneEvent{Channel: "#general"})
	require.Nil(t, cmd)
}

func TestChatScreen_ModelReply_queues_and_paces(t *testing.T) {
	sess := newTestSession(t)
	require.NoError(t, sess.Join(t.Context(), "#general"))

	screen := NewChatScreen(t.Context(), sess)
	*screen.active = "#general"

	// First reply is delivered immediately (via deliverNextReplyMsg).
	first := domain.ModelReplyEvent{
		Message:  domain.Message{Channel: "#general", From: "botty", Body: "line one"},
		Instance: "botty",
	}
	updated, cmd := screen.handleModelReplyEvent(first)
	screen = updated.(ChatScreen)

	require.Len(t, screen.replyQueue, 1)

	msgs := collectMsgs(cmd)
	_, hasDeliver := containsMsg[deliverNextReplyMsg](msgs)
	require.True(t, hasDeliver, "first reply should trigger immediate delivery")

	// Second reply is only enqueued; no new delivery trigger.
	second := domain.ModelReplyEvent{
		Message:  domain.Message{Channel: "#general", From: "botty", Body: "line two"},
		Instance: "botty",
	}
	updated, cmd = screen.handleModelReplyEvent(second)
	screen = updated.(ChatScreen)

	require.Len(t, screen.replyQueue, 2)
	require.Nil(t, cmd, "second reply should not trigger delivery while first is pending")

	// Delivering the first reply should schedule the next after a tick.
	updated, cmd = screen.deliverNextReply()
	screen = updated.(ChatScreen)

	require.Len(t, screen.replyQueue, 1)
	require.NotNil(t, cmd, "should schedule next reply delivery")

	// Delivering the last reply clears the pending indicator.
	updated, cmd = screen.deliverNextReply()
	screen = updated.(ChatScreen)

	require.Empty(t, screen.replyQueue)

	msgs = collectMsgs(cmd)

	pending, ok := containsMsg[components.PendingResponseMsg](msgs)
	require.True(t, ok, "expected PendingResponseMsg after queue drained")
	require.False(t, pending.Pending)
}

func TestChatScreen_handleSessionEvent_routing(t *testing.T) {
	tests := []struct {
		name     string
		event    domain.SessionEvent
		wantType any
	}{
		{
			name: "DispatchStartedEvent routes to pending indicator",
			event: domain.DispatchStartedEvent{
				Channel: "#general",
				Nicks:   []domain.Nick{"botty"},
			},
			wantType: components.PendingResponseMsg{},
		},
		{
			name:     "DispatchDoneEvent routes to pending clear",
			event:    domain.DispatchDoneEvent{Channel: "#general"},
			wantType: components.PendingResponseMsg{},
		},
		{
			name: "ModelReplyEvent routes to delivery",
			event: domain.ModelReplyEvent{
				Message:  domain.Message{Channel: "#general", From: "botty", Body: "hi"},
				Instance: "botty",
			},
			wantType: deliverNextReplyMsg{},
		},
		{
			name: "ErrorEvent routes to stored event",
			event: domain.ErrorEvent{
				Operation: "test op",
				Err:       errors.New("boom"),
				At:        time.Now(),
			},
			wantType: domain.StoredEvent{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			screen := NewChatScreen(t.Context(), newTestSession(t))
			*screen.active = "#general"

			// handleSessionEvent returns tea.Batch(innerCmd,
			// listenForEvents). We only inspect the inner command
			// (first element of the batch) to avoid blocking on the
			// events channel.
			_, cmd := screen.handleSessionEvent(sessionEventMsg{event: tt.event})
			require.NotNil(t, cmd)

			batchMsg := cmd()
			batch, ok := batchMsg.(tea.BatchMsg)
			require.True(t, ok, "expected BatchMsg from handleSessionEvent")
			require.GreaterOrEqual(t, len(batch), 2, "expected at least inner cmd + listenForEvents")

			// The first cmd in the batch is the inner handler's result.
			innerMsgs := collectMsgs(batch[0])

			found := false
			for _, msg := range innerMsgs {
				if sameType(msg, tt.wantType) {
					found = true

					break
				}
			}

			require.True(t, found, "expected %T in batch, got %v", tt.wantType, msgsTypes(innerMsgs))
		})
	}
}

func TestChatScreen_ErrorEvent_no_active_channel(t *testing.T) {
	screen := NewChatScreen(t.Context(), newTestSession(t))

	// No active channel set — error should still produce a StoredEvent
	// message so the UI can display it.
	_, cmd := screen.handleErrorEvent(domain.ErrorEvent{
		Operation: "startup failure",
		Err:       errors.New("no api key"),
		At:        time.Now(),
	})

	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)

	stored, ok := containsMsg[domain.StoredEvent](msgs)
	require.True(t, ok, "expected StoredEvent in batch")

	cmdErr, ok := stored.Event.(domain.ChannelCommandError)
	require.True(t, ok, "expected ChannelCommandError inside StoredEvent, got %T", stored.Event)
	require.Equal(t, "startup failure: no api key", cmdErr.Err)
}

func sameType(a, b any) bool {
	return fmt.Sprintf("%T", a) == fmt.Sprintf("%T", b)
}

func msgsTypes(msgs []tea.Msg) []string {
	types := make([]string, len(msgs))
	for i, msg := range msgs {
		types[i] = fmt.Sprintf("%T", msg)
	}

	return types
}
