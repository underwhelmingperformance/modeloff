package screens

import (
	"iter"
	"log/slog"
	"slices"

	tea "github.com/charmbracelet/bubbletea"

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
// the wire payload. Nil for window-scoped events.
type protocolEventMsg struct {
	event   protocol.Event
	targets []domain.ChannelName
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
	}

	return s, nil, false
}

// handleProtocolEvent dispatches wire-shaped events plus the
// session-emitted events whose ordering relative to the wire
// sequence matters: joins, parts, messages, mode changes, topic
// info, dispatch lifecycle, names replies, the status-window
// signal, and focus changes.
func (s ChatScreen) handleProtocolEvent(msg protocolEventMsg) (ChatScreen, tea.Cmd) {
	var cmd tea.Cmd

	buffered := s.bufferProtocolEvent(msg.event, msg.targets)

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
		s, cmd = s.handleModelDispatchStarted(evt)
	case domain.ModelDispatchDone:
		s, cmd = s.handleModelDispatchDone(evt)
	case domain.NamesReplyEvent:
		s, cmd = s.handleNamesReply(evt)
	}

	return s, tea.Batch(buffered, cmd, s.scrollbackUpdatedCmd(), s.listenForProtocolEvents())
}

// listenForProtocolEvents reads the next delivery from the
// user-client subscription's protocol channel and wraps its event
// in a protocolEventMsg. The chat-screen does not consume the
// span context the delivery carries — that is for model-client
// dispatch goroutines to link their turn spans to the originating
// handler. After each delivery, this should be re-invoked so the
// channel is continuously drained.
func (s ChatScreen) listenForProtocolEvents() tea.Cmd {
	ch := s.client.Events()

	return func() tea.Msg {
		delivery, ok := <-ch
		if !ok {
			return nil
		}

		return protocolEventMsg{event: delivery.Event, targets: delivery.Targets}
	}
}

// scrollbackUpdatedCmd nudges the message list to re-evaluate the
// active window's scrollback after an event was buffered. Without
// the nudge the new content would still render on the next View
// because the message list reads through a getter, but the seen mark
// would never move over it: a line the user watched arrive would
// stay behind the divider, and an off-bottom user would never see
// the "new messages" line at all.
func (s ChatScreen) scrollbackUpdatedCmd() tea.Cmd {
	if s.active == "" {
		return nil
	}

	return msgCmd(components.ScrollbackUpdatedMsg{Channel: s.active})
}

// handleNamesReply applies the joiner-targeted member-list snapshot
// to the local channel cache and proposes the freshly-joined
// channel as the focus target. Pre-existing members of the channel
// — models, other users — are otherwise invisible to the chat
// screen's cache; without this handler, switching to a freshly-
// joined channel would show only the user's own name.
//
// The focus proposal carries the window's `UserTime` (the
// join-event timestamp), so the arbiter in `handleChannelFocus`
// keeps the user where they are if they've already navigated past
// this join, and lands them on the freshest autojoin channel
// otherwise.
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

	cmds := []tea.Cmd{
		msgCmd(chatcmd.ChannelFocusMsg{Channel: msg.Channel, At: w.UserTime}),
	}

	if isChannel && msg.Channel == s.active {
		cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: cw.Members}))
	}

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleJoinEvent(msg domain.Join) (ChatScreen, tea.Cmd) {
	isUser := msg.Instance == s.user.Instance()

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

	if cw != nil && !cw.Members.HasInstance(msg.Instance) {
		cw.Members.Add(msg.Instance)
	}

	if !isUser {
		if msg.Target == s.active && cw != nil {
			return s, msgCmd(components.NickListUpdatedMsg{Members: cw.Members})
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

	cw.Members.ApplyMode(msg.Instance, msg.Flag, msg.Add)

	if msg.Target != s.active {
		return s, nil
	}

	return s, msgCmd(components.NickListUpdatedMsg{Members: cw.Members})
}

// handleUserModeChangeEvent reacts to a user-mode change. When the
// change targets the user-client's own instance, the visible command
// set may have flipped — re-emit CommandsMsg from VisibleCommands so
// the /help slice and the completion popover both reflect the new
// capability state on next render.
func (s ChatScreen) handleUserModeChangeEvent(msg domain.UserModeChange) (ChatScreen, tea.Cmd) {
	if msg.InstanceID != s.user.Instance().ID() {
		return s, nil
	}

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
	isUser := msg.Instance == s.user.Instance()

	// Remove the member from the channel's member list.
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		if m, mOK := cw.Members.GetByInstance(msg.Instance); mOK {
			cw.Members.Remove(m)
		}
	}

	var cmds []tea.Cmd

	if isUser {
		var closed tea.Cmd
		s, closed = s.closeWindow(msg.Target, msg.At)
		cmds = append(cmds, closed)
	}

	var members domain.MemberList

	if s.active != "" {
		if cw, ok := s.channelWindowByName(s.active); ok {
			members = cw.Members
		}
	}

	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))

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

		if m, mOK := cw.Members.GetByInstance(msg.Instance); mOK {
			cw.Members.Remove(m)
		}
	}

	var cmds []tea.Cmd

	for _, ch := range targets {
		if ch != s.active {
			continue
		}

		if cw, ok := s.channelWindowByName(s.active); ok {
			cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: cw.Members}))
		}
	}

	cmds = append(cmds, s.lifecycleBumps(targets, msg.Instance)...)

	next, exitCmd := s.exitOnOwnQuit(msg)
	cmds = append(cmds, exitCmd)

	return next, tea.Batch(cmds...)
}

