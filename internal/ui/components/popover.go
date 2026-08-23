package components

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"

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

	bounds      uv.Rectangle
	boundsKnown bool
}

type popoverLayout struct {
	Rect            uv.Rectangle
	SuggestionRects []uv.Rectangle
}

// NewPopover creates an empty popover.
func NewPopover() Popover {
	return Popover{}
}

// IsVisible returns whether the popover is currently showing.
func (p Popover) IsVisible() bool {
	return p.completion.Visible && (!p.boundsKnown || p.bounds.Dy() > 0)
}

// HasSuggestions returns whether there are any suggestions to show.
func (p Popover) HasSuggestions() bool {
	return len(p.completion.Suggestions) > 0
}

func (p Popover) layout() popoverLayout {
	if p.bounds.Empty() {
		return popoverLayout{}
	}

	layout := popoverLayout{
		Rect:            p.bounds,
		SuggestionRects: make([]uv.Rectangle, 0, min(len(p.visibleSuggestions()), p.bounds.Dy())),
	}

	for i := range min(len(p.visibleSuggestions()), p.bounds.Dy()) {
		layout.SuggestionRects = append(layout.SuggestionRects,
			uv.Rect(p.bounds.Min.X, p.bounds.Min.Y+i, p.bounds.Dx(), 1))
	}

	return layout
}

// Init implements ui.Component.
func (p Popover) Init() tea.Cmd {
	return nil
}

// Handled reports whether the most recent Update consumed its message.
// ChatView checks this to avoid forwarding consumed keys to siblings.
func (p Popover) Handled() bool {
	return p.handled
}

// Update implements ui.Component. It handles keyboard navigation
// (Tab/Up/Down/Esc), mouse interactions, and popover state messages.
func (p Popover) Update(msg tea.Msg) (ui.Component, tea.Cmd) {
	p.handled = false

	switch msg := msg.(type) {
	case ui.BoundsMsg:
		p.bounds = msg.Rect
		p.boundsKnown = true

		return p.ensureSelectionVisible(), nil

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

	case tea.KeyPressMsg:
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

func (p Popover) render(width int) string {
	if !p.IsVisible() {
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

func (p Popover) handleKey(msg tea.KeyPressMsg) (Popover, bool, tea.Cmd) {
	if !p.IsVisible() {
		return p, false, nil
	}

	switch {
	case msg.Code == tea.KeyTab && !msg.Mod.Contains(tea.ModShift):
		if p.HasSuggestions() {
			return p, true, p.acceptCmd(p.selected)
		}
	case msg.Code == tea.KeyEnter:
		// Accept the highlighted suggestion first when doing so would
		// change the typed text, matching Tab. An optional continuation
		// leaves Enter to the input bar because the command is already
		// complete; Tab can still accept the suggestion.
		if p.HasSuggestions() && !p.completion.EnterSubmits {
			suggestion := p.completion.Suggestions[p.selected]
			if acceptedReplacement(p.completion.TypedPrefix, suggestion) != p.completion.TypedPrefix {
				return p, true, p.acceptCmd(p.selected)
			}
		}
	case msg.Code == tea.KeyUp || msg.Code == tea.KeyTab && msg.Mod.Contains(tea.ModShift):
		// Only claim the key when there's more than one suggestion to
		// cycle between; otherwise let it fall through to input
		// history, which the caller would otherwise never reach while
		// composing a command.
		if len(p.completion.Suggestions) > 1 {
			return p.moveSelection(-1), true, nil
		}
	case msg.Code == tea.KeyDown:
		if len(p.completion.Suggestions) > 1 {
			return p.moveSelection(1), true, nil
		}
	case msg.Code == tea.KeyEsc:
		p.completion = command.Completion{}
		p.closed = true
		return p, true, nil
	}

	return p, false, nil
}

// BlocksHistory reports whether the popover currently claims Up/Down
// for suggestion cycling. The input bar consults this before letting
// those keys browse input history instead.
func (p Popover) BlocksHistory() bool {
	return p.IsVisible() && len(p.completion.Suggestions) > 1
}

func (p Popover) handleMouse(msg tea.MouseMsg) (Popover, bool, tea.Cmd) {
	layout := p.layout()

	mouse := msg.Mouse()
	if !contains(layout.Rect, mouse.X, mouse.Y) {
		return p, false, nil
	}

	switch msg.(type) {
	case tea.MouseMotionMsg:
		return p.hoverSuggestion(layout, mouse.X, mouse.Y), true, nil

	case tea.MouseWheelMsg:
		switch mouse.Button {
		case tea.MouseWheelUp:
			return p.moveSelection(-1), true, nil
		case tea.MouseWheelDown:
			return p.moveSelection(1), true, nil
		}

	case tea.MouseClickMsg:
		if mouse.Button == tea.MouseLeft {
			if index, ok := p.suggestionIndexAt(layout, mouse.X, mouse.Y); ok {
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
	replacement := acceptedReplacement(p.completion.TypedPrefix, suggestion)
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

// acceptedReplacement returns the literal text to substitute when the
// user Tab-accepts a suggestion. If the typed prefix already matches
// the canonical value or one of its aliases exactly, the typed text
// stands — Tab on `/j` (alias) keeps `/j`, Tab on `/help` (canonical)
// keeps `/help`. Any other prefix expands to the canonical value as
// before.
func acceptedReplacement(typed string, suggestion command.Suggestion) string {
	if typed == suggestion.Value || slices.Contains(suggestion.Aliases, typed) {
		return typed
	}

	return suggestion.Value
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

func (p Popover) hoverSuggestion(layout popoverLayout, x, y int) Popover {
	for i, rect := range layout.SuggestionRects {
		if contains(rect, x, y) {
			p.selected = p.offset + i
			return p
		}
	}

	return p
}

func (p Popover) suggestionIndexAt(layout popoverLayout, x, y int) (int, bool) {
	for i, rect := range layout.SuggestionRects {
		if contains(rect, x, y) {
			return p.offset + i, true
		}
	}

	return 0, false
}

func (p Popover) height() int {
	if !p.completion.Visible {
		return 0
	}

	return min(len(p.completion.Suggestions), maxPopoverSuggestions)
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

	end := min(start+p.visibleCapacity(), len(p.completion.Suggestions))

	return p.completion.Suggestions[start:end]
}

func (p Popover) ensureSelectionVisible() Popover {
	capacity := p.visibleCapacity()
	if capacity <= 0 {
		p.offset = p.selected

		return p
	}

	if p.selected < p.offset {
		p.offset = p.selected
	}

	if p.selected >= p.offset+capacity {
		p.offset = p.selected - capacity + 1
	}

	p.offset = min(max(p.offset, 0), max(len(p.completion.Suggestions)-capacity, 0))

	return p
}

func (p Popover) visibleCapacity() int {
	if !p.boundsKnown {
		return maxPopoverSuggestions
	}

	return min(max(p.bounds.Dy(), 0), maxPopoverSuggestions)
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
