package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/text/language"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ptr"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
	"github.com/laney/modeloff/internal/ui/timestamp"
)

// HighlightWordsMsg updates the set of words that trigger visual
// highlighting in message lines.
type HighlightWordsMsg struct {
	Words    []string
	UserNick domain.Nick
}

// TimestampFormatMsg updates the timestamp formatting configuration
// for rendered message lines.
type TimestampFormatMsg struct {
	Format *string
	Locale language.Tag
}

const defaultBufferCap = 100

// MessageList displays channel events in a scrollable viewport with
// support for a new-messages divider, a pending-response spinner,
// and an empty-state placeholder.
type MessageList struct {
	channel     domain.ChannelName
	kind        domain.ChannelKind
	events      *RingBuffer[domain.StoredEvent]
	viewport    viewport.Model
	pending     bool
	spinner     spinner.Model
	placeholder string

	// seenCount is the number of events the user has "seen" (i.e.
	// were present when the viewport was last at the bottom). The
	// new-messages divider is rendered between seenCount and the
	// next event during renderedContent.
	seenCount int

	// showDivider is true when the viewport is scrolled up and new
	// events have arrived since.
	showDivider bool

	commands        []*command.Node
	highlightWords  []string
	userNick        domain.Nick
	timestampFormat *string
	locale          language.Tag
}

// NewMessageList creates an empty message list for the given
// channel. Callers should pass the channel kind so renderers that
// differentiate by kind (e.g. status channels suppressing nick
// prefixes) have it available without a follow-up SetChannelMsg.
func NewMessageList(ch domain.ChannelName, kind domain.ChannelKind) MessageList {
	vp := viewport.New(0, 0)
	vp.MouseWheelEnabled = true

	return MessageList{
		channel:  ch,
		kind:     kind,
		events:   NewRingBuffer[domain.StoredEvent](defaultBufferCap),
		viewport: vp,
		locale:   timestamp.CurrentLocale(),
		spinner: spinner.New(
			spinner.WithSpinner(spinner.Dot),
			spinner.WithStyle(theme.Dim),
		),
	}
}

// Len returns the number of events in the buffer.
func (m MessageList) Len() int {
	return m.events.Len()
}

// Pending returns whether the pending indicator is active.
func (m MessageList) Pending() bool {
	return m.pending
}

// SetKeyMap applies viewport key bindings from the ChatView key map.
func (m MessageList) SetKeyMap(km ChatViewKeyMap) MessageList {
	m.viewport.KeyMap = viewport.KeyMap{
		PageDown: km.PageDown.Binding,
		PageUp:   km.PageUp.Binding,
		Down:     km.ScrollDown.Binding,
		Up:       km.ScrollUp.Binding,
	}

	return m
}

// Init implements ui.Model.
func (m MessageList) Init() tea.Cmd {
	return nil
}

// Update implements ui.Model.
func (m MessageList) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case SetChannelMsg:
		m = m.setChannel(msg.Channel)
		m.kind = msg.Kind
		return m, nil

	case HistoryLoadedMsg:
		m = m.loadHistory(msg.Events)
		return m, nil

	case ClearMessagesMsg:
		m.events.Clear()
		m.seenCount = 0
		m.showDivider = false
		m.viewport.SetContent("")
		m.viewport.GotoBottom()
		return m, nil

	case SetPlaceholderMsg:
		m.placeholder = msg.Text
		return m, nil

	case HighlightWordsMsg:
		m.highlightWords = msg.Words
		m.userNick = msg.UserNick
		return m, nil

	case TimestampFormatMsg:
		m.timestampFormat = ptr.CloneString(msg.Format)
		m.locale = msg.Locale
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

	case domain.StoredEvent:
		m = m.appendEvent(msg)

		return m, nil

	case ui.BoundsMsg:
		wasAtBottom := m.viewport.AtBottom() || m.viewport.TotalLineCount() == 0
		m.viewport.Width = max(msg.Rect.Width, 0)
		m.viewport.Height = max(msg.Rect.Height, 0)
		m = m.refreshContent(wasAtBottom)

		return m, nil
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)

	return m, cmd
}

