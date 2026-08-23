package screens

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/testclient"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
)

type instanceSessionReader struct {
	SessionReader

	nicks map[domain.InstanceID]domain.Nick
	err   error
}

func (r instanceSessionReader) ResolveInstanceByID(
	_ context.Context,
	id domain.InstanceID,
) (domain.Nick, error) {
	if r.err != nil {
		return "", r.err
	}

	return r.nicks[id], nil
}

type observedWindowState struct {
	Name        domain.ChannelName
	DisplayName string
	Kind        domain.ChannelKind
	Created     time.Time
	Events      []domain.Event
	Revision    uint64
	Unread      int
	Mentions    bool
	Visits      int
	Activity    bool
	UserTime    time.Time
}

func observeWindowState(window *Window) observedWindowState {
	if window == nil {
		return observedWindowState{}
	}

	return observedWindowState{
		Name:        window.Name(),
		DisplayName: window.DisplayName(),
		Kind:        window.Kind(),
		Created:     window.Created(),
		Events:      window.Scrollback.Events(),
		Revision:    window.Revision,
		Unread:      window.Unread,
		Mentions:    window.Mentions,
		Visits:      window.Visits,
		Activity:    window.Activity,
		UserTime:    window.UserTime,
	}
}

// lateReplyArms are the point-to-point reply arms that render on the
// window their event names. Each is issued from a window that closes
// before the reply comes back, which is the case the shared path in
// [ChatScreen.logAndShowOn] has to answer the same way for all of
// them.
var lateReplyArms = []struct {
	name  string
	reply func(target domain.ChannelName, at time.Time) domain.ProtocolEvent
}{
	{
		name: "whois",
		reply: func(_ domain.ChannelName, at time.Time) domain.ProtocolEvent {
			return domain.Whois{Nick: "botty", ModelID: "test/model", At: at}
		},
	},
	{
		name: "inviting",
		reply: func(target domain.ChannelName, at time.Time) domain.ProtocolEvent {
			return domain.Inviting{Target: target, Invitee: "botty", At: at}
		},
	},
	{
		name: "system notice",
		reply: func(target domain.ChannelName, at time.Time) domain.ProtocolEvent {
			return domain.SystemNotice{Target: target, Text: "no such nick: botty", At: at}
		},
	},
}

func TestChatScreen_reply_delivery_preserves_typed_window_origins(t *testing.T) {
	at := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	whois := domain.Whois{Nick: "botty", ModelID: "test/model", At: at}

	tests := []struct {
		name   string
		window domain.Window
	}{
		{name: "status", window: domain.WindowKey(domain.StatusChannelName)},
		{name: "self DM", window: domain.WindowKey("")},
		{name: "channel", window: domain.NewChannelWindow("#dev", time.Time{})},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			screen := newScreenFixture(t)
			msgs := collectMsgs(screen.deliverReplyEvents(tc.window, chatcmd.ReplyEvents{
				Events: []domain.ProtocolEvent{whois},
			}))

			require.Equal(t, []tea.Msg{
				replyEventMsg{issuingWindow: tc.window, event: whois},
			}, msgs)
		})
	}
}

func TestChatScreen_config_result_keeps_the_issuing_channel_incarnation(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		t.Run(map[bool]string{false: "open", true: "recreated"}[recreate], func(t *testing.T) {
			screen := newScreenFixture(t)
			issuing := domain.NewChannelWindow("#a", time.Time{})
			screen.channels.Insert(newWindow(issuing))
			screen.channels.Insert(newWindow(domain.NewChannelWindow("#b", time.Time{})))
			screen = focused(t, screen, "#a")

			if recreate {
				var closeCmd tea.Cmd
				screen, closeCmd = screen.closeWindow("#a", time.Now())
				collectMsgs(closeCmd)
				screen.channels.Insert(newWindow(domain.NewChannelWindow("#a", time.Now())))
			}
			screen = focused(t, screen, "#b")

			screen, cmd := screen.update(chatcmd.CommandResult{
				IssuingWindow: issuing,
				Message: chatcmd.PokeIntervalSetResult{
					Interval: 5 * time.Minute,
				},
			})

			channelA := normaliseSystemNoticeTimes(screen.scrollbackOf("#a"))
			channelB := normaliseSystemNoticeTimes(screen.scrollbackOf("#b"))
			wantA := []domain.Event{domain.SystemNotice{
				Target: "#a", Text: "Poke interval set to 5m.",
			}}
			wantB := []domain.Event{}
			wantTarget := domain.ChannelName("#a")
			if recreate {
				wantA = []domain.Event{}
				wantB = []domain.Event{domain.SystemNotice{
					Target: "#a", Text: "Poke interval set to 5m.",
				}}
				wantTarget = "#b"
			}

			require.Equal(t, struct {
				Active   domain.ChannelName
				ChannelA []domain.Event
				ChannelB []domain.Event
				Effects  []tea.Msg
			}{
				Active:   "#b",
				ChannelA: wantA,
				ChannelB: wantB,
				Effects: []tea.Msg{
					components.ScrollbackUpdatedMsg{Channel: wantTarget},
				},
			}, struct {
				Active   domain.ChannelName
				ChannelA []domain.Event
				ChannelB []domain.Event
				Effects  []tea.Msg
			}{
				Active:   screen.active.Name(),
				ChannelA: channelA,
				ChannelB: channelB,
				Effects:  collectMsgs(cmd),
			})
		})
	}
}

