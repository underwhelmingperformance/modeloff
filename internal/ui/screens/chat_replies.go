package screens

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
)

// routeReplies answers a command the user issued: the point-to-point
// numerics the dispatcher returned in `protocol.Response.Events`, the
// UI feedback the chat screen raises for itself (help, usage hints,
// the cleared window), and the failure a command came back with. None
// of these is channel activity, so each renders into an in-memory
// scrollback and none reaches the shared channel log.
func (s ChatScreen) routeReplies(msg tea.Msg) (ChatScreen, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case chatcmd.ReplyEvents:
		return s, s.deliverReplyEvents(msg), true

	case chatcmd.HelpResult:
		return s, s.logAndShow(domain.Help{Target: s.active, At: time.Now()}), true

	case chatcmd.ClearResult:
		w, ok := s.windowByName(s.active)
		if !ok {
			return s, nil, true
		}

		w.Scrollback.Clear()

		return s, msgCmd(components.ScrollbackClearedMsg{Channel: s.active}), true

	case chatcmd.TopicInfoResult:
		return s, s.logAndShow(domain.TopicInfo{
			Target:     msg.Window.Name(),
			Topic:      msg.Window.Topic,
			TopicSetBy: msg.Window.TopicSetBy,
			TopicSetAt: msg.Window.TopicSetAt,
			At:         time.Now(),
		}), true

	case chatcmd.UsageError:
		return s, s.logAndShow(domain.UsageHint{
			Target: s.active, Command: msg.Command, Usage: msg.Usage, At: time.Now(),
		}), true

	case chatcmd.NoChannelError:
		usage := "join a channel first"
		if msg.Command == "part" {
			usage = "no channel to part from"
		}

		return s, s.logAndShow(domain.UsageHint{
			Command: msg.Command, Usage: usage, At: time.Now(),
		}), true

	case domain.Invited:
		// Echo path for the inviter's own `/invite` result. `session.handleInvite`
		// returns the resulting `Invited` in `protocol.Response.Events`,
		// which `chatcmd.sendCommand` delivers to the chat-screen via
		// `chatcmd.ReplyEvents`. The session bus does not deliver this event
		// back to the inviter, so this is the only way the inviter sees the
		// RPL_INVITING-equivalent line in scrollback. An invitation confers
		// no membership (RFC 2812 §3.2.7): the nick list gains the invitee
		// only when the JOIN arrives, if it ever does.
		return s, s.logAndShowOn(msg.Target, msg), true

	case domain.SystemNotice:
		// Command-reply feedback path for the issuing client. A handler
		// such as `session.handleInvite` returns a `SystemNotice` (for a
		// failed `/invite`, "no such nick: <target>") in
		// `protocol.Response.Events`, which `chatcmd.sendCommand` delivers
		// via `chatcmd.ReplyEvents`. The session bus does not deliver this
		// notice back over the protocol feed, so this arm renders it on the
		// notice's own target channel.
		return s, s.logAndShowOn(msg.Target, msg), true

	case domain.Whois:
		// Command-reply feedback path for the issuing client's `/whois`.
		// `session.handleWhois` returns the identity snapshot in
		// `protocol.Response.Events`; `chatcmd.sendCommand` delivers it via
		// `chatcmd.ReplyEvents`. The dispatcher stamps the snapshot's
		// `Target` with the window the command was issued from, so this arm
		// renders it there. A whois issued with no active window carries an
		// empty target; `logAndShow` routes it to `&modeloff`, matching the
		// other numeric replies.
		if msg.Target == "" {
			return s, s.logAndShow(msg), true
		}

		return s, s.logAndShowOn(msg.Target, msg), true

	case domain.ListReply:
		// Command-reply feedback path for the issuing client's `/list`.
		// `session.handleList` returns one `ListReply` per channel followed
		// by a closing `ListEnd` in `protocol.Response.Events`;
		// `chatcmd.sendCommand` delivers them in order via
		// `chatcmd.ReplyEvents`. Each renders on the active channel through
		// the generic bus-event path.
		return s, s.logAndShow(msg), true

	case domain.ListEnd:
		return s, s.logAndShow(msg), true

	case domain.ErrorEvent:
		next, cmd := s.handleErrorEvent(msg)
		return next, cmd, true
	}

	return s, nil, false
}

