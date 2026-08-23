package screens

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
)

type replyEventMsg struct {
	issuingWindow domain.Window
	event         domain.ProtocolEvent
}

// routeReplies answers a command the user issued: the point-to-point
// numerics the dispatcher returned in `protocol.Response.Events`, the
// UI feedback the chat screen raises for itself (help, usage hints,
// the cleared window), and the failure a command came back with. None
// of these is channel activity, so each renders into an in-memory
// scrollback and none reaches the shared channel log.
func (s ChatScreen) routeReplies(msg tea.Msg) (ChatScreen, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case chatcmd.CommandResult:
		return s.routeCommandResult(msg)

	case chatcmd.HelpResult, chatcmd.ClearResult, chatcmd.TopicInfoResult,
		chatcmd.UsageError, chatcmd.NoChannelError, chatcmd.PokeRequested:
		return s.routeCommandResult(chatcmd.CommandResult{
			IssuingWindow: activeWindowIdentity(s),
			Message:       msg,
		})

	case replyEventMsg:
		return s, s.logReplyEvent(msg.issuingWindow, msg.event), true

	case domain.Inviting:
		return s, s.logAndShowOn(msg.Target, msg), true

	case domain.SystemNotice:
		return s, s.logAndShowOn(msg.Target, msg), true

	case domain.Whois:
		return s, s.logAndShow(msg), true

	case domain.ListReply:
		return s, s.logAndShow(msg), true

	case domain.ListEnd:
		return s, s.logAndShow(msg), true

	case domain.ErrorEvent:
		next, cmd := s.handleErrorEvent(msg)
		return next, cmd, true
	}

	return s, nil, false
}

func activeWindowIdentity(s ChatScreen) domain.Window {
	if s.active == nil {
		return nil
	}

	return s.active.Window
}

func activeWindowRevision(s ChatScreen) uint64 {
	if s.active == nil {
		return 0
	}

	return s.active.Revision
}

func (s ChatScreen) routeCommandResult(
	result chatcmd.CommandResult,
) (ChatScreen, tea.Cmd, bool) {
	switch msg := result.Message.(type) {
	case chatcmd.ReplyEvents:
		if listReplyEvents(msg.Events) {
			return s.applyListReplyEvents(result.IssuingWindow, msg)
		}

		return s, s.deliverReplyEvents(result.IssuingWindow, msg), true

	case chatcmd.CommandErrorResult:
		next, cmd := s.handleErrorEventForWindow(msg.Error, result.IssuingWindow)
		return next, cmd, true

	case chatcmd.HelpResult:
		return s, s.logReplyEvent(result.IssuingWindow, domain.Help{
			Target: issuingWindowName(result.IssuingWindow), At: time.Now(),
		}), true

	case chatcmd.ClearResult:
		window, ok := s.exactWindow(result.IssuingWindow)
		if !ok {
			return s, nil, true
		}

		window.Scrollback.Clear()

		return s, msgCmd(components.ScrollbackClearedMsg{Channel: window.Name()}), true

	case chatcmd.DMClosedMsg:
		window, ok := s.exactWindow(result.IssuingWindow)
		if !ok || window.Kind() != domain.KindDM || window.Name() != msg.Window ||
			window.Revision != result.IssuingWindowRevision {
			return s, nil, true
		}

		next, cmd := s.handleDMClosedMsg(msg)
		return next, cmd, true

	case chatcmd.TopicInfoResult:
		channel, _ := result.IssuingWindow.(*domain.ChannelWindow)
		return s, s.logAndShowForChannel(channel, msg.Topic), true

	case chatcmd.UsageError:
		return s, s.logReplyEvent(result.IssuingWindow, domain.UsageHint{
			Target:  issuingWindowName(result.IssuingWindow),
			Command: msg.Command,
			Usage:   msg.Usage,
			At:      time.Now(),
		}), true

	case chatcmd.NoChannelError:
		usage := "join a channel first"
		if msg.Command == "part" {
			usage = "no channel to part from"
		}

		return s, s.logReplyEvent(result.IssuingWindow, domain.UsageHint{
			Command: msg.Command, Usage: usage, At: time.Now(),
		}), true

	case chatcmd.PokeRequested:
		return s, s.handlePoke(result.IssuingWindow), true

	case domain.ErrorEvent:
		next, cmd := s.handleErrorEventForWindow(msg, result.IssuingWindow)
		return next, cmd, true

	case domain.SystemNotice:
		return s, s.logReplyEvent(result.IssuingWindow, msg), true
	}

	if next, cmd, ok := s.routeConfigResults(result.IssuingWindow, result.Message); ok {
		return next, cmd, true
	}

	next, cmd := s.route(result.Message)
	return next, cmd, true
}

