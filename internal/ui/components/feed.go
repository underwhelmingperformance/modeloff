package components

import (
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

const feedDividerSentinel = "\x00modeloff-feed-divider"

// FeedView is a scrollable, tail-following read-only viewport.
type FeedView struct {
	lines       []string
	viewport    viewport.Model
	keyMap      ChatViewKeyMap
	placeholder string
	dividerText string
	bounds      ui.Rect
	seenCount   int
}

// NewFeedView creates a read-only scrolling feed.
func NewFeedView(placeholder, dividerText string) FeedView {
	vp := viewport.New(0, 0)
	vp.MouseWheelEnabled = true

	keyMap := DefaultChatViewKeyMap
	vp.KeyMap = viewport.KeyMap{
		PageDown: keyMap.PageDown,
		PageUp:   keyMap.PageUp,
		Down:     keyMap.ScrollDown,
		Up:       keyMap.ScrollUp,
	}

	return FeedView{
		viewport:    vp,
		keyMap:      keyMap,
		placeholder: placeholder,
		dividerText: dividerText,
	}
}

// SetPlaceholder updates the empty-state text.
func (f FeedView) SetPlaceholder(placeholder string) FeedView {
	f.placeholder = placeholder

	return f
}

// ReplaceLines replaces the content and jumps to the bottom.
func (f FeedView) ReplaceLines(lines []string) FeedView {
	f.lines = stripFeedDivider(lines)
	f.seenCount = len(f.lines)
	f.viewport.SetContent("")
	f.viewport.GotoBottom()

	return f.refreshContent(true)
}

// SetLines replaces the content while preserving scroll position and
// tail-follow behaviour.
func (f FeedView) SetLines(lines []string) FeedView {
	return f.SetLinesWithState(lines, f.ScrolledUp())
}

// SetLinesWithState replaces the content using the provided scroll state snapshot.
func (f FeedView) SetLinesWithState(lines []string, scrolledUp bool) FeedView {
	cleaned := stripFeedDivider(lines)
	newContent := len(cleaned) > f.seenCount

	if scrolledUp && newContent && f.seenCount > 0 {
		cleaned = insertFeedDivider(cleaned, f.seenCount)
	} else if hasFeedDivider(f.lines) {
		cleaned = insertFeedDivider(cleaned, f.seenCount)
	}

	f.lines = cleaned

	if !scrolledUp {
		f.seenCount = countFeedLines(cleaned)
	}

	return f.refreshContent(!scrolledUp)
}

// ScrolledUp reports whether the feed is currently away from the tail.
func (f FeedView) ScrolledUp() bool {
	return !f.viewport.AtBottom() && f.viewport.TotalLineCount() > 0
}

// Update handles viewport input and mouse scrolling.
func (f FeedView) Update(msg tea.Msg) (FeedView, tea.Cmd) {
	switch msg := msg.(type) {
	case ui.BoundsMsg:
		f.bounds = msg.Rect
		return f.SyncViewport(msg.Rect.Width, msg.Rect.Height), nil

	case tea.MouseMsg:
		if !f.bounds.Contains(msg.X, msg.Y) {
			return f, nil
		}

		if msg.Action == tea.MouseActionPress {
			switch msg.Button {
			case tea.MouseButtonWheelUp:
				f.viewport.ScrollUp(f.viewport.MouseWheelDelta)
				return f, nil
			case tea.MouseButtonWheelDown:
				f.viewport.ScrollDown(f.viewport.MouseWheelDelta)
				return f, nil
			}
		}
	}

	var cmd tea.Cmd
	f.viewport, cmd = f.viewport.Update(msg)

	return f, cmd
}

// KeyBindings returns the active scroll keybindings.
func (f FeedView) KeyBindings() []key.Binding {
	return []key.Binding{
		ui.WithBindingEnabled(
			key.NewBinding(
				key.WithKeys("pgup", "pgdown"),
				key.WithHelp("PgUp/Dn", "scroll"),
			),
			len(f.lines) > 0,
		),
		ui.WithBindingEnabled(
			key.NewBinding(
				key.WithKeys("ctrl+up", "ctrl+down"),
				key.WithHelp("^↑/↓", "scroll"),
			),
			len(f.lines) > 0,
		),
	}
}

// View renders the feed inside the provided area.
func (f FeedView) View(width, height int) (view string, scrolled bool, scrollPct float64) {
	if len(f.lines) == 0 {
		text := theme.Dim.Render("No entries yet")
		if f.placeholder != "" {
			text = f.placeholder
		}

		return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, text), false, 0
	}

	vp := f.viewport
	vp.Width = width
	vp.Height = height

	content := f.renderedContent(width)
	vp.SetContent(content)

	rendered := vp.View()
	if lipgloss.Height(content) <= height {
		rendered = lipgloss.Place(width, height, lipgloss.Left, lipgloss.Bottom, content)
	}

	return rendered, !vp.AtBottom(), vp.ScrollPercent()
}

// SyncViewport sets the viewport dimensions and re-renders content.
func (f FeedView) SyncViewport(width, height int) FeedView {
	if width < 0 {
		width = 0
	}

	if height < 0 {
		height = 0
	}

	f.viewport.Width = width
	f.viewport.Height = height

	return f.refreshContent(f.viewport.AtBottom() || f.viewport.TotalLineCount() == 0)
}

func (f FeedView) renderedContent(width int) string {
	rendered := make([]string, 0, len(f.lines))

	for _, line := range f.lines {
		if line == feedDividerSentinel {
			rendered = append(rendered, f.renderDivider(width))
			continue
		}

		rendered = append(rendered, line)
	}

	return strings.Join(rendered, "\n")
}

func (f FeedView) refreshContent(wasAtBottom bool) FeedView {
	f.viewport.SetContent(f.renderedContent(f.viewport.Width))

	if wasAtBottom {
		f.viewport.GotoBottom()
	}

	return f
}

func (f FeedView) renderDivider(width int) string {
	label := theme.Warning.Render(" " + f.dividerText + " ")
	labelWidth := lipgloss.Width(label)
	leftWidth := (width - labelWidth) / 2
	rightWidth := width - leftWidth - labelWidth

	left := strings.Repeat("─", max(0, leftWidth))
	right := strings.Repeat("─", max(0, rightWidth))

	return theme.Dim.Render(fmt.Sprintf("%s%s%s", left, label, right))
}

func insertFeedDivider(lines []string, seenCount int) []string {
	pos := min(seenCount, len(lines))

	result := make([]string, 0, len(lines)+1)
	result = append(result, lines[:pos]...)
	result = append(result, feedDividerSentinel)
	result = append(result, lines[pos:]...)

	return result
}

func stripFeedDivider(lines []string) []string {
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		if line == feedDividerSentinel {
			continue
		}

		result = append(result, line)
	}

	return result
}

func countFeedLines(lines []string) int {
	count := 0
	for _, line := range lines {
		if line == feedDividerSentinel {
			continue
		}

		count++
	}

	return count
}

func hasFeedDivider(lines []string) bool {
	return slices.Contains(lines, feedDividerSentinel)
}
