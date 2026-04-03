package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

// SystemEventLine is a styled system event displayed inline in the
// chat view.
type SystemEventLine struct {
	Text string
	Kind EventKind
}

func (SystemEventLine) chatLine() {}

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

// PendingResponseMsg sets or clears the "awaiting response" indicator
// in the chat view.
type PendingResponseMsg struct {
	Pending bool
}

// ChatView displays messages for a single channel with an input bar
// at the bottom.
type ChatView struct {
	channel  domain.ChannelName
	title    string
	userNick domain.Nick
	lines    []ChatLine
	input    InputBar
	scroll   int
	pending  bool
	spinner  spinner.Model
}

// NewChatView creates a chat view for the given channel.
func NewChatView(ch domain.ChannelName, userNick domain.Nick, title string, lines []ChatLine) ChatView {
	return ChatView{
		channel:  ch,
		title:    title,
		userNick: userNick,
		lines:    lines,
		input:    NewInputBar(),
		spinner: spinner.New(
			spinner.WithSpinner(spinner.Dot),
			spinner.WithStyle(theme.Dim),
		),
	}
}

// Init implements ui.Model.
func (c ChatView) Init() tea.Cmd {
	return c.input.Init()
}

// Update implements ui.Model.
func (c ChatView) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case PendingResponseMsg:
		c.pending = msg.Pending

		if c.pending {
			return c, c.spinner.Tick
		}

		return c, nil

	case spinner.TickMsg:
		if !c.pending {
			return c, nil
		}

		var cmd tea.Cmd
		c.spinner, cmd = c.spinner.Update(msg)

		return c, cmd

	case MessagesUpdatedMsg:
		if msg.Channel != c.channel {
			return c, nil
		}

		c.lines = msg.Lines
		c.scroll = 0

		return c, nil

	case tea.KeyMsg:
		if msg.Type == tea.KeyPgUp {
			c.scroll++
			return c, nil
		}

		if msg.Type == tea.KeyPgDown {
			if c.scroll > 0 {
				c.scroll--
			}
			return c, nil
		}
	}

	updated, cmd := c.input.Update(msg)
	c.input = updated.(InputBar)

	return c, cmd
}

// View implements ui.Model.
func (c ChatView) View(width, height int) string {
	nickLabel := theme.UserNick.Render(string(c.userNick))
	inputView := nickLabel + " " + c.input.View(width-lipgloss.Width(nickLabel)-1, 1)
	inputHeight := lipgloss.Height(inputView)

	var topicView string
	topicHeight := 0

	if c.title != "" {
		topicView = c.renderTopic(width)
		topicHeight = lipgloss.Height(topicView)
	}

	var pendingView string
	pendingHeight := 0

	if c.pending {
		pendingView = c.spinner.View() + theme.Info.Render(" responding…")
		pendingHeight = lipgloss.Height(pendingView)
	}

	listHeight := height - inputHeight - topicHeight - pendingHeight
	if listHeight < 0 {
		listHeight = 0
	}

	messageView := c.renderMessages(width, listHeight)

	parts := make([]string, 0, 4)
	if topicView != "" {
		parts = append(parts, topicView)
	}

	parts = append(parts, messageView)

	if pendingView != "" {
		parts = append(parts, pendingView)
	}

	parts = append(parts, inputView)

	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func (c ChatView) renderTopic(width int) string {
	text := theme.ChannelTitle.Render(c.title)

	style := lipgloss.NewStyle().
		Width(width).
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		BorderForeground(lipgloss.ANSIColor(8))

	return style.Render(text)
}

func (c ChatView) renderMessages(width, height int) string {
	if len(c.lines) == 0 {
		return lipgloss.Place(width, height,
			lipgloss.Center, lipgloss.Center,
			theme.Dim.Render("No messages yet"))
	}

	lines := make([]string, 0, len(c.lines))

	for _, line := range c.lines {
		lines = append(lines, c.renderLine(line))
	}

	// Apply scroll offset from the bottom.
	end := len(lines) - c.scroll
	if end < 0 {
		end = 0
	}

	start := end - height
	if start < 0 {
		start = 0
	}

	visible := lines[start:end]

	content := strings.Join(visible, "\n")

	// Pad to fill the available height so the input bar stays at the
	// bottom.
	rendered := lipgloss.Height(content)
	if rendered < height {
		padding := strings.Repeat("\n", height-rendered-1)
		content = padding + content
	}

	return content
}

func (c ChatView) renderLine(line ChatLine) string {
	switch l := line.(type) {
	case SystemEventLine:
		return RenderSystemEvent(l.Text, l.Kind)

	case MessageLine:
		style := theme.ModelNick
		if l.Message.From == c.userNick {
			style = theme.UserNick
		}

		nick := style.Render(fmt.Sprintf("<%s>", string(l.Message.From)))

		return fmt.Sprintf("%s %s", nick, l.Message.Body)

	default:
		return ""
	}
}

// EventKind classifies the severity of a system event for styling.
type EventKind int

const (
	// EventInfo is for informational messages (list output, whois).
	EventInfo EventKind = iota

	// EventSuccess is for successful actions (join, nick change).
	EventSuccess

	// EventWarning is for validation warnings (usage hints).
	EventWarning

	// EventError is for errors (failed commands, unknown nicks).
	EventError
)

// RenderSystemEvent formats a system event with an icon and style
// appropriate to its kind.
func RenderSystemEvent(text string, kind EventKind) string {
	switch kind {
	case EventError:
		return theme.Error.Render("✗ " + text)
	case EventWarning:
		return theme.Warning.Render("⚠ " + text)
	case EventSuccess:
		return theme.Success.Render("✓ " + text)
	default:
		return theme.SystemEvent.Render("*** " + text)
	}
}
