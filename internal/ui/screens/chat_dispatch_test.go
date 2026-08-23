package screens

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/command"
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
		if value, ok := msg.(T); ok {
			return value, true
		}
	}

	var zero T

	return zero, false
}

func msgsTypes(msgs []tea.Msg) []string {
	types := make([]string, len(msgs))
	for i, msg := range msgs {
		types[i] = fmt.Sprintf("%T", msg)
	}

	return types
}

func TestChatScreen_ModelDispatchStarted_marks_nick_thinking(t *testing.T) {
	screen := newScreenFixture(t)

	botty := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)

	cw := domain.NewChannelWindow("#general", time.Time{})
	cw.Members.Add(botty)
	screen.channels.Insert(newWindow(cw))
	screen, _ = screen.focus("#general")

	_, cmd := screen.handleModelDispatchStarted(
		domain.ModelDispatchStarted{Source: domain.ClientSource(botty.ID(), botty.Nick())},
		protocol.ChannelWindowTarget("#general"),
	)

	require.NotNil(t, cmd)

	require.Equal(t, []tea.Msg{
		components.NickListThinkingMsg{Nicks: map[domain.Nick]bool{"botty": true}},
	}, collectMsgs(cmd))
}

func TestChatScreen_ModelDispatchStarted_is_scoped_to_the_turn_window(t *testing.T) {
	screen := newScreenFixture(t)
	botty := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)

	for _, name := range []domain.ChannelName{"#a", "#b"} {
		channel := domain.NewChannelWindow(name, time.Time{})
		channel.Members.Add(botty)
		screen.channels.Insert(newWindow(channel))
	}
	screen, _ = screen.focus("#b")

	updated, cmd := screen.handleModelDispatchStarted(
		domain.ModelDispatchStarted{Source: domain.ClientSource(botty.ID(), botty.Nick())},
		protocol.ChannelWindowTarget("#a"),
	)

	key := dispatchWindowKey{actor: botty.ID(), kind: domain.KindChannel, window: "#a"}
	require.Equal(t, map[dispatchWindowKey]domain.Nick{key: "botty"}, updated.dispatching)
	require.Equal(t, []tea.Msg{
		components.NickListThinkingMsg{Nicks: map[domain.Nick]bool{}},
	}, collectMsgs(cmd))
}

func TestChatScreen_channel_focus_refreshes_window_dispatch_activity(t *testing.T) {
	tests := map[string]struct {
		dispatchWindow domain.ChannelName
		wantThinking   map[domain.Nick]bool
	}{
		"leaving the dispatch window clears the indicator": {
			dispatchWindow: "#a",
			wantThinking:   map[domain.Nick]bool{},
		},
		"entering the dispatch window shows the indicator": {
			dispatchWindow: "#b",
			wantThinking:   map[domain.Nick]bool{"botty": true},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			screen := newScreenFixture(t)
			botty := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)

			for _, channelName := range []domain.ChannelName{"#a", "#b"} {
				channel := domain.NewChannelWindow(channelName, time.Time{})
				channel.Members.Add(botty)
				screen.channels.Insert(newWindow(channel))
			}
			screen, _ = screen.focus("#a")
			screen.dispatching[dispatchWindowKey{
				actor: botty.ID(), kind: domain.KindChannel, window: tt.dispatchWindow,
			}] = botty.Nick()

			screen, cmd := screen.handleChannelFocus(chatcmd.ChannelFocusMsg{
				Channel: "#b",
				At:      time.Unix(1, 0),
			})
			messages := collectMsgs(cmd)
			thinking := make([]components.NickListThinkingMsg, 0, 1)
			for _, message := range messages {
				if update, ok := message.(components.NickListThinkingMsg); ok {
					thinking = append(thinking, update)
				}
			}

			require.Equal(t, struct {
				Active   domain.ChannelName
				Types    []string
				Thinking []components.NickListThinkingMsg
			}{
				Active: "#b",
				Types: []string{
					"components.CompleterMsg",
					"<nil>",
					"<nil>",
					"components.SetPlaceholderMsg",
					"components.SetChannelMsg",
					"components.ChannelActiveMsg",
					"components.ChannelUnreadMsg",
					"components.NickListUpdatedMsg",
					"components.NickListThinkingMsg",
				},
				Thinking: []components.NickListThinkingMsg{{Nicks: tt.wantThinking}},
			}, struct {
				Active   domain.ChannelName
				Types    []string
				Thinking []components.NickListThinkingMsg
			}{
				Active:   screen.activeName(),
				Types:    msgsTypes(messages),
				Thinking: thinking,
			})
		})
	}
}

