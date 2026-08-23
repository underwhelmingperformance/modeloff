package screens

import (
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/laney/modeloff/internal/ui"
)

func renderToBuffer(component ui.Component, width, height int) string {
	screen := uv.NewScreenBuffer(width, height)
	component.Draw(screen, screen.Bounds())

	return screen.Render()
}
