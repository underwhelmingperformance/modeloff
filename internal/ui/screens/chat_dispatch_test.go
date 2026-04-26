package screens

import (
	"errors"
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/store/storetest"
	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/ui/uitest"
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
	screen, err := NewChatScreen(t.Context(), newTestSession(t), nil, domain.KindStatus)
	require.NoError(t, err)
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
	screen, err := NewChatScreen(t.Context(), newTestSession(t), nil, domain.KindStatus)
	require.NoError(t, err)
	*screen.active = "#general"

	_, cmd := screen.handleDispatchDone(domain.DispatchDoneEvent{Channel: "#general"})

	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)

	pending, ok := containsMsg[components.PendingResponseMsg](msgs)
	require.True(t, ok, "expected PendingResponseMsg in batch")
	require.False(t, pending.Pending)
}

func TestChatScreen_DispatchDone_deferred_while_replies_queued(t *testing.T) {
	screen, err := NewChatScreen(t.Context(), newTestSession(t), nil, domain.KindStatus)
	require.NoError(t, err)
	*screen.active = "#general"
	screen.replyQueue["#general"] = []domain.ModelReplyEvent{
		{Channel: "#general", Event: domain.ChannelMessage{Channel: "#general", From: "botty", Body: "queued"}},
	}

	// With replies still queued, DispatchDone should not clear the
	// pending indicator — the queue drainer handles that.
	_, cmd := screen.handleDispatchDone(domain.DispatchDoneEvent{Channel: "#general"})
	require.Nil(t, cmd)
}

func TestChatScreen_ModelReply_queues_and_paces(t *testing.T) {
	sess := newTestSession(t)
	require.NoError(t, sess.Join(t.Context(), "#general"))

	screen, err := NewChatScreen(t.Context(), sess, nil, domain.KindStatus)
	require.NoError(t, err)
	*screen.active = "#general"

	botty := domain.NewModelInstance("bot-1", "botty", "test/model", "", nil)

	// First reply is delivered immediately (via deliverNextReplyMsg).
	first := domain.ModelReplyEvent{
		Channel:  "#general",
		Event:    domain.ChannelMessage{Channel: "#general", From: "botty", Body: "line one"},
		Instance: botty,
	}
	updated, cmd := screen.handleModelReplyEvent(first)
	screen = updated.(ChatScreen)

	require.Equal(t, map[domain.ChannelName][]domain.ModelReplyEvent{
		"#general": {first},
	}, screen.replyQueue)

	msgs := collectMsgs(cmd)
	deliver, hasDeliver := containsMsg[deliverNextReplyMsg](msgs)
	require.True(t, hasDeliver, "first reply should trigger immediate delivery")
	require.Equal(t, deliverNextReplyMsg{Channel: "#general"}, deliver,
		"delivery message must carry the reply's channel")

	// Second reply is only enqueued; no new delivery trigger.
	second := domain.ModelReplyEvent{
		Channel:  "#general",
		Event:    domain.ChannelMessage{Channel: "#general", From: "botty", Body: "line two"},
		Instance: botty,
	}
	updated, cmd = screen.handleModelReplyEvent(second)
	screen = updated.(ChatScreen)

	require.Equal(t, map[domain.ChannelName][]domain.ModelReplyEvent{
		"#general": {first, second},
	}, screen.replyQueue)
	require.Nil(t, cmd, "second reply should not trigger delivery while first is pending")

	// Delivering the first reply should schedule the next after a tick.
	updated, cmd = screen.deliverNextReply(deliverNextReplyMsg{Channel: "#general"})
	screen = updated.(ChatScreen)

	require.Equal(t, map[domain.ChannelName][]domain.ModelReplyEvent{
		"#general": {second},
	}, screen.replyQueue)
	require.NotNil(t, cmd, "should schedule next reply delivery")

	// Delivering the last reply clears the pending indicator.
	updated, cmd = screen.deliverNextReply(deliverNextReplyMsg{Channel: "#general"})
	screen = updated.(ChatScreen)

	require.Equal(t, map[domain.ChannelName][]domain.ModelReplyEvent{}, screen.replyQueue)

	msgs = collectMsgs(cmd)

	pending, ok := containsMsg[components.PendingResponseMsg](msgs)
	require.True(t, ok, "expected PendingResponseMsg after queue drained")
	require.False(t, pending.Pending)
}

