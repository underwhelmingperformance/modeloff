package screens

import (
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
)

// handleProtocolEvent dispatches wire-shaped events plus the
// session-emitted events whose ordering relative to the wire
// sequence matters: joins, parts, messages, mode changes, topic
// info, dispatch lifecycle, names replies, the status-window
// signal, and focus changes.
func (s ChatScreen) handleProtocolEvent(msg protocolEventMsg) (ui.Model, tea.Cmd) {
	var (
		updated ui.Model
		cmd     tea.Cmd
	)

	s.bufferProtocolEvent(msg.event, msg.targets)

	switch evt := msg.event.(type) {
	case domain.Join:
		updated, cmd = s.handleJoinEvent(evt)
	case domain.Part:
		updated, cmd = s.handlePartEvent(evt)
	case domain.Quit:
		updated, cmd = s.handleQuitEvent(evt, msg.targets)
	case domain.ModeChange:
		updated, cmd = s.handleModeChangeEvent(evt)
	case domain.Message:
		updated, cmd = s.handleMessageEvent(evt)
	case domain.TopicChange:
		updated, cmd = s.handleTopicChangeEvent(evt)
	case domain.NickChange:
		updated, cmd = s.handleNickChangeEvent(evt, msg.targets)
	case domain.ModelInvited:
		updated, cmd = s.handleModelInvitedEvent(evt)
	case domain.ModelKicked:
		updated, cmd = s.handleModelKickedEvent(evt)
	case domain.TopicInfo:
		updated, cmd = s.handleTopicInfoEvent(evt)
	case domain.ModelDispatchStarted:
		updated, cmd = s.handleModelDispatchStarted(evt)
	case domain.ModelDispatchDone:
		updated, cmd = s.handleModelDispatchDone(evt)
	case domain.NamesReplyEvent:
		updated, cmd = s.handleNamesReply(evt)
	}

	if updated != nil {
		s = updated.(ChatScreen)
	}

	return s, tea.Batch(cmd, s.scrollbackUpdatedCmd(), s.listenForProtocolEvents())
}

// scrollbackUpdatedCmd nudges the message list to re-evaluate the
// active window's scrollback after an event was buffered. Without
// the nudge the new content would still render on the next View
// because the message list reads through a getter, but the divider
// latch would miss the per-tick growth signal and an off-bottom user
// would never see the "new messages" line.
func (s ChatScreen) scrollbackUpdatedCmd() tea.Cmd {
	if s.active == nil || *s.active == "" {
		return nil
	}

	return msgCmd(components.ScrollbackUpdatedMsg{Channel: *s.active})
}

func (s ChatScreen) handleChannelFocus(msg chatcmd.ChannelFocusMsg) (ui.Model, tea.Cmd) {
	w, exists := s.windowByName(msg.Channel)
	if !exists {
		// A focus event for a window the chat screen doesn't
		// track is either a startup race (cache not yet populated
		// by `bootstrapFromSession` or by a JOIN handler) or a
		// stale event for a channel the user has just parted.
		// The latter must not resurrect the parted channel as
		// the new active, so we don't auto-create here — the
		// JOIN/bootstrap paths own cache population and the
		// focus path defers to whatever they install.
		return s, nil
	}

	if !s.focusWins(msg.At) {
		// A staler focus event than the user's current
		// interaction. Flag the target as having activity for
		// the sidebar to surface; leave the visible area where
		// the user put it.
		w.Activity = true

		return s, msgCmd(components.ChannelHasLifecycleMsg{Channel: msg.Channel})
	}

	*s.active = msg.Channel
	w.UserTime = msg.At

	var members domain.MemberList
	if cw, ok := w.Window.(*domain.ChannelWindow); ok {
		members = cw.Members
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.SetPlaceholderMsg{}))
	cmds = append(cmds, msgCmd(components.SetChannelMsg{
		Channel: msg.Channel,
		Topic:   s.activeTopic(),
		Kind:    w.Kind(),
	}))

	cmds = append(cmds, msgCmd(components.ChannelActiveMsg{Channel: msg.Channel}))
	cmds = append(cmds, s.persistLastChannel(msg.Channel))
	cmds = append(cmds, msgCmd(components.ChannelUnreadMsg{Channel: msg.Channel, Count: 0}))
	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))

	return s, tea.Batch(cmds...)
}

