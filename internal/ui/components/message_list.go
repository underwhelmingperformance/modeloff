package components

import (
	"fmt"
	"strings"

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

// MessageList displays channel events in a scrollable viewport with
// support for a new-messages divider and an empty-state placeholder.
// C is the grammar's completion-context type; it is carried so the
// `/help` renderer can walk the typed command tree supplied by
// [CommandsMsg].
//
// The message list does not own the event storage. The owning
// chat-screen (or test harness) passes an `events` closure that
// returns the current event slice for the active window; the
// message list reads through it on every `View`. A single source
// of truth removes the live-append-vs-snapshot race that an
// internally-owned buffer would introduce.
type MessageList[C command.KindProvider] struct {
	events      func() []domain.StoredEvent
	channel     domain.ChannelName
	kind        domain.ChannelKind
	viewport    viewport.Model
	placeholder string

	// seenLen records how many events each window held the last
	// time the user was at the bottom of it. Once the user
	// scrolls up and the window's event slice grows past that
	// mark, the divider latches on (see [showDivider]) and stays
	// on until the user receives further events while at the
	// bottom — the catch-up signal IRC clients use to acknowledge
	// "I've seen what arrived".
	seenLen map[domain.ChannelName]int

	// lastEventsLen records the events-slice length observed on
	// the previous render-affecting tick, per window. Comparing
	// this to the current length tells us whether new content
	// arrived *during this tick* — which is what the divider
	// latch and the catch-up clear key off, rather than the
	// running gap between seenLen and the current length.
	lastEventsLen map[domain.ChannelName]int

	// showDivider is true once new events have arrived while the
	// viewport was scrolled up. The divider is rendered between
	// `seenLen` and the next event until the user catches up.
	showDivider bool

	commands        []*command.Node[C]
	highlightWords  []string
	userNick        domain.Nick
	timestampFormat *string
	locale          language.Tag
}

// NewMessageList builds a message list that reads its events
// through the supplied closure. `channel` and `kind` seed the
// initial active window; subsequent [SetChannelMsg] updates them.
func NewMessageList[C command.KindProvider](
	events func() []domain.StoredEvent,
	channel domain.ChannelName,
	kind domain.ChannelKind,
) MessageList[C] {
	vp := viewport.New(0, 0)
	vp.MouseWheelEnabled = true

	return MessageList[C]{
		events:        events,
		channel:       channel,
		kind:          kind,
		viewport:      vp,
		seenLen:       map[domain.ChannelName]int{},
		lastEventsLen: map[domain.ChannelName]int{},
		locale:        timestamp.CurrentLocale(),
	}
}

// Len returns the current event count of the active window.
func (m MessageList[C]) Len() int {
	return len(m.events())
}

// SetKeyMap applies viewport key bindings from the ChatView key map.
func (m MessageList[C]) SetKeyMap(km ChatViewKeyMap) MessageList[C] {
	m.viewport.KeyMap = viewport.KeyMap{
		PageDown: km.PageDown.Binding,
		PageUp:   km.PageUp.Binding,
		Down:     km.ScrollDown.Binding,
		Up:       km.ScrollUp.Binding,
	}

	return m
}

// Init implements ui.Model.
func (m MessageList[C]) Init() tea.Cmd {
	return nil
}

// Update implements ui.Model.
func (m MessageList[C]) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case SetChannelMsg:
		// A channel switch reseeds the seen-mark against the new
		// window's current event count so a same-render-cycle
		// divider check does not fire on stale state.
		m.channel = msg.Channel
		m.kind = msg.Kind
		m.seenLen[msg.Channel] = len(m.events())
		m.lastEventsLen[msg.Channel] = len(m.events())
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

	case CommandsMsg[C]:
		m.commands = msg.Commands
		return m, nil

	case ui.BoundsMsg:
		m.viewport.Width = max(msg.Rect.Width, 0)
		m.viewport.Height = max(msg.Rect.Height, 0)
		m = m.syncContent()
		return m, nil

	case ScrollbackUpdatedMsg:
		if msg.Channel != m.channel {
			return m, nil
		}
		m = m.syncContent()
		return m, nil
	}

	m = m.syncContent()

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)

	return m, cmd
}