// TestChatScreen_ModelReply_paces_per_channel_independently pins the
// #14 invariant: a burst of replies in one channel must not delay a
// reply in another channel. Each channel drains at its own pacing
// cadence.
func TestChatScreen_ModelReply_paces_per_channel_independently(t *testing.T) {
	sess := newTestSession(t)
	require.NoError(t, sess.Join(t.Context(), "#channel-a"))
	require.NoError(t, sess.Join(t.Context(), "#channel-b"))

	screen, err := NewChatScreen(t.Context(), sess, nil, domain.KindStatus)
	require.NoError(t, err)
	*screen.active = "#channel-a"

	botty := domain.NewModelInstance("bot-1", "botty", "test/model", "", nil)

	// Two replies queued for #channel-a: first delivers immediately,
	// second is paced behind it.
	aFirst := domain.ModelReplyEvent{
		Channel:  "#channel-a",
		Event:    domain.ChannelMessage{Channel: "#channel-a", From: "botty", Body: "a1"},
		Instance: botty,
	}
	aSecond := domain.ModelReplyEvent{
		Channel:  "#channel-a",
		Event:    domain.ChannelMessage{Channel: "#channel-a", From: "botty", Body: "a2"},
		Instance: botty,
	}

	updated, _ := screen.handleModelReplyEvent(aFirst)
	screen = updated.(ChatScreen)
	updated, _ = screen.handleModelReplyEvent(aSecond)
	screen = updated.(ChatScreen)

	require.Equal(t, []domain.ModelReplyEvent{aFirst, aSecond}, screen.replyQueue["#channel-a"])

	// A reply arriving for #channel-b should ALSO trigger immediate
	// delivery — #channel-a's queue does not hold it up.
	bFirst := domain.ModelReplyEvent{
		Channel:  "#channel-b",
		Event:    domain.ChannelMessage{Channel: "#channel-b", From: "botty", Body: "b1"},
		Instance: botty,
	}
	updated, cmd := screen.handleModelReplyEvent(bFirst)
	screen = updated.(ChatScreen)

	msgs := collectMsgs(cmd)
	deliver, hasDeliver := containsMsg[deliverNextReplyMsg](msgs)
	require.True(t, hasDeliver,
		"first reply on #channel-b must deliver immediately, not wait for #channel-a")
	require.Equal(t, deliverNextReplyMsg{Channel: "#channel-b"}, deliver,
		"delivery message must target #channel-b, not the channel at the head of #channel-a's queue")

	require.Equal(t, map[domain.ChannelName][]domain.ModelReplyEvent{
		"#channel-a": {aFirst, aSecond},
		"#channel-b": {bFirst},
	}, screen.replyQueue)

	// Delivering #channel-b's single reply empties its queue. The
	// application-wide pending indicator must stay on because
	// #channel-a still has queued replies.
	updated, cmd = screen.deliverNextReply(deliverNextReplyMsg{Channel: "#channel-b"})
	screen = updated.(ChatScreen)

	require.Equal(t, map[domain.ChannelName][]domain.ModelReplyEvent{
		"#channel-a": {aFirst, aSecond},
	}, screen.replyQueue)

	msgs = collectMsgs(cmd)
	_, hasPending := containsMsg[components.PendingResponseMsg](msgs)
	require.False(t, hasPending,
		"draining one channel must not clear pending while another channel still has replies")

	// Drain #channel-a fully. Only the final delivery — when no
	// channel has queued replies — clears the pending indicator.
	updated, _ = screen.deliverNextReply(deliverNextReplyMsg{Channel: "#channel-a"})
	screen = updated.(ChatScreen)

	updated, cmd = screen.deliverNextReply(deliverNextReplyMsg{Channel: "#channel-a"})
	screen = updated.(ChatScreen)

	require.Equal(t, map[domain.ChannelName][]domain.ModelReplyEvent{}, screen.replyQueue)

	msgs = collectMsgs(cmd)
	pending, ok := containsMsg[components.PendingResponseMsg](msgs)
	require.True(t, ok, "final drain should clear pending indicator")
	require.False(t, pending.Pending)
}

