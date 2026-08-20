package components

import (
	"fmt"
	"slices"
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
//
// `FirstSeq` is the sequence number [Scrollback] gave `Events[0]`, and
// the rest follow on from it. The list keys the reader's mark and its
// render cache on those numbers, which requires the producer to hold
// to what a scrollback does: for one window, events are added at the
// end and dropped from the front, and `FirstSeq` moves up by however
// many were dropped. A producer that replaces the contents outright
// must renumber them from an unused number, as [Scrollback.Prepend]
// and [Scrollback.Clear] do.
type WindowContent struct {
	Channel  domain.ChannelName
	Events   []domain.Event
	FirstSeq int64
}

// nextSeq returns the sequence number the window's next event will
// take, which is one past the last event it holds.
func (c WindowContent) nextSeq() int64 {
	return c.FirstSeq + int64(len(c.Events))
}

// lineKey names everything one rendered event line depends on beyond
// the event itself. `config` is a generation number the list bumps
// whenever a message changes the highlight words, the user's nick, the
// timestamp format, the locale or the command tree, so a cached line
// outlives only the settings it was rendered under.
type lineKey struct {
	width  int
	kind   domain.ChannelKind
	config int
}

// blockKey names the joined block of lines the viewport is given: the
// per-line inputs, which window the events came from, which events
// those are, and where the new-messages divider sits among them.
type blockKey struct {
	line     lineKey
	channel  domain.ChannelName
	firstSeq int64
	count    int
	divider  int
}

// listCache holds the rendered form of the window in view. Rendering
// an event means building a lipgloss style, parsing IRC formatting,
// testing for highlight words and wrapping the result to the width,
// and the joined block then costs the viewport an ANSI width
// measurement of every line in it. Both are proportional to the whole
// window, and the chat view forwards every message it receives to the
// list, so without a cache a keystroke and the once-a-second metrics
// tick each pay for the window entire.
//
// The list holds this by pointer because `View` renders on a copy of
// the model and Bubble Tea keeps no result from it.
type listCache struct {
	key     lineKey
	channel domain.ChannelName

	// base is the sequence number `lines[0]` was rendered for.
	base  int64
	lines []string

	block    string
	blockKey blockKey
	hasBlock bool

	// blockRows is how many rows `block` occupies. Counting them on
	// each render means scanning the whole block, which would be the
	// one per-render cost still growing with the window's history.
	blockRows int
}

// reset drops every cached line and starts again at `base`.
func (c *listCache) reset(channel domain.ChannelName, key lineKey, base int64) {
	c.key = key
	c.channel = channel
	c.base = base
	c.lines = c.lines[:0]
	c.block = ""
	c.blockRows = 0
	c.hasBlock = false
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

	// seen records, per window, the sequence number of the first line
	// the reader has not caught up with. The divider is drawn there
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
	seen map[domain.ChannelName]int64

	// lastNextSeq is where the active window's numbering had reached
	// at the previous render-affecting tick. Growth between ticks is
	// what advances the mark, since the reader was at the bottom when
	// the content landed and so watched it arrive. A bare
	// scroll-to-bottom keystroke carries no growth and leaves the
	// divider where it is.
	lastNextSeq int64

	// unseen is true when `channel` held events past its seen mark
	// as of the last tick, and is what the divider renders from
	// while that window is still the one in view. The content getter
	// is live, so a render can see an event that no tick has moved
	// the mark over yet; deciding at render time flashed a divider
	// for one frame on every message that arrived in the focused
	// window.
	unseen bool

	cache *listCache

	// vpKey names the block `viewport` was last given, and vpHasKey
	// says whether it was given one at all. Handing the viewport a
	// block it already holds costs an ANSI width measurement of every
	// line in it, so the list hands one over only when it differs.
	// The model holds both, because `View` renders through a copy of
	// the viewport and each copy holds whatever the model it came
	// from was given.
	vpKey    blockKey
	vpHasKey bool

	commands        []*command.Node[C]
	highlightWords  []string
	userNick        domain.Nick
	timestampFormat *string
	locale          language.Tag

	// configGen counts the changes to the render settings above. It
	// is what [lineKey] carries, so a cached line is dropped when one
	// of them moves and kept when nothing did. Each of the three
	// settings messages compares its payload before counting a change:
	// the chat-screen re-sends the highlight words on every peer nick
	// change, and counting that as a change would throw away every
	// line the window had rendered.
	configGen int
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
		content:     content,
		channel:     initial.Channel,
		kind:        kind,
		viewport:    vp,
		seen:        map[domain.ChannelName]int64{},
		lastNextSeq: initial.nextSeq(),
		cache:       &listCache{},
		locale:      timestamp.CurrentLocale(),
	}
}