func issuingWindowName(window domain.Window) domain.ChannelName {
	if window == nil {
		return ""
	}

	return window.Name()
}

func (s ChatScreen) exactWindow(identity domain.Window) (*Window, bool) {
	if identity == nil {
		return nil, false
	}

	window, ok := s.windowByName(identity.Name())
	if !ok || window.Window != identity {
		return nil, false
	}

	return window, true
}

// deliverReplyEvents re-delivers each confirmation event from a
// command's `protocol.Response.Events` as its own message, in
// dispatcher order, so each lands on its per-event render arm. The
// [tea.Sequence] preserves ordering — for `/list` the
// `domain.ListReply` rows render before the closing
// `domain.ListEnd`.
func (s ChatScreen) deliverReplyEvents(
	issuingWindow domain.Window,
	reply chatcmd.ReplyEvents,
) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(reply.Events)+1)
	for _, event := range reply.Events {
		// The user-client's own chat traffic returns over the bus
		// (echo-message) and renders there. The reply path carries the
		// point-to-point numerics (Whois, ListReply, …).
		if _, ok := event.(domain.Message); ok {
			continue
		}
		cmds = append(cmds, msgCmd(replyEventMsg{
			issuingWindow: issuingWindow, event: event,
		}))
	}
	if reply.Error != nil {
		cmds = append(cmds, msgCmd(chatcmd.CommandResult{
			IssuingWindow: issuingWindow,
			Message: chatcmd.CommandErrorResult{
				Error: *reply.Error,
			},
		}))
	}

	return tea.Sequence(cmds...)
}

func listReplyEvents(events []domain.ProtocolEvent) bool {
	if len(events) == 0 {
		return false
	}

	for _, event := range events {
		switch event.(type) {
		case domain.ListReply, domain.ListEnd:
		default:
			return false
		}
	}

	return true
}

func (s ChatScreen) applyListReplyEvents(
	issuingWindow domain.Window,
	reply chatcmd.ReplyEvents,
) (ChatScreen, tea.Cmd, bool) {
	target, ok := s.replyTarget(issuingWindow)
	moveFocus := s.active == nil
	if !ok {
		target = domain.StatusChannelName
		moveFocus = true
	}

	for _, event := range reply.Events {
		s.appendToScrollback(target, event)
	}

	cmds := []tea.Cmd{msgCmd(components.ScrollbackUpdatedMsg{Channel: target})}
	if moveFocus {
		cmds = append(cmds, msgCmd(chatcmd.ChannelFocusMsg{Channel: target, At: time.Now()}))
	}

	return s, tea.Batch(cmds...), true
}

func (s ChatScreen) logReplyEvent(
	issuingWindow domain.Window,
	event domain.Event,
) tea.Cmd {
	if channel, ok := issuingWindow.(*domain.ChannelWindow); ok {
		return s.logAndShowForChannel(channel, event)
	}

	if issuingWindow == nil {
		if s.active == nil {
			return s.logAndShow(event)
		}

		return s.logAndShowOn(domain.StatusChannelName, event)
	}

	target, ok := s.replyTarget(issuingWindow)
	if !ok {
		return s.logAndShow(event)
	}

	return s.logAndShowOn(target, event)
}

// logAndShow renders a numeric or UI-feedback event in the active
// window's in-memory scrollback. These are the issuing client's
// command replies and UI notices; they are transient by design and
// never reach the shared channel log, which holds only genuine
// channel activity that a model later loads.
//
// When no channel is active the user is on the welcome screen with
// no channels. The output is routed to `&modeloff` and a trailing
// `ChannelFocusMsg` brings that window into focus so the user sees
// the response — the focus handler is the one place that moves the
// user, so the routing decision here stays a pure read.
func (s ChatScreen) logAndShow(event domain.Event) tea.Cmd {
	if s.active != nil {
		return s.logAndShowOn(s.active.Name(), event)
	}

	return tea.Batch(
		s.logAndShowOn(domain.StatusChannelName, event),
		msgCmd(chatcmd.ChannelFocusMsg{Channel: domain.StatusChannelName, At: time.Now()}),
	)
}

