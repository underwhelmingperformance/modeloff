package components

import (
	"fmt"
	"strings"
	"time"

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
	placeholder string

	// seenCount tracks how many lines the user has seen (the line
	// count when the viewport was last at the bottom). When new lines
	// arrive while scrolled up, a NewMessagesDivider is inserted at
	// this position.
	seenCount int
}

// NewChatView creates a chat view for the given channel.
func NewChatView(ch domain.ChannelName, userNick domain.Nick, title string, lines []ChatLine) *ChatView {
	vp := viewport.New(0, 0)
	vp.MouseWheelEnabled = true

	return &ChatView{
		channel:   ch,
		title:     title,
		userNick:  userNick,
		lines:     lines,
		seenCount: len(lines),
		input:     NewInputBar(),
		viewport:  vp,
		spinner: spinner.New(
			spinner.WithSpinner(spinner.Dot),
			spinner.WithStyle(theme.Dim),
		),
	}
}

// SetLines replaces the displayed lines, preserving viewport and
// input state. When lines transition from empty to non-empty, the
// viewport content is reset so stale placeholder rendering does not
// leak through. If the viewport is scrolled up and new lines have
// been added, a NewMessagesDivider is inserted at the boundary.
func (c *ChatView) SetLines(lines []ChatLine) {
	wasEmpty := len(c.lines) == 0

	scrolledUp := !c.viewport.AtBottom() && c.viewport.TotalLineCount() > 0
	newContent := len(lines) > c.seenCount

	if scrolledUp && newContent && c.seenCount > 0 {
		lines = c.insertDivider(lines)
	}

	c.lines = lines

	if !scrolledUp {
		c.seenCount = c.countWithoutDivider(lines)
	}

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
	c.seenCount = len(lines)
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
		c.seenCount = len(msg.Lines)
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
		c.clearDivider()
		c.seenCount = c.countWithoutDivider(c.lines)
	}

	return c.viewport.View(), !c.viewport.AtBottom(), c.viewport.ScrollPercent()
}

