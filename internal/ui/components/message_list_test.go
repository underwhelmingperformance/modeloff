package components

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/language"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/uitest"
)

// listKind is the completion-context parameter the message-list tests
// build their lists with.
type listKind domain.ChannelKind

func (k listKind) ChannelKind() domain.ChannelKind { return domain.ChannelKind(k) }

const (
	listWidth  = 60
	listHeight = 6
)

var listTimestamp = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

// listMessage builds a message that renders to one line at
// `listWidth`, naming itself so an assertion can say which events it
// expects to see.
func listMessage(channel domain.ChannelName, i int) domain.Event {
	return domain.Message{
		Target:     channel,
		From:       "alice",
		InstanceID: "inst",
		Body:       fmt.Sprintf("message %d", i),
		At:         listTimestamp,
	}
}

// listLine is what listMessage renders to once the list is given a
// timestamp format.
func listLine(i int) string {
	return fmt.Sprintf("[09:00:00] <alice> message %d", i)
}

// listTimestampFormat is the format the message-list tests render
// under, so the lines they assert on are deterministic.
const listTimestampFormat = "[15:04:05]"

// newTestMessageList builds a sized message list reading through
// `content`, with a fixed timestamp format so the rendered lines are
// deterministic.
func newTestMessageList(content func() WindowContent) ui.Model {
	format := listTimestampFormat

	var m ui.Model = NewMessageList[listKind](content, domain.KindChannel)
	m, _ = m.Update(TimestampFormatMsg{Format: &format, Locale: language.BritishEnglish})
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: listWidth, Height: listHeight}})

	return m
}

// TestMessageList_divider_follows_the_sequence_numbers pins the
// arithmetic that puts the new-messages divider in the right place
// once a window has started dropping its oldest events. The mark
// names an event by its sequence number, so the position it renders
// at moves with every event dropped from the front, and a mark whose
// events have gone means everything left is unread.
func TestMessageList_divider_follows_the_sequence_numbers(t *testing.T) {
	const channel = domain.ChannelName("#general")

	tests := map[string]struct {
		seen     int64
		firstSeq int64
		count    int
		unseen   bool
		want     int
	}{
		"nothing past the mark": {
			seen: 10, firstSeq: 0, count: 10, unseen: false, want: -1,
		},
		"the mark names an event the window still holds": {
			seen: 6, firstSeq: 0, count: 10, unseen: true, want: 6,
		},
		"a dropped event moves the mark's position down": {
			seen: 6, firstSeq: 4, count: 10, unseen: true, want: 2,
		},
		"the events the mark named have been dropped": {
			seen: 6, firstSeq: 9, count: 10, unseen: true, want: 0,
		},
		"the mark is past everything the window holds": {
			seen: 30, firstSeq: 9, count: 10, unseen: true, want: -1,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			content := WindowContent{
				Channel:  channel,
				Events:   make([]domain.Event, tc.count),
				FirstSeq: tc.firstSeq,
			}

			m := NewMessageList[listKind](
				func() WindowContent { return content },
				domain.KindChannel,
			)
			m.seen[channel] = tc.seen
			m.unseen = tc.unseen

			require.Equal(t, tc.want, m.dividerIndex(content))
		})
	}
}

// TestMessageList_renders_the_newest_events_of_a_bounded_window walks
// a window past [ScrollbackLimit] and checks the list still shows the
// end of it. The cached lines are matched to events by sequence
// number, so dropping the oldest has to drop the lines rendered for
// them and leave the rest where they are.
func TestMessageList_renders_the_newest_events_of_a_bounded_window(t *testing.T) {
	const channel = domain.ChannelName("#general")

	scrollback := NewScrollback()
	content := func() WindowContent {
		return WindowContent{
			Channel:  channel,
			Events:   scrollback.Events(),
			FirstSeq: scrollback.FirstSeq(),
		}
	}

	m := newTestMessageList(content)

	// A first batch that fits, rendered and cached, then enough more
	// to push the window past its bound.
	for i := range 20 {
		scrollback.Append(listMessage(channel, i))
	}

	m, _ = m.Update(ScrollbackUpdatedMsg{Channel: channel})

	for i := 20; i < ScrollbackLimit+5; i++ {
		scrollback.Append(listMessage(channel, i))
	}

	m, _ = m.Update(ScrollbackUpdatedMsg{Channel: channel})

	last := ScrollbackLimit + 4
	want := make([]string, 0, listHeight)

	for i := last - listHeight + 1; i <= last; i++ {
		want = append(want, listLine(i))
	}

	require.Equal(t, want, uitest.NonEmptyLines(m.View(listWidth, listHeight)))
}

// cacheShape describes the cached lines a message list is holding:
// how many, what is at each end of them, and how far into the window
// the first one sits. A cache too large to write out line by line is
// still wrong in a way this shows, because a line kept for the wrong
// event turns up at one end or the other.
type cacheShape struct {
	Held   int
	Oldest string
	Newest string
	Base   int64
}

