package components

import (
	"slices"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// PendingResponseMsg sets or clears the "awaiting response" indicator
// in the chat view.
type PendingResponseMsg struct {
	Pending bool
}

// SetChannelMsg updates the channel identity and topic for a channel
// switch. The message list is cleared; history arrives separately via
// HistoryLoadedMsg.
type SetChannelMsg struct {
	Channel domain.ChannelName
	Topic   string
}

// HistoryLoadedMsg populates the message list with events loaded from
// the event log (e.g. on channel switch or scroll-back).
type HistoryLoadedMsg struct {
	Events []domain.StoredEvent
}

// SetLinesMsg replaces the displayed lines, preserving divider logic.
type SetLinesMsg struct {
	Lines []tea.Msg
}

// SetPlaceholderMsg sets text to show when there are no messages.
type SetPlaceholderMsg struct {
	Text string
}

// TopicUpdatedMsg updates the topic displayed in the chat view
// header without replacing the channel or message lines.
type TopicUpdatedMsg struct {
	Topic string
}

// CommandStateMsg updates the available commands for completion.
type CommandStateMsg struct {
	Commands command.Set
}

// ChatView displays messages for a single channel with an input bar
// at the bottom.
type ChatView struct {
	channel  domain.ChannelName
	topic    string
	userNick domain.Nick
	messages MessageList
	input    InputBar
	keyMap   ChatViewKeyMap

	bounds ui.Rect

	popover Popover
}

type chatViewLayout struct {
	InputRect     ui.Rect
	MessageRect   ui.Rect
	PopoverLayout PopoverLayout
}

// NewChatView creates a chat view for the given channel.
func NewChatView(ch domain.ChannelName, userNick domain.Nick, topic string) ChatView {
	keyMap := DefaultChatViewKeyMap

	ml := NewMessageList(ch).SetKeyMap(keyMap)

	return ChatView{
		channel:  ch,
		topic:    topic,
		userNick: userNick,
		messages: ml,
		input:    NewInputBar(),
		keyMap:   keyMap,
		popover:  NewPopover(),
	}
}

// Init implements ui.Model.
func (c ChatView) Init() tea.Cmd {
	return c.input.Init()
}

// KeyBindings implements ui.Keybinding.
func (c ChatView) KeyBindings() []key.Binding {
	bindings := []key.Binding{
		ui.WithBindingEnabled(
			key.NewBinding(
				key.WithKeys("pgup", "pgdown"),
				key.WithHelp("PgUp/Dn", "scroll"),
			),
			c.messages.Len() > 0,
		),
		ui.WithBindingEnabled(
			key.NewBinding(
				key.WithKeys("ctrl+up", "ctrl+down"),
				key.WithHelp("^↑/↓", "scroll"),
			),
			c.messages.Len() > 0,
		),
	}

	popoverVisible := c.popover.IsVisible()
	if popoverVisible {
		bindings = append(bindings,
			ui.WithBindingEnabled(
				key.NewBinding(
					key.WithKeys("tab"),
					key.WithHelp("Tab", "accept"),
				),
				c.popover.HasSuggestions(),
			),
			ui.WithBindingEnabled(
				key.NewBinding(
					key.WithKeys("up", "down", "shift+tab"),
					key.WithHelp("↑↓", "navigate"),
				),
				c.popover.HasSuggestions(),
			),
			key.NewBinding(
				key.WithKeys("esc"),
				key.WithHelp("Esc", "dismiss"),
			),
		)
	} else {
		bindings = append(bindings,
			ui.WithBindingEnabled(
				key.NewBinding(
					key.WithKeys("up", "down"),
					key.WithHelp("↑↓", "history"),
				),
				len(c.input.history) > 0,
			),
		)
	}

	bindings = append(bindings, ui.CollectKeyBindings(c.input)...)

	return bindings
}

// Update implements ui.Model.
func (c ChatView) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case ui.BoundsMsg:
		c.bounds = msg.Rect
		c, _ = c.updatePopover(msg)
		c = c.syncMessageViewport()
		return c, nil

	case SetChannelMsg:
		c.channel = msg.Channel
		c.topic = msg.Topic

		var cmd tea.Cmd
		c, cmd = c.updateMessages(msg)
		c = c.syncMessageViewport()

		return c, cmd

	case TopicUpdatedMsg:
		c.topic = msg.Topic
		c = c.syncMessageViewport()
		return c, nil

	case NickListUpdatedMsg:
		nicks := slices.Collect(msg.Members.Nicks())
		c.input = c.input.SetNicks(nicks)

		return c, nil

	case SetPlaceholderMsg, PendingResponseMsg, HighlightWordsMsg,
		HistoryLoadedMsg, domain.StoredEvent:
		var cmd tea.Cmd
		c, cmd = c.updateMessages(msg)
		c = c.syncMessageViewport()

		return c, cmd

	case CommandStateMsg:
		c, _ = c.updateMessages(msg)
		c, _ = c.updatePopover(PopoverApplyMsg{
			Commands: msg.Commands,
			Raw:      c.input.Value(),
			Cursor:   c.input.Cursor(),
		})
		c = c.syncMessageViewport()

		return c, nil

	case PopoverAcceptMsg:
		c.input = c.input.ReplaceRange(msg.ReplaceStart, msg.ReplaceEnd, msg.Replacement)
		c, _ = c.updatePopover(PopoverRefreshMsg{
			Raw:    c.input.Value(),
			Cursor: c.input.Cursor(),
		})
		c = c.syncMessageViewport()

		return c, nil

	case tea.KeyMsg:
		if c.popover.IsVisible() {
			var cmd tea.Cmd
			c, cmd = c.updatePopover(msg)

			if c.popover.Handled() {
				c = c.syncMessageViewport()
				return c, cmd
			}
		}

	case tea.MouseMsg:
		if updated, handled, cmd := c.handleMouse(msg); handled {
			return updated, cmd
		}
	}

	// Forward to message list for viewport navigation, then input bar.
	var mlCmd tea.Cmd
	c, mlCmd = c.updateMessages(msg)

	updated, inputCmd := c.input.Update(msg)
	c.input = updated.(InputBar)
	c, _ = c.updatePopover(PopoverRefreshMsg{
		Raw:    c.input.Value(),
		Cursor: c.input.Cursor(),
	})
	c = c.syncMessageViewport()

	return c, tea.Batch(mlCmd, inputCmd)
}

