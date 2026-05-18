package screens

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
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

func TestChatScreen_ModelDispatchStarted_marks_nick_thinking(t *testing.T) {
	screen := newScreenFixture(t)
	*screen.active = "#general"

	botty := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)

	cw := domain.NewChannelWindow("#general", time.Time{})
	cw.Members.Add(botty)
	screen.channels.Insert(newWindow(cw))

	_, cmd := screen.handleModelDispatchStarted(domain.ModelDispatchStarted{Instance: botty})

	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)

	thinking, ok := containsMsg[components.NickListThinkingMsg](msgs)
	require.True(t, ok, "expected NickListThinkingMsg in batch")
	require.Equal(t, map[domain.Nick]bool{"botty": true}, thinking.Nicks)
}

func TestChatScreen_ModelDispatchDone_clears_nick_thinking(t *testing.T) {
	screen := newScreenFixture(t)
	*screen.active = "#general"

	botty := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	screen.dispatching[botty] = true

	_, cmd := screen.handleModelDispatchDone(domain.ModelDispatchDone{Instance: botty})

	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)

	thinking, ok := containsMsg[components.NickListThinkingMsg](msgs)
	require.True(t, ok, "expected NickListThinkingMsg in batch")
	require.Empty(t, thinking.Nicks)

	require.Empty(t, screen.dispatching, "Done must remove the instance from the dispatching set")
}

// TestChatScreen_ModelDispatchDone_keeps_thinking_with_concurrent_dispatch
// pins the per-instance contract: a Done for one model does not clear
// the nick-list thinking indicator while another model is still in
// its turn.
func TestChatScreen_ModelDispatchDone_keeps_thinking_with_concurrent_dispatch(t *testing.T) {
	screen := newScreenFixture(t)
	*screen.active = "#general"

	botty := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	other := domain.NewModelInstance("inst-other", "other", "test/model", "", nil)

	cw := domain.NewChannelWindow("#general", time.Time{})
	cw.Members.Add(botty)
	cw.Members.Add(other)
	screen.channels.Insert(newWindow(cw))

	screen.dispatching[botty] = true
	screen.dispatching[other] = true

	_, cmd := screen.handleModelDispatchDone(domain.ModelDispatchDone{Instance: botty})

	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)

	thinking, ok := containsMsg[components.NickListThinkingMsg](msgs)
	require.True(t, ok, "expected NickListThinkingMsg in batch")
	require.Equal(t, map[domain.Nick]bool{"other": true}, thinking.Nicks,
		"Done for one instance must keep the other listed as thinking")
}

func TestChatScreen_ModelReply_queues_and_paces(t *testing.T) {
	sess, mgr, user := newTestSession(t)
	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#general")))

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)
	*screen.active = "#general"

	// First reply is delivered immediately (via deliverNextPacedMsg).
	first := domain.Message{
		Target:     "#general",
		From:       "botty",
		InstanceID: "inst-botty",
		Body:       "line one",
	}
	updated, cmd := screen.handleMessageEvent(first)
	screen = updated.(ChatScreen)

	require.Equal(t, map[domain.ChannelName][]domain.Message{
		"#general": {first},
	}, screen.pacedQueue)

	msgs := collectMsgs(cmd)
	deliver, hasDeliver := containsMsg[deliverNextPacedMsg](msgs)
	require.True(t, hasDeliver, "first paced message should trigger immediate delivery")
	require.Equal(t, deliverNextPacedMsg{Channel: "#general"}, deliver,
		"delivery message must carry the routing key")

	// Second reply is only enqueued; no new delivery trigger.
	second := domain.Message{
		Target:     "#general",
		From:       "botty",
		InstanceID: "inst-botty",
		Body:       "line two",
	}
	updated, cmd = screen.handleMessageEvent(second)
	screen = updated.(ChatScreen)

	require.Equal(t, map[domain.ChannelName][]domain.Message{
		"#general": {first, second},
	}, screen.pacedQueue)
	require.Nil(t, cmd, "second paced message should not trigger delivery while first is pending")

	// Delivering the first message should schedule the next after a tick.
	updated, cmd = screen.deliverNextPaced(deliverNextPacedMsg{Channel: "#general"})
	screen = updated.(ChatScreen)

	require.Equal(t, map[domain.ChannelName][]domain.Message{
		"#general": {second},
	}, screen.pacedQueue)
	require.NotNil(t, cmd, "should schedule next paced delivery")

	// Delivering the last message empties the queue.
	updated, _ = screen.deliverNextPaced(deliverNextPacedMsg{Channel: "#general"})
	screen = updated.(ChatScreen)

	require.Equal(t, map[domain.ChannelName][]domain.Message{}, screen.pacedQueue)
}