func cacheShapeOf(t *testing.T, m ui.Model, base int64) cacheShape {
	t.Helper()

	list, ok := m.(MessageList[listKind])
	require.True(t, ok, "expected MessageList, got %T", m)

	lines := list.cache.lines
	visible := func(line string) string {
		return strings.TrimRight(uitest.StripANSI(line), " ")
	}

	return cacheShape{
		Held:   len(lines),
		Oldest: visible(lines[0]),
		Newest: visible(lines[len(lines)-1]),
		Base:   list.cache.base - base,
	}
}

// TestMessageList_cached_lines_follow_the_events_they_were_rendered_for
// is the direct guard on the cache's bookkeeping. A line is kept for
// the event whose sequence number it was rendered for, so dropping the
// oldest events has to drop their lines with them. Keeping the lines
// and matching them to events by position would leave every cached
// line answering for the event that many places after the one it was
// rendered from.
func TestMessageList_cached_lines_follow_the_events_they_were_rendered_for(t *testing.T) {
	const channel = domain.ChannelName("#general")

	scrollback := NewScrollback()
	base := scrollback.FirstSeq()

	content := func() WindowContent {
		return WindowContent{
			Channel:  channel,
			Events:   scrollback.Events(),
			FirstSeq: scrollback.FirstSeq(),
		}
	}

	m := newTestMessageList(content)

	for i := range 20 {
		scrollback.Append(listMessage(channel, i))
	}

	m, _ = m.Update(ScrollbackUpdatedMsg{Channel: channel})

	require.Equal(t, cacheShape{
		Held:   20,
		Oldest: listLine(0),
		Newest: listLine(19),
		Base:   0,
	}, cacheShapeOf(t, m, base))

	const last = ScrollbackLimit + 4

	for i := 20; i <= last; i++ {
		scrollback.Append(listMessage(channel, i))
	}

	m, _ = m.Update(ScrollbackUpdatedMsg{Channel: channel})

	require.Equal(t, cacheShape{
		Held:   ScrollbackLimit,
		Oldest: listLine(last - ScrollbackLimit + 1),
		Newest: listLine(last),
		Base:   last - ScrollbackLimit + 1,
	}, cacheShapeOf(t, m, base))
}

// TestMessageList_rerenders_when_the_timestamp_format_changes pins
// cache invalidation against the settings a line is rendered under.
// The rendered lines are cached between renders, so a setting that
// changes what a line looks like has to drop them.
func TestMessageList_rerenders_when_the_timestamp_format_changes(t *testing.T) {
	const channel = domain.ChannelName("#general")

	events := []domain.Event{listMessage(channel, 0)}
	content := func() WindowContent {
		return WindowContent{Channel: channel, Events: events}
	}

	m := newTestMessageList(content)

	require.Equal(t, []string{listLine(0)},
		uitest.NonEmptyLines(m.View(listWidth, listHeight)))

	dayFormat := "[2006-01-02]"
	m, _ = m.Update(TimestampFormatMsg{Format: &dayFormat, Locale: language.BritishEnglish})

	require.Equal(t, []string{"[2026-01-01] <alice> message 0"},
		uitest.NonEmptyLines(m.View(listWidth, listHeight)))
}

// TestMessageList_rerenders_at_a_new_width pins the other input every
// cached line depends on. A line is wrapped to the width it was
// rendered at, so a resize cannot reuse it.
func TestMessageList_rerenders_at_a_new_width(t *testing.T) {
	const channel = domain.ChannelName("#general")

	events := []domain.Event{listMessage(channel, 0)}
	content := func() WindowContent {
		return WindowContent{Channel: channel, Events: events}
	}

	m := newTestMessageList(content)

	require.Equal(t, []string{listLine(0)},
		uitest.NonEmptyLines(m.View(listWidth, listHeight)))

	narrow := 20
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: narrow, Height: listHeight}})

	require.Equal(t, []string{"[09:00:00] <alice>", "message 0"},
		uitest.NonEmptyLines(m.View(narrow, listHeight)))
}

// TestMessageList_keeps_one_windows_lines_out_of_another pins the
// cache against a window switch. The lines are cached for the window
// they were rendered from, and two windows number their events from
// zero apiece.
func TestMessageList_keeps_one_windows_lines_out_of_another(t *testing.T) {
	windows := map[domain.ChannelName][]domain.Event{
		"#general": {listMessage("#general", 1)},
		"#random":  {listMessage("#random", 2)},
	}

	active := domain.ChannelName("#general")
	content := func() WindowContent {
		return WindowContent{Channel: active, Events: windows[active]}
	}

	m := newTestMessageList(content)

	require.Equal(t, []string{listLine(1)},
		uitest.NonEmptyLines(m.View(listWidth, listHeight)))

	active = "#random"
	m, _ = m.Update(SetChannelMsg{Channel: active, Kind: domain.KindChannel})

	require.Equal(t, []string{listLine(2)},
		uitest.NonEmptyLines(m.View(listWidth, listHeight)))
}

