package screens

import (
	"iter"
	"log/slog"
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
)

// protocolEventMsg wraps a [protocol.Event] received from the
// user-client subscription's `Events()` channel. The protocol bus
// carries the wire-shaped events the chat-screen renders as IRC
// scrollback (joins, parts, messages, mode changes, etc.).
//
// `targets` carries the per-recipient channel list for
// actor-scoped events (Quit, NickChange) — the intersection
// [Session.fanOutProtocol] computed for this delivery, copied
// off the [protocol.Delivery] envelope so the handler can route
// the line into the user-client's open windows without consulting
// the wire payload. Nil for ordinary window-scoped events.
//
// `window` carries the recipient-relative channel or direct-message
// target for transient lifecycle events whose IRC-shaped payload has
// no target of its own.
type protocolEventMsg struct {
	event   protocol.Event
	targets []domain.ChannelName
	window  protocol.WindowTarget
}

type dispatchWindowKey struct {
	actor  domain.InstanceID
	kind   domain.ChannelKind
	window domain.ChannelName
}

func newDispatchWindowKey(
	actor domain.InstanceID,
	window protocol.WindowTarget,
) (dispatchWindowKey, bool) {
	if channel, ok := protocol.ChannelWindowName(window); ok {
		return dispatchWindowKey{
			actor: actor, kind: domain.KindChannel,
			window: domain.ChannelName(domain.KeyForChannel(channel)),
		}, true
	}
	if peer, ok := protocol.DirectWindowPeer(window); ok {
		return dispatchWindowKey{
			actor: actor, kind: domain.KindDM, window: domain.ChannelName(peer),
		}, true
	}

	return dispatchWindowKey{}, false
}

// NewProtocolEventForTest builds a [tea.Msg] that injects a
// protocol-bus delivery into the chat screen's update loop, mimicking
// the envelope shape the session's fan-out would have produced. The
// returned message is the only supported way to deliver an event into
// the chat screen from outside the `screens` package; tests should
// reach for it via [screenstest.SendProtocolEvent] rather than
// constructing wire events directly.
func NewProtocolEventForTest(evt protocol.Event, targets []domain.ChannelName) tea.Msg {
	return protocolEventMsg{event: evt, targets: targets}
}

// unreadCountedMsg carries the result of an unread-count query back
// to the Update goroutine. `visits` is the target window's visit
// count when the query was made; [ChatScreen.deliverUnreadCount]
// compares it with the window's current one before the badge is
// updated.
type unreadCountedMsg struct {
	channel domain.ChannelName
	count   int
	mention bool
	visits  int
}

type protocolEffectResultMsg struct {
	msg tea.Msg
}

type protocolEffectDoneMsg struct{}

// routeSessionEvents answers what the server sent: a delivery off the
// protocol bus, the paced release of a queued model reply, and the
// unread count a store read came back with.
func (s ChatScreen) routeSessionEvents(msg tea.Msg) (ChatScreen, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case protocolEventMsg:
		next, cmd := s.handleProtocolEvent(msg)
		return next, cmd, true

	case deliverNextPacedMsg:
		next, cmd := s.deliverNextPaced(msg)
		return next, cmd, true

	case unreadCountedMsg:
		next, cmd := s.deliverUnreadCount(msg)
		return next, cmd, true

	case protocolEffectResultMsg:
		next, cmd := s.handleProtocolEffectResult(msg)
		return next, cmd, true

	case protocolEffectDoneMsg:
		next, cmd := s.handleProtocolEffectDone()
		return next, cmd, true
	}

	return s, nil, false
}

// handleProtocolEvent dispatches wire-shaped events plus the
// session-emitted events whose ordering relative to the wire
// sequence matters: joins, parts, messages, mode changes, topic
// info, dispatch lifecycle, names replies, the status-window
// signal, and focus changes.
func (s ChatScreen) handleProtocolEvent(msg protocolEventMsg) (ChatScreen, tea.Cmd) {
	s, buffered := s.bufferProtocolEvent(msg.event, msg.targets, msg.window)
	s, cmd := s.applyProtocolEvent(msg)
	s, effects := s.enqueueProtocolEffects(buffered, cmd, s.scrollbackUpdatedCmd())

	return s, tea.Batch(effects, s.listenForProtocolEvents())
}

