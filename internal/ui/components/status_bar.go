package components

import (
	"slices"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

var (
	dimStyle       = theme.Dim
	dimActiveStyle = theme.Dim.Bold(true)
	dimSeparator   = dimStyle.Render("  ")
)

// RenderStatusBar renders the active keybindings and status items.
func RenderStatusBar(width int, bindings []ui.KeyBinding, items []ui.StatusItem) string {
	leftItems := filterStatusItems(items, ui.StatusSideLeft)
	rightItems := filterStatusItems(items, ui.StatusSideRight)

	fullKeys, shortKeys := renderKeyTexts(bindings)
	leftTexts := []string{fullKeys}
	if shortKeys != fullKeys {
		leftTexts = append(leftTexts, shortKeys)
	}
	leftTexts = append(leftTexts, "")

	var best string

	for _, keyText := range leftTexts {
		rightBudget := width - lipgloss.Width(keyText)
		if keyText != "" {
			rightBudget--
		}
		rightBudget = max(rightBudget, 0)

		rightText := renderStatusItems(rightItems, rightBudget)
		leftBudget := width - lipgloss.Width(rightText)
		if rightText != "" {
			leftBudget--
		}
		leftBudget = max(leftBudget, 0)

		leftText := assembleLeftText(keyText, leftItems, leftBudget)

		best = composeStatusLine(leftText, rightText, width)
		if best != "" && lipgloss.Width(best) <= width {
			break
		}
	}

	if best == "" {
		return ""
	}

	return lipgloss.PlaceHorizontal(width, lipgloss.Left, truncateLine(best, width))
}

func assembleLeftText(keyText string, leftItems []ui.StatusItem, leftBudget int) string {
	leftStatusBudget := leftBudget - lipgloss.Width(keyText)
	if keyText != "" {
		leftStatusBudget -= 2
	}

	leftStatusText := renderStatusItems(leftItems, leftStatusBudget)

	switch {
	case leftStatusText == "":
		return keyText
	case keyText == "":
		return leftStatusText
	default:
		return lipgloss.JoinHorizontal(lipgloss.Top, keyText, dimSeparator, leftStatusText)
	}
}

func composeStatusLine(leftText, rightText string, width int) string {
	if rightText == "" {
		return leftText
	}

	if leftText == "" {
		return rightText
	}

	if lipgloss.Width(leftText)+1+lipgloss.Width(rightText) > width {
		return rightText
	}

	spacing := strings.Repeat(" ", width-lipgloss.Width(leftText)-lipgloss.Width(rightText))

	return lipgloss.JoinHorizontal(lipgloss.Top, leftText, spacing, rightText)
}

func renderKeyTexts(bindings []ui.KeyBinding) (string, string) {
	active := ui.ActiveKeyBindings(bindings)
	if len(active) == 0 {
		return "", ""
	}

	fullParts := make([]string, 0, len(active))
	shortParts := make([]string, 0, len(active))

	for _, binding := range active {
		help := binding.Help()
		style := dimStyle
		if binding.Active {
			style = dimActiveStyle
		}

		keyLabel := style.Render(help.Key)
		shortParts = append(shortParts, keyLabel)

		if help.Desc == "" {
			fullParts = append(fullParts, keyLabel)
			continue
		}

		fullParts = append(fullParts, lipgloss.JoinHorizontal(lipgloss.Top, keyLabel, " ", style.Render(help.Desc)))
	}

	return joinWithSeparator(fullParts), joinWithSeparator(shortParts)
}

func joinWithSeparator(parts []string) string {
	if len(parts) == 0 {
		return ""
	}

	result := make([]string, 0, len(parts)*2-1)
	for i, part := range parts {
		if i > 0 {
			result = append(result, dimSeparator)
		}

		result = append(result, part)
	}

	return lipgloss.JoinHorizontal(lipgloss.Top, result...)
}

func filterStatusItems(items []ui.StatusItem, side ui.StatusSide) []ui.StatusItem {
	filtered := make([]ui.StatusItem, 0, len(items))

	for _, item := range items {
		if item.Side != side {
			continue
		}

		if item.Full == "" && item.Compact == "" {
			continue
		}

		filtered = append(filtered, item)
	}

	return filtered
}

func renderStatusItems(items []ui.StatusItem, width int) string {
	if len(items) == 0 || width <= 0 {
		return ""
	}

	texts := make([]string, len(items))
	compactible := make([]bool, len(items))
	for i, item := range items {
		text := item.Full
		if text == "" {
			text = item.Compact
		}

		texts[i] = text
		compactible[i] = item.Compact != "" && item.Compact != text
	}

	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}

	slices.SortStableFunc(order, func(a, b int) int {
		if items[a].Priority == items[b].Priority {
			return 0
		}

		if items[a].Priority < items[b].Priority {
			return -1
		}

		return 1
	})

	render := func() string {
		var parts []string
		for _, text := range texts {
			if text == "" {
				continue
			}

			parts = append(parts, dimStyle.Render(text))
		}

		return joinWithSeparator(parts)
	}

	result := render()

	for lipgloss.Width(result) > width {
		if result, ok := compactItems(items, texts, compactible, order, width, render); ok {
			return result
		}

		result, changed := dropItems(texts, order, width, render)
		if !changed {
			break
		}

		if lipgloss.Width(result) <= width {
			return result
		}
	}

	return result
}

func compactItems(
	items []ui.StatusItem,
	texts []string,
	compactible []bool,
	order []int,
	width int,
	render func() string,
) (string, bool) {
	for _, index := range order {
		if !compactible[index] {
			continue
		}

		texts[index] = items[index].Compact
		compactible[index] = false

		result := render()
		if lipgloss.Width(result) <= width {
			return result, true
		}
	}

	return "", false
}

func dropItems(
	texts []string,
	order []int,
	width int,
	render func() string,
) (string, bool) {
	changed := false

	for _, index := range order {
		if texts[index] == "" {
			continue
		}

		texts[index] = ""
		changed = true

		result := render()
		if lipgloss.Width(result) <= width {
			return result, true
		}
	}

	return render(), changed
}
