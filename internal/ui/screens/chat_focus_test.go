package screens

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/ui/uitest"
)

type dmWindowReadFailureStore struct {
	*store.SQLiteStore

	err error
}

type lastWindowBlockingStore struct {
	*store.SQLiteStore

	started chan struct{}
	release chan struct{}
}

func (s lastWindowBlockingStore) SetLastWindow(
	ctx context.Context,
	window domain.Window,
) error {
	select {
	case s.started <- struct{}{}:
	default:
	}

	select {
	case <-s.release:
		return s.SQLiteStore.SetLastWindow(ctx, window)
	case <-ctx.Done():
		return ctx.Err()
	}
}

type queuedEventClient struct {
	protocol.Client

	events <-chan protocol.Delivery
}

func TestChatScreen_setChannelCmd_keeps_dm_identity_out_of_the_header(t *testing.T) {
	tests := []struct {
		name string
		peer domain.InstanceID
		nick domain.Nick
	}{
		{name: "model", peer: "inst-botty", nick: "botty"},
		{name: "self", peer: protocol.UserClientID, nick: "testuser"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			screen := newScreenFixture(t)
			dm := newDMWindow(tc.peer, tc.nick, time.Time{})
			screen.channels.Insert(newWindow(dm))
			screen = focused(t, screen, dm.Name())

			require.Equal(t, []tea.Msg{components.SetChannelMsg{
				Channel:     domain.ChannelName(tc.peer),
				DisplayName: string(tc.nick),
				Kind:        domain.KindDM,
			}}, collectMsgs(screen.setChannelCmd()))
		})
	}
}

func (c queuedEventClient) Events() <-chan protocol.Delivery { return c.events }

func (s dmWindowReadFailureStore) ListDMWindows(context.Context) ([]domain.InstanceID, error) {
	return nil, s.err
}

// channelBodies reads the message bodies each named channel's event
// log holds. Collecting every named channel in one map lets a caller
// assert on where a send landed across all of them at once.
func channelBodies(t *testing.T, sess *session.Session, names ...domain.ChannelName) map[domain.ChannelName][]string {
	t.Helper()

	out := make(map[domain.ChannelName][]string, len(names))

	for _, name := range names {
		stored, err := sess.AuditEventsBefore(t.Context(), name, nil, 100)
		require.NoError(t, err)

		bodies := []string{}

		for _, ev := range stored {
			if msg, ok := ev.Event.(domain.Message); ok {
				bodies = append(bodies, msg.Body)
			}
		}

		out[name] = bodies
	}

	return out
}

// TestChatScreen_send_targets_the_channel_it_was_typed_in pins the
// value-semantics contract for the focused window: the send command
// a submit returns carries the window the line was typed in, so a
// channel switch between the submit and the command running cannot
// redirect the line. Bubble Tea runs a returned `tea.Cmd` on its own
// goroutine at an unspecified later point, which is exactly the
// window in which a user can hit a channel-switch key.
func TestChatScreen_send_targets_the_channel_it_was_typed_in(t *testing.T) {
	sess, mgr, user := newTestSession(t)
	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#general")))
	require.NoError(t, user.Join(t.Context(), domain.ChannelName("#random")))

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	for _, name := range []domain.ChannelName{"#general", "#random"} {
		screen.channels.Insert(newWindow(domain.NewChannelWindow(name, time.Time{})))
	}

	screen, _ = screen.focus("#general")

	updated, send := screen.Update(components.MessageSubmitMsg{Text: "typed in general"})
	screen = updated.(ChatScreen)
	require.NotNil(t, send)

	// The user switches window before the send command is run.
	updated, _ = screen.Update(chatcmd.ChannelFocusMsg{Channel: "#random", At: time.Now()})
	screen = updated.(ChatScreen)

	require.Nil(t, send(), "a successful send renders through the echo-message bus, not a message")

	require.Equal(t, map[domain.ChannelName][]string{
		"#general": {"typed in general"},
		"#random":  {},
	}, channelBodies(t, sess, "#general", "#random"))
}