func (s ChatScreen) enqueueProtocolEffects(cmds ...tea.Cmd) (ChatScreen, tea.Cmd) {
	for _, cmd := range cmds {
		if cmd != nil {
			s.protocolEffects = append(s.protocolEffects, cmd)
		}
	}

	return s.startNextProtocolEffect()
}

func (s ChatScreen) startNextProtocolEffect() (ChatScreen, tea.Cmd) {
	if s.protocolEffectRunning || len(s.protocolEffects) == 0 {
		return s, nil
	}

	cmd := s.protocolEffects[0]
	s.protocolEffects = s.protocolEffects[1:]
	s.protocolEffectRunning = true

	return s, func() tea.Msg {
		return protocolEffectResultMsg{msg: cmd()}
	}
}

func (s ChatScreen) handleProtocolEffectResult(msg protocolEffectResultMsg) (ChatScreen, tea.Cmd) {
	if msg.msg == nil {
		return s.handleProtocolEffectDone()
	}

	return s, tea.Sequence(
		msgCmd(msg.msg),
		msgCmd(protocolEffectDoneMsg{}),
	)
}

func (s ChatScreen) handleProtocolEffectDone() (ChatScreen, tea.Cmd) {
	s.protocolEffectRunning = false

	return s.startNextProtocolEffect()
}

// applyProtocolEvent updates client state for one delivery and
// returns only the command produced by that event. The outer handler
// adds buffering, scrollback refresh and the next bus read.
func (s ChatScreen) applyProtocolEvent(msg protocolEventMsg) (ChatScreen, tea.Cmd) {
	var cmd tea.Cmd

	switch evt := msg.event.(type) {
	case domain.Join:
		s, cmd = s.handleJoinEvent(evt)
	case domain.Part:
		s, cmd = s.handlePartEvent(evt)
	case domain.Quit:
		s, cmd = s.handleQuitEvent(evt, msg.targets)
	case domain.ChannelModeChange:
		s, cmd = s.handleChannelModeChangeEvent(evt)
	case domain.UserModeChange:
		s, cmd = s.handleUserModeChangeEvent(evt)
	case domain.Message:
		s, cmd = s.handleMessageEvent(evt)
	case domain.TopicChange:
		s, cmd = s.handleTopicChangeEvent(evt)
	case domain.NickChange:
		s, cmd = s.handleNickChangeEvent(evt, msg.targets)
	case domain.Kicked:
		s, cmd = s.handleKickedEvent(evt)
	case domain.TopicInfo:
		s, cmd = s.handleTopicInfoEvent(evt)
	case domain.ModelDispatchStarted:
		s, cmd = s.handleModelDispatchStarted(evt, msg.window)
	case domain.ModelDispatchDone:
		s, cmd = s.handleModelDispatchDone(evt, msg.window)
	case domain.NamesReplyEvent:
		s, cmd = s.handleNamesReply(evt)
	case domain.NamesEnd:
		s, cmd = s.handleNamesEnd(evt)
	case domain.ConnectionError:
		s, cmd = s.handleConnectionError(evt)
	}

	return s, cmd
}

func (s ChatScreen) handleConnectionError(domain.ConnectionError) (ChatScreen, tea.Cmd) {
	if s.quitting {
		return s, nil
	}

	s.quitting = true

	return s, msgCmd(ui.QuitCompleteMsg{})
}

// listenForProtocolEvents reads the next delivery from the
// user-client subscription's protocol channel and wraps its event
// in a protocolEventMsg. The chat-screen does not consume the
// span context the delivery carries — that is for model-client
// dispatch goroutines to link their turn spans to the originating
// handler. After each delivery, this should be re-invoked so the
// channel is continuously drained.
//
// The wait ends on the application context as well as on a delivery.
// A program that quits with nothing in flight leaves this goroutine
// parked on a channel whose only writer has already gone.
func (s ChatScreen) listenForProtocolEvents() tea.Cmd {
	ch := s.client.Events()
	done := s.baseContext().Done()

	return func() tea.Msg {
		select {
		case delivery, ok := <-ch:
			if !ok {
				return nil
			}

			return protocolEventMsg{
				event: delivery.Event, targets: delivery.Targets, window: delivery.Window,
			}
		case <-done:
			return nil
		}
	}
}

