package ui

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/laney/modeloff/internal/ui/theme"
)

const keyboardHelpHeaderHeight = 2

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

func newKeyboardHelp(width, height int, bindings []KeyBinding) keyboardHelp {
	content := renderKeyboardHelp(bindings)
	vp := viewport.New(
		viewport.WithWidth(max(width, 0)),
		viewport.WithHeight(keyboardHelpBodyHeight(height)),
	)
	vp.MouseWheelEnabled = true
	vp.SetContent(content)

	return keyboardHelp{viewport: vp, content: content}
}

func (h keyboardHelp) resize(width, height int) keyboardHelp {
	h.viewport.SetWidth(max(width, 0))
	h.viewport.SetHeight(keyboardHelpBodyHeight(height))
	h.viewport.SetContent(h.content)

	return h
}

func (h keyboardHelp) update(msg tea.Msg) (keyboardHelp, tea.Cmd) {
	var cmd tea.Cmd
	h.viewport, cmd = h.viewport.Update(msg)

	return h, cmd
}

func (h keyboardHelp) view(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}

	header := keyboardHelpHeader(width)
	if height == 1 {
		return header
	}

	h = h.resize(width, height)

	return lipgloss.JoinVertical(lipgloss.Left, header, "", h.viewport.View())
}

func keyboardHelpBodyHeight(height int) int {
	return max(height-keyboardHelpHeaderHeight, 0)
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
