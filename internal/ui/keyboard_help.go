package ui

import (
	"fmt"
	"image"
	"slices"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/laney/modeloff/internal/ui/theme"
)

const (
	keyboardHelpHeaderHeight = 2
	keyboardHelpMaxWidth     = 76
	keyboardHelpMaxHeight    = 30
	keyboardHelpMarginX      = 4
	keyboardHelpMarginY      = 2
)

var keyboardHelpGroupOrder = []KeyHelpGroup{
	KeyHelpGeneral,
	KeyHelpNavigation,
	KeyHelpMessaging,
	KeyHelpEditing,
	KeyHelpFormatting,
	KeyHelpCompletion,
	KeyHelpPanels,
	KeyHelpApplication,
}

type keyboardHelp struct {
	viewport viewport.Model
	content  string
}

func newKeyboardHelp(bindings []KeyBinding) keyboardHelp {
	content := renderKeyboardHelp(bindings)
	vp := viewport.New(viewport.WithWidth(0), viewport.WithHeight(0))
	vp.MouseWheelEnabled = true
	vp.SetContent(content)

	return keyboardHelp{viewport: vp, content: content}
}

// Init implements Component.
func (h keyboardHelp) Init() tea.Cmd {
	return nil
}

// Update implements Component.
func (h keyboardHelp) Update(msg tea.Msg) (Component, tea.Cmd) {
	if bounds, ok := msg.(BoundsMsg); ok {
		return h.resize(bounds.Rect), nil
	}

	var cmd tea.Cmd
	h.viewport, cmd = h.viewport.Update(msg)

	return h, cmd
}

// HandleKey implements KeyHandler. The help is a modal layer, so every
// key offered to it stops here: the viewport scrolls with the ones it
// recognises and ignores the rest. The keys that dismiss it are named
// on its layer, and the stack removes that layer when one arrives, so
// a dismissal key never reaches this.
func (h keyboardHelp) HandleKey(msg tea.KeyPressMsg) (KeyHandler, bool, tea.Cmd) {
	var cmd tea.Cmd
	h.viewport, cmd = h.viewport.Update(msg)

	return h, true, cmd
}

// UpdateKeys implements KeyHandler.
func (h keyboardHelp) UpdateKeys(msg tea.Msg) (KeyHandler, tea.Cmd) {
	updated, cmd := h.Update(msg)

	return updated.(keyboardHelp), cmd
}

func (h keyboardHelp) resize(area uv.Rectangle) keyboardHelp {
	inner := keyboardHelpInnerBounds(keyboardHelpBounds(area))
	h.viewport.SetWidth(inner.Dx())
	h.viewport.SetHeight(max(inner.Dy()-keyboardHelpHeaderHeight, 0))
	h.viewport.SetContent(h.content)

	return h
}

// Draw implements Component.
func (h keyboardHelp) Draw(screen uv.Screen, area uv.Rectangle) {
	bounds := keyboardHelpBounds(area)
	if bounds.Empty() {
		return
	}

	panelStyle := theme.PaneBorder.Border(lipgloss.RoundedBorder())
	panel := panelStyle.
		Width(bounds.Dx()).
		Height(bounds.Dy()).
		Render(" ")
	uv.NewStyledString(panel).Draw(screen, bounds)

	inner := keyboardHelpInnerBounds(bounds)
	if inner.Empty() {
		return
	}

	headerArea := uv.Rect(inner.Min.X, inner.Min.Y, inner.Dx(), min(inner.Dy(), 1))
	uv.NewStyledString(keyboardHelpHeader(inner.Dx())).Draw(screen, headerArea)

	bodyY := min(inner.Min.Y+keyboardHelpHeaderHeight, inner.Max.Y)
	bodyArea := image.Rect(inner.Min.X, bodyY, inner.Max.X, inner.Max.Y)
	if bodyArea.Empty() {
		return
	}

	viewportView := h.viewport
	viewportView.SetWidth(bodyArea.Dx())
	viewportView.SetHeight(bodyArea.Dy())
	uv.NewStyledString(viewportView.View()).Draw(screen, bodyArea)
}

func keyboardHelpBounds(area uv.Rectangle) uv.Rectangle {
	width := min(keyboardHelpMaxWidth, area.Dx())
	if area.Dx() > 2*keyboardHelpMarginX {
		width = min(width, area.Dx()-2*keyboardHelpMarginX)
	}

	height := min(keyboardHelpMaxHeight, area.Dy())
	if area.Dy() > 2*keyboardHelpMarginY {
		height = min(height, area.Dy()-2*keyboardHelpMarginY)
	}

	if width == 0 || height == 0 {
		return image.Rectangle{}
	}

	minX := area.Min.X + (area.Dx()-width)/2
	minY := area.Min.Y + (area.Dy()-height)/2

	return image.Rect(minX, minY, minX+width, minY+height)
}

func keyboardHelpInnerBounds(bounds uv.Rectangle) uv.Rectangle {
	style := theme.PaneBorder.Border(lipgloss.RoundedBorder())
	left := style.GetBorderLeftSize()
	right := style.GetBorderRightSize()
	top := style.GetBorderTopSize()
	bottom := style.GetBorderBottomSize()

	return uv.Rect(
		min(bounds.Min.X+left, bounds.Max.X),
		min(bounds.Min.Y+top, bounds.Max.Y),
		max(bounds.Dx()-left-right, 0),
		max(bounds.Dy()-top-bottom, 0),
	)
}

func keyboardHelpHeader(width int) string {
	title := theme.Bold.Render("Keyboard shortcuts")
	closeHint := theme.Dim.Render("F1/Esc close")
	space := max(width-lipgloss.Width(title)-lipgloss.Width(closeHint), 1)
	header := title + strings.Repeat(" ", space) + closeHint

	return lipgloss.NewStyle().MaxWidth(width).Render(header)
}

func renderKeyboardHelp(bindings []KeyBinding) string {
	bindings = documentedKeyBindings(bindings)
	keyWidth := keyboardHelpKeyWidth(bindings)
	grouped := make(map[KeyHelpGroup][]KeyBinding)

	for _, binding := range bindings {
		group := binding.HelpGroup
		if group == "" {
			group = KeyHelpGeneral
		}

		grouped[group] = append(grouped[group], binding)
	}

	groups := slices.Clone(keyboardHelpGroupOrder)
	for group := range grouped {
		if !slices.Contains(groups, group) {
			groups = append(groups, group)
		}
	}

	var lines []string
	for _, group := range groups {
		entries := grouped[group]
		if len(entries) == 0 {
			continue
		}

		if len(lines) > 0 {
			lines = append(lines, "")
		}

		lines = append(lines, theme.Bold.Render(string(group)))
		for _, binding := range entries {
			help := binding.Help()
			lines = append(lines, fmt.Sprintf("%-*s%s", keyWidth, help.Key, help.Desc))
		}
	}

	return strings.Join(lines, "\n")
}

func documentedKeyBindings(bindings []KeyBinding) []KeyBinding {
	seen := make(map[string]struct{})
	documented := make([]KeyBinding, 0, len(bindings))

	for _, binding := range bindings {
		help := binding.Help()
		if help.Key == "" && help.Desc == "" {
			continue
		}

		label := help.Key + "\x00" + help.Desc
		if _, ok := seen[label]; ok {
			continue
		}

		seen[label] = struct{}{}
		documented = append(documented, binding)
	}

	return documented
}

func keyboardHelpKeyWidth(bindings []KeyBinding) int {
	width := 0
	for _, binding := range bindings {
		width = max(width, lipgloss.Width(binding.Help().Key))
	}

	return max(width+2, 6)
}