func TestChatScreen_delayed_close_keeps_a_reopened_dm(t *testing.T) {
	screen := newScreenFixture(t)
	issuing := newDMWindow(
		"inst-botty",
		"botty",
		time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC),
	)
	screen.channels.Insert(newWindow(issuing))
	screen = focused(t, screen, issuing.Name())

	var closeCmd tea.Cmd
	screen, closeCmd = screen.closeWindow(issuing.Name(), time.Now())
	collectMsgs(closeCmd)

	reopened := newDMWindow(
		"inst-botty",
		"renamed",
		time.Date(2026, 6, 4, 12, 1, 0, 0, time.UTC),
	)
	screen.channels.Insert(newWindow(reopened))
	screen = focused(t, screen, reopened.Name())

	screen, cmd, handled := screen.routeCommandResult(chatcmd.CommandResult{
		IssuingWindow: issuing,
		Message: chatcmd.DMClosedMsg{
			Window: issuing.Name(),
			At:     time.Date(2026, 6, 4, 12, 2, 0, 0, time.UTC),
		},
	})
	current, open := screen.windowByName(reopened.Name())
	var currentWindow domain.Window
	if current != nil {
		currentWindow = current.Window
	}
	var activeWindow domain.Window
	if screen.active != nil {
		activeWindow = screen.active.Window
	}

	require.Equal(t, struct {
		Handled       bool
		Open          bool
		CurrentWindow domain.Window
		ActiveWindow  domain.Window
		Effects       []tea.Msg
	}{
		Handled:       true,
		Open:          true,
		CurrentWindow: reopened,
		ActiveWindow:  reopened,
	}, struct {
		Handled       bool
		Open          bool
		CurrentWindow domain.Window
		ActiveWindow  domain.Window
		Effects       []tea.Msg
	}{
		Handled:       handled,
		Open:          open,
		CurrentWindow: currentWindow,
		ActiveWindow:  activeWindow,
		Effects:       collectMsgs(cmd),
	})
}

func TestChatScreen_late_reply_avoids_a_reopened_dm(t *testing.T) {
	screen := newScreenFixture(t)
	issuing := newDMWindow(
		"inst-botty",
		"botty",
		time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC),
	)
	other := domain.NewChannelWindow("#other", time.Time{})
	screen.channels.Insert(newWindow(issuing))
	screen.channels.Insert(newWindow(other))
	screen = focused(t, screen, issuing.Name())

	var closeCmd tea.Cmd
	screen, closeCmd = screen.closeWindow(issuing.Name(), time.Now())
	collectMsgs(closeCmd)

	reopened := newDMWindow(
		"inst-botty",
		"renamed",
		time.Date(2026, 6, 4, 12, 1, 0, 0, time.UTC),
	)
	screen.channels.Insert(newWindow(reopened))
	screen = focused(t, screen, other.Name())

	reply := domain.Whois{
		Nick: "botty", ModelID: "test/model",
		At: time.Date(2026, 6, 4, 12, 2, 0, 0, time.UTC),
	}
	screen, cmd := screen.update(replyEventMsg{
		issuingWindow: issuing,
		event:         reply,
	})

	require.Equal(t, struct {
		Active          domain.ChannelName
		Reopened        []domain.Event
		Fallback        []domain.Event
		RenderedEffects []tea.Msg
	}{
		Active:   "#other",
		Fallback: []domain.Event{reply},
		RenderedEffects: []tea.Msg{
			components.ScrollbackUpdatedMsg{Channel: "#other"},
		},
	}, struct {
		Active          domain.ChannelName
		Reopened        []domain.Event
		Fallback        []domain.Event
		RenderedEffects []tea.Msg
	}{
		Active:          screen.active.Name(),
		Reopened:        screen.scrollbackOf(reopened.Name()),
		Fallback:        screen.scrollbackOf(other.Name()),
		RenderedEffects: collectMsgs(cmd),
	})
}

func TestChatScreen_delayed_close_keeps_new_activity_in_the_same_dm(t *testing.T) {
	screen := newScreenFixture(t)
	issuing := newDMWindow(
		"inst-botty",
		"botty",
		time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC),
	)
	screen.channels.Insert(newWindow(issuing))
	screen = focused(t, screen, issuing.Name())

	beforeClose := domain.Message{
		Source: domain.ClientSource("inst-botty", "botty"),
		Target: issuing.Name(),
		Body:   "this arrived before /close",
		At:     time.Date(2026, 6, 4, 11, 59, 0, 0, time.UTC),
	}
	screen.appendToScrollback(issuing.Name(), beforeClose)

	closeCmd := screen.handleCommand(components.CommandSubmitMsg{Raw: "/close"})
	require.NotNil(t, closeCmd)

	afterClose := domain.Message{
		Source: domain.ClientSource("inst-botty", "botty"),
		Target: issuing.Name(),
		Body:   "this arrived after /close",
		At:     time.Date(2026, 6, 4, 12, 1, 0, 0, time.UTC),
	}
	screen.appendToScrollback(issuing.Name(), afterClose)

	result, ok := closeCmd().(chatcmd.CommandResult)
	require.True(t, ok)

	screen, cmd, handled := screen.routeCommandResult(result)
	current, open := screen.windowByName(issuing.Name())
	var currentWindow domain.Window
	var events []domain.Event
	if current != nil {
		currentWindow = current.Window
		events = current.Scrollback.Events()
	}
	var activeWindow domain.Window
	if screen.active != nil {
		activeWindow = screen.active.Window
	}

	require.Equal(t, struct {
		IssuingRevision uint64
		Handled         bool
		Open            bool
		CurrentWindow   domain.Window
		ActiveWindow    domain.Window
		Events          []domain.Event
		Effects         []tea.Msg
	}{
		IssuingRevision: 2,
		Handled:         true,
		Open:            true,
		CurrentWindow:   issuing,
		ActiveWindow:    issuing,
		Events:          []domain.Event{beforeClose, afterClose},
	}, struct {
		IssuingRevision uint64
		Handled         bool
		Open            bool
		CurrentWindow   domain.Window
		ActiveWindow    domain.Window
		Events          []domain.Event
		Effects         []tea.Msg
	}{
		IssuingRevision: result.IssuingWindowRevision,
		Handled:         handled,
		Open:            open,
		CurrentWindow:   currentWindow,
		ActiveWindow:    activeWindow,
		Events:          events,
		Effects:         collectMsgs(cmd),
	})
}