// logAndShowForChannel uses pointer identity to keep a delayed reply tied to
// the channel incarnation in which the command ran. A new channel can reuse
// the same IRC name after the old channel closes.
func (s ChatScreen) logAndShowForChannel(channel *domain.ChannelWindow, event domain.Event) tea.Cmd {
	if s.active == nil {
		return s.logAndShow(event)
	}

	if target, ok := s.replyTargetForChannel(channel); ok {
		return s.logAndShowOn(target, event)
	}

	return s.logAndShow(event)
}

func (s ChatScreen) replyTargetForChannel(channel *domain.ChannelWindow) (domain.ChannelName, bool) {
	if channel == nil {
		return "", false
	}

	current, open := s.windowByName(channel.Name())
	if open && current.Window == channel {
		return channel.Name(), true
	}
	if s.active == nil || open && s.active == current {
		return domain.StatusChannelName, true
	}

	return s.active.Name(), true
}

func (s ChatScreen) replyTarget(window domain.Window) (domain.ChannelName, bool) {
	if channel, ok := window.(*domain.ChannelWindow); ok {
		return s.replyTargetForChannel(channel)
	}
	if window == nil {
		return "", false
	}
	if _, isDM := window.(*dmWindow); isDM {
		if current, ok := s.exactWindow(window); ok {
			return current.Name(), true
		}
		if s.active == nil {
			return "", false
		}

		return s.active.Name(), true
	}

	return s.fallbackTarget(window.Name())
}

// logAndShowOn renders a numeric or UI-feedback event in the
// scrollback of the explicit target window. Callers use this when the
// event's home is not the currently-focused window: a notice carrying
// its own target channel, say, or a `/whois` reply the dispatcher
// stamped with the window it was issued from. The append happens on
// the Update goroutine (the single writer of chat-screen state); the
// returned `ScrollbackUpdatedMsg` nudges the message list to
// re-evaluate that window's scrollback.
//
// A reply can outlive the window it was issued from: the user closes
// the query window or parts the channel while the command is still in
// flight. [ChatScreen.fallbackTarget] is the one answer every reply
// arm takes for that, so the line renders in the window the user is
// looking at and no closed window comes back to hold it. Parting the
// last channel leaves the user with no selected window, and the
// fallback reports that absence; that reply takes
// [ChatScreen.logAndShow]'s answer, which is `&modeloff` with the
// focus moved there. The delegation terminates: logAndShow comes back
// here naming `&modeloff`, which fallbackTarget always resolves to
// itself.
func (s ChatScreen) logAndShowOn(ch domain.ChannelName, event domain.Event) tea.Cmd {
	target, ok := s.fallbackTarget(ch)
	if !ok {
		return s.logAndShow(event)
	}

	s.appendToScrollback(target, event)

	return msgCmd(components.ScrollbackUpdatedMsg{Channel: target})
}

// fallbackTarget resolves ch to itself when the chat-screen still has
// that window open, or to the active window otherwise. A target that
// names a window closed since its event was raised falls back to the
// currently active window. The boolean result distinguishes an empty
// self-DM name from the absence of an active window.
// This is what keeps a closed DM from being silently dropped: a DM
// window needs its counterpart's instance handle to rebuild, which
// [ChatScreen.appendToScrollback]'s placeholder-creation path cannot
// synthesise, so routing straight to a closed DM's target would lose
// the event. The same fallback keeps a parted channel from being
// resurrected client-side by a stale reply, since
// appendToScrollback's placeholder path exists for live traffic
// arriving before a join is seen, not for this.
//
// Every reply the chat-screen renders takes this answer.
// [ChatScreen.logAndShowOn] applies it for the reply arms and for the
// notices the chat-screen raises itself.
// [ChatScreen.handleErrorEvent] and [ChatScreen.appendDispatchFailure]
// read it directly as well, because each stamps the resolved window
// onto the line it builds.
//
// The boolean result is false when the user has no selected window,
// which is where parting the last channel can leave them:
// firstRealChannel skips `&modeloff`, so closeWindow has nowhere to
// move them. logAndShowOn is where that case is answered.
func (s ChatScreen) fallbackTarget(ch domain.ChannelName) (domain.ChannelName, bool) {
	if _, open := s.windowByName(ch); open {
		return ch, true
	}

	// `&modeloff` is the client's own view of the server and lives as
	// long as the session: no PART reaches it and `/close` refuses in
	// it, so a line addressed there was never addressed to a window the
	// user has left. It is also the one kind
	// [ChatScreen.appendToScrollback] can open from the name alone,
	// which is what a screen that has not run Init yet needs.
	if ch == domain.StatusChannelName {
		return ch, true
	}

	if s.active == nil {
		return "", false
	}

	return s.active.Name(), true
}

