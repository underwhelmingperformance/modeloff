package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// HighlightWordsMsg updates the set of words that trigger visual
// highlighting in message lines.
type HighlightWordsMsg struct {
	Words    []string
	UserNick domain.Nick
}

// MessageList displays chat lines in a scrollable viewport with
// support for a new-messages divider, a pending-response spinner,
// and an empty-state placeholder.
type MessageList struct {
	channel     domain.ChannelName
	lines       []tea.Msg
	viewport    viewport.Model
	pending     bool
	spinner     spinner.Model
	placeholder string
	seenCount   int

	// commands is kept so that renderHelp can list them.
	commands command.Set

	highlightWords []string
	userNick       domain.Nick
}

// NewMessageList creates a message list for the given channel.
func NewMessageList(ch domain.ChannelName, lines []tea.Msg) *MessageList {
	vp := viewport.New(0, 0)
	vp.MouseWheelEnabled = true

	return &MessageList{
		channel:   ch,
		lines:     lines,
		seenCount: len(lines),
		viewport:  vp,
		spinner: spinner.New(
			spinner.WithSpinner(spinner.Dot),
			spinner.WithStyle(theme.Dim),
		),
	}
}

// Lines returns the current chat lines.
func (m *MessageList) Lines() []tea.Msg {
	return m.lines
}

// Pending returns whether the pending indicator is active.
func (m *MessageList) Pending() bool {
	return m.pending
}

// Viewport returns a pointer to the underlying viewport for
// external scroll operations (e.g. mouse wheel from ChatView).
func (m *MessageList) Viewport() *viewport.Model {
	return &m.viewport
}

// SetKeyMap applies viewport key bindings from the ChatView key map.
func (m *MessageList) SetKeyMap(km ChatViewKeyMap) {
	m.viewport.KeyMap = viewport.KeyMap{
		PageDown: km.PageDown,
		PageUp:   km.PageUp,
		Down:     km.ScrollDown,
		Up:       km.ScrollUp,
	}
}

// Init implements ui.Model.
func (m *MessageList) Init() tea.Cmd {
	return nil
}

// Update implements ui.Model.
func (m *MessageList) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case SetChannelMsg:
		m.setChannel(msg.Channel, msg.Lines)
		return m, nil

	case SetLinesMsg:
		m.setLines(msg.Lines)
		return m, nil

	case SetPlaceholderMsg:
		m.placeholder = msg.Text
		return m, nil

	case HighlightWordsMsg:
		m.highlightWords = msg.Words
		m.userNick = msg.UserNick
		return m, nil

	case CommandStateMsg:
		m.commands = msg.Commands
		return m, nil

	case PendingResponseMsg:
		m.pending = msg.Pending

		if m.pending {
			return m, m.spinner.Tick
		}

		return m, nil

	case spinner.TickMsg:
		if !m.pending {
			return m, nil
		}

		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)

		return m, cmd

	case MessagesUpdatedMsg:
		if msg.Channel != m.channel {
			return m, nil
		}

		m.lines = msg.Lines
		m.seenCount = len(msg.Lines)
		m.viewport.GotoBottom()

		return m, nil

	case MessageLine, Join, Part, NickChange, TopicChange, ModelInvited, ModelKicked, TopicInfo,
		Help, Whois, ChannelList, APIKeySaved, PokeIntervalSet, NickModelSet, DMOpened,
		UsageHint, NoChannel, CommandError, ConfigChanged, BackendError, NewMessagesDivider:
		m.appendLines([]tea.Msg{msg})

		return m, nil
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)

	return m, cmd
}

