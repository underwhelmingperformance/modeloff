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
	"github.com/laney/modeloff/internal/ui/chatcmd"
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
	screen, _ = screen.focus("#general")

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
	screen, _ = screen.focus("#general")

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
	screen, _ = screen.focus("#general")

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
	screen, _ = screen.focus("#general")

	// First reply is delivered immediately (via deliverNextPacedMsg).
	first := domain.Message{
		Target:     "#general",
		From:       "botty",
		InstanceID: "inst-botty",
		Body:       "line one",
	}
	updated, cmd := screen.handleMessageEvent(first)
	screen = updated

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
	screen = updated

	require.Equal(t, map[domain.ChannelName][]domain.Message{
		"#general": {first, second},
	}, screen.pacedQueue)
	require.Nil(t, cmd, "second paced message should not trigger delivery while first is pending")

	// Delivering the first message should schedule the next after a tick.
	updated, cmd = screen.deliverNextPaced(deliverNextPacedMsg{Channel: "#general"})
	screen = updated

	require.Equal(t, map[domain.ChannelName][]domain.Message{
		"#general": {second},
	}, screen.pacedQueue)
	require.NotNil(t, cmd, "should schedule next paced delivery")

	// Delivering the last message empties the queue.
	updated, _ = screen.deliverNextPaced(deliverNextPacedMsg{Channel: "#general"})
	screen = updated

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
	screen, _ = screen.focus("#channel-a")

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
	screen = updated
	updated, _ = screen.handleMessageEvent(aSecond)
	screen = updated

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
	screen = updated

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
	screen = updated

	require.Equal(t, map[domain.ChannelName][]domain.Message{
		"#channel-a": {aFirst, aSecond},
	}, screen.pacedQueue)

	// Drain #channel-a fully.
	updated, _ = screen.deliverNextPaced(deliverNextPacedMsg{Channel: "#channel-a"})
	screen = updated

	updated, _ = screen.deliverNextPaced(deliverNextPacedMsg{Channel: "#channel-a"})
	screen = updated

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
	screen, _ = screen.focus("#x")

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
	screen = updated

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
			screen, _ = screen.focus("#general")

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

// TestChatScreen_ModelUnavailableError_renders_in_dispatch_channel
// pins that a failed model turn renders in the window the turn ran
// in — domain.ModelUnavailableError.Channel — so a failure in a
// background channel does not get lost among `&modeloff`'s unrelated
// server notices and is not attributed to whatever window the user
// happens to be looking at.
func TestChatScreen_ModelUnavailableError_renders_in_dispatch_channel(t *testing.T) {
	screen := newScreenFixture(t)
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#general", time.Time{})))
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#other", time.Time{})))

	screen, _ = screen.focus("#general")
	screen, _ = screen.focus("#other")

	failure := domain.ModelUnavailableError{
		Channel: "#general",
		Nick:    "botty",
		At:      time.Now(),
	}

	screen.bufferEvent(failure)

	require.Equal(t, []string{failure.Error()}, scrollbackSystemNotices(screen.scrollbackOf("#general")))
	require.Empty(t, scrollbackSystemNotices(screen.scrollbackOf("#other")),
		"the failure must not land in the window the user switched to")
}

// TestChatScreen_ModelUnavailableError_falls_back_to_active_when_channel_closed
// covers a dispatch turn that failed for a channel the chat-screen
// has no open window for — parted, or never joined by the user — the
// same closed-window fallback handleErrorEvent applies to a command
// failure. Routing straight to the named channel would either drop
// the failure (appendToScrollback has no placeholder path for a DM)
// or resurrect a parted channel client-side; fallbackTarget's
// windowByName check catches both.
func TestChatScreen_ModelUnavailableError_falls_back_to_active_when_channel_closed(t *testing.T) {
	screen := newScreenFixture(t)
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#other", time.Time{})))
	screen, _ = screen.focus("#other")

	failure := domain.ModelUnavailableError{
		Channel: "#gone",
		Nick:    "botty",
		At:      time.Now(),
	}

	screen.bufferEvent(failure)

	_, opened := screen.windowByName("#gone")
	require.False(t, opened, "a dispatch failure must not resurrect a channel client-side")

	require.Equal(t, []string{failure.Error()}, scrollbackSystemNotices(screen.scrollbackOf("#other")))
}

