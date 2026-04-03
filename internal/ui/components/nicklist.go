package components

import (
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// NickListUpdatedMsg tells the nick list to refresh its members.
type NickListUpdatedMsg struct {
	Members []domain.Nick
}

// NickList displays the sorted members of the current channel.
type NickList struct {
	members  []domain.Nick
	viewport viewport.Model
}

// NewNickList creates a nick list from the given members. The
// members are copied and sorted alphabetically.
func NewNickList(members []domain.Nick) NickList {
	vp := viewport.New(0, 0)

	nl := NickList{viewport: vp}
	nl.setMembers(members)

	return nl
}

func (n *NickList) setMembers(members []domain.Nick) {
	if len(members) == 0 {
		n.members = nil
		return
	}

	sorted := make([]domain.Nick, len(members))
	copy(sorted, members)
	slices.Sort(sorted)

	n.members = sorted
}

// Init implements ui.Model.
func (n NickList) Init() tea.Cmd {
	return nil
}

// Update implements ui.Model.
func (n NickList) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case NickListUpdatedMsg:
		n.setMembers(msg.Members)
		return n, nil
	default:
		var cmd tea.Cmd
		n.viewport, cmd = n.viewport.Update(msg)
		return n, cmd
	}
}

// View implements ui.Model.
func (n NickList) View(width, height int) string {
	if len(n.members) == 0 {
		return lipgloss.Place(width, height,
			lipgloss.Center, lipgloss.Center,
			theme.Dim.Render("No members"))
	}

	header := theme.Dim.Render(lipgloss.PlaceHorizontal(
		width, lipgloss.Left, theme.Bold.Render("Users")))
	headerHeight := lipgloss.Height(header)

	listHeight := height - headerHeight
	if listHeight < 0 {
		listHeight = 0
	}

	var b strings.Builder

	for i, nick := range n.members {
		line := theme.NickStyle(string(nick)).Render(string(nick))
		line = lipgloss.NewStyle().Width(width).Render(line)
		b.WriteString(line)

		if i < len(n.members)-1 {
			b.WriteByte('\n')
		}
	}

	n.viewport.Width = width
	n.viewport.Height = listHeight
	n.viewport.SetContent(b.String())

	return lipgloss.JoinVertical(lipgloss.Left, header, n.viewport.View())
}
