package components

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// NickListUpdatedMsg tells the nick list to refresh its members.
type NickListUpdatedMsg struct {
	Members domain.MemberList
}

// NickListThinkingMsg updates which nicks are currently responding.
// A nil or empty map clears all thinking indicators.
type NickListThinkingMsg struct {
	Nicks map[domain.Nick]bool
}

func nickListView(thinking map[domain.Nick]bool) func(domain.Member, ViewState, int) string {
	return func(m domain.Member, _ ViewState, _ int) string {
		prefix := m.Mode.String()
		nick := string(m.Nick)

		var text string

		if prefix != "" {
			text = theme.Dim.Render(prefix) + theme.NickStyle(nick).Render(nick)
		} else {
			text = " " + theme.NickStyle(nick).Render(nick)
		}

		if thinking[m.Nick] {
			text += theme.Dim.Render(" …")
		}

		return text
	}
}

// NickList displays the sorted members of the current channel.
type NickList struct {
	panel    Sidebar[domain.Member, domain.Nick]
	thinking map[domain.Nick]bool
}

// NewNickList creates a nick list backed by the given member list.
func NewNickList(members domain.MemberList) NickList {
	nl := NickList{}

	nl.panel = NewSidebar(members.SortedSet(), SidebarConfig[domain.Member, domain.Nick]{
		Key:  func(m domain.Member) domain.Nick { return m.Nick },
		View: nickListView(nl.thinking),
	}).
		SetHeader("Nicks").
		SetEmpty("No members")

	return nl
}

// Init implements ui.Model.
func (n NickList) Init() tea.Cmd {
	return nil
}

// Update implements ui.Model.
func (n NickList) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case NickListUpdatedMsg:
		n.panel = n.panel.SetItems(msg.Members.SortedSet())

		return n, nil

	case NickListThinkingMsg:
		n.thinking = msg.Nicks
		n.panel.cfg.View = nickListView(n.thinking)

		return n, nil

	default:
		updated, cmd := n.panel.Update(msg)
		n.panel = updated.(Sidebar[domain.Member, domain.Nick])

		return n, cmd
	}
}

// View implements ui.Model.
func (n NickList) View(width, height int) string {
	return n.panel.View(width, height)
}
