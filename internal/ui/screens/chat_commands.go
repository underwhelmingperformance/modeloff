package screens

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
)

// Commands builds a parser from current state and returns the
// command set. Completion suggestions always reflect the latest
// channels, instances, and active channel.
func (s ChatScreen) Commands() command.Set {
	return s.parser.Set()
}

func (s ChatScreen) runContext() chatcmd.Context {
	return chatcmd.Context{
		Ctx:     s.ctx,
		Session: s.sess,
		Active:  *s.active,
		Nick:    s.sess.UserNick(),
	}
}

func errorEvent(operation string, err error) domain.ErrorEvent {
	return domain.ErrorEvent{Operation: operation, Err: err, At: time.Now()}
}

func (s ChatScreen) handleCommand(msg components.CommandSubmitMsg) tea.Cmd {
	invocation, err := s.parser.ParseInvocation(msg.Raw)
	if err != nil {
		return func() tea.Msg { return errorEvent("command", err) }
	}

	cmd, ok := invocation.Leaf().(chatcmd.Command)
	if !ok {
		return func() tea.Msg {
			return errorEvent("command",
				fmt.Errorf("parsed command %T does not implement the expected command interface", invocation.Leaf()))
		}
	}

	ctx := s.runContext()
	ctx.Invocation = invocation

	return cmd.Run(ctx)
}

func (s ChatScreen) handlePoke() tea.Cmd {
	return func() tea.Msg {
		if err := s.sess.Poke(s.ctx); err != nil {
			return errorEvent("poke", err)
		}

		return nil
	}
}
