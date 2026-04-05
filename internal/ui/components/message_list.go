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
		rendered = append(rendered, renderLine(line, width, m.highlightWords, m.userNick, m.commands))
	}

	return strings.Join(rendered, "\n")
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