func (c *ChatView) renderLine(line ChatLine, width int) string {
	wrap := lipgloss.NewStyle().Width(width)

	switch l := line.(type) {
	case MessageLine:
		ts := theme.Dim.Render(l.Message.SentAt.Format("[15:04:05]"))
		nick := theme.NickStyle(string(l.Message.From)).
			Render(fmt.Sprintf("<%s>", string(l.Message.From)))

		return wrap.Render(fmt.Sprintf("%s %s %s", ts, nick, l.Message.Body))

	// IRC lifecycle events — "*** " prefix, SystemEvent style.

	case Join:
		text := fmt.Sprintf("%s has joined %s", l.Nick, l.Channel)
		if l.Created {
			text = fmt.Sprintf("Created channel %s", l.Channel)
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

	case Part:
		return wrap.Render(theme.SystemEvent.Render(
			fmt.Sprintf("*** %s has left %s", l.Nick, l.Channel)))

	case NickChange:
		return wrap.Render(theme.SystemEvent.Render(
			fmt.Sprintf("*** %s is now known as %s", l.OldNick, l.NewNick)))

	case TopicChange:
		text := fmt.Sprintf("topic for %s set to: %s", l.Channel, l.Title)
		if l.Title == "" {
			text = fmt.Sprintf("topic for %s cleared", l.Channel)
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

	case ModelInvited:
		text := fmt.Sprintf("%s (%s) has joined %s",
			l.Instance.Nick, l.Instance.ModelID, l.Channel)
		if l.Instance.Persona != "" {
			text = fmt.Sprintf("%s with persona %q", text, l.Instance.Persona)
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

	case ModelKicked:
		return wrap.Render(theme.SystemEvent.Render(
			fmt.Sprintf("*** %s has been kicked from %s", l.Nick, l.Channel)))

	// Application feedback.

	case Help:
		return wrap.Render(c.renderHelp())

	case Whois:
		return wrap.Render(c.renderWhois(l))

	case ChannelList:
		return wrap.Render(c.renderChannelList(l))

	case APIKeySaved:
		return wrap.Render(theme.Success.Render(
			"✓ OpenRouter API key saved. Restart modeloff to use it."))

	case PokeIntervalSet:
		return wrap.Render(theme.Success.Render(
			fmt.Sprintf("✓ Poke interval set to %s.", l.Interval)))

	case DMOpened:
		return wrap.Render(theme.Success.Render(
			fmt.Sprintf("✓ Opened direct message with %s", l.Nick)))

	case UsageHint:
		return wrap.Render(theme.Warning.Render("⚠ " + usageText(l.Command)))

	case NoChannel:
		return wrap.Render(theme.Warning.Render("⚠ join a channel first"))

	case CommandError:
		return wrap.Render(theme.Error.Render("✗ " + l.Err.Error()))

	case NewMessagesDivider:
		return c.renderNewMessagesDivider(width)

	default:
		return ""
	}
}

func (c *ChatView) renderHelp() string {
	lines := []string{
		"/join <channel>                   Join or create a channel",
		"/leave                            Leave the current channel",
		"/list                             List all channels",
		"/invite <model> [--persona text]  Invite a model to the channel",
		"/kick <nick>                      Remove a model from the channel",
		"/msg <nick> [message]             Open a direct message",
		"/nick <name>                      Change your nickname",
		"/title [text]                     Set or clear the channel title",
		"/whois <nick>                     Show info about a model",
		"/config api-key <key>             Set the OpenRouter API key",
		"/config poke-interval <duration>  Set the poke interval",
		"/help                             Show this help",
		"/quit                             Exit modeloff",
	}

	var parts []string
	for _, line := range lines {
		parts = append(parts, theme.SystemEvent.Render("*** "+line))
	}

	return strings.Join(parts, "\n")
}

func (c *ChatView) renderWhois(w Whois) string {
	lines := []string{
		fmt.Sprintf("%s is %s", w.Nick, w.ModelID),
	}

	if w.Persona != "" {
		lines = append(lines, fmt.Sprintf("  persona: %s", w.Persona))
	}

	if len(w.Channels) > 0 {
		var chStrs []string
		for ch := range w.Channels.Sorted() {
			chStrs = append(chStrs, string(ch))
		}

		lines = append(lines, fmt.Sprintf("  channels: %s", strings.Join(chStrs, ", ")))
	}

	var parts []string
	for _, line := range lines {
		parts = append(parts, theme.SystemEvent.Render("*** "+line))
	}

	return strings.Join(parts, "\n")
}

func (c *ChatView) renderChannelList(cl ChannelList) string {
	if len(cl.Channels) == 0 {
		return theme.SystemEvent.Render("*** no channels")
	}

	var parts []string
	for _, ch := range cl.Channels {
		line := string(ch.Name)
		if ch.Title != "" {
			line += " — " + ch.Title
		}

		parts = append(parts, theme.SystemEvent.Render("*** "+line))
	}

	return strings.Join(parts, "\n")
}

func (c *ChatView) renderNewMessagesDivider(width int) string {
	label := theme.Warning.Render(" new messages ")
	labelWidth := lipgloss.Width(label)

	leftWidth := (width - labelWidth) / 2
	rightWidth := width - leftWidth - labelWidth

	left := strings.Repeat("─", max(0, leftWidth))
	right := strings.Repeat("─", max(0, rightWidth))

	return theme.Dim.Render(left) + label + theme.Dim.Render(right)
}

// insertDivider returns a copy of lines with a NewMessagesDivider
// inserted at the seenCount position.
func (c *ChatView) insertDivider(lines []ChatLine) []ChatLine {
	// Remove any existing divider first.
	cleaned := c.stripDivider(lines)

	pos := c.seenCount
	if pos > len(cleaned) {
		pos = len(cleaned)
	}

	result := make([]ChatLine, 0, len(cleaned)+1)
	result = append(result, cleaned[:pos]...)
	result = append(result, NewMessagesDivider{})
	result = append(result, cleaned[pos:]...)

	return result
}

// stripDivider returns lines with any NewMessagesDivider removed.
func (c *ChatView) stripDivider(lines []ChatLine) []ChatLine {
	result := make([]ChatLine, 0, len(lines))

	for _, l := range lines {
		if _, ok := l.(NewMessagesDivider); !ok {
			result = append(result, l)
		}
	}

	return result
}

// clearDivider removes any NewMessagesDivider from the current lines.
func (c *ChatView) clearDivider() {
	c.lines = c.stripDivider(c.lines)
}

// countWithoutDivider returns the number of non-divider lines.
func (c *ChatView) countWithoutDivider(lines []ChatLine) int {
	n := 0

	for _, l := range lines {
		if _, ok := l.(NewMessagesDivider); !ok {
			n++
		}
	}

	return n
}

func usageText(command string) string {
	switch command {
	case "config":
		return "usage: /config api-key <value> | /config poke-interval <duration>"
	case "config api-key":
		return "usage: /config api-key <value>"
	case "config poke-interval":
		return "usage: /config poke-interval <duration>"
	case "invite":
		return "usage: /invite <model-id> [--persona <text>]"
	default:
		return "usage: /" + command
	}
}
