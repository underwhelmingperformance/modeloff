package components

import (
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// NickListUpdatedMsg tells the nick list to refresh its members.
type NickListUpdatedMsg struct {
	Members  domain.MemberList
	Revision uint64
}

// NickListThinkingMsg updates which nicks are currently responding.
// A nil or empty map clears all thinking indicators.
type NickListThinkingMsg struct {
	Nicks map[domain.Nick]bool
}

func nickListView(thinking map[domain.Nick]bool) func(domain.Member, ViewState, int) string {
	return func(m domain.Member, _ ViewState, _ int) string {
		prefix := m.Modes.Rank().String()
		nick := string(m.Nick)

		// Hash the colour by stable identity, not by the live nick
		// snapshot. A rename changes the rendered text but keeps the
		// colour consistent across the session.
		colourSeed := string(m.InstanceID)

		var text string

		if prefix != "" {
			text = theme.Dim.Render(prefix) + theme.NickStyle(colourSeed).Render(nick)
		} else {
			text = " " + theme.NickStyle(colourSeed).Render(nick)
		}

		if thinking[m.Nick] {
			text += theme.Dim.Render(" …")
		}

		return text
	}
}

// NickList displays the sorted members of the current channel.
type NickList struct {
	panel           Sidebar[domain.Member, domain.Nick]
	thinking        map[domain.Nick]bool
	membersRevision uint64
}

// NewNickList creates a nick list backed by the given member list.
func NewNickList(members domain.MemberList) NickList {
	nl := NickList{}

	nl.panel = NewSidebar(members.SortedSet(), SidebarConfig[domain.Member, domain.Nick]{
		Key:  func(m domain.Member) domain.Nick { return m.Nick },
		View: nickListView(nl.thinking),
	}).
		SetHeader("Nicks").
		SetEmpty("No members").
		SetKeyMap(EmptySidebarKeyMap)

	return nl
}

// Init implements ui.Component.
func (n NickList) Init() tea.Cmd {
	return nil
}

// Update implements ui.Component.
func (n NickList) Update(msg tea.Msg) (ui.Component, tea.Cmd) {
	switch msg := msg.(type) {
	case NickListUpdatedMsg:
		if msg.Revision < n.membersRevision {
			return n, nil
		}

		n.membersRevision = msg.Revision
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

// ContentWidth returns the width needed for the current nick rows.
func (n NickList) ContentWidth() int {
	return n.panel.contentWidth(func(member domain.Member) int {
		prefix := member.Modes.Rank().String()
		if prefix == "" {
			prefix = " "
		}

		return ansi.StringWidth(prefix + string(member.Nick) + " …")
	})
}