func TestChatScreen_ModelDispatchDone_clears_nick_thinking(t *testing.T) {
	screen := newScreenFixture(t)
	screen, _ = screen.focus("#general")

	botty := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	key := dispatchWindowKey{actor: botty.ID(), kind: domain.KindChannel, window: "#general"}
	screen.dispatching[key] = botty.Nick()

	_, cmd := screen.handleModelDispatchDone(
		domain.ModelDispatchDone{Source: domain.ClientSource(botty.ID(), botty.Nick())},
		protocol.ChannelWindowTarget("#general"),
	)

	require.NotNil(t, cmd)

	require.Equal(t, []tea.Msg{
		components.NickListThinkingMsg{},
	}, collectMsgs(cmd))
	require.Equal(t, map[dispatchWindowKey]domain.Nick{}, screen.dispatching)
}

func TestChatScreen_NickChange_updates_dispatching_nick(t *testing.T) {
	screen := newScreenFixture(t)
	botty := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	cw := domain.NewChannelWindow("#general", time.Time{})
	cw.Members.Add(botty)
	screen.channels.Insert(newWindow(cw))
	screen, _ = screen.focus("#general")
	key := dispatchWindowKey{actor: botty.ID(), kind: domain.KindChannel, window: "#general"}
	screen.dispatching[key] = botty.Nick()

	screen, cmd := screen.handleNickChangeEvent(domain.NickChange{
		Source: domain.ClientSource(botty.ID(), botty.Nick()), NewNick: "reader",
	}, []domain.ChannelName{"#general"})

	require.Equal(t, map[dispatchWindowKey]domain.Nick{key: "reader"}, screen.dispatching)
	require.Equal(t, map[domain.Nick]bool{"reader": true}, screen.thinkingNicks())

	require.Equal(t, []tea.Msg{
		components.NickListUpdatedMsg{Members: cw.Members, Revision: 1},
		components.NickListThinkingMsg{Nicks: map[domain.Nick]bool{"reader": true}},
		components.HighlightWordsMsg{Words: []string{"$nick"}, UserNick: "testuser"},
	}, collectMsgs(cmd))
}

func TestChatScreen_DM_message_updates_counterpart_nick(t *testing.T) {
	screen := newScreenFixture(t)
	dm := newDMWindow("inst-botty", "botty", time.Time{})
	screen.channels.Insert(newWindow(dm))
	screen, _ = screen.focus(dm.Name())

	screen, cmd := screen.appendMessage(dm.Name(), domain.Message{
		Source: domain.ClientSource("inst-botty", "reader"), Body: "new name",
	})
	members := domain.NewMemberList()
	members.AddIdentity("inst-botty", "reader")

	require.Equal(t, domain.Nick("reader"), dm.nick)
	require.Equal(t, []tea.Msg{
		components.ChannelAddedMsg{Channel: dm},
		components.SetChannelMsg{
			Channel: dm.Name(), DisplayName: "reader", Kind: domain.KindDM,
		},
		components.NickListUpdatedMsg{Members: members, Revision: 1},
	}, collectMsgs(cmd))
}