func (c ChatView) handleMouse(msg tea.MouseMsg) (ChatView, bool, tea.Cmd) {
	if c.bounds.Width == 0 || c.bounds.Height == 0 {
		return c, false, nil
	}

	layout := c.layoutRects()

	if layout.PopoverLayout.Rect.Contains(msg.X, msg.Y) {
		var cmd tea.Cmd
		c, cmd = c.updatePopover(msg)

		return c, true, cmd
	}

	if c.popover.IsVisible() && msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
		c, _ = c.updatePopover(PopoverDismissMsg{Raw: c.input.Value()})
	}

	if layout.InputRect.Contains(msg.X, msg.Y) {
		switch msg.Action {
		case tea.MouseActionPress:
			if msg.Button == tea.MouseButtonLeft {
				localX, _ := layout.InputRect.Local(msg.X, msg.Y)
				c.input = c.input.SetCursorFromCell(localX - c.composerPrefixWidth())
				c, _ = c.updatePopover(PopoverRefreshMsg{
					Raw:    c.input.Value(),
					Cursor: c.input.Cursor(),
				})

				return c, true, nil
			}
		case tea.MouseActionMotion:
			return c, true, nil
		}
	}

	if layout.MessageRect.Contains(msg.X, msg.Y) && msg.Action == tea.MouseActionPress {
		switch msg.Button {
		case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
			c, _ = c.updateMessages(msg)
			c = c.syncMessageViewport()

			return c, true, nil
		}
	}

	return c, false, nil
}

// View implements ui.Model.
func (c ChatView) View(width, height int) string {
	nickLabel := theme.UserNick.Render(string(c.userNick)) + " "
	inputView := nickLabel + c.input.View(width-lipgloss.Width(nickLabel), 1)
	inputHeight := lipgloss.Height(inputView)

	popoverView := c.popover.Render(width)
	popoverHeight := 0
	if popoverView != "" {
		popoverHeight = lipgloss.Height(popoverView)
	}

	var topicView string
	topicHeight := 0
	if c.topic != "" {
		topicView = c.renderTopic(width)
		topicHeight = lipgloss.Height(topicView)
	}

	messageListHeight := max(height-inputHeight-topicHeight-popoverHeight, 0)

	messageView := c.messages.View(width, messageListHeight)

	parts := make([]string, 0, 4)
	if topicView != "" {
		parts = append(parts, topicView)
	}

	parts = append(parts, messageView)

	if popoverView != "" {
		parts = append(parts, popoverView)
	}

	parts = append(parts, inputView)

	view := lipgloss.JoinVertical(lipgloss.Left, parts...)
	if lipgloss.Height(view) >= height {
		return view
	}

	return lipgloss.Place(width, height, lipgloss.Left, lipgloss.Bottom, view)
}

func (c ChatView) layoutRects() chatViewLayout {
	width := c.bounds.Width
	if width <= 0 {
		return chatViewLayout{}
	}

	inputRect := ui.Rect{
		X:      c.bounds.X,
		Y:      c.bounds.Y + c.bounds.Height - 1,
		Width:  width,
		Height: 1,
	}

	popoverLayout := c.popover.Layout(c.bounds, inputRect)

	topicHeight := 0
	if c.topic != "" {
		topicHeight = lipgloss.Height(c.renderTopic(width))
	}

	pendingHeight := 0
	if c.messages.Pending() {
		pendingHeight = 1
	}

	messageRect := ui.Rect{
		X:      c.bounds.X,
		Y:      c.bounds.Y + topicHeight,
		Width:  width,
		Height: c.bounds.Height - topicHeight - pendingHeight - popoverLayout.Rect.Height - 1,
	}
	if messageRect.Height < 0 {
		messageRect.Height = 0
	}

	return chatViewLayout{
		InputRect:     inputRect,
		MessageRect:   messageRect,
		PopoverLayout: popoverLayout,
	}
}

func (c ChatView) updateMessages(msg tea.Msg) (ChatView, tea.Cmd) {
	updated, cmd := c.messages.Update(msg)
	c.messages = updated.(MessageList)

	return c, cmd
}

func (c ChatView) syncMessageViewport() ChatView {
	layout := c.layoutRects()

	updated, _ := c.messages.Update(ui.BoundsMsg{Rect: layout.MessageRect})
	c.messages = updated.(MessageList)

	return c
}

func (c ChatView) updatePopover(msg tea.Msg) (ChatView, tea.Cmd) {
	updated, cmd := c.popover.Update(msg)
	c.popover = updated.(Popover)

	return c, cmd
}

func (c ChatView) composerPrefixWidth() int {
	return lipgloss.Width(theme.UserNick.Render(string(c.userNick))) + 1 + promptWidth()
}

func (c ChatView) renderTopic(width int) string {
	text := theme.ChannelTitle.Render(c.topic)

	style := lipgloss.NewStyle().
		Width(width).
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		BorderForeground(lipgloss.ANSIColor(8))

	return style.Render(text)
}
