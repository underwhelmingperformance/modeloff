package ui

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// ScreenMsg tells Root to switch the active screen. Root only
// holds the pointer and routes future messages to whoever it
// points at; whoever sends this is responsible for the screen
// already being in a usable state. Initialisation is a separate
// concern handled by the sender (typically because the screen has
// been forwarded messages throughout its predecessor's lifetime
// and is already running).
type ScreenMsg struct {
	Screen Model
}

// QuitRequestedMsg signals that a clean client-side quit has been
// initiated (by the /quit command, by Ctrl-C, or by a similar
// shutdown trigger). Screens that receive it should lock input,
// indicate that the client is disconnecting, and run the backend
// quit sequence. The quit completes asynchronously and emits
// QuitCompleteMsg when finished.
type QuitRequestedMsg struct {
	Message string
}

// QuitCompleteMsg signals that the asynchronous backend quit has
// finished. The receiving screen responds with tea.Quit. Err is
// non-nil if the backend reported a problem during shutdown; the UI
// still exits, since the alternative is to refuse to quit.
type QuitCompleteMsg struct {
	Err error
}

// AppKeyMap defines application-level keybindings handled by Root.
type AppKeyMap struct {
	Quit KeyBinding
}

// DefaultAppKeyMap is the default set of application-level
// keybindings.
var DefaultAppKeyMap = AppKeyMap{
	Quit: Bind(key.NewBinding(
		key.WithKeys("ctrl+c"),
		key.WithHelp("^C", "quit"),
	)),
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
		if Matches(msg, r.keyMap.Quit) {
			return r, func() tea.Msg {
				return QuitRequestedMsg{Message: "client exited"}
			}
		}

	case ScreenMsg:
		r.screen = msg.Screen
		return r, nil
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
func (r Root) KeyBindings() []KeyBinding {
	if r.screen == nil {
		return []KeyBinding{r.keyMap.Quit}
	}

	bindings := CollectKeyBindings(r.screen)
	bindings = append(bindings, r.keyMap.Quit)

	return bindings
}