func TestChatScreen_stale_query_opens_without_replacing_newer_focus(t *testing.T) {
	screen := newScreenFixture(t)
	intentAt := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)

	newer := newWindow(domain.NewChannelWindow("#newer", intentAt.Add(time.Minute)))
	screen.channels.Insert(newer)
	screen, focusCmd := screen.handleChannelFocus(chatcmd.ChannelFocusMsg{
		Channel: newer.Name(), At: intentAt.Add(time.Minute),
	})
	collectMsgs(focusCmd)

	screen, queryCmd := screen.handleDMOpenedMsg(chatcmd.DMOpenedMsg{
		CounterpartID: "inst-botty", CounterpartNick: "botty",
		Focus: true, At: intentAt,
	})
	collectMsgs(queryCmd)

	dm, open := screen.windowByName("inst-botty")
	type assertionSnapshot struct {
		Active     domain.ChannelName
		FocusAt    time.Time
		DMOpen     bool
		DMActivity bool
	}

	require.Equal(t, assertionSnapshot{
		Active: "#newer", FocusAt: intentAt.Add(time.Minute),
		DMOpen: true, DMActivity: true,
	}, assertionSnapshot{
		Active:     screen.activeName(),
		FocusAt:    screen.focusAt,
		DMOpen:     open,
		DMActivity: dm != nil && dm.Activity,
	})
}

func TestChatScreen_delayed_part_preserves_fallback_focus_time(t *testing.T) {
	screen := newScreenFixture(t)
	departureAt := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	focusedAt := departureAt.Add(time.Minute)
	competingAt := departureAt.Add(30 * time.Second)

	fallback := newWindow(domain.NewChannelWindow("#a", time.Time{}))
	fallback.UserTime = focusedAt
	competing := newWindow(domain.NewChannelWindow("#b", time.Time{}))
	closing := newWindow(domain.NewChannelWindow("#z", time.Time{}))
	screen.channels.Insert(fallback)
	screen.channels.Insert(competing)
	screen.channels.Insert(closing)
	screen, _ = screen.focus(closing.Name())
	screen.focusAt = departureAt.Add(-time.Minute)

	screen, closeCmd := screen.handlePartEvent(domain.Part{
		Target: closing.Name(),
		Source: domain.ClientSource(protocol.UserClientID, screen.user.Nick()),
		At:     departureAt,
	})
	collectMsgs(closeCmd)
	screen, competingCmd := screen.handleChannelFocus(chatcmd.ChannelFocusMsg{
		Channel: competing.Name(),
		At:      competingAt,
	})
	collectMsgs(competingCmd)

	type assertionSnapshot struct {
		Active           domain.ChannelName
		FocusAt          time.Time
		FallbackUserTime time.Time
		CompetingActive  bool
	}

	require.Equal(t, assertionSnapshot{
		Active: "#a", FocusAt: focusedAt, FallbackUserTime: focusedAt,
		CompetingActive: true,
	}, assertionSnapshot{
		Active:           screen.activeName(),
		FocusAt:          screen.focusAt,
		FallbackUserTime: fallback.UserTime,
		CompetingActive:  competing.Activity,
	})
}