// TestMessageList_counts_a_settings_change_only_when_one_happened
// pins what keeps the render cache alive through the traffic that
// carries no change. Every cached line is dropped when a render
// setting moves, and the chat-screen re-sends the highlight words on
// every peer nick change with the same list each time; counting that
// as a change would re-render the whole window for it.
func TestMessageList_counts_a_settings_change_only_when_one_happened(t *testing.T) {
	const channel = domain.ChannelName("#general")

	format := listTimestampFormat
	otherFormat := "[2006-01-02]"

	words := []string{"art"}
	commands := []*command.Node[listKind]{{Name: "join"}}
	otherCommands := []*command.Node[listKind]{{Name: "part"}}

	tests := map[string]struct {
		msg  tea.Msg
		want int
	}{
		"the same highlight words again": {
			msg:  HighlightWordsMsg{Words: []string{"art"}, UserNick: "laney"},
			want: 0,
		},
		"a different highlight word": {
			msg:  HighlightWordsMsg{Words: []string{"news"}, UserNick: "laney"},
			want: 1,
		},
		"a different user nick": {
			msg:  HighlightWordsMsg{Words: []string{"art"}, UserNick: "someone"},
			want: 1,
		},
		"the same timestamp format again": {
			msg:  TimestampFormatMsg{Format: &format, Locale: language.BritishEnglish},
			want: 0,
		},
		"a different timestamp format": {
			msg:  TimestampFormatMsg{Format: &otherFormat, Locale: language.BritishEnglish},
			want: 1,
		},
		"a different locale": {
			msg:  TimestampFormatMsg{Format: &format, Locale: language.French},
			want: 1,
		},
		"the same command tree again": {
			msg:  CommandsMsg[listKind]{Commands: commands},
			want: 0,
		},
		"a different command tree": {
			msg:  CommandsMsg[listKind]{Commands: otherCommands},
			want: 1,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			content := func() WindowContent {
				return WindowContent{Channel: channel, Events: []domain.Event{listMessage(channel, 0)}}
			}

			m := newTestMessageList(content)
			m, _ = m.Update(HighlightWordsMsg{Words: words, UserNick: "laney"})
			m, _ = m.Update(CommandsMsg[listKind]{Commands: commands})

			before := m.(MessageList[listKind]).configGen

			m, _ = m.Update(tc.msg)

			require.Equal(t, tc.want, m.(MessageList[listKind]).configGen-before)
		})
	}
}

// TestMessageList_draws_a_day_divider_when_the_date_rolls_over is the
// guard on the one part of a block that is not a property of a single
// event. Whether a day-change divider is drawn depends on the two
// events either side of it, so it is worked out while the block is
// assembled and not kept with the cached lines; a message arriving on
// a new day has to bring one with it.
func TestMessageList_draws_a_day_divider_when_the_date_rolls_over(t *testing.T) {
	const channel = domain.ChannelName("#general")

	first := listMessage(channel, 0)
	events := []domain.Event{first}

	content := func() WindowContent {
		return WindowContent{Channel: channel, Events: events}
	}

	m := newTestMessageList(content)

	require.Equal(t, []string{listLine(0)},
		uitest.NonEmptyLines(m.View(listWidth, listHeight)))

	nextDay := first.(domain.Message)
	nextDay.At = listTimestamp.AddDate(0, 0, 1)
	nextDay.Body = "message 1"
	events = append(events, nextDay)

	m, _ = m.Update(ScrollbackUpdatedMsg{Channel: channel})

	divider := uitest.StripANSI(renderDayChangedDivider(listWidth, nextDay.At, language.BritishEnglish))

	require.Equal(t, []string{listLine(0), strings.TrimRight(divider, " "), listLine(1)},
		uitest.NonEmptyLines(m.View(listWidth, listHeight)))
}

// TestMessageList_renders_the_same_view_for_an_unrelated_message is
// the regression guard for the render cache itself. A keystroke and
// the once-a-second metrics tick both reach the list and change
// nothing about the window, and what they render has to be what the
// previous render produced.
func TestMessageList_renders_the_same_view_for_an_unrelated_message(t *testing.T) {
	const channel = domain.ChannelName("#general")

	events := make([]domain.Event, 0, 30)
	for i := range 30 {
		events = append(events, listMessage(channel, i))
	}

	content := func() WindowContent {
		return WindowContent{Channel: channel, Events: events}
	}

	m := newTestMessageList(content)
	before := m.View(listWidth, listHeight)

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})

	require.Equal(t, before, m.View(listWidth, listHeight))
}