func TestChatScreen_newer_dm_intent_supersedes_a_delayed_close(t *testing.T) {
	tests := []struct {
		name string
		open chatcmd.DMOpenedMsg
	}{
		{
			name: "query",
			open: chatcmd.DMOpenedMsg{
				CounterpartID: "inst-botty", CounterpartNick: "botty", Focus: true,
			},
		},
		{
			name: "message",
			open: chatcmd.DMOpenedMsg{
				CounterpartID: "inst-botty", CounterpartNick: "botty", Body: "hello",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			screen := newScreenFixture(t)
			issuing := newDMWindow("inst-botty", "botty", time.Time{})
			screen.channels.Insert(newWindow(issuing))
			screen = focused(t, screen, issuing.Name())

			closeCmd := screen.handleCommand(components.CommandSubmitMsg{Raw: "/close"})
			require.NotNil(t, closeCmd)

			tt.open.At = time.Now()
			var openCmd tea.Cmd
			screen, openCmd = screen.handleDMOpenedMsg(tt.open)
			collectMsgs(openCmd)

			result, ok := closeCmd().(chatcmd.CommandResult)
			require.True(t, ok)
			screen, closeResultCmd, handled := screen.routeCommandResult(result)
			current, open := screen.windowByName(issuing.Name())
			var currentWindow domain.Window
			if current != nil {
				currentWindow = current.Window
			}
			var activeWindow domain.Window
			if screen.active != nil {
				activeWindow = screen.active.Window
			}

			require.Equal(t, struct {
				Handled bool
				Open    bool
				Window  domain.Window
				Active  domain.Window
				Effects []tea.Msg
			}{
				Handled: true,
				Open:    true,
				Window:  issuing,
				Active:  issuing,
			}, struct {
				Handled bool
				Open    bool
				Window  domain.Window
				Active  domain.Window
				Effects []tea.Msg
			}{
				Handled: handled,
				Open:    open,
				Window:  currentWindow,
				Active:  activeWindow,
				Effects: collectMsgs(closeResultCmd),
			})
		})
	}
}

func TestChatScreen_delayed_dm_open_uses_nick_from_current_session_state(t *testing.T) {
	openedAt := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	botty := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	change := domain.NickChange{
		Source:  domain.ClientSource(botty.ID(), botty.Nick()),
		NewNick: "renamed",
		At:      openedAt.Add(time.Minute),
	}

	screen := newScreenFixture(t)
	reader := instanceSessionReader{
		SessionReader: screen.sess,
		nicks:         map[domain.InstanceID]domain.Nick{botty.ID(): change.NewNick},
	}
	screen.sess = reader
	channel := domain.NewChannelWindow("#general", time.Time{})
	channel.Members.Add(botty)
	screen.channels.Insert(newWindow(channel))
	screen, _ = screen.handleNickChangeEvent(change, []domain.ChannelName{"#general"})

	screen, _ = screen.handleDMOpenedMsg(chatcmd.DMOpenedMsg{
		CounterpartID:   botty.ID(),
		CounterpartNick: botty.Nick(),
		At:              openedAt,
	})

	window, open := screen.windowByName(domain.ChannelName(botty.ID()))
	member, memberPresent := channel.Members.GetByID(botty.ID())

	require.Equal(t, struct {
		Open          bool
		Window        observedWindowState
		MemberPresent bool
		Member        domain.Member
		Active        *Window
	}{
		Open: true,
		Window: observedWindowState{
			Name:        domain.ChannelName(botty.ID()),
			DisplayName: "renamed",
			Kind:        domain.KindDM,
			Created:     openedAt,
			Revision:    1,
		},
		MemberPresent: true,
		Member: domain.Member{
			InstanceID: botty.ID(),
			Nick:       "renamed",
		},
	}, struct {
		Open          bool
		Window        observedWindowState
		MemberPresent bool
		Member        domain.Member
		Active        *Window
	}{
		Open:          open,
		Window:        observeWindowState(window),
		MemberPresent: memberPresent,
		Member:        member,
		Active:        screen.active,
	})
}

func TestChatScreen_dm_restore_uses_nick_from_current_session_state(t *testing.T) {
	const counterpartID domain.InstanceID = "inst-botty"

	sess, mgr, user, eventStore := newTestSessionWithStore(t)
	require.NoError(t, eventStore.SaveInstance(t.Context(), domain.NewModelInstance(
		counterpartID, "botty", "test/model", "", nil,
	)))
	require.NoError(t, eventStore.AddDMWindow(t.Context(), counterpartID))
	screen, err := NewChatScreen(
		t.Context, sess, mgr, user, nil, eventStore, domain.KindStatus,
	)
	require.NoError(t, err)
	nicks := map[domain.InstanceID]domain.Nick{counterpartID: "botty"}
	screen.sess = instanceSessionReader{SessionReader: sess, nicks: nicks}

	restored, ok := screen.restoreDMWindows()().(dmWindowsRestoredMsg)
	require.True(t, ok)

	change := domain.NickChange{
		Source:  domain.ClientSource(counterpartID, "botty"),
		NewNick: "renamed",
		At:      time.Date(2026, 8, 26, 12, 1, 0, 0, time.UTC),
	}
	nicks[counterpartID] = change.NewNick
	screen, _ = screen.handleNickChangeEvent(change, nil)

	var restoreCmd tea.Cmd
	screen, restoreCmd = screen.handleDMWindowsRestored(restored)
	collectMsgs(restoreCmd)
	window, open := screen.windowByName(domain.ChannelName(counterpartID))

	require.Equal(t, struct {
		Open   bool
		Window observedWindowState
		Active *Window
	}{
		Open: true,
		Window: observedWindowState{
			Name:        domain.ChannelName(counterpartID),
			DisplayName: "renamed",
			Kind:        domain.KindDM,
			Created:     sess.ConnectedAt(),
		},
	}, struct {
		Open   bool
		Window observedWindowState
		Active *Window
	}{
		Open:   open,
		Window: observeWindowState(window),
		Active: screen.active,
	})
}