func TestChatScreen_join_focus_keeps_the_original_intent_time(t *testing.T) {
	screen := newScreenFixture(t)
	intentAt := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	navigatedAt := intentAt.Add(time.Minute)
	joinedAt := navigatedAt.Add(time.Minute)

	newer := newWindow(domain.NewChannelWindow("#newer", navigatedAt))
	screen.channels.Insert(newer)
	screen, focusCmd := screen.handleChannelFocus(chatcmd.ChannelFocusMsg{
		Channel: newer.Name(),
		At:      navigatedAt,
	})
	collectMsgs(focusCmd)

	screen, pendingCmd := screen.handleChannelJoinFocus(chatcmd.ChannelJoinFocusMsg{
		Channel: "#joined",
		At:      intentAt,
	})
	require.Nil(t, pendingCmd)

	screen, joinCmd := screen.handleJoinEvent(domain.Join{
		Target: "#joined",
		Source: domain.ClientSource(protocol.UserClientID, screen.user.Nick()),
		At:     joinedAt,
	})
	collectMsgs(joinCmd)
	screen, topicCmd := screen.handleTopicInfoEvent(domain.TopicInfo{
		Target: "#joined",
		Topic:  "release topic",
	})
	collectMsgs(topicCmd)
	members := domain.MemberList{}
	members.AddIdentity(protocol.UserClientID, screen.user.Nick())
	members.AddIdentity("inst-botty", "botty")
	screen, namesCmd := screen.handleNamesReply(domain.NamesReplyEvent{
		Channel: "#joined",
		Members: members,
	})
	collectMsgs(namesCmd)

	joined, open := screen.windowByName("#joined")
	var joinedTopic string
	var joinedMembers []domain.Member
	var joinedChannel *domain.ChannelWindow
	if joined != nil {
		joinedChannel, _ = joined.Window.(*domain.ChannelWindow)
	}
	if joinedChannel != nil {
		joinedTopic = joinedChannel.Topic
		joinedMembers = slices.Collect(joinedChannel.Members.All())
	}
	type assertionSnapshot struct {
		Active           domain.ChannelName
		FocusAt          time.Time
		JoinedOpen       bool
		JoinedActivity   bool
		JoinedTopic      string
		JoinedMembers    []domain.Member
		PendingJoinFocus map[domain.ChannelName]time.Time
		JoinReplyDone    map[domain.ChannelName]bool
	}

	require.Equal(t, assertionSnapshot{
		Active:         "#newer",
		FocusAt:        navigatedAt,
		JoinedOpen:     true,
		JoinedActivity: false,
		JoinedTopic:    "release topic",
		JoinedMembers: []domain.Member{
			{InstanceID: "inst-botty", Nick: "botty"},
			{InstanceID: protocol.UserClientID, Nick: screen.user.Nick()},
		},
		PendingJoinFocus: map[domain.ChannelName]time.Time{"#joined": intentAt},
		JoinReplyDone:    map[domain.ChannelName]bool{"#joined": false},
	}, assertionSnapshot{
		Active:           screen.activeName(),
		FocusAt:          screen.focusAt,
		JoinedOpen:       open,
		JoinedActivity:   joined != nil && joined.Activity,
		JoinedTopic:      joinedTopic,
		JoinedMembers:    joinedMembers,
		PendingJoinFocus: screen.pendingJoinFocus,
		JoinReplyDone:    screen.joinReplyDone,
	})

	screen, endCmd := screen.applyProtocolEvent(protocolEventMsg{event: domain.NamesEnd{
		Channel: "#joined",
		At:      joinedAt,
	}})
	collectMsgs(endCmd)

	require.Equal(t, assertionSnapshot{
		Active:         "#newer",
		FocusAt:        navigatedAt,
		JoinedOpen:     true,
		JoinedActivity: true,
		JoinedTopic:    "release topic",
		JoinedMembers: []domain.Member{
			{InstanceID: "inst-botty", Nick: "botty"},
			{InstanceID: protocol.UserClientID, Nick: screen.user.Nick()},
		},
		PendingJoinFocus: map[domain.ChannelName]time.Time{},
		JoinReplyDone:    map[domain.ChannelName]bool{"#joined": true},
	}, assertionSnapshot{
		Active:           screen.activeName(),
		FocusAt:          screen.focusAt,
		JoinedOpen:       open,
		JoinedActivity:   joined != nil && joined.Activity,
		JoinedTopic:      joinedTopic,
		JoinedMembers:    joinedMembers,
		PendingJoinFocus: screen.pendingJoinFocus,
		JoinReplyDone:    screen.joinReplyDone,
	})
}

func TestChatScreen_dm_restore_failure_uses_the_channel_fallback(t *testing.T) {
	backing := storetest.NewMemoryStore(t)
	failingStore := dmWindowReadFailureStore{
		SQLiteStore: backing,
		err:         errors.New("read dm windows"),
	}
	sess, mgr, user := uitest.NewTestSession(
		t, failingStore, stubAPI{}, nil, nil, "", "", t.Context,
	)
	require.NoError(t, user.Join(t.Context(), "#general"))
	require.NoError(t, backing.SetLastWindow(t.Context(), domain.WindowKey("inst-gone")))

	screen, err := NewChatScreen(
		t.Context, sess, mgr, user, nil, failingStore, domain.KindStatus,
	)
	require.NoError(t, err)
	screen.markCurrentJoinRepliesComplete()
	screen.bootstrapFromSession(false)

	msg, ok := screen.restoreDMWindows()().(dmWindowsRestoredMsg)
	require.True(t, ok)
	screen, command := screen.handleDMWindowsRestored(msg)

	for _, message := range collectMsgs(command) {
		screen, _, _ = screen.routeWindows(message)
	}

	_, generalOpen := screen.windowByName("#general")
	type assertionSnapshot struct {
		Active        domain.ChannelName
		DMRestoreDone bool
		GeneralOpen   bool
	}

	require.Equal(t, assertionSnapshot{
		Active:        "#general",
		DMRestoreDone: true,
		GeneralOpen:   true,
	}, assertionSnapshot{
		Active:        screen.activeName(),
		DMRestoreDone: screen.dmRestoreDone,
		GeneralOpen:   generalOpen,
	})
}

