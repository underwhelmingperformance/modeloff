package components

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

const maxPopoverSuggestions = 6

// PopoverAcceptMsg is emitted when the user accepts a popover
// suggestion (via Tab or mouse click). ChatView uses this to
// apply the replacement to the input bar.
type PopoverAcceptMsg struct {
	ReplaceStart int
	ReplaceEnd   int
	Replacement  string
}

// PopoverApplyMsg updates the completer and recomputes suggestions.
type PopoverApplyMsg struct {
	Completer command.Completable
	Raw       string
	Cursor    int
}

// PopoverRefreshMsg recomputes suggestions for the current input.
type PopoverRefreshMsg struct {
	Raw    string
	Cursor int
}

// PopoverDismissMsg hides the popover until the input changes.
type PopoverDismissMsg struct {
	Raw string
}

// Popover renders a command-completion popover above the input bar.
// It owns completion suggestions, selection index, and visibility
// state.
type Popover struct {
	completer  command.Completable
	completion command.Completion
	selected   int
	offset     int
	closed     bool
	handled    bool

	bounds ui.Rect
}

// PopoverLayout describes the absolute hit-test rectangles for the
// popover and each of its visible suggestions.
type PopoverLayout struct {
	Rect            ui.Rect
	SuggestionRects []ui.Rect
}

// NewPopover creates an empty popover.
func NewPopover() Popover {
	return Popover{}
}

// IsVisible returns whether the popover is currently showing.
func (p Popover) IsVisible() bool {
	return p.completion.Visible
}

// HasSuggestions returns whether there are any suggestions to show.
func (p Popover) HasSuggestions() bool {
	return len(p.completion.Suggestions) > 0
}