// scrollbackUpdatedCmd nudges the message list to re-evaluate the
// active window's scrollback after an event was buffered. Without
// the nudge the new content would still appear on the next draw
// because the message list reads through a getter, but the seen mark
// would never move over it: a line the user watched arrive would
// stay behind the divider, and an off-bottom user would never see
// the "new messages" line at all.
func (s ChatScreen) scrollbackUpdatedCmd() tea.Cmd {
	if s.active == nil {
		return nil
	}

	return msgCmd(components.ScrollbackUpdatedMsg{Channel: s.active.Name()})
}

// handleNamesReply applies the joiner-targeted member-list snapshot
// to the local channel cache. Without this handler, the chat screen
// would not learn about members who joined before the user, and a
// newly joined channel would show only the user's own name.
func (s ChatScreen) handleNamesReply(msg domain.NamesReplyEvent) (ChatScreen, tea.Cmd) {
	w, ok := s.windowByName(msg.Channel)
	if !ok {
		// `NamesReplyEvent` only follows a real user-join; the
		// join handler should have populated the cache already.
		// A miss means the upstream sequencing is wrong, but we
		// don't have anything sensible to do here besides log.
		slog.Default().WarnContext(s.baseContext(), "names reply for unknown channel",
			"component", "chat_screen",
			"channel", msg.Channel,
		)

		return s, nil
	}

	cw, isChannel := w.Window.(*domain.ChannelWindow)
	if isChannel {
		cw.Members = msg.Members
	}

	if isChannel && s.active != nil && msg.Channel == s.active.Name() {
		return s.nickListUpdatedCmd()
	}

	return s, nil
}

func (s ChatScreen) handleNamesEnd(msg domain.NamesEnd) (ChatScreen, tea.Cmd) {
	if _, ok := s.windowByName(msg.Channel); !ok {
		return s, nil
	}

	s.joinReplyDone[msg.Channel] = true
	at, pending := s.pendingJoinFocus[msg.Channel]
	if !pending {
		return s, nil
	}

	delete(s.pendingJoinFocus, msg.Channel)

	return s.handleChannelFocus(chatcmd.ChannelFocusMsg{
		Channel: msg.Channel,
		At:      at,
	})
}

func (s ChatScreen) handleJoinEvent(msg domain.Join) (ChatScreen, tea.Cmd) {
	isUser := s.isOwnSource(msg.Source)

	w, channelKnown := s.windowByName(msg.Target)

	if !isUser && !channelKnown {
		return s, nil
	}

	var cw *domain.ChannelWindow
	if channelKnown {
		cw, _ = w.Window.(*domain.ChannelWindow)
	} else {
		// First sighting of this channel. The chat-screen learns
		// about it from the join, so it owns the cache
		// population.
		cw = domain.NewChannelWindow(msg.Target, msg.At)
		w = newWindow(cw)
		s.channels.Insert(w)
	}

	if id, identified := msg.Source.InstanceID(); cw != nil && identified && !cw.Members.HasID(id) {
		cw.Members.AddIdentity(id, msg.Source.Nick())
	}

	if !isUser {
		if s.active != nil && msg.Target == s.active.Name() && cw != nil {
			return s.nickListUpdatedCmd()
		}

		return s, nil
	}

	// `UserTime` stamps the user's deliberate moment with this
	// window. The user-join is the earliest such moment; later
	// keystrokes and focus events bump it. The window may have
	// been pre-created by `bufferProtocolEvent`'s auto-stamping
	// with a zero `UserTime`, so guard on `IsZero` rather than
	// `!channelKnown` to catch that path.
	if w.UserTime.IsZero() {
		w.UserTime = msg.At
	}
	s.joinReplyDone[msg.Target] = false

	s.checklist.channelCount = s.realChannelCount()

	return s, tea.Batch(
		msgCmd(components.ChannelAddedMsg{Channel: w.Window}),
		msgCmd(components.ChannelUnreadMsg{Channel: msg.Target, Count: 0}),
	)
}