// syncContent re-evaluates the active window's events through the
// injected getter, re-renders the viewport, and updates the
// divider/seen-mark bookkeeping. The divider behaviour keys off a
// per-tick *growth* signal — content arriving on this tick — rather
// than the running gap between seen-len and the current length, so a
// bare scroll-to-bottom keystroke neither arms nor clears the
// divider; only fresh content arriving does.
func (m MessageList[C]) syncContent() MessageList[C] {
	wasAtBottom := m.viewport.AtBottom() || m.viewport.TotalLineCount() == 0
	events := m.events()
	grewThisTick := len(events) > m.lastEventsLen[m.channel]
	if grewThisTick {
		if wasAtBottom {
			m.seenLen[m.channel] = len(events)
			m.showDivider = false
		} else {
			m.showDivider = true
		}
	}
	m.lastEventsLen[m.channel] = len(events)
	m.viewport.SetContent(m.renderedContent(events, m.viewport.Width))
	if wasAtBottom {
		m.viewport.GotoBottom()
	}

	return m
}

// View implements ui.Model.
func (m MessageList[C]) View(width, height int) string {
	events := m.events()

	// First sight of the active channel seeds the seen-mark so a
	// freshly-loaded window does not light the divider on its
	// initial frame. Subsequent frames advance it only when the
	// user is at the bottom (handled in [Update] after viewport
	// scroll events).
	if _, tracked := m.seenLen[m.channel]; !tracked {
		m.seenLen[m.channel] = len(events)
	}

	messageView, scrolled, scrollPct := m.renderMessages(events, width, height)

	var scrollView string
	if scrolled {
		indicator := theme.Dim.Render(fmt.Sprintf("(%d%%)", int(scrollPct*100)))
		scrollView = lipgloss.PlaceHorizontal(width, lipgloss.Right, indicator)

		listHeight := max(height-1, 0)

		messageView, _, _ = m.renderMessages(events, width, listHeight)
	}

	parts := make([]string, 0, 2)

	if scrollView != "" {
		parts = append(parts, scrollView)
	}

	parts = append(parts, messageView)

	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// ScrollInfo returns whether the viewport is scrolled up and the
// current scroll percentage.
func (m MessageList[C]) ScrollInfo() (scrolled bool, pct float64) {
	return !m.viewport.AtBottom(), m.viewport.ScrollPercent()
}

func (m MessageList[C]) renderMessages(events []domain.StoredEvent, width, height int) (view string, scrolled bool, scrollPct float64) {
	if len(events) == 0 {
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
	wasAtBottom := vp.AtBottom() || vp.TotalLineCount() == 0
	content := m.renderedContent(events, width)
	vp.SetContent(content)
	if wasAtBottom {
		vp.GotoBottom()
	}

	rendered := vp.View()
	if lipgloss.Height(content) <= height {
		rendered = lipgloss.Place(width, height, lipgloss.Left, lipgloss.Bottom, content)
	}

	return rendered, !vp.AtBottom(), vp.ScrollPercent()
}

func (m MessageList[C]) renderedContent(events []domain.StoredEvent, width int) string {
	rendered := make([]string, 0, len(events)+1)
	seenLen := m.seenLen[m.channel]

	for i, ev := range events {
		if m.showDivider && i == seenLen {
			rendered = append(rendered, renderNewMessagesDivider(width))
		}

		if m.kind == domain.KindDM && isDMSuppressedEvent(ev.Event) {
			continue
		}

		rendered = append(rendered, renderChannelEvent(
			ev.Event,
			m.kind,
			width,
			m.highlightWords,
			m.userNick,
			m.commands,
			m.timestampFormat,
			m.locale,
		))
	}

	if m.showDivider && seenLen >= len(events) {
		rendered = append(rendered, renderNewMessagesDivider(width))
	}

	return strings.Join(rendered, "\n")
}

func isDMSuppressedEvent(event domain.PersistableEvent) bool {
	switch event.(type) {
	case domain.Join,
		domain.Part,
		domain.ModeChange,
		domain.TopicChange,
		domain.ModelInvited,
		domain.ModelKicked:
		return true
	}

	return false
}
