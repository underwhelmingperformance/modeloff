package screens

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
)

// Commands implements ui.CommandSource.
func (s *ChatScreen) Commands() command.Set {
	return s.parser.Set()
}

func (s *ChatScreen) runContext() chatcmd.Context {
	return chatcmd.Context{
		Ctx:     s.ctx,
		Session: s.sess,
		Active:  s.active,
		Nick:    s.sess.UserNick(),
	}
}

func errorEvent(operation string, err error) domain.ErrorEvent {
	return domain.ErrorEvent{Operation: operation, Err: err, At: time.Now()}
}

func (s *ChatScreen) handleCommand(msg components.CommandSubmitMsg) tea.Cmd {
	cmd, err := s.parser.Parse(msg.Raw)
	if err != nil {
		return func() tea.Msg { return errorEvent("command", err) }
	}

	return cmd.Run(s.runContext())
}

func (s *ChatScreen) handlePoke() tea.Cmd {
	return func() tea.Msg {
		events, err := s.sess.Poke(s.ctx)
		if err != nil {
			return errorEvent("poke", err)
		}

		if len(events) == 0 {
			return components.PendingResponseMsg{Pending: false}
		}

		return eventBatchMsg{events: events}
	}
}