func TestChatScreen_dm_open_uses_copied_nick_after_peer_disconnects(t *testing.T) {
	openedAt := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	screen := newScreenFixture(t)
	screen.sess = instanceSessionReader{
		SessionReader: screen.sess,
		err:           errors.New("peer disconnected"),
	}

	screen, _ = screen.handleDMOpenedMsg(chatcmd.DMOpenedMsg{
		CounterpartID:   "inst-botty",
		CounterpartNick: "last-seen",
		At:              openedAt,
	})

	window, open := screen.windowByName("inst-botty")

	require.Equal(t, struct {
		Open   bool
		Window observedWindowState
		Active *Window
	}{
		Open: true,
		Window: observedWindowState{
			Name:        "inst-botty",
			DisplayName: "last-seen",
			Kind:        domain.KindDM,
			Created:     openedAt,
			Revision:    1,
		},
	}, struct {
		Open   bool
		Window observedWindowState
		Active *Window
	}{
		Open:   open,
		Window: observeWindowState(window),
		Active: screen.active,
	})
}

func TestChatScreen_resolved_pending_dm_supersedes_a_delayed_close(t *testing.T) {
	screen := newScreenFixture(t)
	issuing := newDMWindow("inst-botty", "botty", time.Time{})
	screen.channels.Insert(newWindow(issuing))
	screen = focused(t, screen, issuing.Name())

	closeCmd := screen.handleCommand(components.CommandSubmitMsg{Raw: "/close"})
	require.NotNil(t, closeCmd)

	held := domain.Message{
		Source: domain.ClientSource("inst-botty", "botty"),
		Target: issuing.Name(), Body: "held while resolving", At: time.Now(),
	}
	screen.pendingDM[issuing.Name()] = []domain.Event{held}
	var resolvedCmd tea.Cmd
	screen, resolvedCmd = screen.handleDMWindowResolved(dmWindowResolvedMsg{
		window: issuing.Name(),
		counterpart: &dmCounterpart{
			id: "inst-botty", nick: "botty",
		},
	})
	collectMsgs(resolvedCmd)

	result, ok := closeCmd().(chatcmd.CommandResult)
	require.True(t, ok)
	screen, closeResultCmd, handled := screen.routeCommandResult(result)
	current, open := screen.windowByName(issuing.Name())
	var currentWindow domain.Window
	var events []domain.Event
	if current != nil {
		currentWindow = current.Window
		events = current.Scrollback.Events()
	}

	require.Equal(t, struct {
		Handled bool
		Open    bool
		Window  domain.Window
		Events  []domain.Event
		Effects []tea.Msg
	}{
		Handled: true,
		Open:    true,
		Window:  issuing,
		Events:  []domain.Event{held},
	}, struct {
		Handled bool
		Open    bool
		Window  domain.Window
		Events  []domain.Event
		Effects []tea.Msg
	}{
		Handled: handled,
		Open:    open,
		Window:  currentWindow,
		Events:  events,
		Effects: collectMsgs(closeResultCmd),
	})
}

func TestChatScreen_newer_dm_persistence_write_supersedes_an_older_close(t *testing.T) {
	sess, mgr, user, eventStore := newTestSessionWithStore(t)
	require.NoError(t, eventStore.SaveInstance(t.Context(), domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "", nil,
	)))
	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	closeCmd := screen.forgetDMWindowCmd("inst-botty")
	openCmd := screen.recordDMWindowCmd("inst-botty")

	collectMsgs(openCmd)
	collectMsgs(closeCmd)

	open, err := eventStore.ListDMWindows(t.Context())
	require.NoError(t, err)
	require.Equal(t, []domain.InstanceID{"inst-botty"}, open)
}

func TestChatScreen_newer_focus_write_supersedes_an_older_fallback(t *testing.T) {
	sess, mgr, user, eventStore := newTestSessionWithStore(t)
	screen, err := NewChatScreen(
		t.Context, sess, mgr, user, nil, eventStore, domain.KindStatus,
	)
	require.NoError(t, err)
	status := newWindow(domain.NewStatusWindow(time.Time{}))
	dm := newWindow(newDMWindow("inst-botty", "botty", time.Time{}))

	fallbackCmd := screen.persistLastWindow(status)
	dmCmd := screen.persistLastWindow(dm)

	collectMsgs(dmCmd)
	collectMsgs(fallbackCmd)

	stored, err := eventStore.GetLastWindow(t.Context())
	require.NoError(t, err)
	require.Equal(t, domain.WindowKey(dm.Name()), stored)
}