// focusWins decides whether an incoming focus event should take
// over the visible area. The arbiter compares the event's
// timestamp against the active window's `UserTime`: a strictly
// newer event wins, anything stamped at or before the user's last
// interaction with the current active is treated as background
// activity and surfaces on the sidebar instead. An empty active —
// the startup case — accepts any event.
func (s ChatScreen) focusWins(at time.Time) bool {
	if s.active == nil || *s.active == "" {
		return true
	}

	active, ok := s.windowByName(*s.active)
	if !ok {
		return true
	}

	return at.After(active.UserTime)
}

// persistLastChannel writes the user's currently-active channel
// to the store so a subsequent restart restores them to the same
// view. An empty channel name and a nil store are no-ops.
func (s ChatScreen) persistLastChannel(ch domain.ChannelName) tea.Cmd {
	if ch == "" || s.uiState == nil {
		return nil
	}

	return func() tea.Msg {
		if err := s.uiState.SetLastChannel(s.baseContext(), ch); err != nil {
			slog.Default().ErrorContext(s.baseContext(), "persist last channel", "channel", ch, "error", err)
		}

		return nil
	}
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
func (s ChatScreen) handleNamesReply(msg domain.NamesReplyEvent) (ui.Model, tea.Cmd) {
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

	if isChannel && msg.Channel == *s.active {
		cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: cw.Members}))
	}

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleJoinEvent(msg domain.Join) (ui.Model, tea.Cmd) {
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
		if msg.Target == *s.active && cw != nil {
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

func (s ChatScreen) handleModeChangeEvent(msg domain.ModeChange) (ui.Model, tea.Cmd) {
	if !msg.ChannelMode() {
		return s.handleUserModeChangeEvent(msg)
	}

	cw, ok := s.channelWindowByName(msg.Target)
	if !ok {
		return s, nil
	}

	cw.Members.SetMode(msg.Instance, domain.NickModeFor(msg.Flag, msg.Add))

	if msg.Target != *s.active {
		return s, nil
	}

	return s, msgCmd(components.NickListUpdatedMsg{Members: cw.Members})
}

// handleUserModeChangeEvent reacts to a user-mode ModeChange (empty
// Target). When the change targets the user-client's own instance,
// the visible command set may have flipped — re-emit CommandsMsg
// from VisibleCommands so the /help slice and the completion
// popover both reflect the new capability state on next render.
func (s ChatScreen) handleUserModeChangeEvent(msg domain.ModeChange) (ui.Model, tea.Cmd) {
	if msg.InstanceID != s.user.Instance().ID() {
		return s, nil
	}

	return s, msgCmd(components.CommandsMsg[chatcmd.CompletionContext]{
		Commands: command.VisibleCommands(s.parser.Set(), s.client.Caps()),
	})
}

func (s ChatScreen) handlePartEvent(msg domain.Part) (ui.Model, tea.Cmd) {
	leavingActive := *s.active == msg.Target

	// Remove the member from the channel's member list.
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		if m, mOK := cw.Members.GetByInstance(msg.Instance); mOK {
			cw.Members.Remove(m)
		}
	}

	// If the user is leaving, remove the channel and purge any
	// pending paced messages queued for it. Already-scheduled ticks
	// for the parted channel's queue will no-op via
	// deliverNextPaced's empty-queue branch when they fire.
	if msg.Instance == s.user.Instance() {
		s.channels.Remove(windowKey(msg.Target))
		delete(s.pacedQueue, msg.Target)
		s.checklist.channelCount = s.realChannelCount()
	}

	var cmds []tea.Cmd
	cmds = append(cmds, msgCmd(components.ChannelRemovedMsg{Channel: msg.Target}))

	if leavingActive {
		if first, ok := s.firstRealChannel(); ok {
			*s.active = first.Name()
			// Bump the new active's `UserTime` to the Part moment.
			// The Part is the user's freshest deliberate action,
			// so any still-in-flight focus event from before the
			// Part (e.g. a buffered `NamesReply` for the channel
			// we just left) must lose the next [focusWins]
			// comparison and not steal the visible area back.
			first.UserTime = msg.At
		} else {
			*s.active = ""
			cmds = append(cmds, msgCmd(components.SetPlaceholderMsg{
				Text: s.checklist.Render(),
			}))
		}

		cmds = append(cmds, msgCmd(components.SetChannelMsg{
			Channel: *s.active,
			Topic:   s.activeTopic(),
			Kind:    s.activeKind(),
		}))
		cmds = append(cmds, msgCmd(components.ChannelActiveMsg{Channel: *s.active}))
		cmds = append(cmds, s.persistLastChannel(*s.active))
	}

	var members domain.MemberList

	if *s.active != "" {
		if cw, ok := s.channelWindowByName(*s.active); ok {
			members = cw.Members
		}
	}

	cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: members}))

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleQuitEvent(msg domain.Quit, targets []domain.ChannelName) (ui.Model, tea.Cmd) {
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
		if ch != *s.active {
			continue
		}

		if cw, ok := s.channelWindowByName(*s.active); ok {
			cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: cw.Members}))
		}
	}

	cmds = append(cmds, s.lifecycleBumps(targets, msg.Instance)...)

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) handleTopicChangeEvent(msg domain.TopicChange) (ui.Model, tea.Cmd) {
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		cw.Topic = msg.Topic
		cw.TopicSetBy = msg.By
		cw.TopicSetAt = msg.At
	}

	if *s.active != msg.Target {
		return s, nil
	}

	return s, msgCmd(components.TopicUpdatedMsg{Topic: msg.Topic})
}