// scrollbackSystemNotices extracts the Text field of every
// domain.SystemNotice in scrollback, in order.
func scrollbackSystemNotices(scrollback []domain.Event) []string {
	var texts []string

	for _, ev := range scrollback {
		if notice, ok := ev.(domain.SystemNotice); ok {
			texts = append(texts, notice.Text)
		}
	}

	return texts
}

func TestChatScreen_ErrorEvent_no_active_channel(t *testing.T) {
	screen := newScreenFixture(t)

	// No active channel set — the error renders in `&modeloff`'s
	// scrollback, and the handler asks for that window by returning a
	// focus request; the focus handler is the one place that moves the
	// user.
	screen, cmd := screen.handleErrorEvent(domain.ErrorEvent{
		Operation: "startup failure",
		Err:       errors.New("no api key"),
		At:        time.Now(),
	})

	focus, ok := containsMsg[chatcmd.ChannelFocusMsg](collectMsgs(cmd))
	require.True(t, ok, "the error must bring its landing window into view")
	require.Equal(t, domain.StatusChannelName, focus.Channel)

	require.Equal(t, []string{"startup failure: no api key"}, commandErrorTexts(screen.scrollbackOf(domain.StatusChannelName)))
}

// TestChatScreen_ErrorEvent_renders_at_issuing_window covers a
// command issued in one window whose failure arrives after the user
// has switched to another: the error must land in the window the
// command was issued from (ErrorEvent.Target), not wherever the user
// is now looking. Unlike TestChatScreen_ErrorEvent_no_active_channel,
// the issuing window is still open, so nothing should move the
// user's focus to see it.
func TestChatScreen_ErrorEvent_renders_at_issuing_window(t *testing.T) {
	screen := newScreenFixture(t)
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#general", time.Time{})))
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#other", time.Time{})))

	screen, _ = screen.focus("#general")
	screen, _ = screen.focus("#other")

	screen, cmd := screen.handleErrorEvent(domain.ErrorEvent{
		Operation: "topic",
		Err:       errors.New("not a channel operator"),
		Target:    "#general",
		At:        time.Now(),
	})

	msgs := collectMsgs(cmd)
	_, moved := containsMsg[chatcmd.ChannelFocusMsg](msgs)
	require.False(t, moved, "an error at a background window must not move focus")

	generalErrs := commandErrorTexts(screen.scrollbackOf("#general"))
	require.Equal(t, []string{"topic: not a channel operator"}, generalErrs)

	require.Empty(t, commandErrorTexts(screen.scrollbackOf("#other")),
		"the error must not land in the window the user switched to")
}

// TestChatScreen_ErrorEvent_renders_at_issuing_dm_window covers the
// same routing for a command issued in a DM window: ErrorEvent.Target
// is a domain.ChannelName, and a DM window's addressable name (the
// counterpart's InstanceID) is a domain.ChannelName too, so the same
// routing that keeps a channel-issued error out of a window the user
// switched to must also keep a DM-issued one in the DM it was issued
// from. Bare `/topic` fails in a DM window because a DM is never
// persisted as a channel row (SQLiteStore.SaveWindow refuses to save
// one), so GetWindow always errors for a DM's name; that failure
// exercises the same rc.errorEvent path a channel-issued failure
// takes.
func TestChatScreen_ErrorEvent_renders_at_issuing_dm_window(t *testing.T) {
	sess, mgr, user := newTestSession(t)

	counterpart := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	require.NoError(t, sess.SaveInstance(t.Context(), counterpart))

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	dm := domain.NewDMWindow(counterpart, time.Time{})
	screen.channels.Insert(newWindow(dm))
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#other", time.Time{})))

	screen, _ = screen.focus(dm.Name())

	cmd := screen.handleCommand(components.CommandSubmitMsg{Raw: "/topic"})
	require.NotNil(t, cmd)

	// The user switches away before the async GetWindow failure
	// comes back, the same race TestChatScreen_ErrorEvent_renders_at_
	// issuing_window covers for a channel.
	screen, _ = screen.focus("#other")

	errEvent, ok := cmd().(domain.ErrorEvent)
	require.True(t, ok, "want domain.ErrorEvent, got %T", cmd())
	require.Equal(t, dm.Name(), errEvent.Target)

	_, storeErr := sess.GetWindow(t.Context(), dm.Name())
	require.Error(t, storeErr, "a DM window is never persisted, so GetWindow must fail")
	wantText := commandErrorText("topic", storeErr)

	screen, renderCmd := screen.handleErrorEvent(errEvent)

	msgs := collectMsgs(renderCmd)
	_, moved := containsMsg[chatcmd.ChannelFocusMsg](msgs)
	require.False(t, moved, "an error at a background DM window must not move focus")

	require.Equal(t, []string{wantText}, commandErrorTexts(screen.scrollbackOf(dm.Name())))
	require.Empty(t, commandErrorTexts(screen.scrollbackOf("#other")),
		"the error must not land in the window the user switched to")
}