func TestChatScreen_startup_waits_for_all_autojoin_channels_before_focusing(t *testing.T) {
	type startupState struct {
		Active          domain.ChannelName
		DMRestoreDone   bool
		AutojoinPending bool
	}
	type assertionSnapshot struct {
		BeforeAutojoin startupState
		Active         domain.ChannelName
		SavedOpen      bool
		SavedActivity  bool
	}

	screen := newScreenFixture(t)
	connection := NewConnectionScreen(ConnectionConfig{Session: fakeConnector{}}, screen)
	require.IsType(t, ChatScreen{}, connection.chatScreen)
	screen = connection.chatScreen.(ChatScreen)
	landingAt := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	require.NoError(t, screen.user.Join(t.Context(), "#early"))
	screen.markCurrentJoinRepliesComplete()
	screen.bootstrapFromSession(false)

	screen, restoreCmd := screen.handleDMWindowsRestored(dmWindowsRestoredMsg{
		landing:   domain.NewChannelWindow("#saved", time.Time{}),
		landingAt: landingAt,
	})
	for _, message := range collectMsgs(restoreCmd) {
		screen, _, _ = screen.routeWindows(message)
	}

	beforeAutojoin := startupState{
		Active:          screen.activeName(),
		DMRestoreDone:   screen.dmRestoreDone,
		AutojoinPending: screen.autojoinPending,
	}

	require.NoError(t, screen.user.Join(t.Context(), "#saved"))
	screen.joinReplyDone["#saved"] = true
	screen.autojoinPending = false
	for _, command := range screen.bootstrapFromSession(true) {
		message := command()
		var cmd tea.Cmd
		screen, cmd, _ = screen.routeWindows(message)
		collectMsgs(cmd)
	}

	saved, savedOpen := screen.windowByName("#saved")
	require.Equal(t, assertionSnapshot{
		BeforeAutojoin: startupState{
			DMRestoreDone:   true,
			AutojoinPending: true,
		},
		Active:    "#saved",
		SavedOpen: true,
	}, assertionSnapshot{
		BeforeAutojoin: beforeAutojoin,
		Active:         screen.activeName(),
		SavedOpen:      savedOpen,
		SavedActivity:  saved != nil && saved.Activity,
	})
}

func TestChatScreen_protocol_reader_does_not_wait_for_focus_persistence(t *testing.T) {
	backing := storetest.NewMemoryStore(t)
	blockingStore := lastWindowBlockingStore{
		SQLiteStore: backing,
		started:     make(chan struct{}, 1),
		release:     make(chan struct{}),
	}
	defer close(blockingStore.release)

	sess, mgr, user := uitest.NewTestSession(
		t, blockingStore, stubAPI{}, nil, nil, "", "", t.Context,
	)
	screen, err := NewChatScreen(
		t.Context, sess, mgr, user, nil, blockingStore, domain.KindStatus,
	)
	require.NoError(t, err)

	joined := domain.NewChannelWindow("#joined", time.Time{})
	screen.channels.Insert(newWindow(joined))
	screen.pendingJoinFocus["#joined"] = time.Now()
	screen.joinReplyDone["#joined"] = false

	next := protocolEventMsg{event: domain.SystemNotice{
		Target: "#joined",
		Text:   "next delivery",
	}}
	events := make(chan protocol.Delivery, 1)
	events <- protocol.Delivery{Event: next.event}
	screen.client = queuedEventClient{Client: screen.client, events: events}

	screen, effect := screen.handleProtocolEvent(protocolEventMsg{event: domain.NamesEnd{
		Channel: "#joined",
	}})
	require.NotNil(t, effect)
	<-blockingStore.started

	batch, ok := effect().(tea.BatchMsg)
	require.True(t, ok)

	type scheduledEffect struct {
		ProtocolEffect bool
		Event          domain.Event
	}
	scheduled := make([]scheduledEffect, 0, len(batch))
	for _, command := range batch {
		switch msg := command().(type) {
		case protocolEffectResultMsg:
			scheduled = append(scheduled, scheduledEffect{ProtocolEffect: true})
		case protocolEventMsg:
			scheduled = append(scheduled, scheduledEffect{Event: msg.event})
		}
	}

	require.Equal(t, []scheduledEffect{
		{ProtocolEffect: true},
		{Event: next.event},
	}, scheduled)
}