func TestChatScreen_DM_dispatch_activity_uses_the_counterpart_row(t *testing.T) {
	screen := newScreenFixture(t)
	dm := newDMWindow("inst-botty", "botty", time.Time{})
	screen.channels.Insert(newWindow(dm))

	screen, focused := screen.handleChannelFocus(chatcmd.ChannelFocusMsg{
		Channel: dm.Name(),
		At:      time.Unix(1, 0),
	})
	screen, started := screen.handleModelDispatchStarted(
		domain.ModelDispatchStarted{Source: domain.ClientSource("inst-botty", "botty")},
		protocol.DirectWindowTarget("inst-botty"),
	)
	_, done := screen.handleModelDispatchDone(
		domain.ModelDispatchDone{Source: domain.ClientSource("inst-botty", "botty")},
		protocol.DirectWindowTarget("inst-botty"),
	)
	members := domain.NewMemberList()
	members.AddIdentity("inst-botty", "botty")
	var focusedMembers []domain.MemberList
	for _, msg := range collectMsgs(focused) {
		if update, ok := msg.(components.NickListUpdatedMsg); ok {
			focusedMembers = append(focusedMembers, update.Members)
		}
	}

	require.Equal(t, struct {
		FocusedMembers []domain.MemberList
		Started        []tea.Msg
		Done           []tea.Msg
	}{
		FocusedMembers: []domain.MemberList{members},
		Started:        []tea.Msg{components.NickListThinkingMsg{Nicks: map[domain.Nick]bool{"botty": true}}},
		Done:           []tea.Msg{components.NickListThinkingMsg{}},
	}, struct {
		FocusedMembers []domain.MemberList
		Started        []tea.Msg
		Done           []tea.Msg
	}{
		FocusedMembers: focusedMembers,
		Started:        collectMsgs(started),
		Done:           collectMsgs(done),
	})
}

func TestChatScreen_first_DM_uses_the_observed_source(t *testing.T) {
	screen := newScreenFixture(t)
	message := domain.Message{
		Source: domain.ClientSource("inst-botty", "botty"), Body: "one last thing",
	}

	screen, cmd := screen.appendMessage("inst-botty", message)

	window, open := screen.windowByName("inst-botty")
	require.True(t, open)
	dm, direct := window.Window.(*dmWindow)
	require.True(t, direct)
	type firstDMState struct {
		Open       bool
		Window     dmWindow
		Scrollback []domain.Event
		Pending    map[domain.ChannelName][]domain.Event
	}
	require.Equal(t, firstDMState{
		Open:       true,
		Window:     dmWindow{peer: "inst-botty", nick: "botty"},
		Scrollback: []domain.Event{message},
		Pending:    map[domain.ChannelName][]domain.Event{},
	}, firstDMState{
		Open:       open,
		Window:     *dm,
		Scrollback: window.Scrollback.Events(),
		Pending:    screen.pendingDM,
	})

	require.Equal(t, []tea.Msg{
		components.ChannelAddedMsg{Channel: window.Window},
		nil,
	}, collectMsgs(cmd))
}

// TestChatScreen_ModelDispatchDone_keeps_thinking_with_concurrent_dispatch
// pins the per-instance contract: a Done for one model does not clear
// the nick-list thinking indicator while another model is still in
// its turn.
func TestChatScreen_ModelDispatchDone_keeps_thinking_with_concurrent_dispatch(t *testing.T) {
	screen := newScreenFixture(t)

	botty := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	other := domain.NewModelInstance("inst-other", "other", "test/model", "", nil)

	cw := domain.NewChannelWindow("#general", time.Time{})
	cw.Members.Add(botty)
	cw.Members.Add(other)
	screen.channels.Insert(newWindow(cw))
	screen, _ = screen.focus("#general")

	bottyKey := dispatchWindowKey{actor: botty.ID(), kind: domain.KindChannel, window: "#general"}
	otherKey := dispatchWindowKey{actor: other.ID(), kind: domain.KindChannel, window: "#general"}
	screen.dispatching[bottyKey] = botty.Nick()
	screen.dispatching[otherKey] = other.Nick()

	_, cmd := screen.handleModelDispatchDone(
		domain.ModelDispatchDone{Source: domain.ClientSource(botty.ID(), botty.Nick())},
		protocol.ChannelWindowTarget("#general"),
	)

	require.NotNil(t, cmd)

	require.Equal(t, []tea.Msg{
		components.NickListThinkingMsg{Nicks: map[domain.Nick]bool{"other": true}},
	}, collectMsgs(cmd))
	require.Equal(t, map[dispatchWindowKey]domain.Nick{otherKey: "other"}, screen.dispatching)
}