func (s ChatScreen) handleChannelModeChangeEvent(msg domain.ChannelModeChange) (ChatScreen, tea.Cmd) {
	cw, ok := s.channelWindowByName(msg.Target)
	if !ok {
		return s, nil
	}

	if msg.Subject != domain.AnonymousNick && msg.Flag.MemberMode() {
		cw.Members.ApplyModeNick(msg.Subject, msg.Flag, msg.Add)
	}

	if s.active == nil || msg.Target != s.active.Name() {
		return s, nil
	}

	return s.nickListUpdatedCmd()
}

// handleUserModeChangeEvent reacts to a user-mode change. These events
// are delivered only to the affected client, so every event received
// here can change the visible command set.
func (s ChatScreen) handleUserModeChangeEvent(domain.UserModeChange) (ChatScreen, tea.Cmd) {
	return s, msgCmd(components.CommandsMsg[chatcmd.CompletionContext]{
		Commands: command.VisibleCommands(s.parser.Set(), s.client.Caps()),
	})
}

// handlePartEvent narrates a departure and, when the departing actor
// is the user, closes the window. A PART names the actor that left
// (RFC 2812 §3.2.2): a model leaving drops it from the nick list and
// nothing more — the user is still in the channel, so the sidebar
// entry stays and the visible area does not move.
func (s ChatScreen) handlePartEvent(msg domain.Part) (ChatScreen, tea.Cmd) {
	isUser := s.isOwnSource(msg.Source)

	// Remove the member from the channel's member list.
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		if id, identified := msg.Source.InstanceID(); identified {
			cw.Members.RemoveID(id)
		}
	}

	var cmds []tea.Cmd

	if isUser {
		var closed tea.Cmd
		s, closed = s.closeWindow(msg.Target, msg.At)
		cmds = append(cmds, closed)
	}

	var nickListUpdated tea.Cmd
	s, nickListUpdated = s.nickListUpdatedCmd()
	cmds = append(cmds, nickListUpdated)

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleQuitEvent(msg domain.Quit, targets []domain.ChannelName) (ChatScreen, tea.Cmd) {
	// `bufferProtocolEvent` has already fanned the line into
	// every channel in `targets` and any open DM with the actor.
	// The handler updates the in-memory `Members` snapshot for
	// each affected channel and fires the active-window UI
	// refresh. `targets` comes from the per-recipient
	// [protocol.Delivery.Targets] computed by the session at
	// fan-out time.

	for _, ch := range targets {
		cw, ok := s.channelWindowByName(ch)
		if !ok {
			continue
		}

		if id, identified := msg.Source.InstanceID(); identified {
			cw.Members.RemoveID(id)
		}
	}

	var cmds []tea.Cmd

	for _, ch := range targets {
		if s.active == nil || ch != s.active.Name() {
			continue
		}

		if _, ok := s.active.Window.(*domain.ChannelWindow); ok {
			var nickListUpdated tea.Cmd
			s, nickListUpdated = s.nickListUpdatedCmd()
			cmds = append(cmds, nickListUpdated)
		}

		break
	}

	actorID, identified := msg.Source.InstanceID()
	cmds = append(cmds, s.lifecycleBumps(targets, actorID, identified)...)

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleTopicChangeEvent(msg domain.TopicChange) (ChatScreen, tea.Cmd) {
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		cw.Topic = msg.Topic
		cw.TopicSetBy = msg.Source.Nick()
		cw.TopicSetAt = msg.At
	}

	if s.active == nil || s.active.Name() != msg.Target {
		return s, nil
	}

	return s, msgCmd(components.TopicUpdatedMsg{Topic: msg.Topic})
}

func (s ChatScreen) handleTopicInfoEvent(msg domain.TopicInfo) (ChatScreen, tea.Cmd) {
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		cw.Topic = msg.Topic
		cw.TopicSetBy = msg.TopicSetBy
		cw.TopicSetAt = msg.TopicSetAt
	}

	if s.active == nil || s.active.Name() != msg.Target {
		return s, nil
	}

	return s, s.setChannelCmd()
}