func (s ChatScreen) handleTopicInfoEvent(msg domain.TopicInfo) (ui.Model, tea.Cmd) {
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		cw.Topic = msg.Topic
		cw.TopicSetBy = msg.TopicSetBy
		cw.TopicSetAt = msg.TopicSetAt
	}

	if *s.active != msg.Target {
		return s, nil
	}

	return s, msgCmd(components.SetChannelMsg{
		Channel: msg.Target,
		Topic:   msg.Topic,
		Kind:    s.activeKind(),
	})
}

func (s ChatScreen) handleNickChangeEvent(msg domain.NickChange, targets []domain.ChannelName) (ui.Model, tea.Cmd) {
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

	activeIsChannel := slices.Contains(targets, *s.active)

	activeDM, activeIsDM := s.activeDMWith(msg.Instance)
	activeDMVisible := activeIsDM && activeDM.Name() == *s.active

	if activeIsChannel || activeDMVisible {
		if activeIsChannel {
			if cw, ok := s.channelWindowByName(*s.active); ok {
				cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: cw.Members}))
			}
		}

		if msg.Instance == s.user.Instance() {
			cmds = append(cmds, msgCmd(components.UserNickMsg{Nick: msg.NewNick}))
		}

		cmds = append(cmds, msgCmd(components.HighlightWordsMsg{
			Words:    s.highlightWords,
			UserNick: s.user.Nick(),
		}))
	}

	cmds = append(cmds, s.lifecycleBumps(targets, msg.Instance)...)

	return s, tea.Batch(cmds...)
}

// activeDMWith returns the open DM whose counterpart is `actor`,
// if any.
func (s ChatScreen) activeDMWith(actor *domain.Instance) (*domain.DMWindow, bool) {
	if actor == nil {
		return nil, false
	}

	for w := range s.channels.All() {
		dm, ok := w.Window.(*domain.DMWindow)
		if !ok {
			continue
		}

		if dm.Counterpart == actor {
			return dm, true
		}
	}

	return nil, false
}

func (s ChatScreen) handleModelInvitedEvent(msg domain.ModelInvited) (ui.Model, tea.Cmd) {
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		if !cw.Members.HasInstance(msg.Instance) {
			cw.Members.Add(msg.Instance)
		}
	}

	var members domain.MemberList

	if cw, ok := s.channelWindowByName(*s.active); ok {
		members = cw.Members
	}

	return s, msgCmd(components.NickListUpdatedMsg{Members: members})
}

