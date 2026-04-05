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

// ChatLine is a single line in the chat view. It is either a user or
// model message, or a system event.
type ChatLine interface {
	chatLine()
}

// MessageLine wraps a domain.Message for display in the chat view.
type MessageLine struct {
	Message domain.Message
}

func (MessageLine) chatLine() {}

// IRC lifecycle events — rendered with "*** " prefix.

// Join represents a user joining a channel.
type Join struct{ domain.JoinEvent }

func (Join) chatLine() {}

// Part represents a user leaving a channel.
type Part struct{ domain.PartEvent }

func (Part) chatLine() {}

// NickChange represents a nick change.
type NickChange struct{ domain.NickChangeEvent }

func (NickChange) chatLine() {}

// TopicChange represents a channel topic change.
type TopicChange struct{ domain.TopicChangeEvent }

func (TopicChange) chatLine() {}

// ModelInvited represents a model being invited to a channel.
type ModelInvited struct{ domain.ModelInvitedEvent }

func (ModelInvited) chatLine() {}

// ModelKicked represents a model being kicked from a channel.
type ModelKicked struct{ domain.ModelKickedEvent }

func (ModelKicked) chatLine() {}

// Application feedback — typed by origin.

// Help is the output of the /help command.
type Help struct{}

func (Help) chatLine() {}

// Whois is the output of the /whois command.
type Whois struct{ domain.ModelInstance }

func (Whois) chatLine() {}

// ChannelList is the output of the /list command.
type ChannelList struct{ Channels []domain.Channel }

func (ChannelList) chatLine() {}

// APIKeySaved confirms the API key was persisted.
type APIKeySaved struct{}

func (APIKeySaved) chatLine() {}

// PokeIntervalSet confirms the poke interval was changed.
type PokeIntervalSet struct{ Interval time.Duration }

func (PokeIntervalSet) chatLine() {}

// NickModelSet confirms the nick generation model was changed.
type NickModelSet struct{ ModelID domain.ModelID }

func (NickModelSet) chatLine() {}

// DMOpened confirms a direct message was opened.
type DMOpened struct{ Nick domain.Nick }

func (DMOpened) chatLine() {}

// UsageHint is a warning about incorrect command usage.
type UsageHint struct{ Command string }

func (UsageHint) chatLine() {}

// NoChannel is a warning shown when a command requires an active
// channel but none is selected.
type NoChannel struct{}

func (NoChannel) chatLine() {}

// CommandError wraps any error from command execution.
type CommandError struct{ Err error }

func (CommandError) chatLine() {}

// ConfigChanged confirms a configuration change.
type ConfigChanged struct{ Operation string }

func (ConfigChanged) chatLine() {}

// BackendError wraps a backend error for display in the chat view.
type BackendError struct {
	Operation string
	Err       error
}

func (BackendError) chatLine() {}

// NewMessagesDivider is a separator inserted into the chat view when
// new messages arrive while the viewport is scrolled up.
type NewMessagesDivider struct{}

func (NewMessagesDivider) chatLine() {}

// MessagesToLines converts a slice of domain messages into chat lines.
func MessagesToLines(msgs []domain.Message) []ChatLine {
	lines := make([]ChatLine, len(msgs))

	for i, m := range msgs {
		lines[i] = MessageLine{Message: m}
	}

	return lines
}

// MessagesUpdatedMsg tells the chat view to refresh its message list.
type MessagesUpdatedMsg struct {
	Channel domain.ChannelName
	Lines   []ChatLine
}

// AppendLinesMsg appends lines to the chat view incrementally, without
// replacing the entire message list. This is more efficient when adding
// system events or individual messages.
type AppendLinesMsg struct {
	Channel domain.ChannelName
	Lines   []ChatLine
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
	Lines   []ChatLine
}

// SetLinesMsg replaces the displayed lines, preserving divider logic.
type SetLinesMsg struct {
	Lines []ChatLine
}

// SetPlaceholderMsg sets text to show when there are no messages.
type SetPlaceholderMsg struct {
	Text string
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
func NewChatView(ch domain.ChannelName, userNick domain.Nick, topic string, lines []ChatLine) *ChatView {
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

	case NickListUpdatedMsg:
		nicks := make([]domain.Nick, len(msg.Members))
		for i, m := range msg.Members {
			nicks[i] = m.Nick
		}

		c.input = c.input.SetNicks(nicks)

		return c, nil

	case SetLinesMsg, SetPlaceholderMsg, PendingResponseMsg, MessagesUpdatedMsg, AppendLinesMsg:
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