// TestChatScreen_ModelReply_paces_per_channel_independently pins the
// invariant: a burst of paced messages in one channel must not delay
// a message in another channel. Each channel drains at its own
// pacing cadence.
func TestChatScreen_ModelReply_paces_per_channel_independently(t *testing.T) {
	sess, mgr, user := newTestSession(t)
	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#channel-a")))
	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#channel-b")))

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)
	*screen.active = "#channel-a"

	// Two replies queued for #channel-a: first delivers immediately,
	// second is paced behind it.
	aFirst := domain.Message{
		Target:     "#channel-a",
		From:       "botty",
		InstanceID: "inst-botty",
		Body:       "a1",
	}
	aSecond := domain.Message{
		Target:     "#channel-a",
		From:       "botty",
		InstanceID: "inst-botty",
		Body:       "a2",
	}

	updated, _ := screen.handleMessageEvent(aFirst)
	screen = updated.(ChatScreen)
	updated, _ = screen.handleMessageEvent(aSecond)
	screen = updated.(ChatScreen)

	require.Equal(t, []domain.Message{aFirst, aSecond}, screen.pacedQueue["#channel-a"])

	// A reply arriving for #channel-b should ALSO trigger immediate
	// delivery — #channel-a's queue does not hold it up.
	bFirst := domain.Message{
		Target:     "#channel-b",
		From:       "botty",
		InstanceID: "inst-botty",
		Body:       "b1",
	}
	updated, cmd := screen.handleMessageEvent(bFirst)
	screen = updated.(ChatScreen)

	msgs := collectMsgs(cmd)
	deliver, hasDeliver := containsMsg[deliverNextPacedMsg](msgs)
	require.True(t, hasDeliver,
		"first paced message on #channel-b must deliver immediately, not wait for #channel-a")
	require.Equal(t, deliverNextPacedMsg{Channel: "#channel-b"}, deliver,
		"delivery message must target #channel-b, not the channel at the head of #channel-a's queue")

	require.Equal(t, map[domain.ChannelName][]domain.Message{
		"#channel-a": {aFirst, aSecond},
		"#channel-b": {bFirst},
	}, screen.pacedQueue)

	// Delivering #channel-b's single message empties its queue
	// while #channel-a's queue remains untouched.
	updated, _ = screen.deliverNextPaced(deliverNextPacedMsg{Channel: "#channel-b"})
	screen = updated.(ChatScreen)

	require.Equal(t, map[domain.ChannelName][]domain.Message{
		"#channel-a": {aFirst, aSecond},
	}, screen.pacedQueue)

	// Drain #channel-a fully.
	updated, _ = screen.deliverNextPaced(deliverNextPacedMsg{Channel: "#channel-a"})
	screen = updated.(ChatScreen)

	updated, _ = screen.deliverNextPaced(deliverNextPacedMsg{Channel: "#channel-a"})
	screen = updated.(ChatScreen)

	require.Equal(t, map[domain.ChannelName][]domain.Message{}, screen.pacedQueue)
}

// TestChatScreen_parting_channel_purges_paced_queue pins the F4
// invariant: when the user parts a channel with pending paced
// messages, the queue entry is dropped and any stale tick that
// fires afterwards no-ops cleanly through deliverNextPaced's
// empty-queue branch. Dropped messages remain in the session
// store, so re-joining the channel restores history — this purge
// only affects the in-flight pacing queue.
func TestChatScreen_parting_channel_purges_paced_queue(t *testing.T) {
	sess, mgr, user := newTestSession(t)
	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#x")))

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)
	*screen.active = "#x"

	queued := []domain.Message{
		{Target: "#x", From: "botty", InstanceID: "inst-botty", Body: "one"},
		{Target: "#x", From: "botty", InstanceID: "inst-botty", Body: "two"},
	}
	screen.pacedQueue["#x"] = queued

	// User parts #x — the handler drops both the channel and its
	// pending-paced queue entry.
	updated, _ := screen.handlePartEvent(domain.Part{
		Target:   "#x",
		Instance: user.Instance(),
	})
	screen = updated.(ChatScreen)

	_, stillQueued := screen.pacedQueue["#x"]
	require.False(t, stillQueued, "paced queue for parted channel must be dropped")

	// A stale tick for the parted channel fires. deliverNextPaced's
	// empty-queue branch no-ops cleanly.
	_, cmd := screen.deliverNextPaced(deliverNextPacedMsg{Channel: "#x"})

	msgs := collectMsgs(cmd)

	_, hasStored := containsMsg[domain.StoredEvent](msgs)
	require.False(t, hasStored, "stale tick must not render a queued message for the parted channel")

	_, hasUnread := containsMsg[components.ChannelUnreadMsg](msgs)
	require.False(t, hasUnread, "stale tick must not mark the parted channel as unread")
}

