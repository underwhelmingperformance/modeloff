package components

import (
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
	Kind    domain.ChannelKind
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

// CommandStateMsg updates the available commands for completion and
// help rendering. Completer is used by the popover; Commands is used
// by the message list for /help output.
type CommandStateMsg struct {
	Completer command.Completable
	Commands  []*command.Node
}

// ClearMessagesMsg clears the visible messages in the current channel
// without affecting the persistent event log.
type ClearMessagesMsg struct{}

// UserNickMsg updates the user's nick in the chat view and input bar.
type UserNickMsg struct {
	Nick domain.Nick
}

// ChatView displays messages for a single channel with an input bar
// at the bottom.
type ChatView struct {
	channel  domain.ChannelName
	kind     domain.ChannelKind
	topic    string
	userNick domain.Nick
	messages MessageList
	input    InputBar
	keyMap   ChatViewKeyMap

	bounds ui.Rect
}

type chatViewLayout struct {
	InputRect   ui.Rect
	MessageRect ui.Rect
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
		input:    NewInputBar(userNick),
		keyMap:   keyMap,
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

	bindings = append(bindings, ui.CollectKeyBindings(c.input)...)

	return bindings
}

// Update implements ui.Model.
func (c ChatView) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case ui.BoundsMsg:
		c.bounds = msg.Rect
		c = c.updateInput(msg)
		c = c.syncMessageViewport()
		return c, nil

	case SetChannelMsg:
		c.channel = msg.Channel
		c.kind = msg.Kind
		c.topic = msg.Topic

		var cmd tea.Cmd
		c, cmd = c.updateMessages(msg)
		c = c.syncMessageViewport()

		return c, cmd

	case TopicUpdatedMsg:
		c.topic = msg.Topic
		c = c.syncMessageViewport()
		return c, nil

	case UserNickMsg:
		c.userNick = msg.Nick
		c = c.updateInput(msg)

		return c, nil

	case NickListUpdatedMsg:
		c = c.updateInput(msg)

		return c, nil

	case SetPlaceholderMsg, PendingResponseMsg, HighlightWordsMsg, TimestampFormatMsg,
		HistoryLoadedMsg, ClearMessagesMsg, domain.StoredEvent:
		var cmd tea.Cmd
		c, cmd = c.updateMessages(msg)
		c = c.syncMessageViewport()

		return c, cmd

	case CommandStateMsg:
		c, _ = c.updateMessages(msg)
		c = c.updateInput(msg)
		c = c.syncMessageViewport()

		return c, nil

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
	c = c.syncMessageViewport()

	return c, tea.Batch(mlCmd, inputCmd)
}

func (c ChatView) handleMouse(msg tea.MouseMsg) (ChatView, bool, tea.Cmd) {
	if c.bounds.Width == 0 || c.bounds.Height == 0 {
		return c, false, nil
	}

	layout := c.layoutRects()

	if layout.InputRect.Contains(msg.X, msg.Y) {
		updated, cmd := c.input.Update(msg)
		c.input = updated.(InputBar)

		if cmd != nil {
			return c, true, cmd
		}

		return c, true, nil
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
	inputView := c.input.View(width, 1)
	inputHeight := lipgloss.Height(inputView)

	var topicView string
	topicHeight := 0
	if c.topic != "" && c.kind != domain.KindDM {
		topicView = c.renderTopic(width)
		topicHeight = lipgloss.Height(topicView)
	}

	messageListHeight := max(height-inputHeight-topicHeight, 0)

	messageView := c.messages.View(width, messageListHeight)

	parts := make([]string, 0, 3)
	if topicView != "" {
		parts = append(parts, topicView)
	}

	parts = append(parts, messageView, inputView)

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

	inputView := c.input.View(width, 1)
	inputHeight := lipgloss.Height(inputView)

	inputRect := ui.Rect{
		X:      c.bounds.X,
		Y:      c.bounds.Y + c.bounds.Height - inputHeight,
		Width:  width,
		Height: inputHeight,
	}

	topicHeight := 0
	if c.topic != "" && c.kind != domain.KindDM {
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
		Height: c.bounds.Height - topicHeight - pendingHeight - inputHeight,
	}
	if messageRect.Height < 0 {
		messageRect.Height = 0
	}

	return chatViewLayout{
		InputRect:   inputRect,
		MessageRect: messageRect,
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

func (c ChatView) updateInput(msg tea.Msg) ChatView {
	updated, _ := c.input.Update(msg)
	c.input = updated.(InputBar)

	return c
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