// TestChatScreen_ErrorEvent_dm_window_closed_before_it_arrives covers
// the case the two "still open" tests above don't: the window the
// error targets can vanish between the command being issued and the
// failure coming back. appendToScrollback's placeholder-creation
// switch has no case for a closed DM (a DM window needs its
// counterpart's instance handle to rebuild, which the switch cannot
// synthesise), so routing straight to msg.Target there would drop the
// error where the user would never see it. handleErrorEvent must
// notice the window is gone and fall back to the active one, the same
// window an error always reached before ErrorEvent carried a Target
// at all.
func TestChatScreen_ErrorEvent_dm_window_closed_before_it_arrives(t *testing.T) {
	sess, mgr, user := newTestSession(t)

	counterpart := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	require.NoError(t, sess.SaveInstance(t.Context(), counterpart))

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	dm := domain.NewDMWindow(counterpart, time.Time{})
	screen.channels.Insert(newWindow(dm))
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#other", time.Time{})))

	screen, _ = screen.focus(dm.Name())

	// The command was issued while the DM was in view.
	errEvent := domain.ErrorEvent{
		Operation: "topic",
		Err:       errors.New("not a channel"),
		Target:    dm.Name(),
		At:        time.Now(),
	}

	// The user closes the DM before the failure comes back.
	// closeWindow is what /close runs on a query window.
	screen, closeCmd := screen.closeWindow(dm.Name(), time.Now())
	collectMsgs(closeCmd)
	require.Equal(t, domain.ChannelName("#other"), screen.active,
		"closing the only other window in view must land the user on #other")

	screen, cmd := screen.handleErrorEvent(errEvent)

	msgs := collectMsgs(cmd)
	_, moved := containsMsg[chatcmd.ChannelFocusMsg](msgs)
	require.False(t, moved, "the user already landed on #other when the DM closed; nothing should move focus again")

	_, reopened := screen.windowByName(dm.Name())
	require.False(t, reopened, "a late error must not recreate the closed DM window")

	require.Equal(t, []string{"topic: not a channel"}, commandErrorTexts(screen.scrollbackOf("#other")))
}

// TestChatScreen_ErrorEvent_channel_parted_before_it_arrives is the
// channel counterpart: appendToScrollback does have a case for an
// unopened channel, but it is meant for live traffic arriving before
// a join is seen. Routing a stale reply to a channel the user has
// since left through that case would resurrect #a client-side with
// no membership behind it, so handleErrorEvent's closed-window
// fallback must catch this case too.
func TestChatScreen_ErrorEvent_channel_parted_before_it_arrives(t *testing.T) {
	screen := newScreenFixture(t)
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#a", time.Time{})))
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#b", time.Time{})))

	screen, _ = screen.focus("#a")

	// The command was issued while #a was in view.
	errEvent := domain.ErrorEvent{
		Operation: "topic",
		Err:       errors.New("not a channel operator"),
		Target:    "#a",
		At:        time.Now(),
	}

	// The user parts #a and switches to #b before the failure comes
	// back. closeWindow is what /part and /close both run.
	screen, closeCmd := screen.closeWindow("#a", time.Now())
	collectMsgs(closeCmd)
	screen, _ = screen.focus("#b")

	screen, cmd := screen.handleErrorEvent(errEvent)

	msgs := collectMsgs(cmd)
	_, moved := containsMsg[chatcmd.ChannelFocusMsg](msgs)
	require.False(t, moved, "the user already switched to #b; nothing should move focus again")

	_, reappeared := screen.windowByName("#a")
	require.False(t, reappeared, "a late error must not resurrect a parted channel")

	require.Equal(t, []string{"topic: not a channel operator"}, commandErrorTexts(screen.scrollbackOf("#b")))
}