func TestChatScreen_stale_restore_does_not_reopen_a_closed_dm(t *testing.T) {
	sess, mgr, user, eventStore := newTestSessionWithStore(t)
	require.NoError(t, eventStore.AddDMWindow(t.Context(), ""))
	screen, err := NewChatScreen(
		t.Context, sess, mgr, user, nil, eventStore, domain.KindStatus,
	)
	require.NoError(t, err)

	restored, ok := screen.restoreDMWindows()().(dmWindowsRestoredMsg)
	require.True(t, ok)

	var openCmd tea.Cmd
	screen, openCmd = screen.handleDMOpenedMsg(chatcmd.DMOpenedMsg{
		CounterpartID: "", CounterpartNick: "testuser",
		Focus: true, At: time.Now(),
	})
	collectMsgs(openCmd)
	var closeCmd tea.Cmd
	screen, closeCmd = screen.handleDMClosedMsg(chatcmd.DMClosedMsg{
		Window: "", At: time.Now(),
	})
	collectMsgs(closeCmd)

	var restoreCmd tea.Cmd
	screen, restoreCmd = screen.handleDMWindowsRestored(restored)
	collectMsgs(restoreCmd)
	_, open := screen.windowByName("")
	stored, err := eventStore.ListDMWindows(t.Context())
	require.NoError(t, err)

	require.Equal(t, struct {
		Open   bool
		Stored []domain.InstanceID
	}{
		Stored: nil,
	}, struct {
		Open   bool
		Stored []domain.InstanceID
	}{
		Open:   open,
		Stored: stored,
	})
}

func TestChatScreen_background_dm_open_during_restore_keeps_a_landing(t *testing.T) {
	sess, mgr, user, eventStore := newTestSessionWithStore(t)
	ctx := t.Context()
	bot := testclient.NewStored(
		"botty", sess, eventStore, testclient.WithInstanceID("inst-botty"),
	)
	require.NoError(t, bot.Attach(ctx))
	t.Cleanup(bot.Detach)
	require.NoError(t, user.Join(ctx, "#general"))
	require.NoError(t, eventStore.AddDMWindow(ctx, "inst-botty"))
	require.NoError(t, eventStore.SetLastWindow(ctx, domain.WindowKey("inst-botty")))
	screen, err := NewChatScreen(
		t.Context, sess, mgr, user, nil, eventStore, domain.KindStatus,
	)
	require.NoError(t, err)
	screen.markCurrentJoinRepliesComplete()
	collectMsgs(tea.Sequence(screen.bootstrapFromSession(false)...))

	restored, ok := screen.restoreDMWindows()().(dmWindowsRestoredMsg)
	require.True(t, ok)

	var opened tea.Cmd
	screen, opened = screen.appendMessage("inst-botty", domain.Message{
		Source: domain.ClientSource("inst-botty", "botty"),
		Target: "", Body: "background arrival",
		At: restored.landingAt.Add(time.Second),
	})
	collectMsgs(opened)

	var restoreCmd tea.Cmd
	screen, restoreCmd = screen.handleDMWindowsRestored(restored)
	for _, message := range collectMsgs(restoreCmd) {
		screen, _, _ = screen.routeWindows(message)
	}

	var active domain.ChannelName
	if screen.active != nil {
		active = screen.active.Name()
	}
	_, generalOpen := screen.windowByName("#general")
	_, dmOpen := screen.windowByName("inst-botty")
	require.Equal(t, struct {
		Active      domain.ChannelName
		GeneralOpen bool
		DMOpen      bool
	}{
		Active:      "inst-botty",
		GeneralOpen: true,
		DMOpen:      true,
	}, struct {
		Active      domain.ChannelName
		GeneralOpen bool
		DMOpen      bool
	}{
		Active:      active,
		GeneralOpen: generalOpen,
		DMOpen:      dmOpen,
	})
}

func TestChatScreen_stale_restore_does_not_replace_a_newer_focus(t *testing.T) {
	tests := []struct {
		name string
		act  func(ChatScreen, time.Time) (ChatScreen, tea.Cmd, domain.Window)
	}{
		{
			name: "channel focus",
			act: func(screen ChatScreen, at time.Time) (ChatScreen, tea.Cmd, domain.Window) {
				other := domain.NewChannelWindow("#other", at)
				screen.channels.Insert(newWindow(other))
				screen, cmd := screen.handleChannelFocus(chatcmd.ChannelFocusMsg{
					Channel: other.Name(), At: at,
				})

				return screen, cmd, other
			},
		},
		{
			name: "query focus",
			act: func(screen ChatScreen, at time.Time) (ChatScreen, tea.Cmd, domain.Window) {
				screen, cmd := screen.handleDMOpenedMsg(chatcmd.DMOpenedMsg{
					CounterpartID: "inst-botty", CounterpartNick: "botty",
					Focus: true, At: at,
				})

				return screen, cmd, newDMWindow("inst-botty", "botty", at)
			},
		},
		{
			name: "focus cleared",
			act: func(screen ChatScreen, at time.Time) (ChatScreen, tea.Cmd, domain.Window) {
				other := domain.NewChannelWindow("#other", at)
				screen.channels.Insert(newWindow(other))
				screen, focusCmd := screen.handleChannelFocus(chatcmd.ChannelFocusMsg{
					Channel: other.Name(), At: at,
				})
				collectMsgs(focusCmd)
				screen, closeCmd := screen.closeWindow(other.Name(), at.Add(-2*time.Second))

				return screen, closeCmd, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess, mgr, user, eventStore := newTestSessionWithStore(t)
			ctx := t.Context()
			require.NoError(t, eventStore.AddDMWindow(ctx, ""))
			require.NoError(t, eventStore.SetLastWindow(ctx, domain.WindowKey("")))
			screen, err := NewChatScreen(
				t.Context, sess, mgr, user, nil, eventStore, domain.KindStatus,
			)
			require.NoError(t, err)

			restored, ok := screen.restoreDMWindows()().(dmWindowsRestoredMsg)
			require.True(t, ok)
			screen, actionCmd, expectedActive := tt.act(
				screen, restored.landingAt.Add(time.Second),
			)
			collectMsgs(actionCmd)

			var restoreCmd tea.Cmd
			screen, restoreCmd = screen.handleDMWindowsRestored(restored)
			require.NotNil(t, restoreCmd)
			screen, landingCmd := screen.handleDMLandingRestored(dmLandingRestoredMsg{
				channel: "", at: restored.landingAt,
			})
			dm, open := screen.windowByName("")
			var restoredDM domain.Window
			if dm != nil {
				restoredDM = dm.Window
			}
			var active domain.Window
			if screen.active != nil {
				active = screen.active.Window
			}

			require.Equal(t, struct {
				Open    bool
				DM      domain.Window
				Active  domain.Window
				Landing tea.Cmd
			}{
				Open:   true,
				DM:     newDMWindow("", "testuser", sess.ConnectedAt()),
				Active: expectedActive,
			}, struct {
				Open    bool
				DM      domain.Window
				Active  domain.Window
				Landing tea.Cmd
			}{
				Open:    open,
				DM:      restoredDM,
				Active:  active,
				Landing: landingCmd,
			})
		})
	}
}