// handleErrorEvent turns a command failure into the transcript line
// the user sees. The full Go error chain (`msg.Err`) goes to the
// observability log unconditionally, since it carries the detail an
// operator needs to diagnose a transport failure; the transcript
// itself gets commandErrorText's short, actionable copy so a raw
// wrapped chain ("send: send message: Post \"https://...\": dial
// tcp: ...") never lands in front of the user. The error renders at
// fallbackTarget(msg.Target): the window the failed command was
// issued from when the chat-screen still has that window open, the
// active window otherwise.
func (s ChatScreen) handleErrorEvent(msg domain.ErrorEvent) (ChatScreen, tea.Cmd) {
	target, ok := s.fallbackTarget(msg.Target)
	return s.handleErrorEventAt(msg, target, ok, s.replyWindowTarget(msg.Target))
}

func (s ChatScreen) handleErrorEventForWindow(
	msg domain.ErrorEvent,
	issuingWindow domain.Window,
) (ChatScreen, tea.Cmd) {
	if issuingWindow == nil {
		return s.handleErrorEventAt(msg, domain.StatusChannelName, true, nil)
	}

	target, ok := s.replyTarget(issuingWindow)
	return s.handleErrorEventAt(
		msg, target, ok, protocol.WindowTargetForKey(issuingWindow.Name()),
	)
}

func (s ChatScreen) handleErrorEventAt(
	msg domain.ErrorEvent,
	target domain.ChannelName,
	ok bool,
	issuingWindow protocol.WindowTarget,
) (ChatScreen, tea.Cmd) {
	var cmds []tea.Cmd

	slog.Default().ErrorContext(s.baseContext(), "command failed",
		"operation", msg.Operation, "error", msg.Err)

	if !ok {
		target = domain.StatusChannelName
	}

	commandError := domain.CommandError{
		Target: target,
		Err:    commandErrorText(msg.Operation, msg.Err),
		At:     msg.At,
	}

	if s.active == nil {
		cmds = append(cmds, s.logAndShow(commandError))
	} else {
		cmds = append(cmds, s.logAndShowOn(target, commandError))
	}
	cmds = append(cmds, s.recordReply(issuingWindow, commandError))
	cmds = append(cmds, msgCmd(components.NickListThinkingMsg{}))

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) replyWindowTarget(name domain.ChannelName) protocol.WindowTarget {
	window, open := s.windowByName(name)
	if !open || window.Kind() == domain.KindStatus {
		return nil
	}
	if window.Kind() == domain.KindDM {
		return protocol.DirectWindowTarget(domain.InstanceID(name))
	}

	return protocol.ChannelWindowTarget(name)
}

// commandErrorText renders a command failure for the transcript: the
// operation plus a short, actionable description. A network or
// context-cancellation failure carries nothing the user can act on
// beyond "try again" or "check the connection", and its Go error
// chain buries that behind dialer and URL detail that belongs in the
// log, not the transcript; commandErrorText collapses that class to
// one fixed sentence. Every other error's own Error() text is
// already short by construction (a domain-typed error such as
// [domain.PokeIntervalOutOfRangeError], or a store/protocol
// sentinel), so it passes through unchanged.
func commandErrorText(operation string, err error) string {
	return operation + ": " + shortErrorText(err)
}

// shortErrorText classifies err into the one problematic class this
// package knows how to shorten (network/transport failures and
// context cancellation) and falls back to err.Error() for everything
// else, which is assumed to already be short by construction.
func shortErrorText(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out; try again"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return "could not reach the API; check your network connection and base URL"
	}

	return err.Error()
}

// recordReply persists one of the user's own point-to-point replies
// to its reply log through the user-client. It is best-effort and
// renders nothing: the live view is already served by the
// accompanying `logAndShow`.
func (s ChatScreen) recordReply(window protocol.WindowTarget, reply domain.IssuerReply) tea.Cmd {
	return func() tea.Msg {
		s.user.RecordReply(s.baseContext(), window, reply)
		return nil
	}
}