func (s ChatScreen) handleModelKickedEvent(msg domain.ModelKicked) (ui.Model, tea.Cmd) {
	if cw, ok := s.channelWindowByName(msg.Target); ok {
		if m, mOK := cw.Members.GetByInstance(msg.Instance); mOK {
			cw.Members.Remove(m)
		}
	}

	var members domain.MemberList

	if cw, ok := s.channelWindowByName(*s.active); ok {
		members = cw.Members
	}

	return s, msgCmd(components.NickListUpdatedMsg{Members: members})
}

// handleMessageEvent renders an incoming Message. User-originated
// Messages (the synthesised echo from the send-cmd path, identified
// by an empty InstanceID matching [protocol.UserClientID]) render
// inline. Model-originated Messages enter the per-channel paced
// queue: the first message in an empty queue delivers immediately,
// subsequent messages drain at [pacedInterval] cadence.
func (s ChatScreen) handleMessageEvent(msg domain.Message) (ui.Model, tea.Cmd) {
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
func (s ChatScreen) renderMessage(msg domain.Message, key domain.ChannelName) tea.Cmd {
	if key == *s.active {
		return nil
	}

	count, _ := s.sess.UnreadCount(s.baseContext(), key)
	mention := s.isHighlight(msg.Body)

	return msgCmd(components.ChannelUnreadMsg{Channel: key, Count: count, Mention: mention})
}

// handleDMOpenedMsg materialises the DM window in the sidebar
// (insert is idempotent), optionally focus-switches, and
// optionally sends a trailing body. `/query` sets `Focus`;
// `/msg` does not.
func (s ChatScreen) handleDMOpenedMsg(msg chatcmd.DMOpenedMsg) (ui.Model, tea.Cmd) {
	dm := domain.NewDMWindow(msg.Counterpart, msg.At)
	name := dm.Name()

	_, alreadyOpen := s.channels.Get(windowKey(name))

	if !alreadyOpen {
		s.channels.Insert(newWindow(dm))
	}

	var cmds []tea.Cmd

	if !alreadyOpen {
		cmds = append(cmds, msgCmd(components.ChannelAddedMsg{Channel: dm}))
	}

	if msg.Focus {
		*s.active = name
		cmds = append(cmds, msgCmd(components.SetPlaceholderMsg{}))
		cmds = append(cmds, msgCmd(components.SetChannelMsg{
			Channel: name,
			Topic:   s.activeTopic(),
			Kind:    domain.KindDM,
		}))
		cmds = append(cmds, msgCmd(components.ChannelActiveMsg{Channel: name}))
		cmds = append(cmds, s.persistLastChannel(name))
		cmds = append(cmds, msgCmd(components.NickListUpdatedMsg{Members: domain.MemberList{}}))
	}

	if msg.Body != "" {
		cmds = append(cmds, s.sendMessageCmd(name, msg.Body))
	}

	return s, tea.Sequence(cmds...)
}

// sendMessageCmd fires a user `SendMessage` and returns the
// persisted [domain.Message] as a tea.Msg for local render.
func (s ChatScreen) sendMessageCmd(target domain.ChannelName, body string) tea.Cmd {
	return func() tea.Msg {
		msg, err := s.user.SendMessage(s.baseContext(), target, body)
		if err != nil {
			return domain.ErrorEvent{Operation: "msg", Err: err, At: time.Now()}
		}

		return msg
	}
}

func (s ChatScreen) handleErrorEvent(msg domain.ErrorEvent) (ui.Model, tea.Cmd) {
	var cmds []tea.Cmd

	cmds = append(cmds, s.logAndShow(domain.CommandError{
		Target: *s.active,
		Err:    fmt.Sprintf("%s: %s", msg.Operation, msg.Err),
		At:     msg.At,
	}))

	cmds = append(cmds, msgCmd(components.NickListThinkingMsg{}))

	return s, tea.Batch(cmds...)
}

// handleModelDispatchStarted marks `msg.Instance` as currently
// dispatching and refreshes the nick list's thinking indicator,
// which surfaces every dispatching instance whose membership the
// active window can see.
func (s ChatScreen) handleModelDispatchStarted(msg domain.ModelDispatchStarted) (ui.Model, tea.Cmd) {
	if msg.Instance == nil {
		return s, nil
	}

	s.dispatching[msg.Instance] = true

	return s, msgCmd(components.NickListThinkingMsg{Nicks: s.thinkingNicks()})
}

// handleModelDispatchDone clears the dispatching mark for
// `msg.Instance` and refreshes the nick list's thinking indicator.
func (s ChatScreen) handleModelDispatchDone(msg domain.ModelDispatchDone) (ui.Model, tea.Cmd) {
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
	if s.active == nil || *s.active == "" || len(s.dispatching) == 0 {
		return nil
	}

	cw, ok := s.channelWindowByName(*s.active)
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

func (s ChatScreen) isHighlight(body string) bool {
	return components.ContainsHighlightWord(body, s.highlightWords, s.user.Nick())
}

func (s ChatScreen) handleLiveModelsLoaded(msg liveModelsLoadedMsg) (ui.Model, tea.Cmd) {
	*s.liveModels = msg.models
	*s.liveModelsState = command.SuggestionStateReady
	s.checklist.modelCount = len(msg.models)

	if s.realChannelCount() == 0 {
		return s, msgCmd(components.SetPlaceholderMsg{Text: s.checklist.Render()})
	}

	return s, nil
}

// handleLiveModelsLoadFailed is the UI-policy home for live-model
// load failures. When `*s.active` is empty — no real channel
// joined yet — the notice is routed to `&modeloff`, the
// chat-screen-owned default landing window.
func (s ChatScreen) handleLiveModelsLoadFailed(msg liveModelsLoadFailedMsg) (ui.Model, tea.Cmd) {
	*s.liveModels = nil
	s.checklist.modelCount = 0

	// ErrNoAPIKey here is a TOCTOU between loadLiveModels' HasAPIKey
	// short-circuit and Session.ListModels' check; treat as silent.
	if errors.Is(msg.err, modelclient.ErrNoAPIKey) {
		*s.liveModelsState = command.SuggestionStateReady
		return s, nil
	}

	*s.liveModelsState = command.SuggestionStateError

	channel := *s.active
	if channel == "" {
		channel = domain.StatusChannelName
	}

	slog.Default().WarnContext(s.baseContext(), "live models load failed",
		"component", "ui",
		"channel", string(channel),
		"error", msg.err,
	)

	return s, s.logAndShowOn(channel, domain.SystemNotice{
		Target: channel,
		Text:   fmt.Sprintf("Model list unavailable: %s.", msg.err),
		At:     time.Now(),
	})
}

func (s ChatScreen) activeTopic() string {
	if *s.active == "" {
		return ""
	}

	cw, ok := s.channelWindowByName(*s.active)
	if !ok {
		return ""
	}

	return cw.Topic
}

func (s ChatScreen) activeKind() domain.ChannelKind {
	if *s.active == "" {
		return domain.KindChannel
	}

	w, ok := s.windowByName(*s.active)
	if !ok {
		return domain.InferChannelKind(*s.active)
	}

	return w.Kind()
}

func (s ChatScreen) activeMemberNicks() iter.Seq[domain.Nick] {
	cw, ok := s.channelWindowByName(*s.active)
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
		cw, ok := s.channelWindowByName(*s.active)
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

// windowByName returns the cached `*Window` for the given name.
func (s ChatScreen) windowByName(name domain.ChannelName) (*Window, bool) {
	return s.channels.Get(windowKey(name))
}

// scrollbackOf returns the in-memory scrollback for the named
// window, or nil if the chat-screen has no entry for it. Test-only
// helper; production reads go through the message-list's getter
// closure.
func (s ChatScreen) scrollbackOf(name domain.ChannelName) []domain.StoredEvent {
	w, ok := s.windowByName(name)
	if !ok {
		return nil
	}

	return w.Scrollback
}

// channelWindowByName looks up the cached entry and asserts its
// embedded [domain.Window] is a `*ChannelWindow`. Returns false
// either way for non-channel kinds (status / DM) or absent entries;
// the channel-only handlers (`handleJoinEvent`,
// `handleModeChangeEvent`, etc.) use this to read and mutate
// `Members` / `Topic` off the typed handle.
func (s ChatScreen) channelWindowByName(name domain.ChannelName) (*domain.ChannelWindow, bool) {
	w, ok := s.windowByName(name)
	if !ok {
		return nil, false
	}

	cw, ok := w.Window.(*domain.ChannelWindow)
	return cw, ok
}