func normaliseSystemNoticeTimes(events []domain.Event) []domain.Event {
	normalised := make([]domain.Event, len(events))
	for i, event := range events {
		if notice, ok := event.(domain.SystemNotice); ok {
			notice.At = time.Time{}
			event = notice
		}
		normalised[i] = event
	}

	return normalised
}

func TestChatScreen_whois_renders_in_the_status_window_it_came_from(t *testing.T) {
	screen := newScreenFixture(t)
	screen.channels.Insert(newWindow(domain.NewStatusWindow(time.Time{})))
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#dev", time.Time{})))
	screen, _ = screen.focus("#dev")
	whois := domain.Whois{Nick: "botty", ModelID: "test/model"}

	screen, _ = screen.update(replyEventMsg{
		issuingWindow: domain.WindowKey(domain.StatusChannelName), event: whois,
	})

	require.Equal(t, []domain.Event{whois}, screen.scrollbackOf(domain.StatusChannelName))
	require.Empty(t, screen.scrollbackOf("#dev"))
}

func TestChatScreen_whois_distinguishes_a_self_DM_from_status(t *testing.T) {
	screen := newScreenFixture(t)
	screen.channels.Insert(newWindow(newDMWindow("", "testuser", time.Time{})))
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#dev", time.Time{})))
	screen, _ = screen.focus("#dev")
	whois := domain.Whois{Nick: "testuser"}

	screen, _ = screen.update(replyEventMsg{
		issuingWindow: domain.WindowKey(""), event: whois,
	})

	require.Equal(t, []domain.Event{whois}, screen.scrollbackOf(""))
	require.Empty(t, screen.scrollbackOf(domain.StatusChannelName))
	require.Empty(t, screen.scrollbackOf("#dev"))
}

// lateReplyFixture builds a chat screen holding a DM with `botty` and
// the channel `#other`, with the DM in view. It is the state a command
// issued from a query window leaves the screen in.
func lateReplyFixture(t *testing.T) (ChatScreen, *dmWindow) {
	t.Helper()

	sess, mgr, user, eventStore := newTestSessionWithStore(t)

	counterpart := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	require.NoError(t, eventStore.SaveInstance(t.Context(), counterpart))

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	dm := newDMWindow(counterpart.ID(), counterpart.Nick(), time.Time{})
	screen.channels.Insert(newWindow(dm))
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#other", time.Time{})))

	screen, _ = screen.focus(dm.Name())

	return screen, dm
}

// TestChatScreen_late_reply_to_a_closed_dm covers a reply arriving
// after the query window it was issued from has been closed. A DM
// window is built around its counterpart's instance handle, which
// [ChatScreen.appendToScrollback] cannot synthesise, so routing the
// reply straight to its own target would drop it where the user would
// never see it.
func TestChatScreen_late_reply_to_a_closed_dm(t *testing.T) {
	for _, arm := range lateReplyArms {
		t.Run(arm.name, func(t *testing.T) {
			screen, dm := lateReplyFixture(t)

			at := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
			reply := arm.reply(dm.Name(), at)

			screen, closeCmd := screen.closeWindow(dm.Name(), time.Now())
			collectMsgs(closeCmd)
			require.Equal(t, domain.ChannelName("#other"), screen.active.Name(),
				"closing the window in view must land the user on #other")

			screen, _ = screen.update(replyEventMsg{
				issuingWindow: dm, event: reply,
			})

			_, reopened := screen.windowByName(dm.Name())
			require.False(t, reopened, "a late reply must not recreate the closed DM window")

			require.Equal(t, []domain.Event{reply}, screen.scrollbackOf("#other"))
		})
	}
}

// TestChatScreen_late_reply_to_a_parted_channel is the channel
// counterpart. [ChatScreen.appendToScrollback] does create a window
// for an unknown channel, but that path is for live traffic arriving
// before a join is seen; a stale reply taking it would put a channel
// the user has left back in the window set with no membership behind
// it.
func TestChatScreen_late_reply_to_a_parted_channel(t *testing.T) {
	for _, arm := range lateReplyArms {
		t.Run(arm.name, func(t *testing.T) {
			screen := newScreenFixture(t)
			issuingChannel := domain.NewChannelWindow("#a", time.Time{})
			screen.channels.Insert(newWindow(issuingChannel))
			screen.channels.Insert(newWindow(domain.NewChannelWindow("#b", time.Time{})))

			screen, _ = screen.focus("#a")

			at := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
			reply := arm.reply("#a", at)

			screen, closeCmd := screen.closeWindow("#a", time.Now())
			collectMsgs(closeCmd)
			screen, _ = screen.focus("#b")

			screen, _ = screen.update(replyEventMsg{
				issuingWindow: issuingChannel, event: reply,
			})

			_, reappeared := screen.windowByName("#a")
			require.False(t, reappeared, "a late reply must not resurrect a parted channel")

			require.Equal(t, []domain.Event{reply}, screen.scrollbackOf("#b"))
		})
	}
}