// TestChatScreen_parting_channel_purges_reply_queue pins the F4
// invariant: when the user parts a channel with pending replies, the
// queue entry is dropped and any stale tick that fires afterwards
// no-ops cleanly through deliverNextReply's empty-queue branch.
// Dropped replies remain in the session store, so re-joining the
// channel restores history — this purge only affects the in-flight
// pacing queue.
func TestChatScreen_parting_channel_purges_reply_queue(t *testing.T) {
	sess := newTestSession(t)
	require.NoError(t, sess.Join(t.Context(), "#x"))

	screen, err := NewChatScreen(t.Context(), sess, nil, domain.KindStatus)
	require.NoError(t, err)
	*screen.active = "#x"

	botty := domain.NewModelInstance("bot-1", "botty", "test/model", "", nil)
	queued := []domain.ModelReplyEvent{
		{Channel: "#x", Event: domain.ChannelMessage{Channel: "#x", From: "botty", Body: "one"}, Instance: botty},
		{Channel: "#x", Event: domain.ChannelMessage{Channel: "#x", From: "botty", Body: "two"}, Instance: botty},
	}
	screen.replyQueue["#x"] = queued

	// User parts #x — the handler drops both the channel and its
	// pending-reply queue entry.
	updated, _ := screen.handlePartEvent(domain.ChannelPart{
		Channel:  "#x",
		Instance: sess.UserInstance(),
	})
	screen = updated.(ChatScreen)

	_, stillQueued := screen.replyQueue["#x"]
	require.False(t, stillQueued, "reply queue for parted channel must be dropped")

	// A stale tick for the parted channel fires. deliverNextReply's
	// empty-queue branch no-ops for render, and clears the
	// application-wide pending indicator because no other channel
	// has pending work.
	_, cmd := screen.deliverNextReply(deliverNextReplyMsg{Channel: "#x"})

	msgs := collectMsgs(cmd)

	_, hasStored := containsMsg[domain.StoredEvent](msgs)
	require.False(t, hasStored, "stale tick must not render a queued reply for the parted channel")

	_, hasUnread := containsMsg[components.ChannelUnreadMsg](msgs)
	require.False(t, hasUnread, "stale tick must not mark the parted channel as unread")

	pending, ok := containsMsg[components.PendingResponseMsg](msgs)
	require.True(t, ok, "stale tick on an empty queue with nothing else pending should clear the indicator")
	require.False(t, pending.Pending)
}