// View implements ui.Model.
func (m MessageList) View(width, height int) string {
	var pendingView string
	pendingHeight := 0

	if m.pending {
		pendingView = m.spinner.View() + theme.Info.Render(" responding…")
		pendingHeight = lipgloss.Height(pendingView)
	}

	maxListHeight := max(height-pendingHeight, 0)

	messageView, scrolled, scrollPct := m.renderMessages(width, maxListHeight)

	var scrollView string
	if scrolled {
		indicator := theme.Dim.Render(fmt.Sprintf("(%d%%)", int(scrollPct*100)))
		scrollView = lipgloss.PlaceHorizontal(width, lipgloss.Right, indicator)

		listHeight := max(maxListHeight-1, 0)

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
func (m MessageList) ScrollInfo() (scrolled bool, pct float64) {
	return !m.viewport.AtBottom(), m.viewport.ScrollPercent()
}

func (m MessageList) setChannel(ch domain.ChannelName) MessageList {
	m.channel = ch
	m.events.Clear()
	m.seenCount = 0
	m.showDivider = false
	m.viewport.SetContent("")
	m.viewport.GotoBottom()

	return m
}

func (m MessageList) loadHistory(events []domain.StoredEvent) MessageList {
	m.events.Clear()

	for _, e := range events {
		m.events.Append(e)
	}

	m.seenCount = m.events.Len()
	m.showDivider = false
	m.viewport.GotoBottom()

	return m.refreshContent(true)
}

// Replace replaces the event buffer with the given events and
// re-renders the viewport. Used by callers that don't drive the list
// via session events (for example, the ConnectionScreen status pane).
func (m MessageList) Replace(events []domain.StoredEvent) MessageList {
	return m.loadHistory(events)
}

// Append adds one or more events to the end of the buffer and
// re-renders the viewport. Used by callers that incrementally
// feed events into the list without owning the session-events
// drain (for example, the ConnectionScreen status pane refreshing
// itself on each tick).
func (m MessageList) Append(events ...domain.StoredEvent) MessageList {
	for _, evt := range events {
		m = m.appendEvent(evt)
	}

	return m
}

func (m MessageList) appendEvent(event domain.StoredEvent) MessageList {
	wasAtBottom := m.viewport.AtBottom() || m.viewport.TotalLineCount() == 0
	scrolledUp := !wasAtBottom && m.viewport.TotalLineCount() > 0

	m.events.Append(event)

	if scrolledUp && m.seenCount > 0 {
		m.showDivider = true
	}

	if !scrolledUp {
		m.seenCount = m.events.Len()
		m.showDivider = false
	}

	return m.refreshContent(wasAtBottom)
}

func (m MessageList) refreshContent(wasAtBottom bool) MessageList {
	if m.viewport.Width == 0 {
		return m
	}

	m.viewport.SetContent(m.renderedContent(m.viewport.Width))

	if wasAtBottom {
		m.viewport.GotoBottom()
	}

	return m
}

func (m MessageList) renderMessages(width, height int) (view string, scrolled bool, scrollPct float64) {
	if m.events.Len() == 0 {
		text := theme.Dim.Render("No messages yet")
		if m.placeholder != "" {
			text = m.placeholder
		}

		return lipgloss.Place(width, height,
			lipgloss.Center, lipgloss.Center, text), false, 0
	}

	vp := m.viewport
	vp.Width = width
	vp.Height = height
	content := m.renderedContent(width)
	vp.SetContent(content)

	rendered := vp.View()
	if lipgloss.Height(content) <= height {
		rendered = lipgloss.Place(width, height, lipgloss.Left, lipgloss.Bottom, content)
	}

	return rendered, !m.viewport.AtBottom(), m.viewport.ScrollPercent()
}

func (m MessageList) renderedContent(width int) string {
	rendered := make([]string, 0, m.events.Len()+1)

	for i := range m.events.Len() {
		if m.showDivider && i == m.seenCount {
			rendered = append(rendered, renderNewMessagesDivider(width))
		}

		event, _ := m.events.GetAt(i)
		if m.kind == domain.KindDM && isDMSuppressedEvent(event.Event) {
			continue
		}

		rendered = append(rendered, renderChannelEvent(
			event.Event,
			width,
			m.highlightWords,
			m.userNick,
			m.commands,
			m.timestampFormat,
			m.locale,
		))
	}

	// Divider at the end if seenCount == total (all seen, new arrived
	// while scrolled up).
	if m.showDivider && m.seenCount >= m.events.Len() {
		rendered = append(rendered, renderNewMessagesDivider(width))
	}

	return strings.Join(rendered, "\n")
}

func isDMSuppressedEvent(event domain.ChannelEvent) bool {
	switch event.(type) {
	case domain.ChannelJoin,
		domain.ChannelPart,
		domain.ChannelModeChange,
		domain.ChannelTopicChange,
		domain.ChannelModelInvited,
		domain.ChannelModelKicked:
		return true
	}

	return false
}