// TestChatScreen_late_reply_with_no_window_in_view covers the user
// parting their last channel while the command is still in flight.
// [ChatScreen.firstRealChannel] skips `&modeloff`, so closeWindow
// leaves them looking at nothing and the fallback has no window to
// answer with. The reply takes [ChatScreen.logAndShow]'s answer:
// `&modeloff`, with the focus moved there so the user sees it.
func TestChatScreen_late_reply_with_no_window_in_view(t *testing.T) {
	for _, arm := range lateReplyArms {
		t.Run(arm.name, func(t *testing.T) {
			screen := newScreenFixture(t)
			screen.channels.Insert(newWindow(domain.NewStatusWindow(time.Time{})))
			issuingChannel := domain.NewChannelWindow("#a", time.Time{})
			screen.channels.Insert(newWindow(issuingChannel))

			screen, _ = screen.focus("#a")

			at := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
			reply := arm.reply("#a", at)

			screen, closeCmd := screen.closeWindow("#a", time.Now())
			collectMsgs(closeCmd)
			require.Nil(t, screen.active,
				"parting the only real channel leaves the user looking at nothing")

			screen, cmd := screen.update(replyEventMsg{
				issuingWindow: issuingChannel, event: reply,
			})

			_, reappeared := screen.windowByName("#a")
			require.False(t, reappeared, "a late reply must not resurrect a parted channel")

			require.Equal(t, []domain.Event{reply}, screen.scrollbackOf(domain.StatusChannelName))

			focus, moved := containsMsg[chatcmd.ChannelFocusMsg](collectMsgs(cmd))
			require.True(t, moved, "the user must be moved to the window holding the reply")
			require.Equal(t, domain.StatusChannelName, focus.Channel)
		})
	}
}

// TestChatScreen_reply_renders_on_its_own_target_while_the_window_is_open
// pins the ordinary case the fallback must leave alone: a reply naming
// a window the user still has open renders there, whatever window they
// are looking at now.
func TestChatScreen_reply_renders_on_its_own_target_while_the_window_is_open(t *testing.T) {
	for _, arm := range lateReplyArms {
		t.Run(arm.name, func(t *testing.T) {
			screen, dm := lateReplyFixture(t)

			at := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
			reply := arm.reply(dm.Name(), at)

			screen, _ = screen.focus("#other")
			screen, _ = screen.update(replyEventMsg{
				issuingWindow: dm, event: reply,
			})

			require.Equal(t, []domain.Event{reply}, screen.scrollbackOf(dm.Name()))
			require.Empty(t, screen.scrollbackOf("#other"),
				"the reply belongs to the window it was issued from, not the one in view")
		})
	}
}

func TestChatScreen_topic_reply_uses_the_issuing_channel(t *testing.T) {
	topic := domain.TopicInfo{
		Target:     "#a",
		Topic:      "release planning",
		TopicSetBy: "alice",
		TopicSetAt: time.Date(2026, 6, 4, 11, 0, 0, 0, time.UTC),
		At:         time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC),
	}
	tests := []struct {
		name               string
		closeIssuing       bool
		reopenIssuing      bool
		focusReopened      bool
		wantIssuingCurrent bool
		wantTarget         domain.ChannelName
		wantActive         domain.ChannelName
		wantStatus         []domain.Event
		wantA              []domain.Event
		wantB              []domain.Event
	}{
		{
			name: "issuing channel remains open", wantIssuingCurrent: true,
			wantTarget: "#a", wantActive: "#b", wantA: []domain.Event{topic},
		},
		{
			name: "issuing channel was parted", closeIssuing: true, wantTarget: "#b",
			wantActive: "#b", wantB: []domain.Event{topic},
		},
		{
			name: "channel name was reused", closeIssuing: true, reopenIssuing: true,
			wantTarget: "#b", wantActive: "#b", wantB: []domain.Event{topic},
		},
		{
			name: "replacement channel is active", closeIssuing: true,
			reopenIssuing: true, focusReopened: true,
			wantTarget: domain.StatusChannelName, wantActive: "#a",
			wantStatus: []domain.Event{topic},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			screen := newScreenFixture(t)
			issuingChannel := domain.NewChannelWindow("#a", time.Time{})
			screen.channels.Insert(newWindow(issuingChannel))
			screen.channels.Insert(newWindow(domain.NewChannelWindow("#b", time.Time{})))
			screen, _ = screen.focus("#a")
			if tc.closeIssuing {
				var closeCmd tea.Cmd
				screen, closeCmd = screen.closeWindow("#a", time.Now())
				collectMsgs(closeCmd)
			}
			if tc.reopenIssuing {
				screen.channels.Insert(newWindow(domain.NewChannelWindow("#a", time.Now())))
			}
			if tc.focusReopened {
				screen, _ = screen.focus("#a")
			} else {
				screen, _ = screen.focus("#b")
			}

			screen, cmd := screen.update(chatcmd.CommandResult{
				IssuingWindow: issuingChannel,
				Message:       chatcmd.TopicInfoResult{Topic: topic},
			})

			currentChannel, issuingOpen := screen.channelWindowByName("#a")
			require.Equal(t, struct {
				IssuingOpen    bool
				IssuingCurrent bool
				Active         domain.ChannelName
				Status         []domain.Event
				ChannelA       []domain.Event
				ChannelB       []domain.Event
				Messages       []tea.Msg
			}{
				IssuingOpen:    !tc.closeIssuing || tc.reopenIssuing,
				IssuingCurrent: tc.wantIssuingCurrent,
				Active:         tc.wantActive,
				Status:         tc.wantStatus,
				ChannelA:       tc.wantA,
				ChannelB:       tc.wantB,
				Messages: []tea.Msg{
					components.ScrollbackUpdatedMsg{Channel: tc.wantTarget},
				},
			}, struct {
				IssuingOpen    bool
				IssuingCurrent bool
				Active         domain.ChannelName
				Status         []domain.Event
				ChannelA       []domain.Event
				ChannelB       []domain.Event
				Messages       []tea.Msg
			}{
				IssuingOpen:    issuingOpen,
				IssuingCurrent: currentChannel == issuingChannel,
				Active:         screen.active.Name(),
				Status:         screen.scrollbackOf(domain.StatusChannelName),
				ChannelA:       screen.scrollbackOf("#a"),
				ChannelB:       screen.scrollbackOf("#b"),
				Messages:       collectMsgs(cmd),
			})
		})
	}
}

