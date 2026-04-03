package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
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
	channel     domain.ChannelName
	title       string
	userNick    domain.Nick
	lines       []ChatLine
	input       InputBar
	viewport    viewport.Model
	pending     bool
	spinner     spinner.Model
	width       int
	placeholder string
}

// NewChatView creates a chat view for the given channel.
func NewChatView(ch domain.ChannelName, userNick domain.Nick, title string, lines []ChatLine) *ChatView {
	vp := viewport.New(0, 0)
	vp.MouseWheelEnabled = true

	return &ChatView{
		channel:  ch,
		title:    title,
		userNick: userNick,
		lines:    lines,
		input:    NewInputBar(),
		viewport: vp,
		spinner: spinner.New(
			spinner.WithSpinner(spinner.Dot),
			spinner.WithStyle(theme.Dim),
		),
	}
}

// SetLines replaces the displayed lines, preserving viewport and
// input state. When lines transition from empty to non-empty, the
// viewport content is reset so stale placeholder rendering does not
// leak through.
func (c *ChatView) SetLines(lines []ChatLine) {
	wasEmpty := len(c.lines) == 0
	c.lines = lines

	if wasEmpty && len(lines) > 0 {
		c.viewport.SetContent("")
	}
}

// SetTitle updates the channel title in place.
func (c *ChatView) SetTitle(title string) {
	c.title = title
}

// SetPlaceholder sets text to show when there are no messages,
// replacing the default "No messages yet".
func (c *ChatView) SetPlaceholder(text string) {
	c.placeholder = text
}

// SetChannel updates the channel identity, title, and lines for a
// channel switch. The viewport content is cleared so stale
// placeholder rendering does not leak through.
func (c *ChatView) SetChannel(ch domain.ChannelName, title string, lines []ChatLine) {
	c.channel = ch
	c.title = title
	c.lines = lines
	c.viewport.SetContent("")
	c.viewport.GotoBottom()
}

// Init implements ui.Model.
func (c *ChatView) Init() tea.Cmd {
	return c.input.Init()
}

// Update implements ui.Model.
func (c *ChatView) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
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
		c.viewport.GotoBottom()

		return c, nil
	}

	// Forward to viewport for scroll handling (PgUp/PgDown, mouse
	// wheel). The viewport consumes scroll keys so they don't reach
	// the input bar.
	var vpCmd tea.Cmd
	c.viewport, vpCmd = c.viewport.Update(msg)

	updated, inputCmd := c.input.Update(msg)
	c.input = updated.(InputBar)

	return c, tea.Batch(vpCmd, inputCmd)
}

// View implements ui.Model.
func (c *ChatView) View(width, height int) string {
	nickLabel := theme.UserNick.Render(string(c.userNick)) + " "
	inputView := nickLabel + c.input.View(width-lipgloss.Width(nickLabel), 1)
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

	// First pass: render messages at maximum available height to
	// determine scroll state.
	maxListHeight := height - inputHeight - topicHeight - pendingHeight
	if maxListHeight < 0 {
		maxListHeight = 0
	}

	messageView, scrolled, scrollPct := c.renderMessages(width, maxListHeight)

	// If scrolled, add indicator and re-render with one less line.
	var scrollView string

	if scrolled {
		indicator := theme.Dim.Render(fmt.Sprintf("(%d%%)", int(scrollPct*100)))
		scrollView = lipgloss.PlaceHorizontal(width, lipgloss.Right, indicator)

		listHeight := maxListHeight - 1
		if listHeight < 0 {
			listHeight = 0
		}

		messageView, _, _ = c.renderMessages(width, listHeight)
	}

	parts := make([]string, 0, 6)
	if topicView != "" {
		parts = append(parts, topicView)
	}

	if scrollView != "" {
		parts = append(parts, scrollView)
	}

	parts = append(parts, messageView)

	if pendingView != "" {
		parts = append(parts, pendingView)
	}

	parts = append(parts, inputView)

	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func (c *ChatView) renderTopic(width int) string {
	text := theme.ChannelTitle.Render(c.title)

	style := lipgloss.NewStyle().
		Width(width).
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		BorderForeground(lipgloss.ANSIColor(8))

	return style.Render(text)
}

func (c *ChatView) renderMessages(width, height int) (view string, scrolled bool, scrollPct float64) {
	if len(c.lines) == 0 {
		text := theme.Dim.Render("No messages yet")
		if c.placeholder != "" {
			text = c.placeholder
		}

		return lipgloss.Place(width, height,
			lipgloss.Center, lipgloss.Center, text), false, 0
	}

	rendered := make([]string, 0, len(c.lines))

	for _, line := range c.lines {
		rendered = append(rendered, c.renderLine(line, width))
	}

	content := strings.Join(rendered, "\n")

	c.viewport.Width = width
	c.viewport.Height = height

	wasAtBottom := c.viewport.AtBottom() || c.viewport.TotalLineCount() == 0
	c.viewport.SetContent(content)

	if wasAtBottom {
		c.viewport.GotoBottom()
	}

	return c.viewport.View(), !c.viewport.AtBottom(), c.viewport.ScrollPercent()
}

func (c *ChatView) renderLine(line ChatLine, width int) string {
	wrap := lipgloss.NewStyle().Width(width)

	switch l := line.(type) {
	case SystemEventLine:
		return wrap.Render(RenderSystemEvent(l.Text, l.Kind))

	case MessageLine:
		ts := theme.Dim.Render(l.Message.SentAt.Format("[15:04:05]"))
		nick := theme.NickStyle(string(l.Message.From)).
			Render(fmt.Sprintf("<%s>", string(l.Message.From)))

		return wrap.Render(fmt.Sprintf("%s %s %s", ts, nick, l.Message.Body))

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