func (s ChatScreen) handleNickChangeEvent(msg domain.NickChange, targets []domain.ChannelName) (ChatScreen, tea.Cmd) {
	actorID, identified := msg.Source.InstanceID()
	dispatchNickChanged := false
	if identified {
		for key := range s.dispatching {
			if key.actor != actorID {
				continue
			}

			s.dispatching[key] = msg.NewNick
			dispatchNickChanged = true
		}
	}
	// The session has already applied the new nick. Update the snapshot in
	// each affected channel's member list, then fire the
	// active-window UI side-effects exactly once. `targets`
	// comes from the per-recipient [protocol.Delivery.Targets]
	// computed by the session at fan-out time.
	for _, ch := range targets {
		cw, ok := s.channelWindowByName(ch)
		if !ok {
			continue
		}

		if identified && cw.Members.HasID(actorID) {
			cw.Members.RenameID(actorID, msg.NewNick)
		} else {
			cw.Members.RenameNick(msg.Source.Nick(), msg.NewNick)
		}
	}

	var cmds []tea.Cmd

	activeIsChannel := s.active != nil && slices.Contains(targets, s.active.Name())

	activeDM, activeIsDM := s.activeDMWith(actorID, identified)
	activeDMVisible := activeIsDM && s.active != nil && activeDM.Name() == s.active.Name()
	if activeIsDM && activeDM.observeNick(msg.NewNick) {
		cmds = append(cmds, msgCmd(components.ChannelAddedMsg{Channel: activeDM}))
		if activeDMVisible {
			var nickListUpdated tea.Cmd
			s, nickListUpdated = s.nickListUpdatedCmd()
			cmds = append(cmds,
				s.setChannelCmd(),
				nickListUpdated,
			)
		}
	}

	if activeIsChannel {
		if _, ok := s.active.Window.(*domain.ChannelWindow); ok {
			var nickListUpdated tea.Cmd
			s, nickListUpdated = s.nickListUpdatedCmd()
			cmds = append(cmds, nickListUpdated)
		}
	}
	if dispatchNickChanged {
		cmds = append(cmds, msgCmd(components.NickListThinkingMsg{Nicks: s.thinkingNicks()}))
	}

	if identified && actorID == s.user.ID() {
		var own tea.Cmd
		s, own = s.handleOwnNickChange(msg, activeIsChannel || activeDMVisible)
		cmds = append(cmds, own)
	} else if activeIsChannel || activeDMVisible {
		cmds = append(cmds, msgCmd(components.HighlightWordsMsg{
			Words:    s.highlightWords,
			UserNick: s.user.Nick(),
		}))
	}

	cmds = append(cmds, s.lifecycleBumps(targets, actorID, identified)...)

	return s, tea.Batch(cmds...)
}

// handleOwnNickChange applies the user's own rename to the three
// parts of the UI that carry the user's own name: the input bar's
// prompt, the highlight-word set that decides what counts as a
// mention, and the welcome checklist. All three are global to the
// session, so all three move on every rename, whatever window is in
// view. Only the nick list belongs to a window, and its refresh stays
// with the caller.
//
// The confirmation line follows the same rule. `bufferActorEvent` has
// already filed the rename into every window the session named as a
// target; `renderedInActive` says whether the visible window was one
// of them, and it is rendered here when it was not, so the user sees
// the answer wherever they typed the command.
func (s ChatScreen) handleOwnNickChange(msg domain.NickChange, renderedInActive bool) (ChatScreen, tea.Cmd) {
	s.checklist.nick = msg.NewNick

	cmds := []tea.Cmd{
		msgCmd(components.UserNickMsg{Nick: msg.NewNick}),
		msgCmd(components.HighlightWordsMsg{
			Words:    s.highlightWords,
			UserNick: msg.NewNick,
		}),
	}

	if !renderedInActive {
		cmds = append(cmds, s.logAndShow(msg))
	}

	if s.realChannelCount() == 0 {
		cmds = append(cmds, msgCmd(components.SetPlaceholderMsg{Text: s.checklist.text()}))
	}

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleKickedEvent(msg domain.Kicked) (ChatScreen, tea.Cmd) {
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		if msg.Subject != domain.AnonymousNick {
			cw.Members.RemoveNick(msg.Subject)
		}
	}

	var cmds []tea.Cmd
	if msg.SubjectIsSelf {
		if persist := s.user.ObserveKick(msg); persist != nil {
			ctx := s.baseContext()
			cmds = append(cmds, func() tea.Msg {
				persist(ctx)
				return nil
			})
		}

		var closed tea.Cmd
		s, closed = s.closeWindow(msg.Target, msg.At)
		cmds = append(cmds, closed)
	}

	var nickListUpdated tea.Cmd
	s, nickListUpdated = s.nickListUpdatedCmd()
	cmds = append(cmds, nickListUpdated)

	return s, tea.Batch(cmds...)
}

