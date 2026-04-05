package components

import (
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// MessageLine wraps a domain.Message for display in the chat view.
type MessageLine struct {
	Message domain.Message
}

// IRC lifecycle events — rendered with "*** " prefix.

// Join represents a user joining a channel.
type Join struct{ domain.JoinEvent }

// Part represents a user leaving a channel.
type Part struct{ domain.PartEvent }

// NickChange represents a nick change.
type NickChange struct{ domain.NickChangeEvent }

// TopicChange represents a channel topic change.
type TopicChange struct{ domain.TopicChangeEvent }

// ModelInvited represents a model being invited to a channel.
type ModelInvited struct{ domain.ModelInvitedEvent }

// ModelKicked represents a model being kicked from a channel.
type ModelKicked struct{ domain.ModelKickedEvent }

// TopicInfo displays the current topic with metadata (who set it, when).
type TopicInfo struct{ Channel domain.Channel }

// Application feedback — typed by origin.

// Help is the output of the /help command.
type Help struct{}

// Whois is the output of the /whois command.
type Whois struct{ domain.ModelInstance }

// ChannelList is the output of the /list command.
type ChannelList struct{ Channels []domain.Channel }

// APIKeySaved confirms the API key was persisted.
type APIKeySaved struct{}

// PokeIntervalSet confirms the poke interval was changed.
type PokeIntervalSet struct{ Interval time.Duration }

// NickModelSet confirms the nick generation model was changed.
type NickModelSet struct{ ModelID domain.ModelID }

// DMOpened confirms a direct message was opened.
type DMOpened struct{ Nick domain.Nick }

// UsageHint is a warning about incorrect command usage.
type UsageHint struct{ Command string }

// NoChannel is a warning shown when a command requires an active
// channel but none is selected.
type NoChannel struct{}

// CommandError wraps any error from command execution.
type CommandError struct{ Err error }

// ConfigChanged confirms a configuration change.
type ConfigChanged struct{ Operation string }

// BackendError wraps a backend error for display in the chat view.
type BackendError struct {
	Operation string
	Err       error
}

// NewMessagesDivider is a separator inserted into the chat view when
// new messages arrive while the viewport is scrolled up.
type NewMessagesDivider struct{}

// MessagesToLines converts a slice of domain messages into line values.
func MessagesToLines(msgs []domain.Message) []tea.Msg {
	lines := make([]tea.Msg, len(msgs))

	for i, m := range msgs {
		lines[i] = MessageLine{Message: m}
	}

	return lines
}

// MessagesUpdatedMsg tells the chat view to refresh its message list.
type MessagesUpdatedMsg struct {
	Channel domain.ChannelName
	Lines   []tea.Msg
}

// PendingResponseMsg sets or clears the "awaiting response" indicator
// in the chat view.
type PendingResponseMsg struct {
	Pending bool
}

// SetChannelMsg updates the channel identity, topic, and lines for a
// channel switch.
type SetChannelMsg struct {
	Channel domain.ChannelName
	Topic   string
	Lines   []tea.Msg
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
	messages *MessageList
	input    InputBar
	keyMap   ChatViewKeyMap

	bounds ui.Rect

	popover *Popover
}

type chatViewLayout struct {
	InputRect     ui.Rect
	MessageRect   ui.Rect
	PopoverLayout PopoverLayout
}

// NewChatView creates a chat view for the given channel.
func NewChatView(ch domain.ChannelName, userNick domain.Nick, topic string, lines []tea.Msg) *ChatView {
	keyMap := DefaultChatViewKeyMap

	ml := NewMessageList(ch, lines)
	ml.SetKeyMap(keyMap)

	return &ChatView{
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
func (c *ChatView) Init() tea.Cmd {
	return c.input.Init()
}

// KeyBindings implements ui.Keybinding.
func (c *ChatView) KeyBindings() []key.Binding {
	bindings := []key.Binding{
		ui.WithBindingEnabled(
			key.NewBinding(
				key.WithKeys("pgup", "pgdown"),
				key.WithHelp("PgUp/Dn", "scroll"),
			),
			len(c.messages.Lines()) > 0,
		),
		ui.WithBindingEnabled(
			key.NewBinding(
				key.WithKeys("ctrl+up", "ctrl+down"),
				key.WithHelp("^↑/↓", "scroll"),
			),
			len(c.messages.Lines()) > 0,
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
func (c *ChatView) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case ui.BoundsMsg:
		c.bounds = msg.Rect
		c.popover.SetBounds(msg.Rect)
		return c, nil

	case SetChannelMsg:
		c.channel = msg.Channel
		c.topic = msg.Topic
		_, cmd := c.messages.Update(msg)
		return c, cmd

	case TopicUpdatedMsg:
		c.topic = msg.Topic
		return c, nil

	case NickListUpdatedMsg:
		nicks := make([]domain.Nick, len(msg.Members))
		for i, m := range msg.Members {
			nicks[i] = m.Nick
		}

		c.input = c.input.SetNicks(nicks)

		return c, nil

	case SetLinesMsg, SetPlaceholderMsg, PendingResponseMsg, MessagesUpdatedMsg, HighlightWordsMsg,
		MessageLine, Join, Part, NickChange, TopicChange, ModelInvited, ModelKicked, TopicInfo,
		Help, Whois, ChannelList, APIKeySaved, PokeIntervalSet, NickModelSet, DMOpened,
		UsageHint, NoChannel, CommandError, ConfigChanged, BackendError, NewMessagesDivider:
		_, cmd := c.messages.Update(msg)
		return c, cmd

	case CommandStateMsg:
		c.messages.Update(msg)
		c.popover.Apply(msg.Commands, c.input.Value(), c.input.Cursor())
		return c, nil

	case PopoverAcceptMsg:
		c.input = c.input.ReplaceRange(msg.ReplaceStart, msg.ReplaceEnd, msg.Replacement)
		c.popover.Refresh(c.input.Value(), c.input.Cursor())
		return c, nil

	case tea.KeyMsg:
		if c.popover.IsVisible() {
			_, cmd := c.popover.Update(msg)
			if c.popover.Handled() {
				return c, cmd
			}
		}

	case tea.MouseMsg:
		if handled, cmd := c.handleMouse(msg); handled {
			return c, cmd
		}
	}

	// Forward to message list for viewport navigation, then input bar.
	_, mlCmd := c.messages.Update(msg)

	updated, inputCmd := c.input.Update(msg)
	c.input = updated.(InputBar)
	c.popover.Refresh(c.input.Value(), c.input.Cursor())

	return c, tea.Batch(mlCmd, inputCmd)
}

func (c *ChatView) handleMouse(msg tea.MouseMsg) (bool, tea.Cmd) {
	if c.bounds.Width == 0 || c.bounds.Height == 0 {
		return false, nil
	}

	layout := c.layoutRects()

	if layout.PopoverLayout.Rect.Contains(msg.X, msg.Y) {
		_, cmd := c.popover.Update(msg)
		return true, cmd
	}

	if c.popover.IsVisible() && msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
		c.popover.Dismiss(c.input.Value())
	}

	if layout.InputRect.Contains(msg.X, msg.Y) {
		switch msg.Action {
		case tea.MouseActionPress:
			if msg.Button == tea.MouseButtonLeft {
				localX, _ := layout.InputRect.Local(msg.X, msg.Y)
				c.input = c.input.SetCursorFromCell(localX - c.composerPrefixWidth())
				c.popover.Refresh(c.input.Value(), c.input.Cursor())
				return true, nil
			}
		case tea.MouseActionMotion:
			return true, nil
		}
	}

	if layout.MessageRect.Contains(msg.X, msg.Y) && msg.Action == tea.MouseActionPress {
		c.messages.SyncViewport(layout.MessageRect.Width, layout.MessageRect.Height)
		vp := c.messages.Viewport()

		switch msg.Button {
		case tea.MouseButtonWheelUp:
			vp.ScrollUp(vp.MouseWheelDelta)
			return true, nil
		case tea.MouseButtonWheelDown:
			vp.ScrollDown(vp.MouseWheelDelta)
			return true, nil
		}
	}

	return false, nil
}

// View implements ui.Model.
func (c *ChatView) View(width, height int) string {
	nickLabel := theme.UserNick.Render(string(c.userNick)) + " "
	inputView := nickLabel + c.input.View(width-lipgloss.Width(nickLabel), 1)
	inputHeight := lipgloss.Height(inputView)

	popoverView := c.popover.Render(width)
	popoverHeight := lipgloss.Height(popoverView)

	var topicView string
	topicHeight := 0
	if c.topic != "" {
		topicView = c.renderTopic(width)
		topicHeight = lipgloss.Height(topicView)
	}

	messageListHeight := height - inputHeight - topicHeight - popoverHeight
	if messageListHeight < 0 {
		messageListHeight = 0
	}

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

func (c *ChatView) layoutRects() chatViewLayout {
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

func (c *ChatView) composerPrefixWidth() int {
	return lipgloss.Width(theme.UserNick.Render(string(c.userNick))) + 1 + promptWidth()
}

func (c *ChatView) renderTopic(width int) string {
	text := theme.ChannelTitle.Render(c.topic)

	style := lipgloss.NewStyle().
		Width(width).
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		BorderForeground(lipgloss.ANSIColor(8))

	return style.Render(text)
}

func usageText(command string) string {
	switch command {
	case "config":
		return "usage: /config api-key <value> | /config nick-model <model-id> | /config poke-interval <duration>"
	case "config api-key":
		return "usage: /config api-key <value>"
	case "config nick-model":
		return "usage: /config nick-model <model-id>"
	case "config poke-interval":
		return "usage: /config poke-interval <duration>"
	case "invite":
		return "usage: /invite <model-id> [--persona <text>]"
	default:
		return "usage: /" + command
	}
}