// View implements ui.Model. It renders the message list, pending
// indicator, and spinner into the given dimensions. It returns the
// rendered view, whether the viewport is scrolled up, and the scroll
// percentage.
func (m *MessageList) View(width, height int) string {
	var pendingView string
	pendingHeight := 0

	if m.pending {
		pendingView = m.spinner.View() + theme.Info.Render(" responding…")
		pendingHeight = lipgloss.Height(pendingView)
	}

	maxListHeight := height - pendingHeight
	if maxListHeight < 0 {
		maxListHeight = 0
	}

	messageView, scrolled, scrollPct := m.renderMessages(width, maxListHeight)

	var scrollView string
	if scrolled {
		indicator := theme.Dim.Render(fmt.Sprintf("(%d%%)", int(scrollPct*100)))
		scrollView = lipgloss.PlaceHorizontal(width, lipgloss.Right, indicator)

		listHeight := maxListHeight - 1
		if listHeight < 0 {
			listHeight = 0
		}

		messageView, _, _ = m.renderMessages(width, listHeight)
	}

	parts := make([]string, 0, 4)

	if scrollView != "" {
		parts = append(parts, scrollView)
	}

	parts = append(parts, messageView)

	if pendingView != "" {
		parts = append(parts, pendingView)
	}

	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// ScrollInfo returns whether the viewport is scrolled up and the
// current scroll percentage.
func (m *MessageList) ScrollInfo() (scrolled bool, pct float64) {
	return !m.viewport.AtBottom(), m.viewport.ScrollPercent()
}

// SyncViewport recalculates viewport content for the given dimensions.
func (m *MessageList) SyncViewport(width, height int) {
	if width < 0 {
		width = 0
	}

	if height < 0 {
		height = 0
	}

	m.viewport.Width = width
	m.viewport.Height = height
	m.viewport.SetContent(m.renderedContent(width))
}

func (m *MessageList) setLines(lines []tea.Msg) {
	wasEmpty := len(m.lines) == 0

	scrolledUp := !m.viewport.AtBottom() && m.viewport.TotalLineCount() > 0
	newContent := len(lines) > m.seenCount

	if scrolledUp && newContent && m.seenCount > 0 {
		lines = m.insertDivider(lines)
	}

	m.lines = lines

	if !scrolledUp {
		m.seenCount = m.countWithoutDivider(lines)
	}

	if wasEmpty && len(lines) > 0 {
		m.viewport.SetContent("")
	}
}

func (m *MessageList) setChannel(ch domain.ChannelName, lines []tea.Msg) {
	m.channel = ch
	m.lines = lines
	m.seenCount = len(lines)
	m.viewport.SetContent("")
	m.viewport.GotoBottom()
}

func (m *MessageList) appendLines(newLines []tea.Msg) {
	m.lines = append(m.lines, newLines...)
	m.seenCount = len(m.lines)
	m.viewport.GotoBottom()
}

func (m *MessageList) renderMessages(width, height int) (view string, scrolled bool, scrollPct float64) {
	if len(m.lines) == 0 {
		text := theme.Dim.Render("No messages yet")
		if m.placeholder != "" {
			text = m.placeholder
		}

		return lipgloss.Place(width, height,
			lipgloss.Center, lipgloss.Center, text), false, 0
	}

	content := m.renderedContent(width)
	m.viewport.Width = width
	m.viewport.Height = height

	wasAtBottom := m.viewport.AtBottom() || m.viewport.TotalLineCount() == 0
	m.viewport.SetContent(content)

	if wasAtBottom {
		m.viewport.GotoBottom()
		m.clearDivider()
		m.seenCount = m.countWithoutDivider(m.lines)
	}

	return m.viewport.View(), !m.viewport.AtBottom(), m.viewport.ScrollPercent()
}

func (m *MessageList) renderedContent(width int) string {
	rendered := make([]string, 0, len(m.lines))
	for _, line := range m.lines {
		rendered = append(rendered, m.renderLine(line, width))
	}

	return strings.Join(rendered, "\n")
}

func (m *MessageList) renderLine(line tea.Msg, width int) string {
	wrap := lipgloss.NewStyle().Width(width)

	switch l := line.(type) {
	case MessageLine:
		ts := theme.Dim.Render(l.Message.SentAt.Format("[15:04:05]"))
		highlighted := m.containsHighlightWord(l.Message.Body)

		body := l.Message.Body
		if highlighted {
			body = theme.Highlight.Render(body)
		}

		if l.Message.Action {
			nick := theme.NickStyle(string(l.Message.From)).
				Render(string(l.Message.From))
			return wrap.Render(fmt.Sprintf("%s * %s %s", ts, nick, body))
		}

		nick := theme.NickStyle(string(l.Message.From)).
			Render(fmt.Sprintf("<%s>", string(l.Message.From)))

		return wrap.Render(fmt.Sprintf("%s %s %s", ts, nick, body))

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
		var text string

		if l.Topic == "" {
			text = fmt.Sprintf("topic for %s cleared by %s", l.Channel, l.By)
		} else if l.By != "" {
			text = fmt.Sprintf("topic for %s set by %s: %s", l.Channel, l.By, l.Topic)
		} else {
			text = fmt.Sprintf("topic for %s set to: %s", l.Channel, l.Topic)
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

	case TopicInfo:
		if l.Channel.Topic == "" {
			return wrap.Render(theme.SystemEvent.Render(
				fmt.Sprintf("*** No topic set for %s", l.Channel.Name)))
		}

		text := fmt.Sprintf("topic for %s: %s", l.Channel.Name, l.Channel.Topic)
		if l.Channel.TopicSetBy != "" {
			text += fmt.Sprintf(" (set by %s on %s)",
				l.Channel.TopicSetBy, l.Channel.TopicSetAt.Format("2006-01-02 15:04"))
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

	case Help:
		return wrap.Render(m.renderHelp())

	case Whois:
		return wrap.Render(m.renderWhois(l))

	case ChannelList:
		return wrap.Render(m.renderChannelList(l))

	case APIKeySaved:
		return wrap.Render(theme.Success.Render(
			"✓ OpenRouter API key saved and activated."))

	case PokeIntervalSet:
		return wrap.Render(theme.Success.Render(
			fmt.Sprintf("✓ Poke interval set to %s.", l.Interval)))

	case NickModelSet:
		return wrap.Render(theme.Success.Render(
			fmt.Sprintf("✓ Nick generation model set to %s.", l.ModelID)))

	case DMOpened:
		return wrap.Render(theme.Success.Render(
			fmt.Sprintf("✓ Opened direct message with %s", l.Nick)))

	case UsageHint:
		return wrap.Render(theme.Warning.Render("⚠ " + usageText(l.Command)))

	case NoChannel:
		return wrap.Render(theme.Warning.Render("⚠ join a channel first"))

	case CommandError:
		return wrap.Render(theme.Error.Render("✗ " + l.Err.Error()))

	case ConfigChanged:
		return wrap.Render(theme.Success.Render("✓ " + l.Operation))

	case BackendError:
		return wrap.Render(theme.Error.Render(
			fmt.Sprintf("✗ %s: %s", l.Operation, l.Err)))

	case NewMessagesDivider:
		return m.renderNewMessagesDivider(width)

	default:
		return ""
	}
}

func (m *MessageList) containsHighlightWord(body string) bool {
	return ContainsHighlightWord(body, m.highlightWords, m.userNick)
}

// ContainsHighlightWord reports whether body contains any of the
// given highlight words. The placeholder "$nick" is expanded to the
// provided userNick. Matching is case-insensitive.
func ContainsHighlightWord(body string, words []string, userNick domain.Nick) bool {
	if len(words) == 0 {
		return false
	}

	lower := strings.ToLower(body)

	for _, word := range words {
		w := word
		if w == "$nick" {
			w = string(userNick)
		}

		if w == "" {
			continue
		}

		if strings.Contains(lower, strings.ToLower(w)) {
			return true
		}
	}

	return false
}

func (m *MessageList) renderHelp() string {
	lines := make([]string, 0, len(m.commands.Commands))
	for _, node := range m.commands.Commands {
		usage := node.Usage()

		line := usage
		if node.Help != "" {
			line = fmt.Sprintf("%-32s %s", usage, node.Help)
		}

		lines = append(lines, strings.TrimRight(line, " "))
	}

	if len(lines) == 0 {
		lines = []string{"/help                            Show available commands."}
	}

	var parts []string
	for _, line := range lines {
		parts = append(parts, theme.SystemEvent.Render("*** "+line))
	}

	return strings.Join(parts, "\n")
}

func (m *MessageList) renderWhois(w Whois) string {
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

func (m *MessageList) renderChannelList(cl ChannelList) string {
	if len(cl.Channels) == 0 {
		return theme.SystemEvent.Render("*** no channels")
	}

	var parts []string
	for _, ch := range cl.Channels {
		line := string(ch.Name)
		if ch.Topic != "" {
			line += " — " + ch.Topic
		}

		parts = append(parts, theme.SystemEvent.Render("*** "+line))
	}

	return strings.Join(parts, "\n")
}

func (m *MessageList) renderNewMessagesDivider(width int) string {
	label := theme.Warning.Render(" new messages ")
	labelWidth := lipgloss.Width(label)

	leftWidth := (width - labelWidth) / 2
	rightWidth := width - leftWidth - labelWidth

	left := strings.Repeat("─", max(0, leftWidth))
	right := strings.Repeat("─", max(0, rightWidth))

	return theme.Dim.Render(left) + label + theme.Dim.Render(right)
}

func (m *MessageList) insertDivider(lines []tea.Msg) []tea.Msg {
	cleaned := m.stripDivider(lines)

	pos := m.seenCount
	if pos > len(cleaned) {
		pos = len(cleaned)
	}

	result := make([]tea.Msg, 0, len(cleaned)+1)
	result = append(result, cleaned[:pos]...)
	result = append(result, NewMessagesDivider{})
	result = append(result, cleaned[pos:]...)

	return result
}

func (m *MessageList) stripDivider(lines []tea.Msg) []tea.Msg {
	result := make([]tea.Msg, 0, len(lines))

	for _, l := range lines {
		if _, ok := l.(NewMessagesDivider); !ok {
			result = append(result, l)
		}
	}

	return result
}

func (m *MessageList) clearDivider() {
	m.lines = m.stripDivider(m.lines)
}

func (m *MessageList) countWithoutDivider(lines []tea.Msg) int {
	n := 0

	for _, l := range lines {
		if _, ok := l.(NewMessagesDivider); !ok {
			n++
		}
	}

	return n
}