func TestChatScreen_protocol_effects_follow_delivery_order(t *testing.T) {
	screen := newScreenFixture(t)
	firstMsg := components.NickListThinkingMsg{
		Nicks: map[domain.Nick]bool{"botty": true},
	}
	lastMsg := components.NickListThinkingMsg{}

	screen, first := screen.enqueueProtocolEffects(msgCmd(firstMsg))
	screen, queued := screen.enqueueProtocolEffects(msgCmd(lastMsg))
	require.Nil(t, queued)

	var delivered []tea.Msg
	for next := first; next != nil; {
		result, ok := next().(protocolEffectResultMsg)
		require.True(t, ok)
		delivered = append(delivered, result.msg)
		screen, next = screen.handleProtocolEffectDone()
	}

	require.Equal(t, []tea.Msg{firstMsg, lastMsg}, delivered)
}

func collectDeliveryEffects(
	t *testing.T,
	screen ChatScreen,
	cmd tea.Cmd,
) (ChatScreen, []tea.Msg) {
	t.Helper()

	batch, ok := cmd().(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 2, "a protocol delivery must schedule its effects and the next bus read")

	result, ok := batch[0]().(protocolEffectResultMsg)
	require.True(t, ok)

	var delivered []tea.Msg
	for {
		delivered = append(delivered, collectMsgs(func() tea.Msg { return result.msg })...)

		var next tea.Cmd
		screen, next = screen.handleProtocolEffectDone()
		if next == nil {
			return screen, delivered
		}

		result, ok = next().(protocolEffectResultMsg)
		require.True(t, ok)
	}
}

func TestChatScreen_own_nick_change_in_focused_self_DM_is_buffered_once(t *testing.T) {
	screen := newScreenFixture(t)
	channel := newWindow(domain.NewChannelWindow("#general", time.Time{}))
	dm := newDMWindow(screen.user.ID(), screen.user.Nick(), time.Time{})
	selfDM := newWindow(dm)
	screen.channels.Insert(channel)
	screen.channels.Insert(selfDM)
	screen = focused(t, screen, dm.Name())

	initialRevision := selfDM.Revision
	initialFirstSeq := selfDM.Scrollback.FirstSeq()
	initialNickListRevision := screen.nickListRevision
	change := domain.NickChange{
		Source:  domain.ClientSource(screen.user.ID(), screen.user.Nick()),
		NewNick: "newnick",
	}

	screen, cmd := screen.handleProtocolEvent(protocolEventMsg{
		event:   change,
		targets: []domain.ChannelName{"#general"},
	})
	screen, effects := collectDeliveryEffects(t, screen, cmd)

	members := domain.NewMemberList()
	members.AddIdentity(screen.user.ID(), "newnick")

	type windowState struct {
		Name        domain.ChannelName
		DisplayName string
		Kind        domain.ChannelKind
		Events      []domain.Event
		Revision    uint64
		Visits      int
		Unread      int
		Mentions    bool
		Activity    bool
		Active      bool
	}
	state := func(window *Window) windowState {
		return windowState{
			Name:        window.Name(),
			DisplayName: window.DisplayName(),
			Kind:        window.Kind(),
			Events:      window.Scrollback.Events(),
			Revision:    window.Revision,
			Visits:      window.Visits,
			Unread:      window.Unread,
			Mentions:    window.Mentions,
			Activity:    window.Activity,
			Active:      screen.active == window,
		}
	}

	type assertionSnapshot struct {
		SelfDM        windowState
		SharedChannel windowState
		Visible       components.WindowContent
		ChecklistNick domain.Nick
		Effects       []tea.Msg
	}

	require.Equal(t, assertionSnapshot{
		SelfDM: windowState{
			DisplayName: "newnick",
			Kind:        domain.KindDM,
			Events:      []domain.Event{change},
			Revision:    initialRevision + 1,
			Visits:      1,
			Active:      true,
		},
		SharedChannel: windowState{
			Name:        "#general",
			DisplayName: "#general",
			Kind:        domain.KindChannel,
			Events:      []domain.Event{change},
			Revision:    1,
		},
		Visible: components.WindowContent{
			Events:   []domain.Event{change},
			FirstSeq: initialFirstSeq,
		},
		ChecklistNick: "newnick",
		Effects: []tea.Msg{
			components.ChannelAddedMsg{Channel: dm},
			components.SetChannelMsg{DisplayName: "newnick", Kind: domain.KindDM},
			components.NickListUpdatedMsg{
				Members: members, Revision: initialNickListRevision + 1,
			},
			components.UserNickMsg{Nick: "newnick"},
			components.HighlightWordsMsg{Words: screen.highlightWords, UserNick: "newnick"},
			components.ChannelHasLifecycleMsg{Channel: "#general"},
			components.ScrollbackUpdatedMsg{},
		},
	}, assertionSnapshot{
		SelfDM:        state(selfDM),
		SharedChannel: state(channel),
		Visible:       screen.visible.content(),
		ChecklistNick: screen.checklist.nick,
		Effects:       effects,
	})
}

