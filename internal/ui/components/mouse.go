package components

import tea "charm.land/bubbletea/v2"

func mouseAt(msg tea.MouseMsg, x, y int) tea.MouseMsg {
	mouse := msg.Mouse()
	mouse.X = x
	mouse.Y = y

	switch msg.(type) {
	case tea.MouseClickMsg:
		return tea.MouseClickMsg(mouse)
	case tea.MouseReleaseMsg:
		return tea.MouseReleaseMsg(mouse)
	case tea.MouseWheelMsg:
		return tea.MouseWheelMsg(mouse)
	case tea.MouseMotionMsg:
		return tea.MouseMotionMsg(mouse)
	default:
		return msg
	}
}
