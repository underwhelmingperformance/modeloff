package ui

import tea "github.com/charmbracelet/bubbletea"

// Model is the interface that all UI components in modeloff implement.
// It mirrors the standard Bubble Tea model interface but adds width
// and height parameters to View so that components always render
// responsively in their available space.
type Model interface {
	// Init is called when the model is first created. It can return
	// an initial command to run.
	Init() tea.Cmd

	// Update is called when a message is sent to the model. It
	// returns the updated model and an optional command to run.
	Update(msg tea.Msg) (Model, tea.Cmd)

	// View returns the string representation of the model, rendered
	// to fit within the given width and height.
	View(width, height int) string
}
