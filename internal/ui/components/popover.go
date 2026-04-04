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

// Popover renders a command-completion popover above the input bar.
// It owns completion suggestions, selection index, and visibility
// state.
type Popover struct {
	commands   command.Set
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
func NewPopover() *Popover {
	return &Popover{}
}

// Apply updates the command set, then recomputes suggestions for the
// current input state.
func (p *Popover) Apply(commands command.Set, raw string, cursor int) {
	p.commands = commands
	p.refresh(raw, cursor)
}

// Refresh recomputes suggestions for the current input value and
// cursor position. ChatView calls this after every input change.
func (p *Popover) Refresh(raw string, cursor int) {
	p.closed = false
	p.refresh(raw, cursor)
}

// Dismiss hides the popover until the input changes to a non-command.
func (p *Popover) Dismiss(raw string) {
	p.closed = true
	p.refresh(raw, 0)
}

// IsVisible returns whether the popover is currently showing.
func (p *Popover) IsVisible() bool {
	return p.completion.Visible
}

// HasSuggestions returns whether there are any suggestions to show.
func (p *Popover) HasSuggestions() bool {
	return len(p.completion.Suggestions) > 0
}

// Layout computes absolute hit-test rectangles for the popover
// given the parent bounds and input bar rectangle.
func (p *Popover) Layout(bounds, inputRect ui.Rect) PopoverLayout {
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
func (p *Popover) Init() tea.Cmd {
	return nil
}

// Handled reports whether the most recent Update consumed its message.
// ChatView checks this to avoid forwarding consumed keys to siblings.
func (p *Popover) Handled() bool {
	return p.handled
}

// Update implements ui.Model. It handles keyboard navigation
// (Tab/Up/Down/Esc) and mouse interactions within the popover.
func (p *Popover) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	p.handled = false

	switch msg := msg.(type) {
	case tea.KeyMsg:
		if handled, cmd := p.handleKey(msg); handled {
			p.handled = true
			return p, cmd
		}

	case tea.MouseMsg:
		if handled, cmd := p.handleMouse(msg); handled {
			p.handled = true
			return p, cmd
		}
	}

	return p, nil
}

// View implements ui.Model.
func (p *Popover) View(width, _ int) string {
	return p.Render(width)
}

// Render returns the rendered popover string for the given width.
func (p *Popover) Render(width int) string {
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

func (p *Popover) handleKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	if p.completion.Visible && p.HasSuggestions() {
		switch msg.Type {
		case tea.KeyTab:
			return true, p.AcceptCmd(p.selected)
		case tea.KeyShiftTab, tea.KeyUp:
			p.MoveSelection(-1)
			return true, nil
		case tea.KeyDown:
			p.MoveSelection(1)
			return true, nil
		case tea.KeyEsc:
			p.completion = command.Completion{}
			p.closed = true
			return true, nil
		}
	}

	if msg.Type == tea.KeyEsc && p.completion.Visible {
		p.completion = command.Completion{}
		p.closed = true
		return true, nil
	}

	return false, nil
}

func (p *Popover) handleMouse(msg tea.MouseMsg) (bool, tea.Cmd) {
	layout := p.Layout(p.bounds, ui.Rect{
		X:      p.bounds.X,
		Y:      p.bounds.Y + p.bounds.Height - 1,
		Width:  p.bounds.Width,
		Height: 1,
	})

	if !layout.Rect.Contains(msg.X, msg.Y) {
		return false, nil
	}

	switch msg.Action {
	case tea.MouseActionMotion:
		if p.hoverSuggestion(layout, msg.X, msg.Y) {
			return true, nil
		}

	case tea.MouseActionPress:
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			p.MoveSelection(-1)
			return true, nil
		case tea.MouseButtonWheelDown:
			p.MoveSelection(1)
			return true, nil
		case tea.MouseButtonLeft:
			if index, ok := p.suggestionIndexAt(layout, msg.X, msg.Y); ok {
				return true, p.AcceptCmd(index)
			}

			return true, nil
		}
	}

	return true, nil
}

// SetBounds updates the bounds used for mouse hit-testing.
func (p *Popover) SetBounds(bounds ui.Rect) {
	p.bounds = bounds
}

// AcceptCmd returns a command that emits a PopoverAcceptMsg for the
// suggestion at the given index.
func (p *Popover) AcceptCmd(index int) tea.Cmd {
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

// MoveSelection changes the selected suggestion by delta, wrapping
// around at boundaries.
func (p *Popover) MoveSelection(delta int) {
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

func (p *Popover) hoverSuggestion(layout PopoverLayout, x, y int) bool {
	for i, rect := range layout.SuggestionRects {
		if rect.Contains(x, y) {
			p.selected = p.offset + i
			return true
		}
	}

	return false
}

func (p *Popover) suggestionIndexAt(layout PopoverLayout, x, y int) (int, bool) {
	for i, rect := range layout.SuggestionRects {
		if rect.Contains(x, y) {
			return p.offset + i, true
		}
	}

	return 0, false
}

func (p *Popover) height() int {
	if !p.completion.Visible {
		return 0
	}

	return len(p.visibleSuggestions())
}

func (p *Popover) refresh(raw string, cursor int) {
	if p.closed && !strings.HasPrefix(raw, "/") {
		p.closed = false
	}

	if p.closed {
		p.completion = command.Completion{}
		return
	}

	p.completion = command.Complete(p.commands, raw, cursor)
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

func (p *Popover) visibleSuggestions() []command.Suggestion {
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

func (p *Popover) ensureSelectionVisible() {
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