// handleMessageEvent renders an incoming Message. The user-client
// holds echo-message, so its own chat traffic returns over the
// protocol bus with [domain.MessageAuthorshipSelf] and renders
// inline. Model-originated
// Messages enter the per-channel paced queue: the first message in an
// empty queue delivers immediately,
// subsequent messages drain at [pacedInterval] cadence.
func (s ChatScreen) handleMessageEvent(msg domain.Message) (ChatScreen, tea.Cmd) {
	key, ok := msg.RoutingKey(s.user.ID())
	if !ok {
		// Foreign DM (model-to-model traffic the user is not a
		// party to). Not surfaced in the user's UI.
		return s, nil
	}

	if msg.AuthoredBy(s.user.ID()) {
		return s, s.renderMessage(msg, key)
	}

	wasEmpty := len(s.pacedQueue[key]) == 0
	s.pacedQueue[key] = append(s.pacedQueue[key], msg)

	// If this channel had no pending messages, deliver immediately;
	// pacing is per-channel, so unrelated channels keep their own
	// schedules.
	if wasEmpty {
		return s, s.deliverNextPacedCmd(key)
	}

	return s, nil
}

// renderMessage emits the off-channel unread bump for a Message
// not targeting the active window. Active-channel messages render
// on the next frame because the message list reads scrollback
// through a getter and `bufferEvent` has already appended the
// message; no live `StoredEvent` is needed.
//
// Whether the body mentions the user is decided here, on the Update
// goroutine, off the highlight set the screen already holds. The
// count is a store query, so it runs in the command: a message
// arriving in a window the user is not reading must not make the one
// they are reading wait on a database.
func (s ChatScreen) renderMessage(msg domain.Message, key domain.ChannelName) tea.Cmd {
	if s.active != nil && key == s.active.Name() {
		return nil
	}

	var visits int
	if w, ok := s.windowByName(key); ok {
		visits = w.Visits
	}

	return s.unreadCountCmd(key, s.isHighlight(msg), visits)
}

// unreadCountCmd asks the store how much of `key` the user has not
// read and hands the answer back as an [unreadCountedMsg]. `visits`
// is the window's visit count at the moment of the request, which
// travels with the result so [ChatScreen.deliverUnreadCount] can
// tell a count that is still current from one the user has read
// past.
func (s ChatScreen) unreadCountCmd(key domain.ChannelName, mention bool, visits int) tea.Cmd {
	return func() tea.Msg {
		ctx := s.baseContext()

		count, err := s.sess.UnreadCount(ctx, key)
		if err != nil {
			slog.Default().WarnContext(ctx, "unread count",
				"component", "ui",
				"screen", "chat",
				"channel", key,
				"error", err,
			)
		}

		return unreadCountedMsg{
			channel: key,
			count:   count,
			mention: mention,
			visits:  visits,
		}
	}
}

