package ui

import (
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	uvscreen "github.com/charmbracelet/ultraviolet/screen"

	"github.com/laney/modeloff/internal/ui/theme"
)

// ScreenMsg tells Root that the active component is ready to hand
// control to its next screen.
type ScreenMsg struct{}

// ScreenTransition is implemented by a component that can hand its
// current child state to Root when it emits ScreenMsg.
type ScreenTransition interface {
	NextScreen() Component
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

type quitConfirmationExpiredMsg struct {
	armedAt time.Time
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
// the responsive ui.Component interface.
type Root struct {
	width  int
	height int
	screen Component
	keyMap AppKeyMap

	mouseEnabled bool
	quitArmedAt  time.Time
	now          func() time.Time

	helpVisible bool
	keyHelp     Component
}

// NewRoot creates the top-level Root model with the given initial screen.
func NewRoot(screen Component) Root {
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
			help, cmd := r.keyHelp.Update(BoundsMsg{Rect: r.bounds()})
			r.keyHelp = help

			r, screenCmd := r.applyScreenBounds()
			return r, tea.Batch(cmd, screenCmd)
		}

		return r.applyScreenBounds()

	case tea.KeyPressMsg:
		if r.helpVisible {
			if Matches(msg, r.keyMap.ShowHelp) || msg.Code == tea.KeyEsc {
				r.helpVisible = false

				return r.updateScreen(tea.FocusMsg{})
			}

			help, cmd := r.keyHelp.Update(msg)
			r.keyHelp = help

			return r, cmd
		}

		if Matches(msg, r.keyMap.ShowHelp) {
			var screenCmd tea.Cmd
			r, screenCmd = r.updateScreen(tea.BlurMsg{})
			r.keyHelp = newKeyboardHelp(r.KeyBindings())
			help, cmd := r.keyHelp.Update(BoundsMsg{Rect: r.bounds()})
			r.keyHelp = help
			r.helpVisible = true

			return r, tea.Batch(screenCmd, cmd)
		}

		if Matches(msg, r.keyMap.Quit) {
			if r.quitArmed() {
				return r, func() tea.Msg {
					return QuitRequestedMsg{Message: "client exited"}
				}
			}

			r.quitArmedAt = r.now()

			r, boundsCmd := r.applyScreenBounds()
			return r, tea.Batch(boundsCmd, r.quitConfirmationExpiryCmd())
		}

		if Matches(msg, r.keyMap.ToggleMouse) {
			r.mouseEnabled = !r.mouseEnabled

			return r.applyScreenBounds()
		}

	case tea.MouseMsg:
		if r.helpVisible {
			help, cmd := r.keyHelp.Update(msg)
			r.keyHelp = help

			return r, cmd
		}

	case tea.FocusMsg:
		if r.helpVisible {
			return r.updateScreen(tea.BlurMsg{})
		}

	case ScreenMsg:
		transition, ok := r.screen.(ScreenTransition)
		if !ok {
			return r, nil
		}

		next := transition.NextScreen()
		if next == nil {
			return r, nil
		}

		r.screen = next
		r, boundsCmd := r.applyScreenBounds()
		if !r.helpVisible {
			return r, boundsCmd
		}

		r, focusCmd := r.updateScreen(tea.BlurMsg{})

		return r, tea.Batch(boundsCmd, focusCmd)

	case quitConfirmationExpiredMsg:
		if r.quitArmedAt != msg.armedAt {
			return r, nil
		}

		r.quitArmedAt = time.Time{}
		return r.applyScreenBounds()
	}

	return r.updateScreen(msg)
}

func (r Root) quitConfirmationExpiryCmd() tea.Cmd {
	armedAt := r.quitArmedAt

	return tea.Tick(quitConfirmWindow, func(time.Time) tea.Msg {
		return quitConfirmationExpiredMsg{armedAt: armedAt}
	})
}

// View implements tea.Model.
func (r Root) View() tea.View {
	canvas := lipgloss.NewCanvas(max(r.width, 0), max(r.height, 0))
	r.draw(canvas, canvas.Bounds())
	view := tea.NewView(canvas.Render())
	view.AltScreen = true
	if r.mouseEnabled {
		view.MouseMode = tea.MouseModeCellMotion
	}

	return view
}

func (r Root) draw(screen uv.Screen, area uv.Rectangle) {
	uvscreen.ClearArea(screen, area)
	contentArea := r.contentBounds(area)

	var banners []string
	if !r.mouseEnabled {
		banners = append(banners, theme.Dim.Render(mouseOffBanner))
	}
	if r.quitConfirmationVisible() {
		banners = append(banners, theme.Warning.Render(quitConfirmBanner))
	}
	if len(banners) > 0 {
		banner := lipgloss.JoinVertical(lipgloss.Left, banners...)
		bannerHeight := min(lipgloss.Height(banner), area.Dy())
		bannerArea := uv.Rect(area.Min.X, area.Min.Y, area.Dx(), bannerHeight)
		uv.NewStyledString(banner).Draw(screen, bannerArea)
	}

	if r.screen != nil && !contentArea.Empty() {
		r.screen.Draw(screen, contentArea)
	}

	if r.helpVisible {
		r.keyHelp.Draw(screen, area)
	}
}

func (r Root) bounds() uv.Rectangle {
	return uv.Rect(0, 0, max(r.width, 0), max(r.height, 0))
}

func (r Root) contentBounds(area uv.Rectangle) uv.Rectangle {
	if !r.mouseEnabled {
		area.Min.Y = min(area.Min.Y+1, area.Max.Y)
	}
	if r.quitConfirmationVisible() {
		area.Min.Y = min(area.Min.Y+1, area.Max.Y)
	}

	return area
}

func (r Root) applyScreenBounds() (Root, tea.Cmd) {
	if r.screen == nil {
		return r, nil
	}

	bounds := r.contentBounds(r.bounds())
	screen, cmd := r.screen.Update(BoundsMsg{Rect: bounds})
	r.screen = screen

	return r, cmd
}

func (r Root) updateScreen(msg tea.Msg) (Root, tea.Cmd) {
	if r.screen == nil {
		return r, nil
	}

	screen, cmd := r.screen.Update(msg)
	r.screen = screen

	return r, cmd
}

// quitArmed reports whether a first Ctrl-C is still within its
// confirmation window, awaiting a second press to actually quit.
func (r Root) quitArmed() bool {
	return !r.quitArmedAt.IsZero() && r.now().Sub(r.quitArmedAt) <= quitConfirmWindow
}

func (r Root) quitConfirmationVisible() bool {
	return !r.quitArmedAt.IsZero()
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
