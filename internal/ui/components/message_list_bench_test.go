package components_test

import (
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

// benchKind is the message list's completion-context parameter for
// the benchmarks. It is separate from the one the behaviour tests use
// so the two files can change independently.
type benchKind domain.ChannelKind

func (k benchKind) ChannelKind() domain.ChannelKind { return domain.ChannelKind(k) }

// benchScrollbackSize is a busy session's scrollback: a channel the
// user has been sitting in all day.
const benchScrollbackSize = 2000

// benchWidth and benchHeight are a full-screen terminal, which is what
// a message list costs the most at.
const (
	benchWidth  = 200
	benchHeight = 50
)

func benchEvents(n int) []domain.Event {
	base := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	nicks := []domain.Nick{"alice", "bob", "carol", "dave"}
	events := make([]domain.Event, n)

	for i := range events {
		events[i] = domain.Message{
			Target:     "#general",
			From:       nicks[i%len(nicks)],
			InstanceID: domain.InstanceID(fmt.Sprintf("inst-%d", i%len(nicks))),
			Body:       fmt.Sprintf("message %d: a realistic line of chat with a handful of words in it.", i),
			At:         base.Add(time.Duration(i) * time.Second),
		}
	}

	return events
}

// benchMessageList builds a message list holding `events`, sized to a
// full-screen terminal and settled at the bottom of its scrollback.
func benchMessageList(events []domain.Event) ui.Model {
	content := func() components.WindowContent {
		return components.WindowContent{Channel: "#general", Events: events}
	}

	var m ui.Model = components.NewMessageList[benchKind](content, domain.KindChannel)
	m, _ = m.Update(components.HighlightWordsMsg{Words: []string{"$nick"}, UserNick: "laney"})
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: benchWidth, Height: benchHeight}})

	return m
}

// BenchmarkMessageListKeystroke measures what one keystroke costs the
// message list at a busy channel's scrollback: the Update the
// chat-view forwards plus the View that follows it. The list matches
// no arm for an ordinary key, so this is the cost every unrelated
// message on the bus pays too.
func BenchmarkMessageListKeystroke(b *testing.B) {
	m := benchMessageList(benchEvents(benchScrollbackSize))
	key := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		m, _ = m.Update(key)
		_ = m.View(benchWidth, benchHeight)
	}
}

// benchLayout builds the layout the chat screen runs inside: a sidebar
// that sizes itself to the channels it holds, and the message list as
// the content area beside it.
func benchLayout(events []domain.Event) ui.Model {
	content := func() components.WindowContent {
		return components.WindowContent{Channel: "#general", Events: events}
	}

	list := components.NewMessageList[benchKind](content, domain.KindChannel)

	var m ui.Model = components.NewMainLayout(components.NewChannelSidebar(), list)
	m, _ = m.Update(components.HighlightWordsMsg{Words: []string{"$nick"}, UserNick: "laney"})
	m, _ = m.Update(tea.WindowSizeMsg{Width: benchWidth, Height: benchHeight})

	return m
}

// BenchmarkMessageListKeystrokeAfterSidebarGrowth measures a keystroke
// once a channel with a long name has joined. The sidebar sizes itself
// to what it holds, so the content area narrows with no terminal
// resize. A content model left holding the width the last resize gave
// it is asked to render at one width and told another, and a cache
// keyed on the width a line was rendered at misses on every frame.
func BenchmarkMessageListKeystrokeAfterSidebarGrowth(b *testing.B) {
	m := benchLayout(benchEvents(benchScrollbackSize))

	m, _ = m.Update(components.SetChannelsMsg{
		Channels: []domain.Window{
			domain.NewChannelWindow("#a-channel-with-a-very-long-name", time.Time{}),
		},
	})

	key := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		m, _ = m.Update(key)
		_ = m.View(benchWidth, benchHeight)
	}
}

// BenchmarkMessageListKeystrokeScrolledUp measures the same keystroke
// with the reader scrolled off the bottom, which is when the view
// carries a scroll indicator.
func BenchmarkMessageListKeystrokeScrolledUp(b *testing.B) {
	m := benchMessageList(benchEvents(benchScrollbackSize))
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})

	key := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		m, _ = m.Update(key)
		_ = m.View(benchWidth, benchHeight)
	}
}

// BenchmarkMessageListMessageArrival measures the cost of one message
// landing in the window in view: the scrollback grows by a line and
// the chat-screen nudges the list to re-read it. Each iteration starts
// from the same scrollback length, so the measured work does not drift
// upwards as the loop runs.
func BenchmarkMessageListMessageArrival(b *testing.B) {
	base := benchEvents(benchScrollbackSize)
	arrival := domain.Message{
		Target: "#general", From: "alice", InstanceID: "inst-0",
		Body: "and one more line", At: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	}

	b.ReportAllocs()

	for range b.N {
		b.StopTimer()

		events := make([]domain.Event, benchScrollbackSize, benchScrollbackSize+1)
		copy(events, base)

		held := &events
		content := func() components.WindowContent {
			return components.WindowContent{Channel: "#general", Events: *held}
		}

		var m ui.Model = components.NewMessageList[benchKind](content, domain.KindChannel)
		m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: benchWidth, Height: benchHeight}})
		_ = m.View(benchWidth, benchHeight)

		b.StartTimer()

		*held = append(*held, arrival)
		m, _ = m.Update(components.ScrollbackUpdatedMsg{Channel: "#general"})
		_ = m.View(benchWidth, benchHeight)
	}
}