func TestChatScreen_ModelReply_queues_and_paces(t *testing.T) {
	sess, mgr, user := newTestSession(t)
	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#general")))

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#general", time.Time{})))
	screen, _ = screen.focus("#general")

	// First reply is delivered immediately (via deliverNextPacedMsg).
	first := domain.Message{
		Target: "#general",
		Source: domain.ClientSource("inst-botty", "botty"),
		Body:   "line one",
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
		Target: "#general",
		Source: domain.ClientSource("inst-botty", "botty"),
		Body:   "line two",
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

func TestChatScreen_anonymous_peer_message_is_paced_and_highlighted(t *testing.T) {
	sess, mgr, user := newTestSession(t)
	require.NoError(t, user.Join(t.Context(), "#general"))

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#general", time.Time{})))
	screen, _ = screen.focus("#general")

	message := domain.Message{
		Target: "#general",
		Source: domain.AnonymousSource(),
		Body:   "hello testuser",
	}

	updated, cmd := screen.handleMessageEvent(message)

	require.Equal(t, map[domain.ChannelName][]domain.Message{
		"#general": {message},
	}, updated.pacedQueue)
	require.True(t, updated.isHighlight(message))
	_, ok := containsMsg[deliverNextPacedMsg](collectMsgs(cmd))
	require.True(t, ok)
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
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#channel-a", time.Time{})))
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#channel-b", time.Time{})))
	screen, _ = screen.focus("#channel-a")

	// Two replies queued for #channel-a: first delivers immediately,
	// second is paced behind it.
	aFirst := domain.Message{
		Target: "#channel-a",
		Source: domain.ClientSource("inst-botty", "botty"),
		Body:   "a1",
	}
	aSecond := domain.Message{
		Target: "#channel-a",
		Source: domain.ClientSource("inst-botty", "botty"),
		Body:   "a2",
	}

	updated, _ := screen.handleMessageEvent(aFirst)
	screen = updated
	updated, _ = screen.handleMessageEvent(aSecond)
	screen = updated

	require.Equal(t, []domain.Message{aFirst, aSecond}, screen.pacedQueue["#channel-a"])

	// A reply arriving for #channel-b should ALSO trigger immediate
	// delivery — #channel-a's queue does not hold it up.
	bFirst := domain.Message{
		Target: "#channel-b",
		Source: domain.ClientSource("inst-botty", "botty"),
		Body:   "b1",
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
// empty-queue branch. The session retains the event log, but a later
// join starts a new visibility interval and does not restore this
// window's old scrollback.
func TestChatScreen_parting_channel_purges_paced_queue(t *testing.T) {
	sess, mgr, user := newTestSession(t)
	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#x")))

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)
	screen, _ = screen.focus("#x")

	queued := []domain.Message{
		{Target: "#x", Source: domain.ClientSource("inst-botty", "botty"), Body: "one"},
		{Target: "#x", Source: domain.ClientSource("inst-botty", "botty"), Body: "two"},
	}
	screen.pacedQueue["#x"] = queued

	// User parts #x — the handler drops both the channel and its
	// pending-paced queue entry.
	updated, _ := screen.handlePartEvent(domain.Part{
		Target: "#x",
		Source: domain.ClientSource(protocol.UserClientID, user.Nick()),
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

func TestChatScreen_being_kicked_closes_the_channel_window(t *testing.T) {
	stored := storetest.NewMemoryStore(t)
	sess, mgr, user := uitest.NewTestSession(
		t, stored, stubAPI{}, nil, nil, "", "", t.Context,
	)
	require.NoError(t, user.Join(t.Context(), "#x"))

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)
	screen, _ = screen.focus("#x")
	screen.pacedQueue["#x"] = []domain.Message{{Target: "#x", Body: "queued"}}

	updated, cmd := screen.handleKickedEvent(domain.Kicked{
		Target:        "#x",
		Source:        domain.LegacyClientSource("operator"),
		Subject:       "nick-before-kick",
		SubjectIsSelf: true,
	})
	collectMsgs(cmd)

	_, open := updated.windowByName("#x")
	require.False(t, open)
	require.Empty(t, updated.scrollbackOf("#x"))
	require.NotContains(t, updated.pacedQueue, domain.ChannelName("#x"))

	autojoin, err := stored.ListAutojoinChannels(t.Context())
	require.NoError(t, err)
	require.Empty(t, autojoin)
}

func TestChatScreen_user_mode_change_does_not_depend_on_a_later_nick(t *testing.T) {
	screen := newScreenFixture(t)

	_, cmd := screen.handleUserModeChangeEvent(domain.UserModeChange{
		Subject: "nick-before-mode-delivery",
		Flag:    domain.ModeOperator,
		Add:     true,
	})

	require.Equal(t, []tea.Msg{components.CommandsMsg[chatcmd.CompletionContext]{
		Commands: command.VisibleCommands(screen.parser.Set(), screen.client.Caps()),
	}}, collectMsgs(cmd))
}

func TestChatScreen_masked_join_keeps_the_anonymous_member_projection(t *testing.T) {
	screen := newScreenFixture(t)
	window := domain.NewChannelWindow("#anon", time.Time{})
	window.Modes.Anonymous = true
	window.Members = domain.AnonymousMembers()
	screen.channels.Insert(newWindow(window))

	updated, _ := screen.handleJoinEvent(domain.Join{
		Target: "#anon",
		Source: domain.AnonymousSource(),
	})

	got, ok := updated.channelWindowByName("#anon")
	require.True(t, ok)
	require.Equal(t, []domain.Nick{domain.AnonymousNick}, slices.Collect(got.Members.Nicks()))
}

func TestChatScreen_handleProtocolEvent_routing(t *testing.T) {
	tests := []struct {
		name   string
		event  protocol.Event
		window protocol.WindowTarget
		want   []tea.Msg
	}{
		{
			name:   "ModelDispatchStarted routes to nick-list thinking",
			event:  domain.ModelDispatchStarted{Source: domain.ClientSource("inst-botty", "botty")},
			window: protocol.ChannelWindowTarget("#general"),
			want:   []tea.Msg{components.NickListThinkingMsg{Nicks: map[domain.Nick]bool{}}},
		},
		{
			name:   "ModelDispatchDone routes to nick-list thinking clear",
			event:  domain.ModelDispatchDone{Source: domain.ClientSource("inst-botty", "botty")},
			window: protocol.ChannelWindowTarget("#general"),
			want:   []tea.Msg{components.NickListThinkingMsg{}},
		},
		{
			name: "Message from model routes to paced delivery",
			event: domain.Message{
				Target: "#general",
				Source: domain.ClientSource("inst-botty", "botty"),
				Body:   "hi",
			},
			want: []tea.Msg{deliverNextPacedMsg{Channel: "#general"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			screen := newScreenFixture(t)
			screen.channels.Insert(newWindow(domain.NewChannelWindow("#general", time.Time{})))
			screen, _ = screen.focus("#general")

			_, cmd := screen.applyProtocolEvent(protocolEventMsg{event: tt.event, window: tt.window})
			require.Equal(t, tt.want, collectMsgs(cmd))
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
		Source: domain.LegacyClientSource("botty"),
		At:     time.Now(),
	}

	screen, _ = screen.handleProtocolEvent(protocolEventMsg{
		event: failure, window: protocol.ChannelWindowTarget("#general"),
	})

	require.Equal(t, []string{failure.Error()}, scrollbackSystemNotices(screen.scrollbackOf("#general")))
	require.Empty(t, scrollbackSystemNotices(screen.scrollbackOf("#other")),
		"the failure must not land in the window the user switched to")
}

func TestChatScreen_ModelUnavailableError_renders_in_the_recipient_DM(t *testing.T) {
	screen := newScreenFixture(t)
	dm := newDMWindow("inst-botty", "botty", time.Time{})
	screen.channels.Insert(newWindow(dm))
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#other", time.Time{})))
	screen, _ = screen.focus("#other")

	at := time.Unix(1, 0)
	failure := domain.ModelUnavailableError{
		Source: domain.ClientSource("inst-botty", "botty"), At: at,
	}
	updated, _ := screen.handleProtocolEvent(protocolEventMsg{
		event: failure, window: protocol.DirectWindowTarget("inst-botty"),
	})
	notice := domain.SystemNotice{
		Target: dm.Name(), Text: failure.Error(), At: at,
	}

	require.Equal(t, struct {
		Active         domain.ChannelName
		DirectMessages []domain.Event
		OtherChannel   []domain.Event
	}{
		Active:         "#other",
		DirectMessages: []domain.Event{notice},
	}, struct {
		Active         domain.ChannelName
		DirectMessages []domain.Event
		OtherChannel   []domain.Event
	}{
		Active:         updated.activeName(),
		DirectMessages: updated.scrollbackOf(dm.Name()),
		OtherChannel:   updated.scrollbackOf("#other"),
	})
}

func TestChatScreen_ModelUnavailableError_from_another_DM_renders_in_status(t *testing.T) {
	screen := newScreenFixture(t)
	peer := newDMWindow("inst-alice", "alice", time.Time{})
	screen.channels.Insert(newWindow(peer))
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#other", time.Time{})))
	screen, _ = screen.focus("#other")

	at := time.Unix(1, 0)
	failure := domain.ModelUnavailableError{
		Source: domain.ClientSource("inst-botty", "botty"), At: at,
	}
	updated, _ := screen.handleProtocolEvent(protocolEventMsg{event: failure})

	require.Equal(t, struct {
		Active       domain.ChannelName
		Status       []domain.Event
		DirectPeer   []domain.Event
		OtherChannel []domain.Event
	}{
		Active: "#other",
		Status: []domain.Event{domain.SystemNotice{
			Target: domain.StatusChannelName,
			Text:   `model "botty" unavailable for dispatch`,
			At:     at,
		}},
	}, struct {
		Active       domain.ChannelName
		Status       []domain.Event
		DirectPeer   []domain.Event
		OtherChannel []domain.Event
	}{
		Active:       updated.activeName(),
		Status:       updated.scrollbackOf(domain.StatusChannelName),
		DirectPeer:   updated.scrollbackOf(peer.Name()),
		OtherChannel: updated.scrollbackOf("#other"),
	})
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
		Source: domain.LegacyClientSource("botty"),
		At:     time.Now(),
	}

	screen, _ = screen.handleProtocolEvent(protocolEventMsg{
		event: failure, window: protocol.ChannelWindowTarget("#gone"),
	})

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
// exercises the same issuing-window result path a channel-issued failure
// takes.
func TestChatScreen_ErrorEvent_renders_at_issuing_dm_window(t *testing.T) {
	sess, mgr, user, eventStore := newTestSessionWithStore(t)

	counterpart := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	require.NoError(t, eventStore.SaveInstance(t.Context(), counterpart))

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	dm := newDMWindow(counterpart.ID(), counterpart.Nick(), time.Time{})
	screen.channels.Insert(newWindow(dm))
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#other", time.Time{})))

	screen, _ = screen.focus(dm.Name())

	cmd := screen.handleCommand(components.CommandSubmitMsg{Raw: "/topic"})
	require.NotNil(t, cmd)

	// The user switches away before the async GetWindow failure
	// comes back, the same race TestChatScreen_ErrorEvent_renders_at_
	// issuing_window covers for a channel.
	screen, _ = screen.focus("#other")

	result, ok := cmd().(chatcmd.CommandResult)
	require.True(t, ok)
	errorResult, ok := result.Message.(chatcmd.CommandErrorResult)
	require.True(t, ok)
	wantText := commandErrorText("topic", errorResult.Error.Err)

	screen, renderCmd, handled := screen.routeReplies(result)
	messages := collectMsgs(renderCmd)

	require.Equal(t, struct {
		IssuingWindow domain.Window
		Handled       bool
		Active        domain.ChannelName
		DMScrollback  []domain.Event
		Other         []domain.Event
		Messages      []tea.Msg
	}{
		IssuingWindow: dm,
		Handled:       true,
		Active:        "#other",
		DMScrollback: []domain.Event{domain.CommandError{
			Target: dm.Name(), Err: wantText, At: errorResult.Error.At,
		}},
		Messages: []tea.Msg{
			components.ScrollbackUpdatedMsg{Channel: dm.Name()},
			nil,
			components.NickListThinkingMsg{},
		},
	}, struct {
		IssuingWindow domain.Window
		Handled       bool
		Active        domain.ChannelName
		DMScrollback  []domain.Event
		Other         []domain.Event
		Messages      []tea.Msg
	}{
		IssuingWindow: result.IssuingWindow,
		Handled:       handled,
		Active:        screen.active.Name(),
		DMScrollback:  screen.scrollbackOf(dm.Name()),
		Other:         screen.scrollbackOf("#other"),
		Messages:      messages,
	})
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
	sess, mgr, user, eventStore := newTestSessionWithStore(t)

	counterpart := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	require.NoError(t, eventStore.SaveInstance(t.Context(), counterpart))

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	dm := newDMWindow(counterpart.ID(), counterpart.Nick(), time.Time{})
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
	require.Equal(t, domain.ChannelName("#other"), screen.active.Name(),
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
	apiClient := &uitest.FakeAPI{
		GenerateNickFn: func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error) {
			return "outsider", nil
		},
	}
	sess, mgr, user := uitest.NewTestSession(t, s, apiClient, nil, nil, "", "", t.Context)
	require.NoError(t, user.Join(ctx, "#else"))
	uitest.AddModel(t, user, "#else", "test/model", "")

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

// the stable instance ID, so later lifecycle events can carry
// independent actor snapshots and still update the same entry.
func TestChatScreen_NickChange_then_Quit_removes_instance(t *testing.T) {
	screen := newScreenFixture(t)

	// Seed the channel so the join handler finds it.
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#general", time.Time{})))
	screen, _ = screen.focus("#general")

	now := time.Now()

	bot := domain.NewModelInstance("bot-1", "oldnick", "test/model", "", nil)

	screen, _ = screen.handleJoinEvent(domain.Join{
		Target: "#general",
		Source: domain.ClientSource(bot.ID(), bot.Nick()),
		At:     now,
	})

	cw := requireChannelWindow(t, screen, "#general")
	require.Equal(t, []domain.Member{{
		InstanceID: bot.ID(),
		Nick:       "oldnick",
		Modes:      domain.MemberModes{},
	}}, slices.Collect(cw.Members.All()))

	// Rename: the session mutates the instance's own nick before
	// emitting the event, so the handle's Nick() is already the new
	// value. The channel member list's snapshot must be updated in
	// place via RenameTo so sort order stays correct.
	bot.SetNick("newnick")

	_, _ = screen.handleNickChangeEvent(domain.NickChange{
		Source:  domain.ClientSource(bot.ID(), "oldnick"),
		NewNick: "newnick",
		At:      now,
	}, []domain.ChannelName{"#general"})

	cw = requireChannelWindow(t, screen, "#general")
	require.Equal(t, []domain.Member{{
		InstanceID: bot.ID(),
		Nick:       "newnick",
		Modes:      domain.MemberModes{},
	}}, slices.Collect(cw.Members.All()),
		"nick change should sync the member snapshot while preserving identity")

	// A separate snapshot with the same ID still removes the member.
	_, _ = screen.handleQuitEvent(domain.Quit{
		Source: domain.ClientSource(bot.ID(), "newnick"),
		At:     now,
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
	quit := domain.Quit{Source: domain.ClientSource(bot.ID(), "botty"), At: now}

	screen, _ = screen.bufferProtocolEvent(quit, []domain.ChannelName{"#x", "#y"}, nil)

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
		Source:  domain.ClientSource(bot.ID(), "oldnick"),
		NewNick: "newnick",
		At:      now,
	}

	screen, _ = screen.bufferProtocolEvent(nick, []domain.ChannelName{"#x", "#y"}, nil)

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