// mark returns the reader's place in the window `content` describes. A
// window the list has not shown before takes its mark from what the
// window holds at that moment: the reader is arriving at it now, so
// the connection narration and anything else already there counts as
// read.
func (m MessageList[C]) mark(content WindowContent) int64 {
	seen, tracked := m.seen[content.Channel]
	if !tracked {
		return content.nextSeq()
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
		delete(m.seen, msg.Channel)

		m = m.syncContent()

		return m, nil

	case SetPlaceholderMsg:
		m.placeholder = msg.Text
		return m, nil

	case HighlightWordsMsg:
		if slices.Equal(m.highlightWords, msg.Words) && m.userNick == msg.UserNick {
			return m, nil
		}

		m.highlightWords = msg.Words
		m.userNick = msg.UserNick
		m.configGen++

		return m, nil

	case TimestampFormatMsg:
		if ptr.EqualString(m.timestampFormat, msg.Format) && m.locale == msg.Locale {
			return m, nil
		}

		m.timestampFormat = ptr.CloneString(msg.Format)
		m.locale = msg.Locale
		m.configGen++

		return m, nil

	case CommandsMsg[C]:
		if slices.Equal(m.commands, msg.Commands) {
			return m, nil
		}

		m.commands = msg.Commands
		m.configGen++

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
// When it has changed since the last tick, the numbering belongs to
// different windows, and the viewport is reset onto the new one.
func (m MessageList[C]) syncContent() MessageList[C] {
	content := m.content()
	next := content.nextSeq()
	switched := content.Channel != m.channel

	if switched {
		m.viewport.SetContent("")
		m.viewport.GotoBottom()
		m.vpHasKey = false
	}

	wasAtBottom := m.viewport.AtBottom() || m.viewport.TotalLineCount() == 0
	seen := m.mark(content)

	if !switched && next > m.lastNextSeq && wasAtBottom {
		seen = next
	}

	m.channel = content.Channel
	m.seen[content.Channel] = seen
	m.lastNextSeq = next
	m.unseen = seen < next

	block, _, key := m.renderedContent(content, m.viewport.Width)

	if !m.vpHasKey || m.vpKey != key {
		m.viewport.SetContent(block)
		m.vpKey = key
		m.vpHasKey = true
	}

	if wasAtBottom {
		m.viewport.GotoBottom()
	}

	return m
}

// View implements ui.Model.
func (m MessageList[C]) View(width, height int) string {
	content := m.content()

	messageView, scrolled, scrollPct := m.renderMessages(content, width, height)
	if !scrolled {
		return messageView
	}

	indicator := theme.Dim.Render(fmt.Sprintf("(%d%%)", int(scrollPct*100)))
	scrollView := lipgloss.PlaceHorizontal(width, lipgloss.Right, indicator)

	// The indicator takes a row from the list, so the transcript is
	// laid out again one row shorter. The block itself is unchanged,
	// so this second pass reads it back from the cache and leaves the
	// viewport's own copy of it alone.
	messageView, _, _ = m.renderMessages(content, width, max(height-1, 0))

	return lipgloss.JoinVertical(lipgloss.Left, scrollView, messageView)
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

	block, rows, key := m.renderedContent(content, width)

	vp := m.viewport
	vp.Width = width
	vp.Height = height
	wasAtBottom := vp.AtBottom() || vp.TotalLineCount() == 0

	if !m.vpHasKey || m.vpKey != key {
		vp.SetContent(block)
	}

	if wasAtBottom {
		vp.GotoBottom()
	}

	view = vp.View()
	if rows <= height {
		view = lipgloss.Place(width, height, lipgloss.Left, lipgloss.Bottom, block)
	}

	return view, !vp.AtBottom(), vp.ScrollPercent()
}

// dividerIndex returns the position in the window's events that the
// new-messages divider is drawn at, or -1 when the window holds
// nothing past the reader's mark.
//
// A window switch reaches the list one message after the content
// getter starts answering with the new window, so a render can fall
// between the two. The mark for that window is what the reader last
// saw in it, and nothing has arrived in it since the switch was
// decided, so the divider is worked out from the mark directly.
//
// The mark can name an event the window no longer holds. It sits
// before the oldest one when the events it pointed at have been
// dropped, in which case everything left is unread and the divider
// goes at the top; it sits at or past the newest one when the window
// was cleared under a mark this tick has not caught up with, in which
// case there is nothing to divide.
func (m MessageList[C]) dividerIndex(content WindowContent) int {
	seen := m.mark(content)

	unseen := m.unseen
	if content.Channel != m.channel {
		unseen = seen < content.nextSeq()
	}

	if !unseen {
		return -1
	}

	at := int(seen - content.FirstSeq)

	if at < 0 {
		return 0
	}

	if at >= len(content.Events) {
		return -1
	}

	return at
}

// renderedContent returns the window's rendered events as one block,
// with the new-messages divider at the seen mark whenever the window
// holds events past it, along with the key naming what went into it.
//
// The day-change dividers are worked out while the block is put
// together: whether one is drawn depends on the two events either
// side of it, while a cached line depends on its own event alone. The
// block's key names both the events in the block and, in its render
// settings, the locale a divider's date is written under, so
// rebuilding the block draws the same dividers again.
func (m MessageList[C]) renderedContent(content WindowContent, width int) (block string, rows int, key blockKey) {
	key = blockKey{
		line:     lineKey{width: width, kind: m.kind, config: m.configGen},
		channel:  content.Channel,
		firstSeq: content.FirstSeq,
		count:    len(content.Events),
		divider:  m.dividerIndex(content),
	}

	cache := m.cache
	if cache.hasBlock && cache.blockKey == key {
		return cache.block, cache.blockRows, key
	}

	m.refreshLines(content, key.line)

	rendered := make([]string, 0, len(content.Events)+1)

	var lastDay time.Time

	for i, event := range content.Events {
		if i == key.divider {
			rendered = append(rendered, renderNewMessagesDivider(width))
		}

		if m.kind == domain.KindDM && isDMSuppressedEvent(event) {
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
		if pe, ok := event.(domain.PersistableEvent); ok {
			if at := domain.EventTime(pe); !at.IsZero() {
				day := calendarDay(at)

				if !lastDay.IsZero() && !day.Equal(lastDay) {
					rendered = append(rendered, renderDayChangedDivider(width, at, m.locale))
				}

				lastDay = day
			}
		}

		rendered = append(rendered, cache.lines[i])
	}

	cache.block = strings.Join(rendered, "\n")
	cache.blockRows = lipgloss.Height(cache.block)
	cache.blockKey = key
	cache.hasBlock = true

	return cache.block, cache.blockRows, key
}

// refreshLines brings the cached per-event lines up to date with the
// window in view, rendering only the events it has no line for.
//
// The cache is dropped outright when the window changed or one of the
// render settings moved. Otherwise the events it holds are matched by
// sequence number: those dropped from the front of the window are
// dropped from the front of the cache, and any event past the end of
// the cache is rendered and appended. A window holding fewer events
// than the cache has lines for is one whose contents were replaced
// without renumbering, which is not something a [Scrollback] does, so
// the cache is dropped there too.
func (m MessageList[C]) refreshLines(content WindowContent, key lineKey) {
	cache := m.cache

	if cache.key != key || cache.channel != content.Channel || content.FirstSeq < cache.base {
		cache.reset(content.Channel, key, content.FirstSeq)
	}

	if dropped := content.FirstSeq - cache.base; dropped > 0 {
		if dropped >= int64(len(cache.lines)) {
			cache.lines = cache.lines[:0]
		} else {
			cache.lines = append(cache.lines[:0], cache.lines[dropped:]...)
		}

		cache.base = content.FirstSeq
	}

	if len(content.Events) < len(cache.lines) {
		cache.lines = cache.lines[:0]
	}

	for i := len(cache.lines); i < len(content.Events); i++ {
		event := content.Events[i]

		if m.kind == domain.KindDM && isDMSuppressedEvent(event) {
			cache.lines = append(cache.lines, "")
			continue
		}

		cache.lines = append(cache.lines, renderChannelEvent(
			event,
			m.kind,
			key.width,
			m.highlightWords,
			m.userNick,
			m.commands,
			m.timestampFormat,
			m.locale,
		))
	}
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
