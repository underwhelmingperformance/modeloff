package screens

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
)

const pacedInterval = 400 * time.Millisecond

func (s ChatScreen) scheduleNextPaced(ch domain.ChannelName) tea.Cmd {
	return tea.Tick(pacedInterval, func(time.Time) tea.Msg {
		return deliverNextPacedMsg{Channel: ch}
	})
}

// deliverNextPacedCmd returns a tea.Cmd that delivers the next
// paced message from the given channel's queue immediately (without
// pacing delay).
func (s ChatScreen) deliverNextPacedCmd(ch domain.ChannelName) tea.Cmd {
	return func() tea.Msg { return deliverNextPacedMsg{Channel: ch} }
}

func (s ChatScreen) deliverNextPaced(msg deliverNextPacedMsg) (ui.Model, tea.Cmd) {
	queue := s.pacedQueue[msg.Channel]
	if len(queue) == 0 {
		return s, nil
	}

	next := queue[0]
	queue = queue[1:]

	if len(queue) == 0 {
		delete(s.pacedQueue, msg.Channel)
	} else {
		s.pacedQueue[msg.Channel] = queue
	}

	cmd := s.renderMessage(next, msg.Channel)

	if len(s.pacedQueue[msg.Channel]) > 0 {
		cmd = tea.Batch(cmd, s.scheduleNextPaced(msg.Channel))
	}

	return s, cmd
}