// TestChatScreen_own_nick_change_confirms_in_the_focused_window pins
// the user-side NICK feedback: the prompt nick, the highlight set and
// the checklist follow the user whatever window is in view, and the
// confirmation line is rendered in the focused window exactly when
// the fan-out did not already file it there. `&modeloff` is never a
// NICK target, so the status-window case covers a rename the fan-out
// reaches no visible window with.
func TestChatScreen_own_nick_change_confirms_in_the_focused_window(t *testing.T) {
	targets := []domain.ChannelName{"#general"}

	tests := map[string]struct {
		focused          domain.ChannelName
		wantMsgs         []string
		wantConfirmation bool
	}{
		"focused window is not a target": {
			focused: domain.StatusChannelName,
			wantMsgs: []string{
				"components.UserNickMsg",
				"components.HighlightWordsMsg",
				"components.ScrollbackUpdatedMsg",
				"components.ChannelHasLifecycleMsg",
			},
			wantConfirmation: true,
		},
		"focused window is a target": {
			focused: "#general",
			wantMsgs: []string{
				"components.NickListUpdatedMsg",
				"components.UserNickMsg",
				"components.HighlightWordsMsg",
			},
			// The fan-out files the line into every target window
			// before the handler runs, so the handler adds nothing.
			wantConfirmation: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			screen := newScreenFixture(t)

			screen.channels.Insert(newWindow(domain.NewStatusWindow(time.Time{})))
			screen.channels.Insert(newWindow(domain.NewChannelWindow("#general", time.Time{})))

			screen, _ = screen.focus(tc.focused)

			change := domain.NickChange{
				Source:  domain.ClientSource(screen.user.ID(), "testuser"),
				NewNick: "newnick",
			}

			screen, cmd := screen.handleNickChangeEvent(change, targets)

			msgs := collectMsgs(cmd)
			require.Equal(t, tc.wantMsgs, msgsTypes(msgs))

			nick, ok := containsMsg[components.UserNickMsg](msgs)
			require.True(t, ok)
			require.Equal(t, components.UserNickMsg{Nick: "newnick"}, nick)

			highlight, ok := containsMsg[components.HighlightWordsMsg](msgs)
			require.True(t, ok)
			require.Equal(t, components.HighlightWordsMsg{
				Words:    screen.highlightWords,
				UserNick: "newnick",
			}, highlight)

			var wantScrollback []domain.Event
			if tc.wantConfirmation {
				wantScrollback = []domain.Event{change}
			}

			require.Equal(t, wantScrollback, screen.scrollbackOf(tc.focused))
			require.Equal(t, domain.Nick("newnick"), screen.checklist.nick,
				"the welcome checklist must follow the rename")
		})
	}
}
