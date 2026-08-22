package ui

import (
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/laney/modeloff/internal/ui/theme"
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
	Quit        KeyBinding
	ToggleMouse KeyBinding
	ShowHelp    KeyBinding
}

// DefaultAppKeyMap is the default set of application-level
// keybindings.
var DefaultAppKeyMap = AppKeyMap{
	Quit: Bind(key.NewBinding(
		key.WithKeys("ctrl+c"),
		key.WithHelp("^C", "quit"),
	)).WithHelpMetadata(KeyHelpApplication, KeyHintLow),
	ToggleMouse: Bind(key.NewBinding(
		key.WithKeys("alt+m"),
		key.WithHelp("M-m", "mouse"),
	)).WithHelpMetadata(KeyHelpApplication, KeyHintNone),
	ShowHelp: Bind(key.NewBinding(
		key.WithKeys("f1"),
		key.WithHelp("F1", "shortcuts"),
	)).WithHelpMetadata(KeyHelpApplication, KeyHintEssential),
}

// quitConfirmWindow is how long a first Ctrl-C leaves the quit
// confirmation armed, mirroring the "press again to quit" convention
// many terminal IRC clients and multiplexers use in place of a
// blocking confirm dialog. A second Ctrl-C within the window quits;
// once it elapses, Ctrl-C starts over as a fresh first press.
const quitConfirmWindow = 3 * time.Second

// The banners Root renders above the active screen, each taking a row
// from it while it is shown.
const (
	mouseOffBanner    = "Mouse tracking off (Alt+M re-enables), use the terminal's own selection to copy"
	quitConfirmBanner = "Press Ctrl+C again to quit"
)

// Root is the top-level model that acts as a router between screens.
// It implements tea.Model and bridges to child screens that implement
// the responsive ui.Model interface.
type Root struct {
	width  int
	height int
	screen Model
	keyMap AppKeyMap

	mouseEnabled bool
	quitArmedAt  time.Time
	now          func() time.Time

	helpVisible bool
	keyHelp     keyboardHelp
}

// NewRoot creates the top-level Root model with the given initial
// screen. If screen is nil, Root renders an empty view until a
// ScreenMsg arrives.
func NewRoot(screen Model) Root {
	return Root{
		screen:       screen,
		keyMap:       DefaultAppKeyMap,
		mouseEnabled: true,
		now:          time.Now,
	}
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
		if r.helpVisible {
			r.keyHelp = r.keyHelp.resize(msg.Width, msg.Height)
		}

	case tea.KeyPressMsg:
		if r.helpVisible {
			if Matches(msg, r.keyMap.ShowHelp) || msg.Code == tea.KeyEsc {
				r.helpVisible = false
				return r, nil
			}

			var cmd tea.Cmd
			r.keyHelp, cmd = r.keyHelp.update(msg)

			return r, cmd
		}

		if Matches(msg, r.keyMap.ShowHelp) {
			r.keyHelp = newKeyboardHelp(r.width, r.height, r.KeyBindings())
			r.helpVisible = true

			return r, nil
		}

		if Matches(msg, r.keyMap.Quit) {
			if r.quitArmed() {
				return r, func() tea.Msg {
					return QuitRequestedMsg{Message: "client exited"}
				}
			}

			r.quitArmedAt = r.now()

			return r, nil
		}

		if Matches(msg, r.keyMap.ToggleMouse) {
			r.mouseEnabled = !r.mouseEnabled

			return r, nil
		}

	case tea.MouseMsg:
		if r.helpVisible {
			var cmd tea.Cmd
			r.keyHelp, cmd = r.keyHelp.update(msg)

			return r, cmd
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
func (r Root) View() tea.View {
	content := r.viewContent()
	view := tea.NewView(content)
	view.AltScreen = true
	if r.mouseEnabled {
		view.MouseMode = tea.MouseModeCellMotion
	}

	return view
}

func (r Root) viewContent() string {
	if r.helpVisible {
		return r.keyHelp.view(r.width, r.height)
	}

	if r.screen == nil {
		return ""
	}

	var banners []string

	if !r.mouseEnabled {
		banners = append(banners, theme.Dim.Render(mouseOffBanner))
	}

	if r.quitArmed() {
		banners = append(banners, theme.Warning.Render(quitConfirmBanner))
	}

	if len(banners) == 0 {
		return r.screen.View(r.width, r.height)
	}

	banner := lipgloss.JoinVertical(lipgloss.Left, banners...)
	screenHeight := max(r.height-lipgloss.Height(banner), 0)

	return lipgloss.JoinVertical(lipgloss.Left, banner, r.screen.View(r.width, screenHeight))
}

// quitArmed reports whether a first Ctrl-C is still within its
// confirmation window, awaiting a second press to actually quit.
func (r Root) quitArmed() bool {
	return !r.quitArmedAt.IsZero() && r.now().Sub(r.quitArmedAt) <= quitConfirmWindow
}

// KeyBindings implements Keybinding.
func (r Root) KeyBindings() []KeyBinding {
	if r.screen == nil {
		return []KeyBinding{r.keyMap.Quit, r.keyMap.ToggleMouse, r.keyMap.ShowHelp}
	}

	bindings := CollectKeyBindings(r.screen)
	bindings = append(bindings, r.keyMap.Quit, r.keyMap.ToggleMouse, r.keyMap.ShowHelp)

	return bindings
}
