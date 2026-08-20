package screens

import (
	"fmt"
	"log/slog"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
)

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