// deliverUnreadCount hands a store-read unread count to the sidebar,
// unless the user has visited the window since the count was
// requested. Focusing a window clears its badge and moves the read
// cursor to the end of the channel, so a count read before that
// visit describes a state the user has already left behind, and
// applying it would put the badge back on a window they are reading.
func (s ChatScreen) deliverUnreadCount(msg unreadCountedMsg) (ChatScreen, tea.Cmd) {
	w, ok := s.windowByName(msg.channel)
	if !ok || w.Visits != msg.visits {
		return s, nil
	}

	return s, msgCmd(components.ChannelUnreadMsg{
		Channel: msg.channel,
		Count:   msg.count,
		Mention: msg.mention,
	})
}

// handleModelDispatchStarted marks the model as dispatching in the
// recipient-relative window carried by the delivery and refreshes
// the nick list's thinking indicator.
func (s ChatScreen) handleModelDispatchStarted(
	msg domain.ModelDispatchStarted,
	window protocol.WindowTarget,
) (ChatScreen, tea.Cmd) {
	id, identified := msg.Source.InstanceID()
	if !identified {
		return s, nil
	}
	key, valid := newDispatchWindowKey(id, window)
	if !valid {
		return s, nil
	}

	s.dispatching[key] = msg.Source.Nick()

	return s, msgCmd(components.NickListThinkingMsg{Nicks: s.thinkingNicks()})
}

// handleModelDispatchDone clears the model's dispatching mark for
// this window and refreshes the nick list's thinking indicator.
func (s ChatScreen) handleModelDispatchDone(
	msg domain.ModelDispatchDone,
	window protocol.WindowTarget,
) (ChatScreen, tea.Cmd) {
	if id, identified := msg.Source.InstanceID(); identified {
		if key, valid := newDispatchWindowKey(id, window); valid {
			delete(s.dispatching, key)
		}
	}

	return s, msgCmd(components.NickListThinkingMsg{Nicks: s.thinkingNicks()})
}

// thinkingNicks returns the nicks of the models dispatching in the
// active window. Channel membership is checked again because a
// PART or KICK can arrive before the matching completion.
func (s ChatScreen) thinkingNicks() map[domain.Nick]bool {
	if s.active == nil || len(s.dispatching) == 0 {
		return nil
	}

	activeTarget := protocol.WindowTargetForKey(s.active.Name())
	activeKey, valid := newDispatchWindowKey("", activeTarget)
	if !valid {
		return nil
	}

	thinking := make(map[domain.Nick]bool, len(s.dispatching))
	for key, nick := range s.dispatching {
		if key.kind != activeKey.kind || key.window != activeKey.window {
			continue
		}
		if cw, ok := s.active.Window.(*domain.ChannelWindow); ok && !cw.Members.HasID(key.actor) {
			continue
		}

		thinking[nick] = true
	}

	return thinking
}

// isHighlight reports whether msg's body mentions the user, matching
// [components.renderMessage]'s exemption for the user's own messages.
// Older synthetic events can lack recipient-relative authorship, so
// the empty user ID remains the fallback for those events.
func (s ChatScreen) isHighlight(msg domain.Message) bool {
	return !msg.AuthoredBy(s.user.ID()) &&
		components.ContainsHighlightWord(msg.Body, s.highlightWords, s.user.Nick())
}

func (s ChatScreen) activeMemberNicks() iter.Seq[domain.Nick] {
	if s.active == nil {
		return func(func(domain.Nick) bool) {}
	}

	cw, ok := s.active.Window.(*domain.ChannelWindow)
	if !ok {
		return func(func(domain.Nick) bool) {}
	}

	return cw.Members.Nicks()
}

// otherInstances iterates every registered instance except this
// client's own. The session's directory names every client with a
// connection record, this one included; a completion offers a nick
// to address, and typing your own nick after `/msg` or `/invite` is
// not what the suggestion list is for.
func (s ChatScreen) otherInstances() iter.Seq[domain.InstanceDirectoryEntry] {
	return func(yield func(domain.InstanceDirectoryEntry) bool) {
		self := s.user.ID()

		for inst := range s.sess.Instances(s.baseContext()) {
			if inst.InstanceID == self {
				continue
			}

			if !yield(inst) {
				return
			}
		}
	}
}

func (s ChatScreen) isOwnSource(source domain.Source) bool {
	id, identified := source.InstanceID()
	return identified && id == s.user.ID()
}
