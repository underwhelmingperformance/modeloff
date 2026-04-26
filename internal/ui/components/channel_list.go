package components

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/set"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// SetChannelsMsg replaces the entire channel list in the sidebar.
// Used for initial load where the full state is needed.
type SetChannelsMsg struct {
	Channels []domain.Window
	Active   domain.ChannelName
	Unread   map[domain.ChannelName]int
}

// ChannelSidebar is a ui.Model that wraps Sidebar with
// channel-specific keybindings, header, and unread tracking.
type ChannelSidebar struct {
	panel    Sidebar[domain.Window, domain.ChannelName]
	unread   map[domain.ChannelName]int
	mentions map[domain.ChannelName]bool
}

func windowLess(a, b domain.Window) bool {
	if a.Kind() != b.Kind() {
		return channelKindOrder(a.Kind()) < channelKindOrder(b.Kind())
	}

	return a.Name() < b.Name()
}

// channelKindOrder defines sidebar grouping: status pinned at the
// top, then regular channels, then DMs at the bottom.
func channelKindOrder(kind domain.ChannelKind) int {
	switch kind {
	case domain.KindStatus:
		return 0
	case domain.KindChannel:
		return 1
	case domain.KindDM:
		return 2
	}

	return 3
}

func windowView(unread map[domain.ChannelName]int, mentions map[domain.ChannelName]bool) func(domain.Window, ViewState, int) string {
	return func(w domain.Window, state ViewState, _ int) string {
		name := w.DisplayName()

		count := unread[w.Name()]
		if count > 0 {
			name += fmt.Sprintf(" (%d)", count)
		}

		highlighted := count > 0
		mention := mentions[w.Name()]

		style := theme.SidebarInactive

		switch {
		case state == StateActiveSelected:
			style = theme.SidebarActiveSelected
		case state == StateActive:
			style = theme.SidebarActive
		case state == StateSelected && mention:
			style = theme.SidebarMentionSelected
		case state == StateSelected && highlighted:
			style = theme.SidebarHighlightedSelected
		case state == StateSelected:
			style = theme.SidebarSelected
		case mention:
			style = theme.SidebarMention
		case highlighted:
			style = theme.SidebarHighlighted
		}

		prefix := " "
		if state == StateSelected || state == StateActiveSelected {
			prefix = "▸"
		}

		return style.Render(prefix + name)
	}
}

// NewChannelSidebar creates an empty channel list sidebar.
func NewChannelSidebar() ChannelSidebar {
	unread := make(map[domain.ChannelName]int)
	mentions := make(map[domain.ChannelName]bool)

	return ChannelSidebar{
		panel: NewSidebar(
			set.NewSorted(windowLess),
			SidebarConfig[domain.Window, domain.ChannelName]{
				Key:  func(w domain.Window) domain.ChannelName { return w.Name() },
				View: windowView(unread, mentions),
				OnActivate: func(w domain.Window) tea.Cmd {
					return func() tea.Msg {
						return ChannelSelectedMsg{Channel: w.Name()}
					}
				},
			},
		).
			SetHeader("Channels").
			SetEmpty("No channels").
			SetKeyMap(DefaultSidebarKeyMap.
				WithHelp(SidebarDown, "channels").
				WithHelp(SidebarUp, "channels").
				WithHelp(SidebarSelect, "switch channel")),
		unread:   unread,
		mentions: mentions,
	}
}

// Init implements ui.Model.
func (cl ChannelSidebar) Init() tea.Cmd {
	return cl.panel.Init()
}

// Update implements ui.Model.
func (cl ChannelSidebar) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case SetChannelsMsg:
		return cl.setChannels(msg), nil

	case ChannelAddedMsg:
		cl.panel.items.Insert(msg.Channel)

		if msg.Unread > 0 {
			cl.unread[msg.Channel.Name()] = msg.Unread
		}

		return cl, nil

	case ChannelRemovedMsg:
		cl.panel.items.Remove(domain.WindowKey(msg.Channel))
		delete(cl.unread, msg.Channel)
		delete(cl.mentions, msg.Channel)

		return cl, nil

	case ChannelActiveMsg:
		cl.panel = cl.panel.SetActiveKey(msg.Channel)
		delete(cl.mentions, msg.Channel)

		return cl, nil

	case ChannelUnreadMsg:
		if msg.Count > 0 {
			cl.unread[msg.Channel] = msg.Count

			if msg.Mention {
				cl.mentions[msg.Channel] = true
			}
		} else {
			delete(cl.unread, msg.Channel)
			delete(cl.mentions, msg.Channel)
		}

		return cl, nil

	default:
		updated, cmd := cl.panel.Update(msg)
		cl.panel = updated.(Sidebar[domain.Window, domain.ChannelName])

		return cl, cmd
	}
}

func (cl ChannelSidebar) setChannels(msg SetChannelsMsg) ChannelSidebar {
	items := set.NewSorted(windowLess)

	for _, ch := range msg.Channels {
		items.Insert(ch)
	}

	cl.unread = make(map[domain.ChannelName]int)
	cl.mentions = make(map[domain.ChannelName]bool)

	if msg.Unread != nil {
		for k, v := range msg.Unread {
			if v > 0 {
				cl.unread[k] = v
			}
		}
	}

	cl.panel.cfg.View = windowView(cl.unread, cl.mentions)
	cl.panel = cl.panel.SetItems(items)

	if msg.Active != "" {
		cl.panel = cl.panel.SetActiveKey(msg.Active)
	}

	return cl
}

// View implements ui.Model.
func (cl ChannelSidebar) View(width, height int) string {
	return cl.panel.View(width, height)
}

// KeyBindings implements ui.Keybinding.
func (cl ChannelSidebar) KeyBindings() []ui.KeyBinding {
	return cl.panel.KeyBindings()
}