// exitOnOwnQuit takes the screen down when the QUIT it just handled
// was this client's own. A `/quit` reaches here too, but the quit
// handler has already set `quitting` and has its own
// [ui.QuitCompleteMsg] in flight, so the only QUIT that gets this far
// unannounced is one the server ran without being asked: a KILL
// naming this client.
//
// Ending the session-active marker is the same bookkeeping
// [userclient.UserClient.Quit] does, and for the same reason: the
// connection ended through the server's own teardown, so the
// memberships are already gone and the next start has nothing to
// reconcile.
func (s ChatScreen) exitOnOwnQuit(msg domain.Quit) (ChatScreen, tea.Cmd) {
	if s.quitting || msg.Instance != s.user.Instance() {
		return s, nil
	}

	s.quitting = true

	return s, func() tea.Msg {
		return ui.QuitCompleteMsg{Err: s.user.Disconnected(s.baseContext())}
	}
}

func (s ChatScreen) handleTopicChangeEvent(msg domain.TopicChange) (ChatScreen, tea.Cmd) {
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		cw.Topic = msg.Topic
		cw.TopicSetBy = msg.By
		cw.TopicSetAt = msg.At
	}

	if s.active != msg.Target {
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

	if s.active != msg.Target {
		return s, nil
	}

	return s, s.setChannelCmd()
}

func (s ChatScreen) handleNickChangeEvent(msg domain.NickChange, targets []domain.ChannelName) (ChatScreen, tea.Cmd) {
	// `msg.Instance.Nick()` is already the new value — the
	// session renames before emitting. Update the snapshot in
	// each affected channel's member list, then fire the
	// active-window UI side-effects exactly once. `targets`
	// comes from the per-recipient [protocol.Delivery.Targets]
	// computed by the session at fan-out time.
	for _, ch := range targets {
		cw, ok := s.channelWindowByName(ch)
		if !ok {
			continue
		}

		if cw.Members.HasInstance(msg.Instance) {
			cw.Members.RenameTo(msg.Instance, msg.NewNick)
		}
	}

	var cmds []tea.Cmd

	activeIsChannel := slices.Contains(targets, s.active)

	activeDM, activeIsDM := s.activeDMWith(msg.Instance)
	activeDMVisible := activeIsDM && activeDM.Name() == s.active

	if activeIsChannel {
		if cw, ok := s.channelWindowByName(s.active); ok {
			cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: cw.Members}))
		}
	}

	if msg.Instance == s.user.Instance() {
		var own tea.Cmd
		s, own = s.handleOwnNickChange(msg, activeIsChannel)
		cmds = append(cmds, own)
	} else if activeIsChannel || activeDMVisible {
		cmds = append(cmds, msgCmd(components.HighlightWordsMsg{
			Words:    s.highlightWords,
			UserNick: s.user.Nick(),
		}))
	}

	cmds = append(cmds, s.lifecycleBumps(targets, msg.Instance)...)

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
		cmds = append(cmds, msgCmd(components.SetPlaceholderMsg{Text: s.checklist.Render()}))
	}

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleKickedEvent(msg domain.Kicked) (ChatScreen, tea.Cmd) {
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		if m, mOK := cw.Members.GetByInstance(msg.Instance); mOK {
			cw.Members.Remove(m)
		}
	}

	var members domain.MemberList

	if cw, ok := s.channelWindowByName(s.active); ok {
		members = cw.Members
	}

	return s, msgCmd(components.NickListUpdatedMsg{Members: members})
}

