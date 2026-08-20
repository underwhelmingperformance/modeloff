package components

import (
	"fmt"
	"strings"
	"time"

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

// WindowContent is the window the message list renders: which window
// it is, and what that window holds. The chat-screen answers both in
// one read, so the list always knows whose events it has and can
// never charge one window's arrivals to another.
type WindowContent struct {
	Channel domain.ChannelName
	Events  []domain.Event
}

// MessageList displays channel events in a scrollable viewport with
// support for a new-messages divider and an empty-state placeholder.
// C is the grammar's completion-context type; it is carried so the
// `/help` renderer can walk the typed command tree supplied by
// [CommandsMsg].
//
// The message list does not own the event storage. The owning
// chat-screen (or test harness) passes a `content` closure that
// returns the window in view and its events; the message list reads
// through it on every `View`. A single source of truth removes the
// live-append-vs-snapshot race that an internally-owned buffer would
// introduce.
type MessageList[C command.KindProvider] struct {
	content func() WindowContent

	// channel is the window the last tick was about. The chat-screen
	// moves the window in view during its own Update, and the list
	// hears about it on the next message, so `content()` can already
	// be answering with another window when a render happens in
	// between. Rendering compares the two and works the divider out
	// afresh when they differ.
	channel domain.ChannelName

	kind        domain.ChannelKind
	viewport    viewport.Model
	placeholder string

	// seenLen records, per window, how far into that window's
	// events the reader has caught up. The divider is drawn there
	// when they come back to it, so the entry has to outlive the
	// switch away.
	//
	// Both inputs to the mark live here: the viewport decides
	// whether the reader is at the bottom, and the content getter
	// says how much the window holds. Keeping the mark itself in
	// the chat-screen would make the answer depend on the order in
	// which two models exchange messages, and a mark that arrived
	// late would silently fold a missed message into "already
	// read". [ScrollbackClearedMsg] is how the chat-screen drops an
	// entry when a window's history goes away.
	seenLen map[domain.ChannelName]int

	// lastEventsLen is the active window's event count at the
	// previous render-affecting tick. Growth between ticks is what
	// advances the mark, since the reader was at the bottom when
	// the content landed and so watched it arrive. A bare
	// scroll-to-bottom keystroke carries no growth and leaves the
	// divider where it is.
	lastEventsLen int

	// unseen is true when `channel` held events past its seen mark
	// as of the last tick, and is what the divider renders from
	// while that window is still the one in view. The content getter
	// is live, so a render can see an event that no tick has moved
	// the mark over yet; deciding at render time flashed a divider
	// for one frame on every message that arrived in the focused
	// window.
	unseen bool

	commands        []*command.Node[C]
	highlightWords  []string
	userNick        domain.Nick
	timestampFormat *string
	locale          language.Tag
}

// NewMessageList builds a message list that reads the window in view
// through the supplied closure. `kind` is the initial window's kind;
// subsequent [SetChannelMsg] updates it.
func NewMessageList[C command.KindProvider](
	content func() WindowContent,
	kind domain.ChannelKind,
) MessageList[C] {
	vp := viewport.New(0, 0)
	vp.MouseWheelEnabled = true

	initial := content()

	return MessageList[C]{
		content:       content,
		channel:       initial.Channel,
		kind:          kind,
		viewport:      vp,
		seenLen:       map[domain.ChannelName]int{},
		lastEventsLen: len(initial.Events),
		locale:        timestamp.CurrentLocale(),
	}
}

// mark returns the reader's place in `ch`, given that it holds `held`
// events. A window the list has not shown before takes its mark from
// what the window holds at that moment: the reader is arriving at it
// now, so the connection narration and anything else already there
// counts as read.
func (m MessageList[C]) mark(ch domain.ChannelName, held int) int {
	seen, tracked := m.seenLen[ch]
	if !tracked {
		return held
	}

	return seen
}

// Len returns the current event count of the window in view.
func (m MessageList[C]) Len() int {
	return len(m.content().Events)
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
		m.kind = msg.Kind
		m = m.syncContent()

		return m, nil

	case ScrollbackClearedMsg:
		delete(m.seenLen, msg.Channel)

		if msg.Channel == m.channel {
			m.lastEventsLen = 0
		}

		m = m.syncContent()

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
		if msg.Channel != m.content().Channel {
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

// syncContent re-reads the window in view, re-renders the viewport,
// and advances that window's seen mark when content arrived on this
// tick with the reader at the bottom to watch it. The growth signal
// is what makes a bare scroll-to-bottom keystroke leave the divider
// alone: only fresh content the reader was there for clears it.
//
// Growth is only meaningful while the window in view stays the same.
// When it has changed since the last tick, the counts belong to
// different windows, and the viewport is reset onto the new one.
func (m MessageList[C]) syncContent() MessageList[C] {
	content := m.content()
	held := len(content.Events)
	switched := content.Channel != m.channel

	if switched {
		m.viewport.SetContent("")
		m.viewport.GotoBottom()
	}

	wasAtBottom := m.viewport.AtBottom() || m.viewport.TotalLineCount() == 0
	seen := m.mark(content.Channel, held)

	if !switched && held > m.lastEventsLen && wasAtBottom {
		seen = held
	}

	m.channel = content.Channel
	m.seenLen[content.Channel] = seen
	m.lastEventsLen = held
	m.unseen = seen < held
	m.viewport.SetContent(m.renderedContent(content, m.viewport.Width))

	if wasAtBottom {
		m.viewport.GotoBottom()
	}

	return m
}

// View implements ui.Model.
func (m MessageList[C]) View(width, height int) string {
	content := m.content()

	messageView, scrolled, scrollPct := m.renderMessages(content, width, height)

	var scrollView string
	if scrolled {
		indicator := theme.Dim.Render(fmt.Sprintf("(%d%%)", int(scrollPct*100)))
		scrollView = lipgloss.PlaceHorizontal(width, lipgloss.Right, indicator)

		listHeight := max(height-1, 0)

		messageView, _, _ = m.renderMessages(content, width, listHeight)
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

func (m MessageList[C]) renderMessages(content WindowContent, width, height int) (view string, scrolled bool, scrollPct float64) {
	if len(content.Events) == 0 {
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
	rendered := m.renderedContent(content, width)
	vp.SetContent(rendered)
	if wasAtBottom {
		vp.GotoBottom()
	}

	view = vp.View()
	if lipgloss.Height(rendered) <= height {
		view = lipgloss.Place(width, height, lipgloss.Left, lipgloss.Bottom, rendered)
	}

	return view, !vp.AtBottom(), vp.ScrollPercent()
}

// renderedContent renders the window's events, with the new-messages
// divider at the seen mark whenever the window holds events past it.
//
// A window switch reaches the list one message after the content
// getter starts answering with the new window, so a render can fall
// between the two. The mark for that window is what the reader last
// saw in it, and nothing has arrived in it since the switch was
// decided, so the divider is worked out from the mark directly.
func (m MessageList[C]) renderedContent(content WindowContent, width int) string {
	events := content.Events
	rendered := make([]string, 0, len(events)+1)
	seen := m.mark(content.Channel, len(events))

	unseen := m.unseen
	if content.Channel != m.channel {
		unseen = seen < len(events)
	}

	var lastDay time.Time

	for i, ev := range events {
		if unseen && i == seen {
			rendered = append(rendered, renderNewMessagesDivider(width))
		}

		if m.kind == domain.KindDM && isDMSuppressedEvent(ev) {
			continue
		}

		// A day-change divider marks a date rollover between two
		// consecutive rendered events, irssi's convention for keeping
		// the date visible without repeating it on every line now
		// that the default timestamp shows only the time of day.
		// Nothing is drawn before the window's first rendered event:
		// there is no prior day to have rolled over from. An event
		// with no timestamp (the zero time) carries no real calendar
		// day, so it neither triggers a divider nor becomes lastDay.
		if pe, ok := ev.(domain.PersistableEvent); ok {
			if at := domain.EventTime(pe); !at.IsZero() {
				day := calendarDay(at)

				if !lastDay.IsZero() && !day.Equal(lastDay) {
					rendered = append(rendered, renderDayChangedDivider(width, at, m.locale))
				}

				lastDay = day
			}
		}

		rendered = append(rendered, renderChannelEvent(
			ev,
			m.kind,
			width,
			m.highlightWords,
			m.userNick,
			m.commands,
			m.timestampFormat,
			m.locale,
		))
	}

	return strings.Join(rendered, "\n")
}

// calendarDay truncates t to midnight in its own location, so two
// timestamps compare equal exactly when they fall on the same
// calendar day.
func calendarDay(t time.Time) time.Time {
	y, mo, d := t.Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, t.Location())
}

func isDMSuppressedEvent(event domain.Event) bool {
	switch event.(type) {
	case domain.Join,
		domain.Part,
		domain.ChannelModeChange,
		domain.TopicChange,
		domain.Invited,
		domain.Kicked:
		return true
	}

	return false
}