// deliverReplyEvents re-delivers each confirmation event from a
// command's `protocol.Response.Events` as its own message, in
// dispatcher order, so each lands on its per-event render arm. The
// [tea.Sequence] preserves ordering — for `/list` the
// `domain.ListReply` rows render before the closing
// `domain.ListEnd`.
func (s ChatScreen) deliverReplyEvents(events chatcmd.ReplyEvents) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(events))
	for _, event := range events {
		// The user-client's own chat traffic returns over the bus
		// (echo-message) and renders there. The reply path carries the
		// point-to-point numerics (Whois, ListReply, …).
		if _, ok := event.(domain.Message); ok {
			continue
		}

		cmds = append(cmds, msgCmd(event))
	}

	return tea.Sequence(cmds...)
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
	if s.active != "" {
		return s.logAndShowOn(s.active, event)
	}

	return tea.Batch(
		s.logAndShowOn(domain.StatusChannelName, event),
		msgCmd(chatcmd.ChannelFocusMsg{Channel: domain.StatusChannelName, At: time.Now()}),
	)
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
// last channel leaves the user looking at nothing, and the fallback
// answers with the empty window; that reply takes
// [ChatScreen.logAndShow]'s answer, which is `&modeloff` with the
// focus moved there. The delegation terminates: logAndShow comes back
// here naming `&modeloff`, which fallbackTarget always resolves to
// itself.
func (s ChatScreen) logAndShowOn(ch domain.ChannelName, event domain.Event) tea.Cmd {
	target := s.fallbackTarget(ch)
	if target == "" {
		return s.logAndShow(event)
	}

	s.appendToScrollback(target, event)

	return msgCmd(components.ScrollbackUpdatedMsg{Channel: target})
}

// fallbackTarget resolves ch to itself when the chat-screen still has
// that window open, or to the active window otherwise. An empty ch
// and one naming a window closed since the event that carried it was
// raised both fail the same windowByName check, so both fall back to
// the currently active window.
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
// The answer is the empty window when the user is looking at nothing,
// which is where parting the last channel leaves them: firstRealChannel
// skips `&modeloff`, so closeWindow has nowhere to move them.
// logAndShowOn is where that case is answered.
func (s ChatScreen) fallbackTarget(ch domain.ChannelName) domain.ChannelName {
	if _, open := s.windowByName(ch); open {
		return ch
	}

	// `&modeloff` is the client's own view of the server and lives as
	// long as the session: no PART reaches it and `/close` refuses in
	// it, so a line addressed there was never addressed to a window the
	// user has left. It is also the one kind
	// [ChatScreen.appendToScrollback] can open from the name alone,
	// which is what a screen that has not run Init yet needs.
	if ch == domain.StatusChannelName {
		return ch
	}

	return s.active
}

// handleErrorEvent turns a command failure into the transcript line
// the user sees. The full Go error chain (`msg.Err`) goes to the
// observability log unconditionally, since it carries the detail an
// operator needs to diagnose a transport failure; the transcript
// itself gets commandErrorText's short, actionable copy so a raw
// wrapped chain ("send: send message: Post \"https://...\": dial
// tcp: ...") never lands in front of the user. The error renders at
// fallbackTarget(msg.Target): the window the failed command was
// issued from when the chat-screen still has it open, the active
// window otherwise.
func (s ChatScreen) handleErrorEvent(msg domain.ErrorEvent) (ChatScreen, tea.Cmd) {
	var cmds []tea.Cmd

	slog.Default().ErrorContext(s.baseContext(), "command failed",
		"operation", msg.Operation, "error", msg.Err)

	target := s.fallbackTarget(msg.Target)

	commandError := domain.CommandError{
		Target: target,
		Err:    commandErrorText(msg.Operation, msg.Err),
		At:     msg.At,
	}

	cmds = append(cmds, s.logAndShowOn(target, commandError))
	cmds = append(cmds, s.recordReply(commandError))
	cmds = append(cmds, msgCmd(components.NickListThinkingMsg{}))

	return s, tea.Batch(cmds...)
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
func (s ChatScreen) recordReply(reply domain.IssuerReply) tea.Cmd {
	return func() tea.Msg {
		s.user.RecordReply(s.baseContext(), reply)
		return nil
	}
}