// handleMessageEvent renders an incoming Message. The user-client
// holds echo-message, so its own chat traffic returns over the
// protocol bus; identified here by an empty InstanceID matching
// [protocol.UserClientID], it renders inline. Model-originated
// Messages enter the per-channel paced queue: the first message in an
// empty queue delivers immediately,
// subsequent messages drain at [pacedInterval] cadence.
func (s ChatScreen) handleMessageEvent(msg domain.Message) (ChatScreen, tea.Cmd) {
	key, ok := msg.RoutingKey(s.user.Instance().ID())
	if !ok {
		// Foreign DM (model-to-model traffic the user is not a
		// party to). Not surfaced in the user's UI.
		return s, nil
	}

	if msg.InstanceID == protocol.UserClientID {
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
	if key == s.active {
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

// handleModelDispatchStarted marks `msg.Instance` as currently
// dispatching and refreshes the nick list's thinking indicator,
// which surfaces every dispatching instance whose membership the
// active window can see.
func (s ChatScreen) handleModelDispatchStarted(msg domain.ModelDispatchStarted) (ChatScreen, tea.Cmd) {
	if msg.Instance == nil {
		return s, nil
	}

	s.dispatching[msg.Instance] = true

	return s, msgCmd(components.NickListThinkingMsg{Nicks: s.thinkingNicks()})
}

// handleModelDispatchDone clears the dispatching mark for
// `msg.Instance` and refreshes the nick list's thinking indicator.
func (s ChatScreen) handleModelDispatchDone(msg domain.ModelDispatchDone) (ChatScreen, tea.Cmd) {
	if msg.Instance != nil {
		delete(s.dispatching, msg.Instance)
	}

	return s, msgCmd(components.NickListThinkingMsg{Nicks: s.thinkingNicks()})
}

// thinkingNicks returns the nicks of every dispatching instance
// that is also a member of the active channel. Models running in
// channels the user is not in stay invisible — RFC 2812 §3.3.1's
// intersection rule applied to the local view.
func (s ChatScreen) thinkingNicks() map[domain.Nick]bool {
	if s.active == "" || len(s.dispatching) == 0 {
		return nil
	}

	cw, ok := s.channelWindowByName(s.active)
	if !ok {
		return nil
	}

	thinking := make(map[domain.Nick]bool, len(s.dispatching))
	for inst := range s.dispatching {
		if !cw.Members.HasInstance(inst) {
			continue
		}

		thinking[inst.Nick()] = true
	}

	return thinking
}

// isHighlight reports whether msg's body mentions the user, matching
// [components.renderMessage]'s exemption for the user's own messages:
// the user-client carries no [domain.InstanceID] (the empty string is
// [protocol.UserClientID]'s sentinel), so an empty InstanceID picks
// out the user's own message and exempts it from the mention check.
// The user should never see a mention badge on their own words.
func (s ChatScreen) isHighlight(msg domain.Message) bool {
	return msg.InstanceID != "" && components.ContainsHighlightWord(msg.Body, s.highlightWords, s.user.Nick())
}

func (s ChatScreen) activeMemberNicks() iter.Seq[domain.Nick] {
	cw, ok := s.channelWindowByName(s.active)
	if !ok {
		return func(func(domain.Nick) bool) {}
	}

	return cw.Members.Nicks()
}

// activeChannelInstances iterates the `*Instance` handles for every
// member of the currently-active channel. Tab completion sources this
// iterator: the user only gets completions for nicks they can already
// see in their nick list, matching IRC semantics.
func (s ChatScreen) activeChannelInstances() iter.Seq[*domain.Instance] {
	return func(yield func(*domain.Instance) bool) {
		cw, ok := s.channelWindowByName(s.active)
		if !ok {
			return
		}

		for m := range cw.Members.All() {
			if !yield(m.Instance) {
				return
			}
		}
	}
}
