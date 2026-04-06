package components

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/ui/theme"
)

// PanelPadLeft is the number of columns of left padding applied to
// every line in a PanelList. Consumers should account for this when
// truncating item text.
const PanelPadLeft = 1

// PanelList renders a scrollable list of pre-styled items with
// consistent padding and width enforcement. Both the channel sidebar
// and nick list use this for uniform rendering.
type PanelList struct {
	viewport viewport.Model
}

// NewPanelList creates a PanelList with a zero-sized viewport.
func NewPanelList() PanelList {
	return PanelList{viewport: viewport.New(0, 0)}
}

// PanelContent holds the configuration for a single render pass.
type PanelContent struct {
	Items  []string // Pre-styled lines (panel adds padding and width).
	Header string   // Optional header rendered above the list.
	Cursor int      // Item index to highlight, -1 for none.
	Empty  string   // Placeholder shown when Items is empty.
}

// Update forwards messages to the internal viewport.
func (p *PanelList) Update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	p.viewport, cmd = p.viewport.Update(msg)

	return cmd
}

// YOffset returns the current scroll offset of the viewport.
func (p *PanelList) YOffset() int {
	return p.viewport.YOffset
}

// Render produces the panel output for the given dimensions.
func (p *PanelList) Render(width, height int, c PanelContent) string {
	if len(c.Items) == 0 {
		empty := c.Empty
		if empty == "" {
			empty = "Empty"
		}

		return lipgloss.Place(width, height,
			lipgloss.Center, lipgloss.Center,
			theme.Dim.Render(empty))
	}

	var headerStr string
	var headerHeight int

	if c.Header != "" {
		headerStr = lipgloss.NewStyle().
			PaddingLeft(PanelPadLeft).
			Width(width).
			Render(theme.Dim.Render(theme.Bold.Render(c.Header)))
		headerHeight = lipgloss.Height(headerStr)
	}

	listHeight := max(height-headerHeight, 0)

	lineStyle := lipgloss.NewStyle().PaddingLeft(PanelPadLeft).Width(width)
	cursorStyle := lineStyle.Inherit(theme.CursorHighlight)

	var b strings.Builder

	for i, item := range c.Items {
		style := lineStyle
		if i == c.Cursor {
			style = cursorStyle
		}

		b.WriteString(style.Render(item))

		if i < len(c.Items)-1 {
			b.WriteByte('\n')
		}
	}

	p.viewport.Width = width
	p.viewport.Height = listHeight
	p.viewport.SetContent(b.String())

	if c.Cursor >= 0 {
		if c.Cursor < p.viewport.YOffset {
			p.viewport.SetYOffset(c.Cursor)
		} else if c.Cursor >= p.viewport.YOffset+listHeight {
			p.viewport.SetYOffset(c.Cursor - listHeight + 1)
		}
	}

	if headerStr != "" {
		return lipgloss.JoinVertical(lipgloss.Left, headerStr, p.viewport.View())
	}

	return p.viewport.View()
}
