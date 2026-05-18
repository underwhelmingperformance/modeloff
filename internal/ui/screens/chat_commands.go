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
		Config:  s.cfgStore,
		Active:  *s.active,
		Actor:   s.sess.UserInstance(),
		Client:  s.client,
	}
}

func errorEvent(operation string, err error) domain.ErrorEvent {
	return domain.ErrorEvent{Operation: operation, Err: err, At: time.Now()}
}

func (s ChatScreen) handleCommand(msg components.CommandSubmitMsg) tea.Cmd {
	invocation, err := s.parser.ParseInvocation(msg.Raw)
	if err != nil {
		slog.Default().WarnContext(s.baseContext(), "command parse failed",
			"component", "ui",
			"raw", msg.Raw,
			"error", err,
		)

		return func() tea.Msg { return errorEvent("command", err) }
	}

	cmd, ok := invocation.Leaf().(chatcmd.Command)
	if !ok {
		return func() tea.Msg {
			return errorEvent("command",
				fmt.Errorf("parsed command %T does not implement the expected command interface", invocation.Leaf()))
		}
	}

	slog.Default().InfoContext(s.baseContext(), "command executed",
		"component", "ui",
		"command", invocation.Selected().Name,
		"raw", msg.Raw,
		"channel", string(*s.active),
	)

	rc := s.runContext()
	rc.Invocation = invocation

	return cmd.Run(s.baseContext(), rc)
}

func (s ChatScreen) handlePoke() tea.Cmd {
	return func() tea.Msg {
		if err := s.sess.Poke(s.baseContext()); err != nil {
			return errorEvent("poke", err)
		}

		return nil
	}
}
