package ui

import tea "github.com/charmbracelet/bubbletea"

// Root is the top-level model that acts as a router between screens.
type Root struct {
	width  int
	height int
}

// NewRoot creates the top-level Root model.
func NewRoot() Root {
	return Root{}
}

// Init implements tea.Model.
func (r Root) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model.
func (r Root) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		r.width = msg.Width
		r.height = msg.Height

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return r, tea.Quit
		}
	}

	return r, nil
}

// View implements tea.Model.
func (r Root) View() string {
	return "modeloff — starting up..."
}
