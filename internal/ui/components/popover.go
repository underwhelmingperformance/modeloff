package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

const maxPopoverSuggestions = 6

type commandPopover struct {
	scope      command.Scope
	context    command.CompletionContext
	completion command.Completion
	selected   int
	offset     int
	closed     bool
}

type commandPopoverLayout struct {
	Rect            ui.Rect
	SuggestionRects []ui.Rect
}

func (p *commandPopover) Apply(scope command.Scope, ctx command.CompletionContext, raw string, cursor int) {
	p.scope = scope
	p.context = ctx
	p.refresh(raw, cursor)
}

func (p *commandPopover) Dismiss(raw string) {
	p.closed = true
	p.refresh(raw, 0)
}

func (p *commandPopover) IsVisible() bool {
	return p.completion.Visible
}

func (p *commandPopover) hasSuggestions() bool {
	return len(p.completion.Suggestions) > 0
}

func (p *commandPopover) height() int {
	if !p.completion.Visible {
		return 0
	}

	return len(p.visibleSuggestions())
}

func (p *commandPopover) layout(bounds, inputRect ui.Rect) commandPopoverLayout {
	popoverHeight := p.height()
	if popoverHeight == 0 {
		return commandPopoverLayout{}
	}

	popoverRect := ui.Rect{
		X:      bounds.X,
		Y:      inputRect.Y - popoverHeight,
		Width:  bounds.Width,
		Height: popoverHeight,
	}

	layout := commandPopoverLayout{
		Rect:            popoverRect,
		SuggestionRects: make([]ui.Rect, 0, len(p.visibleSuggestions())),
	}

	for i := range p.visibleSuggestions() {
		layout.SuggestionRects = append(layout.SuggestionRects, ui.Rect{
			X:      popoverRect.X,
			Y:      popoverRect.Y + i,
			Width:  popoverRect.Width,
			Height: 1,
		})
	}

	return layout
}

func (p *commandPopover) Render(width int) string {
	if !p.completion.Visible {
		return ""
	}

	visible := p.visibleSuggestions()
	if len(visible) == 0 {
		return ""
	}

	lines := make([]string, 0, len(visible))

	for i, suggestion := range visible {
		index := p.offset + i
		line := suggestion.Label
		if suggestion.Usage != "" {
			// Show the argument signature after the command name,
			// e.g. "/join <channel>" where Usage is "/join <channel>".
			args := strings.TrimPrefix(suggestion.Usage, suggestion.Label)
			args = strings.TrimLeft(args, " ")
			if args != "" {
				line = fmt.Sprintf("%s %s", line, args)
			}
		}
		if suggestion.Detail != "" {
			line = fmt.Sprintf("%s  %s", line, theme.Dim.Render(suggestion.Detail))
		}

		style := lipgloss.NewStyle().Width(width)
		if index == p.selected {
			style = theme.PopoverSelection.Width(width)
		}

		lines = append(lines, style.Render(truncateLine(line, width)))
	}

	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

func (p *commandPopover) MoveSelection(delta int) {
	if len(p.completion.Suggestions) == 0 {
		return
	}

	p.selected += delta
	if p.selected < 0 {
		p.selected = len(p.completion.Suggestions) - 1
	}
	if p.selected >= len(p.completion.Suggestions) {
		p.selected = 0
	}

	p.ensureSelectionVisible()
}

func (p *commandPopover) HoverSuggestion(layout commandPopoverLayout, x, y int) bool {
	for i, rect := range layout.SuggestionRects {
		if rect.Contains(x, y) {
			p.selected = p.offset + i
			return true
		}
	}

	return false
}

func (p *commandPopover) AcceptSuggestion(input InputBar, index int) InputBar {
	if index < 0 || index >= len(p.completion.Suggestions) {
		return input
	}

	suggestion := p.completion.Suggestions[index]
	replacement := suggestion.Value
	if p.completion.AppendSpace {
		replacement += " "
	}

	return input.ReplaceRange(p.completion.ReplaceStart, p.completion.ReplaceEnd, replacement)
}

func (p *commandPopover) SuggestionIndexAt(layout commandPopoverLayout, x, y int) (int, bool) {
	for i, rect := range layout.SuggestionRects {
		if rect.Contains(x, y) {
			return p.offset + i, true
		}
	}

	return 0, false
}

func (p *commandPopover) refresh(raw string, cursor int) {
	if p.closed && !strings.HasPrefix(raw, "/") {
		p.closed = false
	}

	if p.closed {
		p.completion = command.Completion{}
		return
	}

	p.completion = command.Complete(p.scope, raw, cursor, p.context)
	if !p.completion.Visible || len(p.completion.Suggestions) == 0 {
		p.selected = 0
		p.offset = 0
		return
	}

	if p.selected >= len(p.completion.Suggestions) {
		p.selected = len(p.completion.Suggestions) - 1
	}
	if p.selected < 0 {
		p.selected = 0
	}

	p.ensureSelectionVisible()
}

func (p *commandPopover) visibleSuggestions() []command.Suggestion {
	if len(p.completion.Suggestions) == 0 {
		return nil
	}

	start := p.offset
	if start >= len(p.completion.Suggestions) {
		start = 0
	}

	end := start + maxPopoverSuggestions
	if end > len(p.completion.Suggestions) {
		end = len(p.completion.Suggestions)
	}

	return p.completion.Suggestions[start:end]
}

func (p *commandPopover) ensureSelectionVisible() {
	if p.selected < p.offset {
		p.offset = p.selected
	}

	if p.selected >= p.offset+maxPopoverSuggestions {
		p.offset = p.selected - maxPopoverSuggestions + 1
	}

	if p.offset < 0 {
		p.offset = 0
	}
}

func truncateLine(text string, width int) string {
	if width <= 0 {
		return ""
	}

	if lipgloss.Width(text) <= width {
		return text
	}

	runes := []rune(text)
	for len(runes) > 0 && lipgloss.Width(string(runes)+"…") > width {
		runes = runes[:len(runes)-1]
	}

	return string(runes) + "…"
}