func TestChatScreen_channel_error_avoids_a_replacement(t *testing.T) {
	for _, active := range []domain.ChannelName{"#a", "#b"} {
		t.Run(string(active), func(t *testing.T) {
			screen := newScreenFixture(t)
			issuingChannel := domain.NewChannelWindow("#a", time.Time{})
			screen.channels.Insert(newWindow(issuingChannel))
			screen.channels.Insert(newWindow(domain.NewChannelWindow("#b", time.Time{})))
			screen, _ = screen.focus("#a")
			screen, closeCmd := screen.closeWindow("#a", time.Now())
			collectMsgs(closeCmd)
			screen.channels.Insert(newWindow(domain.NewChannelWindow("#a", time.Now())))
			screen, _ = screen.focus(active)
			at := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)

			screen, cmd, handled := screen.routeReplies(chatcmd.CommandResult{
				IssuingWindow: issuingChannel,
				Message: chatcmd.CommandErrorResult{Error: domain.ErrorEvent{
					Operation: "topic", Err: errors.New("store unavailable"),
					Target: "#a", At: at,
				}},
			})
			messages := collectMsgs(cmd)

			wantTarget := active
			if active == "#a" {
				wantTarget = domain.StatusChannelName
			}
			require.Equal(t, struct {
				Handled               bool
				Active                domain.ChannelName
				ReplacementScrollback []domain.Event
				TargetScrollback      []domain.Event
				Messages              []tea.Msg
			}{
				Handled: true,
				Active:  active,
				TargetScrollback: []domain.Event{domain.CommandError{
					Target: wantTarget, Err: "topic: store unavailable", At: at,
				}},
				Messages: []tea.Msg{
					components.ScrollbackUpdatedMsg{Channel: wantTarget},
					nil,
					components.NickListThinkingMsg{},
				},
			}, struct {
				Handled               bool
				Active                domain.ChannelName
				ReplacementScrollback []domain.Event
				TargetScrollback      []domain.Event
				Messages              []tea.Msg
			}{
				Handled:               handled,
				Active:                screen.active.Name(),
				ReplacementScrollback: screen.scrollbackOf("#a"),
				TargetScrollback:      screen.scrollbackOf(wantTarget),
				Messages:              messages,
			})
		})
	}
}

func TestChatScreen_list_reply_uses_the_issuing_window(t *testing.T) {
	for _, closeIssuingWindow := range []bool{false, true} {
		t.Run(map[bool]string{false: "open", true: "parted"}[closeIssuingWindow], func(t *testing.T) {
			screen := newScreenFixture(t)
			issuingChannel := domain.NewChannelWindow("#a", time.Time{})
			screen.channels.Insert(newWindow(issuingChannel))
			screen.channels.Insert(newWindow(domain.NewChannelWindow("#b", time.Time{})))
			screen, _ = screen.focus("#a")

			if closeIssuingWindow {
				next, closeCmd := screen.closeWindow("#a", time.Now())
				screen = next
				collectMsgs(closeCmd)
			}
			screen, _ = screen.focus("#b")

			at := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
			listReply := domain.ListReply{Channel: "#listed", Members: 2, At: at}
			listEnd := domain.ListEnd{At: at}
			replies := []domain.Event{listReply, listEnd}
			events := []domain.ProtocolEvent{listReply, listEnd}

			screen, cmd := screen.update(chatcmd.CommandResult{
				IssuingWindow: issuingChannel,
				Message:       chatcmd.ReplyEvents{Events: events},
			})
			collectMsgs(cmd)

			if closeIssuingWindow {
				_, reopened := screen.windowByName("#a")
				require.False(t, reopened)
				require.Equal(t, replies, screen.scrollbackOf("#b"))
				return
			}

			require.Equal(t, replies, screen.scrollbackOf("#a"))
			require.Empty(t, screen.scrollbackOf("#b"))
		})
	}
}

// TestChatScreen_names_reply_for_a_parted_channel_is_dropped records
// the one point-to-point arm that needs no fallback.
// [ChatScreen.handleNamesReply] applies a member-list snapshot to a
// window and renders nothing, so a channel the user has left leaves it
// with no window to update and nothing to show.
func TestChatScreen_names_reply_for_a_parted_channel_is_dropped(t *testing.T) {
	screen := newScreenFixture(t)
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#b", time.Time{})))
	screen, _ = screen.focus("#b")

	screen, cmd := screen.handleNamesReply(domain.NamesReplyEvent{
		Channel: "#a",
		Members: domain.NewMemberList(),
	})

	require.Nil(t, cmd)

	_, reappeared := screen.windowByName("#a")
	require.False(t, reappeared, "a names reply must not resurrect a parted channel")
	require.Empty(t, screen.scrollbackOf("#b"))
}
