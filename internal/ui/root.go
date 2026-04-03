package ui

import tea "github.com/charmbracelet/bubbletea"

// Root is the top-level model that acts as a router between screens.
type Root struct {
	width  int
	height int
}

func NewRoot() Root {
	return Root{}
}

func (r Root) Init() tea.Cmd {
	return nil
}

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

func (r Root) View() string {
	return "modeloff — starting up..."
}