func TestChatScreen_handleProtocolEvent_routing(t *testing.T) {
	tests := []struct {
		name     string
		event    protocol.Event
		wantType any
	}{
		{
			name: "ModelDispatchStarted routes to nick-list thinking",
			event: domain.ModelDispatchStarted{
				Instance: domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil),
			},
			wantType: components.NickListThinkingMsg{},
		},
		{
			name: "ModelDispatchDone routes to nick-list thinking clear",
			event: domain.ModelDispatchDone{
				Instance: domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil),
			},
			wantType: components.NickListThinkingMsg{},
		},
		{
			name: "Message from model routes to paced delivery",
			event: domain.Message{
				Target:     "#general",
				From:       "botty",
				InstanceID: "inst-botty",
				Body:       "hi",
			},
			wantType: deliverNextPacedMsg{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			screen := newScreenFixture(t)
			*screen.active = "#general"

			// The handler returns tea.Batch(innerCmd, re-arm-listener).
			// Inspect only the inner command to avoid blocking on the
			// re-arm pump.
			_, cmd := screen.handleProtocolEvent(protocolEventMsg{event: tt.event})
			require.NotNil(t, cmd)

			batchMsg := cmd()
			batch, ok := batchMsg.(tea.BatchMsg)
			require.True(t, ok, "expected BatchMsg")
			require.GreaterOrEqual(t, len(batch), 2, "expected at least inner cmd + re-arm cmd")

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
	screen := newScreenFixture(t)

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

	cmdErr, ok := stored.Event.(domain.CommandError)
	require.True(t, ok, "expected CommandError inside StoredEvent, got %T", stored.Event)
	require.Equal(t, "startup failure: no api key", cmdErr.Err)
}

// TestChatScreen_MessageSubmit_on_status_channel_renders_usage_hint
// pins the chat-screen-side status-channel guard: with `&modeloff`
// active, a `MessageSubmitMsg` short-circuits to a `UsageHint`
// rather than sending. `&modeloff` is a chat-screen-owned window
// the session has no concept of, so the validation lives here.
func TestChatScreen_MessageSubmit_on_status_channel_renders_usage_hint(t *testing.T) {
	screen := newScreenFixture(t)
	*screen.active = domain.StatusChannelName

	_, cmd := screen.Update(components.MessageSubmitMsg{Text: "hello"})

	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)
	stored, ok := containsMsg[domain.StoredEvent](msgs)
	require.True(t, ok, "expected StoredEvent in batch, got %v", msgsTypes(msgs))

	hint, ok := stored.Event.(domain.UsageHint)
	require.True(t, ok, "expected UsageHint, got %T", stored.Event)
	require.Equal(t, "send", hint.Command)
	require.Equal(t,
		"the status channel doesn't take messages — try /msg <nick-or-#channel> instead",
		hint.Usage)
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

	apiClient := &uitest.FakeAPI{}
	sess, mgr, user := uitest.NewTestSession(t, s, apiClient, nil, nil, "", "", t.Context)

	screen, err := NewChatScreen(func() context.Context { return ctx }, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	// Seed an active channel whose membership does NOT include
	// "outsider". The regression would have hidden the outsider
	// from completion because the context wired `Instances:` to
	// the active channel's members.
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#general", time.Time{})))
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
	screen := newScreenFixture(t)

	// Seed the channel so handleModelInvitedEvent finds it.
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#general", time.Time{})))
	*screen.active = "#general"

	now := time.Now()

	bot := domain.NewModelInstance("bot-1", "oldnick", "test/model", "", nil)

	_, _ = screen.handleModelInvitedEvent(domain.ModelInvited{
		Target:   "#general",
		Instance: bot,
		By:       "testuser",
		At:       now,
	})

	cw := requireChannelWindow(t, screen, "#general")
	require.Equal(t, []domain.Member{{
		Instance: bot,
		Nick:     "oldnick",
		Mode:     domain.ModeNone,
	}}, slices.Collect(cw.Members.All()))

	// Rename: the session mutates the instance's own nick before
	// emitting the event, so the handle's Nick() is already the new
	// value. The channel member list's snapshot must be updated in
	// place via RenameTo so sort order stays correct.
	bot.SetNick("newnick")

	_, _ = screen.handleNickChangeEvent(domain.NickChange{
		Instance: bot,
		OldNick:  "oldnick",
		NewNick:  "newnick",
		At:       now,
	}, []domain.ChannelName{"#general"})

	cw = requireChannelWindow(t, screen, "#general")
	require.Equal(t, []domain.Member{{
		Instance: bot,
		Nick:     "newnick",
		Mode:     domain.ModeNone,
	}}, slices.Collect(cw.Members.All()),
		"nick change should sync the member snapshot while preserving identity")

	// Quit keyed by the same *Instance pointer cleanly removes the
	// member regardless of the nick carried on the event.
	_, _ = screen.handleQuitEvent(domain.Quit{
		Instance: bot,
		At:       now,
	}, []domain.ChannelName{"#general"})

	cw = requireChannelWindow(t, screen, "#general")
	require.Empty(t, slices.Collect(cw.Members.All()),
		"quit keyed by *Instance should remove the member regardless of the nick carried on the event")
}

// TestChatScreen_QuitEvent_routes_to_targets_only pins the
// chat-screen-side intersection rule: a QUIT for an actor in
// channels #x and #y is filed into #x and #y scrollbacks but not
// #z, even though #z is a known window. The chat-screen consumes
// the per-recipient `Targets` from the [protocol.Delivery]
// envelope rather than reading any wire-side channel list off the
// event itself.
func TestChatScreen_QuitEvent_routes_to_targets_only(t *testing.T) {
	screen := newScreenFixture(t)

	for _, name := range []domain.ChannelName{"#x", "#y", "#z"} {
		screen.channels.Insert(newWindow(domain.NewChannelWindow(name, time.Time{})))
	}
	*screen.active = "#x"

	bot := domain.NewModelInstance("bot-1", "botty", "test/model", "", nil)
	now := time.Now()
	quit := domain.Quit{Nick: "botty", Instance: bot, At: now}

	screen.bufferProtocolEvent(quit, []domain.ChannelName{"#x", "#y"})

	expected := domain.StoredEvent{Event: quit}

	require.Equal(t, []domain.StoredEvent{expected}, screen.scrollbackOf("#x"))
	require.Equal(t, []domain.StoredEvent{expected}, screen.scrollbackOf("#y"))
	require.Empty(t, screen.scrollbackOf("#z"),
		"a QUIT for {#x, #y} must not surface in #z's scrollback")
}

// TestChatScreen_NickChangeEvent_routes_to_targets_only mirrors
// the QUIT routing test for nick changes: the chat-screen files
// the line into the per-recipient `Targets` only, leaving
// unrelated windows untouched.
func TestChatScreen_NickChangeEvent_routes_to_targets_only(t *testing.T) {
	screen := newScreenFixture(t)

	for _, name := range []domain.ChannelName{"#x", "#y", "#z"} {
		screen.channels.Insert(newWindow(domain.NewChannelWindow(name, time.Time{})))
	}
	*screen.active = "#x"

	bot := domain.NewModelInstance("bot-1", "newnick", "test/model", "", nil)
	now := time.Now()
	nick := domain.NickChange{
		OldNick:  "oldnick",
		NewNick:  "newnick",
		Instance: bot,
		At:       now,
	}

	screen.bufferProtocolEvent(nick, []domain.ChannelName{"#x", "#y"})

	expected := domain.StoredEvent{Event: nick}

	require.Equal(t, []domain.StoredEvent{expected}, screen.scrollbackOf("#x"))
	require.Equal(t, []domain.StoredEvent{expected}, screen.scrollbackOf("#y"))
	require.Empty(t, screen.scrollbackOf("#z"),
		"a NICK for {#x, #y} must not surface in #z's scrollback")
}

// requireChannelWindow looks the named channel up in the chat
// screen's cache and asserts it materialised as a `*ChannelWindow`.
func requireChannelWindow(t *testing.T, screen ChatScreen, name domain.ChannelName) *domain.ChannelWindow {
	t.Helper()

	w, ok := screen.channels.Get(windowKey(name))
	require.True(t, ok, "expected channel %q in cache", name)

	cw, ok := w.Window.(*domain.ChannelWindow)
	require.True(t, ok, "expected *ChannelWindow for %q, got %T", name, w.Window)

	return cw
}
