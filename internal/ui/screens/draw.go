package screens

import (
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/ui/theme"
)

const chatStatusBarHeight = 1

// Draw implements ui.Component.
func (s ChatScreen) Draw(screen uv.Screen, area uv.Rectangle) {
	if area.Dx() < theme.MinTerminalWidth {
		s.layout.Draw(screen, area)
		return
	}

	barHeight := min(chatStatusBarHeight, area.Dy())
	layoutArea := s.layoutBounds(area)
	s.layout.Draw(screen, layoutArea)

	if barHeight > 0 {
		barArea := uv.Rect(area.Min.X, layoutArea.Max.Y, area.Dx(), barHeight)
		components.NewStatusBar(s.KeyBindings(), s.StatusItems()).Draw(screen, barArea)
	}
}

// Draw implements ui.Component.
func (s ConnectionScreen) Draw(screen uv.Screen, area uv.Rectangle) {
	uv.NewStyledString(s.render(area.Dx(), area.Dy())).Draw(screen, area)
}