// Layout computes absolute hit-test rectangles for the popover
// given the parent bounds and input bar rectangle.
func (p Popover) Layout(bounds, inputRect ui.Rect) PopoverLayout {
	popoverHeight := p.height()
	if popoverHeight == 0 {
		return PopoverLayout{}
	}

	popoverRect := ui.Rect{
		X:      bounds.X,
		Y:      inputRect.Y - popoverHeight,
		Width:  bounds.Width,
		Height: popoverHeight,
	}

	layout := PopoverLayout{
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

// Init implements ui.Model.
func (p Popover) Init() tea.Cmd {
	return nil
}

// Handled reports whether the most recent Update consumed its message.
// ChatView checks this to avoid forwarding consumed keys to siblings.
func (p Popover) Handled() bool {
	return p.handled
}

// Update implements ui.Model. It handles keyboard navigation
// (Tab/Up/Down/Esc), mouse interactions, and popover state messages.
func (p Popover) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	p.handled = false

	switch msg := msg.(type) {
	case ui.BoundsMsg:
		p.bounds = msg.Rect
		return p, nil

	case PopoverApplyMsg:
		p.completer = msg.Completer
		p = p.refresh(msg.Raw, msg.Cursor)
		return p, nil

	case PopoverRefreshMsg:
		p.closed = false
		p = p.refresh(msg.Raw, msg.Cursor)
		return p, nil

	case PopoverDismissMsg:
		p.closed = true
		p = p.refresh(msg.Raw, 0)
		return p, nil

	case tea.KeyMsg:
		if updated, handled, cmd := p.handleKey(msg); handled {
			updated.handled = true
			return updated, cmd
		}

	case tea.MouseMsg:
		if updated, handled, cmd := p.handleMouse(msg); handled {
			updated.handled = true
			return updated, cmd
		}
	}

	return p, nil
}

// View implements ui.Model.
func (p Popover) View(width, _ int) string {
	return p.Render(width)
}

// Render returns the rendered popover string for the given width.
func (p Popover) Render(width int) string {
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

func (p Popover) handleKey(msg tea.KeyMsg) (Popover, bool, tea.Cmd) {
	if p.completion.Visible && p.HasSuggestions() {
		switch msg.Type {
		case tea.KeyTab:
			return p, true, p.acceptCmd(p.selected)
		case tea.KeyShiftTab, tea.KeyUp:
			return p.moveSelection(-1), true, nil
		case tea.KeyDown:
			return p.moveSelection(1), true, nil
		case tea.KeyEsc:
			p.completion = command.Completion{}
			p.closed = true
			return p, true, nil
		}
	}

	if msg.Type == tea.KeyEsc && p.completion.Visible {
		p.completion = command.Completion{}
		p.closed = true
		return p, true, nil
	}

	return p, false, nil
}

func (p Popover) handleMouse(msg tea.MouseMsg) (Popover, bool, tea.Cmd) {
	layout := p.Layout(p.bounds, ui.Rect{
		X:      p.bounds.X,
		Y:      p.bounds.Y + p.bounds.Height - 1,
		Width:  p.bounds.Width,
		Height: 1,
	})

	if !layout.Rect.Contains(msg.X, msg.Y) {
		return p, false, nil
	}

	switch msg.Action {
	case tea.MouseActionMotion:
		return p.hoverSuggestion(layout, msg.X, msg.Y), true, nil

	case tea.MouseActionPress:
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			return p.moveSelection(-1), true, nil
		case tea.MouseButtonWheelDown:
			return p.moveSelection(1), true, nil
		case tea.MouseButtonLeft:
			if index, ok := p.suggestionIndexAt(layout, msg.X, msg.Y); ok {
				return p, true, p.acceptCmd(index)
			}

			return p, true, nil
		}
	}

	return p, true, nil
}

func (p Popover) acceptCmd(index int) tea.Cmd {
	if index < 0 || index >= len(p.completion.Suggestions) {
		return nil
	}

	suggestion := p.completion.Suggestions[index]
	replacement := suggestion.Value
	if p.completion.AppendSpace {
		replacement += " "
	}

	return func() tea.Msg {
		return PopoverAcceptMsg{
			ReplaceStart: p.completion.ReplaceStart,
			ReplaceEnd:   p.completion.ReplaceEnd,
			Replacement:  replacement,
		}
	}
}

func (p Popover) moveSelection(delta int) Popover {
	if len(p.completion.Suggestions) == 0 {
		return p
	}

	p.selected += delta
	if p.selected < 0 {
		p.selected = len(p.completion.Suggestions) - 1
	}
	if p.selected >= len(p.completion.Suggestions) {
		p.selected = 0
	}

	return p.ensureSelectionVisible()
}

func (p Popover) hoverSuggestion(layout PopoverLayout, x, y int) Popover {
	for i, rect := range layout.SuggestionRects {
		if rect.Contains(x, y) {
			p.selected = p.offset + i
			return p
		}
	}

	return p
}

func (p Popover) suggestionIndexAt(layout PopoverLayout, x, y int) (int, bool) {
	for i, rect := range layout.SuggestionRects {
		if rect.Contains(x, y) {
			return p.offset + i, true
		}
	}

	return 0, false
}

func (p Popover) height() int {
	if !p.completion.Visible {
		return 0
	}

	return len(p.visibleSuggestions())
}

func (p Popover) refresh(raw string, cursor int) Popover {
	if p.closed && !strings.HasPrefix(raw, "/") {
		p.closed = false
	}

	if p.closed {
		p.completion = command.Completion{}
		return p
	}

	if p.completer == nil {
		p.completion = command.Completion{}
		return p
	}

	p.completion = p.completer.Complete(raw, cursor)
	if !p.completion.Visible || len(p.completion.Suggestions) == 0 {
		p.selected = 0
		p.offset = 0
		return p
	}

	if p.selected >= len(p.completion.Suggestions) {
		p.selected = len(p.completion.Suggestions) - 1
	}
	if p.selected < 0 {
		p.selected = 0
	}

	return p.ensureSelectionVisible()
}

func (p Popover) visibleSuggestions() []command.Suggestion {
	if len(p.completion.Suggestions) == 0 {
		return nil
	}

	start := p.offset
	if start >= len(p.completion.Suggestions) {
		start = 0
	}

	end := min(start+maxPopoverSuggestions, len(p.completion.Suggestions))

	return p.completion.Suggestions[start:end]
}

func (p Popover) ensureSelectionVisible() Popover {
	if p.selected < p.offset {
		p.offset = p.selected
	}

	if p.selected >= p.offset+maxPopoverSuggestions {
		p.offset = p.selected - maxPopoverSuggestions + 1
	}

	if p.offset < 0 {
		p.offset = 0
	}

	return p
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
