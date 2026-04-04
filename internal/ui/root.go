package ui

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// ScreenMsg tells Root to switch to a different screen.
type ScreenMsg struct {
	Screen Model
}

// AppKeyMap defines application-level keybindings handled by Root.
type AppKeyMap struct {
	Quit key.Binding
}

// DefaultAppKeyMap is the default set of application-level
// keybindings.
var DefaultAppKeyMap = AppKeyMap{
	Quit: key.NewBinding(
		key.WithKeys("ctrl+c"),
		key.WithHelp("^C", "quit"),
	),
}

// Root is the top-level model that acts as a router between screens.
// It implements tea.Model and bridges to child screens that implement
// the responsive ui.Model interface.
type Root struct {
	width  int
	height int
	screen Model
	keyMap AppKeyMap
}

// NewRoot creates the top-level Root model with the given initial
// screen. If screen is nil, Root renders an empty view until a
// ScreenMsg arrives.
func NewRoot(screen Model) Root {
	return Root{screen: screen, keyMap: DefaultAppKeyMap}
}

// Init implements tea.Model.
func (r Root) Init() tea.Cmd {
	if r.screen == nil {
		return nil
	}

	return r.screen.Init()
}

// Update implements tea.Model.
func (r Root) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		r.width = msg.Width
		r.height = msg.Height

	case tea.KeyMsg:
		if key.Matches(msg, r.keyMap.Quit) {
			return r, tea.Quit
		}

	case ScreenMsg:
		r.screen = msg.Screen
		return r, r.screen.Init()
	}

	if r.screen == nil {
		return r, nil
	}

	screen, cmd := r.screen.Update(msg)
	r.screen = screen

	return r, cmd
}

// View implements tea.Model.
func (r Root) View() string {
	if r.screen == nil {
		return ""
	}

	return r.screen.View(r.width, r.height)
}

// KeyBindings implements Keybinding.
func (r Root) KeyBindings() []key.Binding {
	if r.screen == nil {
		return []key.Binding{r.keyMap.Quit}
	}

	bindings := CollectKeyBindings(r.screen)
	bindings = append(bindings, r.keyMap.Quit)

	return bindings
}