func TestChatScreen_handleSessionEvent_routing(t *testing.T) {
	tests := []struct {
		name     string
		event    domain.Event
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
				Event:    domain.ChannelMessage{Channel: "#general", From: "botty", Body: "hi"},
				Instance: domain.NewModelInstance("bot-1", "botty", "test/model", "", nil),
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
			screen, err := NewChatScreen(t.Context(), newTestSession(t), nil, domain.KindStatus)
			require.NoError(t, err)
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
	screen, err := NewChatScreen(t.Context(), newTestSession(t), nil, domain.KindStatus)
	require.NoError(t, err)

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

func TestChatScreen_ErrorEvent_status_channel_guard_renders_as_usage_hint(t *testing.T) {
	screen, err := NewChatScreen(t.Context(), newTestSession(t), nil, domain.KindStatus)
	require.NoError(t, err)

	at := time.Now()
	_, cmd := screen.handleErrorEvent(domain.ErrorEvent{
		Operation: "send",
		Err: domain.StatusChannelGuardError{
			Command: "msg",
			Hint:    "the status channel doesn't take messages — try /msg <nick> for a model or /join <channel> for a channel",
		},
		At: at,
	})

	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)

	stored, ok := containsMsg[domain.StoredEvent](msgs)
	require.True(t, ok, "expected StoredEvent in batch")

	require.Equal(t, domain.ChannelUsageHint{
		Command: "msg",
		Usage:   "the status channel doesn't take messages — try /msg <nick> for a model or /join <channel> for a channel",
		At:      at,
	}, stored.Event)
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

// TestChatScreen_NickChange_then_Quit_removes_instance guards the
// invariant that renaming an instance (via NickChangeEvent) doesn't
// orphan its entry in the channel's member list. Identity is keyed by
// TestChatScreen_completion_all_instance_commands_see_instances_outside_active_channel
// pins the invariant that `/invite`, `/msg`, and `/whois` all see
// model instances that live in other channels, not just the active
// channel's members. The original refactor wired `Instances:` to the
// active channel's member list; the completion context now separates
// `Instances` (session-wide, from `sess.Instances`) from
// `ChannelMembers` (active-channel only). `/add-model` is intentionally
// excluded — its argument is a fresh OpenRouter model ID, not an
// existing instance nick.
func TestChatScreen_completion_all_instance_commands_see_instances_outside_active_channel(t *testing.T) {
	ctx := t.Context()
	s := storetest.NewMemoryStore(t)

	require.NoError(t, s.SaveInstance(ctx, domain.NewModelInstance(
		"inst-outsider", "outsider", "test/model", "", nil,
	)))

	sess := session.New(s, nil, &uitest.FakeAPI{}, "testuser", "", "")

	screen, err := NewChatScreen(ctx, sess, nil, domain.KindStatus)
	require.NoError(t, err)

	// Seed an active channel whose membership does NOT include
	// "outsider". The regression would have hidden the outsider
	// from completion because the context wired `Instances:` to
	// the active channel's members.
	screen.channels.Insert(domain.Channel{
		Name:    "#general",
		Kind:    domain.KindChannel,
		Members: domain.NewMemberList(),
	})
	*screen.active = "#general"

	completer := screen.completionSet()

	hasOutsider := func(t *testing.T, raw string) {
		t.Helper()

		c := completer.Complete(raw, len(raw))

		for _, suggestion := range c.Suggestions {
			if suggestion.Value == "outsider" {
				return
			}
		}

		t.Fatalf("%q: outsider not suggested: got %+v", raw, c.Suggestions)
	}

	for _, raw := range []string{
		"/invite outsider",
		"/msg outsider",
		"/whois outsider",
	} {
		t.Run(raw, func(t *testing.T) { hasOutsider(t, raw) })
	}
}

// the *Instance pointer, so a later QuitEvent carrying the same
// handle still finds and removes the entry cleanly regardless of the
// nick carried on the event.
func TestChatScreen_NickChange_then_Quit_removes_instance(t *testing.T) {
	screen, err := NewChatScreen(t.Context(), newTestSession(t), nil, domain.KindStatus)
	require.NoError(t, err)

	// Seed the channel so handleModelInvitedEvent finds it.
	screen.channels.Insert(domain.Channel{
		Name:    "#general",
		Kind:    domain.KindChannel,
		Members: domain.NewMemberList(),
	})
	*screen.active = "#general"

	now := time.Now()

	bot := domain.NewModelInstance("bot-1", "oldnick", "test/model", "", nil)

	_, _ = screen.handleModelInvitedEvent(domain.ChannelModelInvited{
		Channel:  "#general",
		Instance: bot,
		By:       "testuser",
		At:       now,
	})

	ch, ok := screen.channels.Get(domain.Channel{Name: "#general"})
	require.True(t, ok)
	require.Equal(t, []domain.Member{{
		Instance: bot,
		Nick:     "oldnick",
		Mode:     domain.ModeNone,
	}}, ch.Members.Slice())

	// Rename: the session mutates the instance's own nick before
	// emitting the event, so the handle's Nick() is already the new
	// value. The channel member list's snapshot must be updated in
	// place via RenameTo so sort order stays correct.
	bot.SetNick("newnick")

	_, _ = screen.handleNickChangeEvent(domain.ChannelNickChange{
		Channel:  "#general",
		Instance: bot,
		OldNick:  "oldnick",
		NewNick:  "newnick",
		At:       now,
	})

	ch, ok = screen.channels.Get(domain.Channel{Name: "#general"})
	require.True(t, ok)
	require.Equal(t, []domain.Member{{
		Instance: bot,
		Nick:     "newnick",
		Mode:     domain.ModeNone,
	}}, ch.Members.Slice(),
		"nick change should sync the member snapshot while preserving identity")

	// Quit keyed by the same *Instance pointer cleanly removes the
	// member regardless of the nick carried on the event.
	_, _ = screen.handleQuitEvent(domain.ChannelQuit{
		Instance: bot,
		At:       now,
	})

	ch, ok = screen.channels.Get(domain.Channel{Name: "#general"})
	require.True(t, ok)
	require.Empty(t, ch.Members.Slice(),
		"quit keyed by *Instance should remove the member regardless of the nick carried on the event")
}
