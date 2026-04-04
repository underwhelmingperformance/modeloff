package components

import (
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// RenderStatusBar renders the active keybindings and status items.
func RenderStatusBar(width int, bindings []key.Binding, items []ui.StatusItem) string {
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
		if rightBudget < 0 {
			rightBudget = 0
		}

		rightText := renderStatusItems(rightItems, rightBudget)
		leftBudget := width - lipgloss.Width(rightText)
		if rightText != "" {
			leftBudget--
		}
		if leftBudget < 0 {
			leftBudget = 0
		}

		leftText := keyText
		leftStatusBudget := leftBudget - lipgloss.Width(keyText)
		if keyText != "" {
			leftStatusBudget -= 2
		}

		leftStatusText := renderStatusItems(leftItems, leftStatusBudget)
		if leftStatusText != "" {
			if leftText != "" {
				leftText += "  " + leftStatusText
			} else {
				leftText = leftStatusText
			}
		}

		if rightText == "" {
			best = leftText
			if lipgloss.Width(best) <= width {
				break
			}

			continue
		}

		if leftText == "" {
			best = rightText
			if lipgloss.Width(best) <= width {
				break
			}

			continue
		}

		if lipgloss.Width(leftText)+1+lipgloss.Width(rightText) > width {
			best = rightText
			continue
		}

		spacing := strings.Repeat(" ", width-lipgloss.Width(leftText)-lipgloss.Width(rightText))
		best = leftText + spacing + rightText
		break
	}

	if best == "" {
		return ""
	}

	return theme.Dim.Width(width).Render(truncateLine(best, width))
}

func renderKeyTexts(bindings []key.Binding) (string, string) {
	active := ui.ActiveKeyBindings(bindings)
	if len(active) == 0 {
		return "", ""
	}

	fullParts := make([]string, 0, len(active))
	shortParts := make([]string, 0, len(active))

	for _, binding := range active {
		help := binding.Help()
		shortParts = append(shortParts, help.Key)

		if help.Desc == "" {
			fullParts = append(fullParts, help.Key)
			continue
		}

		fullParts = append(fullParts, fmt.Sprintf("%s %s", help.Key, help.Desc))
	}

	return strings.Join(fullParts, "  "), strings.Join(shortParts, "  ")
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

			parts = append(parts, text)
		}

		return strings.Join(parts, "  ")
	}

	result := render()
	for lipgloss.Width(result) > width {
		changed := false

		for _, index := range order {
			if !compactible[index] {
				continue
			}

			texts[index] = items[index].Compact
			compactible[index] = false
			result = render()
			changed = true

			if lipgloss.Width(result) <= width {
				return result
			}
		}

		if changed {
			continue
		}

		for _, index := range order {
			if texts[index] == "" {
				continue
			}

			texts[index] = ""
			result = render()
			changed = true

			if lipgloss.Width(result) <= width {
				return result
			}
		}

		if !changed {
			break
		}
	}

	return result
}