// commandErrorTexts extracts the Err field of every domain.CommandError
// in scrollback, in order.
func commandErrorTexts(scrollback []domain.Event) []string {
	var texts []string

	for _, ev := range scrollback {
		if cmdErr, ok := ev.(domain.CommandError); ok {
			texts = append(texts, cmdErr.Err)
		}
	}

	return texts
}

// TestChatScreen_MessageSubmit_on_status_channel_renders_usage_hint
// pins the chat-screen-side status-channel guard: with `&modeloff`
// active, a `MessageSubmitMsg` short-circuits to a `UsageHint`
// rather than sending. `&modeloff` is a chat-screen-owned window
// the session has no concept of, so the validation lives here.
func TestChatScreen_MessageSubmit_on_status_channel_renders_usage_hint(t *testing.T) {
	screen := newScreenFixture(t)
	screen, _ = screen.focus(domain.StatusChannelName)

	screen2, cmd := screen.Update(components.MessageSubmitMsg{Text: "hello"})

	require.NotNil(t, cmd)

	scrollback := screen2.(ChatScreen).scrollbackOf(domain.StatusChannelName)
	hints := make([]domain.UsageHint, 0, len(scrollback))
	for _, ev := range scrollback {
		if hint, ok := ev.(domain.UsageHint); ok {
			hint.At = time.Time{}
			hints = append(hints, hint)
		}
	}
	require.Equal(t, []domain.UsageHint{{
		Command: "send",
		Usage:   "the status channel doesn't take messages — try /msg <nick-or-#channel> instead",
	}}, hints)
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
	screen, _ = screen.focus("#general")

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

	// Seed the channel so the join handler finds it.
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#general", time.Time{})))
	screen, _ = screen.focus("#general")

	now := time.Now()

	bot := domain.NewModelInstance("bot-1", "oldnick", "test/model", "", nil)

	screen, _ = screen.handleJoinEvent(domain.Join{
		Target:   "#general",
		Instance: bot,
		At:       now,
	})

	cw := requireChannelWindow(t, screen, "#general")
	require.Equal(t, []domain.Member{{
		Instance: bot,
		Nick:     "oldnick",
		Modes:    domain.MemberModes{},
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
		Modes:    domain.MemberModes{},
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
	screen, _ = screen.focus("#x")

	bot := domain.NewModelInstance("bot-1", "botty", "test/model", "", nil)
	now := time.Now()
	quit := domain.Quit{Nick: "botty", Instance: bot, At: now}

	screen.bufferProtocolEvent(quit, []domain.ChannelName{"#x", "#y"})

	expected := []domain.Event{quit}

	require.Equal(t, expected, screen.scrollbackOf("#x"))
	require.Equal(t, expected, screen.scrollbackOf("#y"))
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
	screen, _ = screen.focus("#x")

	bot := domain.NewModelInstance("bot-1", "newnick", "test/model", "", nil)
	now := time.Now()
	nick := domain.NickChange{
		OldNick:  "oldnick",
		NewNick:  "newnick",
		Instance: bot,
		At:       now,
	}

	screen.bufferProtocolEvent(nick, []domain.ChannelName{"#x", "#y"})

	expected := []domain.Event{nick}

	require.Equal(t, expected, screen.scrollbackOf("#x"))
	require.Equal(t, expected, screen.scrollbackOf("#y"))
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
