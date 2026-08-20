package screens

import (
	"fmt"
	"log/slog"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
)

// routeInput answers what the user typed or pressed: a chat line, a
// `/`-command, the manual `/poke`, and the one keybinding the chat
// screen answers itself, the nick-list toggle.
func (s ChatScreen) routeInput(msg tea.Msg) (ChatScreen, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case components.MessageSubmitMsg:
		next, cmd := s.handleMessageSubmit(msg)
		return next, cmd, true

	case components.CommandSubmitMsg:
		return s, s.handleCommand(msg), true

	case chatcmd.PokeRequested:
		return s, s.handlePoke(), true

	case tea.KeyMsg:
		if !ui.Matches(msg, s.keyMap.ToggleNickList) {
			return s, nil, false
		}

		slog.Default().InfoContext(s.baseContext(), "keybind triggered",
			"component", "ui",
			"action", "toggle_nick_list",
			"key", msg.String(),
		)

		return s, msgCmd(components.NickListToggleMsg{}), true
	}

	return s, nil, false
}

func (s ChatScreen) runContext() chatcmd.Context {
	return chatcmd.Context{
		Session: s.sess,
		Manager: s.mgr,
		Config:  s.cfgStore,
		Active:  s.active,
		Actor:   s.user.Instance(),
		Client:  s.client,
	}
}

// errorEvent builds an [domain.ErrorEvent] stamped with the window
// the failure belongs to, matching [chatcmd.Context.errorEvent]'s
// convention for the chat-screen's own error paths (command parsing,
// `/poke`), which run outside any `chatcmd.Command.Run`.
func errorEvent(target domain.ChannelName, operation string, err error) domain.ErrorEvent {
	return domain.ErrorEvent{Operation: operation, Err: err, Target: target, At: time.Now()}
}

func (s ChatScreen) handleCommand(msg components.CommandSubmitMsg) tea.Cmd {
	// Redacted once up front so a command whose arguments carry a
	// credential (/config api-key) never has that value reach the
	// log, whether parsing succeeds or a malformed line fails after
	// the command and any subcommand name still resolved.
	raw := s.parser.RedactedRaw(msg.Raw)

	invocation, err := s.parser.ParseInvocation(msg.Raw)
	if err != nil {
		slog.Default().WarnContext(s.baseContext(), "command parse failed",
			"component", "ui",
			"raw", raw,
			"error", err,
		)

		return func() tea.Msg { return errorEvent(s.active, "command", err) }
	}

	cmd, ok := invocation.Leaf().(chatcmd.Command)
	if !ok {
		return func() tea.Msg {
			return errorEvent(s.active, "command",
				fmt.Errorf("parsed command %T does not implement the expected command interface", invocation.Leaf()))
		}
	}

	slog.Default().InfoContext(s.baseContext(), "command executed",
		"component", "ui",
		"command", invocation.Selected().Name,
		"raw", raw,
		"channel", string(s.active),
	)

	rc := s.runContext()
	rc.Invocation = invocation

	return cmd.Run(s.baseContext(), rc)
}

func (s ChatScreen) handlePoke() tea.Cmd {
	return func() tea.Msg {
		if err := s.user.Poke(s.baseContext()); err != nil {
			return errorEvent(s.active, "poke", err)
		}

		return nil
	}
}

// handleMessageSubmit dispatches a user-typed chat line. With no
// active channel the user is on the welcome screen; the hint
// directs them to join one. `&modeloff` is server-narrated only
// and refuses chat with a hint that points at the right command.
// Everything else flows through to the user-client's
// [userclient.UserClient.SendMessage].
//
// The target window is read here, on the Update goroutine, and handed
// to [ChatScreen.sendMessageCmd] as a value, so a channel switch
// between the submit and Bubble Tea running the command cannot
// redirect the line the user typed.
func (s ChatScreen) handleMessageSubmit(msg components.MessageSubmitMsg) (ChatScreen, tea.Cmd) {
	if s.active == "" {
		return s, s.logAndShow(domain.UsageHint{
			Usage: "join a channel first", At: time.Now(),
		})
	}

	if s.active == domain.StatusChannelName {
		return s, s.logAndShow(domain.UsageHint{
			Command: "send",
			Usage:   "the status channel doesn't take messages — try /msg <nick-or-#channel> instead",
			At:      time.Now(),
		})
	}

	return s, s.sendMessageCmd("send", s.active, msg.Text)
}

// sendMessageCmd fires a user `SendMessage` at `target`. The window
// is a parameter, so the command carries the window its caller
// resolved on the Update goroutine and a later focus change cannot
// move the line. On success the sent line returns over the protocol
// bus via echo-message and renders through the normal event path;
// only a failure surfaces here, labelled with the `operation` the
// user asked for.
func (s ChatScreen) sendMessageCmd(operation string, target domain.ChannelName, body string) tea.Cmd {
	return func() tea.Msg {
		if _, err := s.user.SendMessage(s.baseContext(), target, body); err != nil {
			return domain.ErrorEvent{Operation: operation, Err: err, Target: target, At: time.Now()}
		}

		return nil
	}
}
