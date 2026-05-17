package components_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/language"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

// testKind is a minimal KindProvider for component tests.
type testKind domain.ChannelKind

func (k testKind) ChannelKind() domain.ChannelKind { return domain.ChannelKind(k) }

const testKindChannel = testKind(domain.KindChannel)

var testEvents = []domain.StoredEvent{
	{Event: domain.Message{Target: "#general", From: "alice", Body: "hello", At: time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)}},
	{Event: domain.Message{Target: "#general", From: "bob", Body: "hi there", At: time.Date(2025, 1, 1, 10, 1, 0, 0, time.UTC)}},
	{Event: domain.Message{Target: "#general", From: "alice", Body: "how are you?", At: time.Date(2025, 1, 1, 10, 2, 0, 0, time.UTC)}},
}

// nilEvents is the message-list events getter for tests that
// construct a chat view without any pre-loaded scrollback.
func nilEvents() []domain.StoredEvent { return nil }

// staticEvents returns an events getter that always reports `evs`.
func staticEvents(evs []domain.StoredEvent) func() []domain.StoredEvent {
	return func() []domain.StoredEvent { return evs }
}

// messagesToEvents converts channel messages into stored events.
func messagesToEvents(msgs []domain.Message) []domain.StoredEvent {
	events := make([]domain.StoredEvent, len(msgs))

	for i, m := range msgs {
		events[i] = domain.StoredEvent{Event: m}
	}

	return events
}

// newChatViewWithEvents creates a ChatView that reads the given
// events through its message-list getter.
func newChatViewWithEvents(ch domain.ChannelName, userNick domain.Nick, topic string, events []domain.StoredEvent) components.ChatView[testKind] {
	cv := components.NewChatView[testKind](
		func() []domain.StoredEvent { return events },
		ch, domain.KindChannel, userNick, topic,
	)
	timestampFmt := "[15:04:05]"
	updated, _ := cv.Update(components.TimestampFormatMsg{
		Format: &timestampFmt,
		Locale: language.BritishEnglish,
	})

	return updated.(components.ChatView[testKind])
}

// chatRegionLines returns the chat region's visible lines with the
// final input-prompt row removed. The prompt row is detected via a
// ` >` substring heuristic; if the prompt rendering changes shape (a
// different separator, extra trailing glyph) this heuristic will stop
// trimming the row and callers will see the prompt in their results.
func chatRegionLines(view string) []string {
	lines := visibleLines(view)
	if len(lines) == 0 {
		return nil
	}

	last := lines[len(lines)-1]
	if strings.Contains(last, " >") {
		return lines[:len(lines)-1]
	}

	return lines
}

func chatSegments(view string) []string {
	lines := chatRegionLines(view)
	segments := make([]string, 0, len(lines))

	for _, line := range lines {
		cleaned := strings.TrimSpace(line)
		if cleaned == "" {
			continue
		}

		if strings.Trim(cleaned, "┌┐└┘─│├┤┬┴┼") == "" {
			continue
		}

		segments = append(segments, cleaned)
	}

	return segments
}

func chatRenderedLines(view string) []string {
	return chatRegionLines(ansi.Strip(view))
}

func visibleEventLines(view string) []string {
	lines := chatRenderedLines(view)
	events := make([]string, 0, len(lines))

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		switch {
		case trimmed == "":
			continue
		case strings.HasPrefix(trimmed, "(") && strings.HasSuffix(trimmed, "%)"):
			continue
		case strings.Contains(trimmed, "responding"):
			continue
		case strings.Contains(trimmed, " new messages "):
			continue
		case !strings.Contains(trimmed, "<") && !strings.HasPrefix(trimmed, "***") && trimmed != "No messages yet":
			continue
		}

		events = append(events, trimmed)
	}

	return events
}

func scrollIndicatorLine(view string) string {
	for _, line := range chatRenderedLines(view) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "(") && strings.HasSuffix(trimmed, "%)") {
			return trimmed
		}
	}

	return ""
}

func pendingIndicatorLine(view string) string {
	for _, line := range chatRenderedLines(view) {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "responding") {
			return trimmed
		}
	}

	return ""
}

func dividerLine(view string) string {
	for _, line := range chatRenderedLines(view) {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, " new messages ") {
			return trimmed
		}
	}

	return ""
}

// expectedDivider returns the exact "new messages" divider line as
// rendered into a viewport of the given width: a left-padded run of
// box-drawing dashes, the literal " new messages " label, and a
// right-padded run of dashes that fills the rest of the line.
func expectedDivider(width int) string {
	const label = " new messages "

	if width <= len(label) {
		return label
	}

	pad := width - len([]rune(label))
	left := pad / 2
	right := pad - left

	return strings.Repeat("─", left) + label + strings.Repeat("─", right)
}

func rawRenderedLines(view string) []string {
	lines := strings.Split(strings.ReplaceAll(view, "\r\n", "\n"), "\n")

	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}

	return lines
}

func withoutTimestamp(line string) string {
	if idx := strings.Index(line, " <"); idx >= 0 {
		return line[idx+1:]
	}

	if idx := strings.Index(line, " ***"); idx >= 0 {
		return line[idx+1:]
	}

	return line
}

// visibleEventsWithoutTimestamps returns every visible event line with
// its rendered timestamp prefix stripped, so full-slice assertions
// remain stable against non-deterministic timestamps.
func visibleEventsWithoutTimestamps(view string) []string {
	events := visibleEventLines(view)
	out := make([]string, len(events))

	for i, line := range events {
		out[i] = withoutTimestamp(line)
	}

	return out
}

// numberedUserMessages builds the expected event slice for tests that
// render `<user> <body-prefix> N` messages across a contiguous index
// range [start, start+count).
func numberedUserMessages(bodyPrefix string, start, count int) []string {
	out := make([]string, count)

	for i := range count {
		out[i] = fmt.Sprintf("<user> %s %d", bodyPrefix, start+i)
	}

	return out
}

func chatInputTokens(view string) []string {
	lines := visibleLines(view)
	if len(lines) == 0 {
		return nil
	}

	return strings.Fields(lines[len(lines)-1])
}

func popoverLines(view string) []string {
	lines := visibleLines(ansi.Strip(view))
	if len(lines) == 0 {
		return nil
	}

	popover := lines[:len(lines)-1]
	filtered := make([]string, 0, len(popover))

	for _, line := range popover {
		if line == "No messages yet" {
			continue
		}

		filtered = append(filtered, line)
	}

	return filtered
}

func TestChatView_View_shows_messages(t *testing.T) {
	cv := newChatViewWithEvents("#general", "testuser", "", testEvents)
	v := cv.View(80, 24)

	require.Equal(t, []string{
		"[10:00:00] <alice> hello",
		"[10:01:00] <bob> hi there",
		"[10:02:00] <alice> how are you?",
	}, chatRegionLines(v))
	require.Equal(t, []string{"testuser", ">"}, chatInputTokens(v))
}

func TestChatView_clear_messages_removes_visible_messages(t *testing.T) {
	// The pure-view chat view shows the empty-state placeholder
	// when its events closure returns nothing; clearing is the
	// caller's responsibility (it owns the underlying slice).
	events := append([]domain.StoredEvent(nil), testEvents...)
	cv := components.NewChatView[testKind](
		func() []domain.StoredEvent { return events },
		"#general", domain.KindChannel, "testuser", "",
	)
	timestampFmt := "[15:04:05]"
	updated, _ := cv.Update(components.TimestampFormatMsg{
		Format: &timestampFmt,
		Locale: language.BritishEnglish,
	})
	cv = updated.(components.ChatView[testKind])

	events = nil

	v := cv.View(80, 24)
	require.Equal(t, []string{"No messages yet"}, chatRegionLines(v))
	require.Equal(t, []string{"testuser", ">"}, chatInputTokens(v))
}

func TestChatView_View_fits_available_width_with_input_prefix(t *testing.T) {
	cv := newChatViewWithEvents("#general", "testuser", "", []domain.StoredEvent{
		{Event: domain.Join{Target: "#general", Nick: "testuser"}},
	})

	v := cv.View(40, 10)

	require.LessOrEqual(t, lipgloss.Width(v), 40)
}

func TestChatView_View_shows_timestamps(t *testing.T) {
	cv := newChatViewWithEvents("#general", "testuser", "", testEvents)
	v := cv.View(80, 24)

	require.Equal(t, []string{
		"[10:00:00] <alice> hello",
		"[10:01:00] <bob> hi there",
		"[10:02:00] <alice> how are you?",
	}, chatRegionLines(v))
}

func TestChatView_View_disables_timestamps(t *testing.T) {
	events := testEvents
	cv := components.NewChatView[testKind](
		func() []domain.StoredEvent { return events },
		"#general", domain.KindChannel, "testuser", "",
	)
	disabled := ""
	var m ui.Model = cv

	m, _ = m.Update(components.TimestampFormatMsg{
		Format: &disabled,
		Locale: language.BritishEnglish,
	})

	v := m.View(80, 24)

	require.Equal(t, []string{
		"<alice> hello",
		"<bob> hi there",
		"<alice> how are you?",
	}, chatRegionLines(v))
}

func TestChatView_View_uses_strftime_timestamp_format(t *testing.T) {
	events := testEvents[:1]
	cv := components.NewChatView[testKind](
		func() []domain.StoredEvent { return events },
		"#general", domain.KindChannel, "testuser", "",
	)
	format := "%X"
	var m ui.Model = cv

	m, _ = m.Update(components.TimestampFormatMsg{
		Format: &format,
		Locale: language.BritishEnglish,
	})

	v := ansi.Strip(m.View(80, 24))

	require.Equal(t, []string{"10:00:00 <alice> hello"}, chatRegionLines(v))
}

func TestChatView_View_wraps_long_messages(t *testing.T) {
	longBody := strings.Repeat("word ", 30)
	events := []domain.StoredEvent{
		{Event: domain.Message{Target: "#general", From: "alice", Body: longBody, At: time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)}},
	}

	cv := newChatViewWithEvents("#general", "testuser", "", events)
	v := cv.View(40, 24)

	// The message should wrap, producing more rendered lines than one.
	require.Greater(t, lipgloss.Height(v), 3,
		"long message should wrap to multiple lines at narrow width")

	require.Equal(t, []string{
		"[10:00:00] <alice> word word word word",
		"word word word word word word word word",
		"word word word word word word word word",
		"word word word word word word word word",
		"word word",
	}, chatRegionLines(v))
}

func TestChatView_View_empty_messages(t *testing.T) {
	cv := components.NewChatView[testKind](nilEvents, "#general", domain.KindChannel, "testuser", "")
	v := cv.View(80, 24)

	require.Equal(t, []string{"No messages yet"}, chatRegionLines(v))
}

func TestChatView_View_has_input_prompt(t *testing.T) {
	cv := newChatViewWithEvents("#general", "testuser", "", testEvents)
	v := cv.View(80, 24)

	require.Equal(t, []string{"testuser", ">"}, chatInputTokens(v))
}

func TestChatView_typing_goes_to_input(t *testing.T) {
	cv := components.NewChatView[testKind](nilEvents, "#general", domain.KindChannel, "testuser", "")
	var m ui.Model = cv

	m = typeText(t, m, "test message")
	m, cmd := enter(t, m)

	require.NotNil(t, cmd)

	msg := cmd()
	require.Equal(t, components.MessageSubmitMsg{Text: "test message"}, msg)

	_ = m
}

func TestChatView_command_from_input(t *testing.T) {
	cv := components.NewChatView[testKind](nilEvents, "#general", domain.KindChannel, "testuser", "")
	var m ui.Model = cv

	m = typeText(t, m, "/join #random")
	_, cmd := enter(t, m)

	require.NotNil(t, cmd)

	msg := cmd()
	require.Equal(t, components.CommandSubmitMsg{Raw: "/join #random"}, msg)
}

func TestChatView_messages_updated(t *testing.T) {
	updatedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	cv := newChatViewWithEvents("#general", "testuser", "", []domain.StoredEvent{
		{Event: domain.Message{Target: "#general", From: "charlie", Body: "new message", At: updatedAt}},
	})

	v := cv.View(80, 24)
	require.Equal(t, []string{"[12:00:00] <charlie> new message"}, chatRegionLines(v))
}

func TestChatView_original_messages_persist(t *testing.T) {
	events := append([]domain.StoredEvent(nil), testEvents...)
	cv := components.NewChatView[testKind](
		func() []domain.StoredEvent { return events },
		"#general", domain.KindChannel, "testuser", "",
	)
	timestampFmt := "[15:04:05]"
	updated, _ := cv.Update(components.TimestampFormatMsg{
		Format: &timestampFmt,
		Locale: language.BritishEnglish,
	})

	appendAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	events = append(events, domain.StoredEvent{
		Event: domain.Message{Target: "#general", From: "charlie", Body: "extra", At: appendAt},
	})

	v := updated.View(80, 24)
	require.Equal(t, []string{
		"[10:00:00] <alice> hello",
		"[10:01:00] <bob> hi there",
		"[10:02:00] <alice> how are you?",
		"[12:00:00] <charlie> extra",
	}, chatRegionLines(v))
}

func TestChatView_append_event(t *testing.T) {
	events := append([]domain.StoredEvent(nil), testEvents...)
	cv := components.NewChatView[testKind](
		func() []domain.StoredEvent { return events },
		"#general", domain.KindChannel, "testuser", "",
	)
	timestampFmt := "[15:04:05]"
	updated, _ := cv.Update(components.TimestampFormatMsg{
		Format: &timestampFmt,
		Locale: language.BritishEnglish,
	})

	appendAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	events = append(events, domain.StoredEvent{
		Event: domain.Message{Target: "#general", From: "dave", Body: "appended message", At: appendAt},
	})

	v := updated.View(80, 24)
	require.Equal(t, []string{
		"[10:00:00] <alice> hello",
		"[10:01:00] <bob> hi there",
		"[10:02:00] <alice> how are you?",
		"[12:00:00] <dave> appended message",
	}, chatRegionLines(v))
}

func TestChatView_scroll(t *testing.T) {
	// Create many messages to fill the view.
	msgs := make([]domain.Message, 30)
	for i := range msgs {
		msgs[i] = domain.Message{
			Target: "#general",
			From:   "user",
			Body:   fmt.Sprintf("message %d", i),
		}
	}

	cv := newChatViewWithEvents("#general", "testuser", "", messagesToEvents(msgs))
	var m ui.Model = cv

	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 24}})

	// Scroll up.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})

	v := m.View(80, 24)
	require.Equal(t, numberedUserMessages("message", 0, 22), visibleEventsWithoutTimestamps(v))
	require.Equal(t, "(0%)", scrollIndicatorLine(v))

	// Scroll back down.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})

	v = m.View(80, 24)
	require.Equal(t, numberedUserMessages("message", 7, 23), visibleEventsWithoutTimestamps(v))
	require.Equal(t, "", scrollIndicatorLine(v))
}

func TestChatView_scroll_indicator(t *testing.T) {
	msgs := make([]domain.Message, 30)
	for i := range msgs {
		msgs[i] = domain.Message{
			Target: "#general",
			From:   "user",
			Body:   fmt.Sprintf("message %d", i),
		}
	}

	cv := newChatViewWithEvents("#general", "testuser", "", messagesToEvents(msgs))
	var m ui.Model = cv
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 24}})

	// At the bottom — no indicator.
	v := m.View(80, 24)
	require.Equal(t, "", scrollIndicatorLine(v))

	// Scroll up — indicator appears.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})

	v = m.View(80, 24)
	require.Equal(t, "(0%)", scrollIndicatorLine(v))

	// Total height stays the same.
	require.Equal(t, 24, lipgloss.Height(v))

	// Scroll back to bottom — indicator disappears.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})

	v = m.View(80, 24)
	require.Equal(t, "", scrollIndicatorLine(v))
}

func TestChatView_ctrl_arrow_scroll(t *testing.T) {
	msgs := make([]domain.Message, 30)
	for i := range msgs {
		msgs[i] = domain.Message{
			Target: "#general",
			From:   "user",
			Body:   fmt.Sprintf("message %d", i),
		}
	}

	cv := newChatViewWithEvents("#general", "testuser", "", messagesToEvents(msgs))
	var m ui.Model = cv
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 24}})

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlUp})

	v := m.View(80, 24)
	require.Equal(t, numberedUserMessages("message", 6, 22), visibleEventsWithoutTimestamps(v))
	require.Equal(t, "(85%)", scrollIndicatorLine(v))

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlDown})

	v = m.View(80, 24)
	require.Equal(t, numberedUserMessages("message", 7, 23), visibleEventsWithoutTimestamps(v))
	require.Equal(t, "", scrollIndicatorLine(v))
}

func TestChatView_scroll_does_not_go_negative(t *testing.T) {
	cv := newChatViewWithEvents("#general", "testuser", "", testEvents)
	var m ui.Model = cv

	// Try to scroll down past zero.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})

	v := m.View(80, 24)
	require.Equal(t, []string{
		"[10:00:00] <alice> hello",
		"[10:01:00] <bob> hi there",
		"[10:02:00] <alice> how are you?",
	}, visibleEventLines(v))
}

func TestChatView_arrow_keys_stay_with_input(t *testing.T) {
	msgs := make([]domain.Message, 30)
	for i := range msgs {
		msgs[i] = domain.Message{
			Target: "#general",
			From:   "user",
			Body:   fmt.Sprintf("message %d", i),
		}
	}

	cv := newChatViewWithEvents("#general", "testuser", "", messagesToEvents(msgs))
	var m ui.Model = cv
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 24}})

	m = typeText(t, m, "first")
	m, _ = enter(t, m)
	m = typeText(t, m, "second")
	m, _ = enter(t, m)
	m = typeText(t, m, "draft")

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})

	v := m.View(80, 24)
	require.Equal(t, []string{"testuser", ">", "second"}, chatInputTokens(v))
	require.Equal(t, numberedUserMessages("message", 7, 23), visibleEventsWithoutTimestamps(v))

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = typeText(t, m, "X")

	v = m.View(80, 24)
	require.Equal(t, []string{"testuser", ">", "draXft"}, chatInputTokens(v))
	require.Equal(t, numberedUserMessages("message", 7, 23), visibleEventsWithoutTimestamps(v))
}

func TestChatView_nicks_use_hashed_colours(t *testing.T) {
	// Force the ANSI profile so escape codes are deterministic.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	aliceToken := extractStyledNickToken(t, renderNick(t, "alice", "from user"), "alice")
	botToken := extractStyledNickToken(t, renderNick(t, "bot", "from model"), "bot")

	// Each token must consist of an SGR introducer, the literal
	// `<nick>`, and an SGR reset. Anything matching that shape proves
	// the nick is wrapped in ANSI styling rather than rendered bare.
	const styledNickShape = `^\x1b\[[0-9;]+m<[a-z]+>\x1b\[[0-9;]*m$`
	require.Regexp(t, styledNickShape, aliceToken)
	require.Regexp(t, styledNickShape, botToken)

	// Distinct nicks produce distinct styled tokens (they must differ in
	// at least the escape codes or the visible text).
	require.NotEqual(t, aliceToken, botToken)

	// Rendering alice again produces an identical token — pins the
	// determinism of the hash.
	aliceAgain := extractStyledNickToken(t, renderNick(t, "alice", "from user"), "alice")
	require.Equal(t, aliceToken, aliceAgain)
}

// renderNick renders a single chat message and returns the raw line
// containing the nick token, including its ANSI escapes.
func renderNick(t *testing.T, nick, body string) string {
	t.Helper()

	cv := newChatViewWithEvents("#general", "user", "", messagesToEvents([]domain.Message{
		{Target: "#general", From: domain.Nick(nick), Body: body},
	}))

	lines := rawRenderedLines(cv.View(80, 24))
	for _, line := range lines {
		if strings.Contains(line, "<"+nick+">") {
			return line
		}
	}

	t.Fatalf("no line containing <%s> in view:\n%s", nick, strings.Join(lines, "\n"))

	return ""
}

// extractStyledNickToken pulls the substring from the raw line
// consisting of the ANSI escape wrapping the nick token plus the nick
// token itself and the trailing reset.
func extractStyledNickToken(t *testing.T, rawLine, nick string) string {
	t.Helper()

	re := regexp.MustCompile(`\x1b\[[0-9;]*m<` + regexp.QuoteMeta(nick) + `>\x1b\[[0-9;]*m`)
	token := re.FindString(rawLine)
	require.NotEmpty(t, token, "no styled <%s> token in line: %q", nick, rawLine)

	return token
}

func TestChatView_shows_nick_in_input_area(t *testing.T) {
	cv := newChatViewWithEvents("#general", "alice", "", testEvents)
	v := cv.View(80, 24)

	require.Equal(t, []string{"alice", ">"}, chatInputTokens(v))
}

func TestChatView_nick_updates_after_change(t *testing.T) {
	cv1 := newChatViewWithEvents("#general", "oldnick", "", testEvents)
	cv2 := newChatViewWithEvents("#general", "newnick", "", testEvents)

	v1 := cv1.View(80, 24)
	v2 := cv2.View(80, 24)

	require.Equal(t, []string{"oldnick", ">"}, chatInputTokens(v1))
	require.Equal(t, []string{"newnick", ">"}, chatInputTokens(v2))
}

func TestChatView_dm_hides_topic_bar(t *testing.T) {
	cv := components.NewChatView[testKind](nilEvents, "botname", domain.KindChannel, "testuser", "")

	m, _ := cv.Update(components.SetChannelMsg{
		Channel: "botname",
		Topic:   "should not appear",
		Kind:    domain.KindDM,
	})
	cv = m.(components.ChatView[testKind])

	v := cv.View(80, 24)

	// With no events and DM kind, the topic bar must not render
	// even though a topic was supplied with `SetChannelMsg`.
	require.Equal(t, []string{"No messages yet"}, chatSegments(ansi.Strip(v)))
}

func TestChatView_dm_suppresses_join_part_events(t *testing.T) {
	now := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	events := []domain.StoredEvent{
		{Event: domain.Join{Target: "botname", Nick: "testuser", At: now}},
		{Event: domain.Part{Target: "botname", Nick: "testuser", At: now}},
		{Event: domain.ModeChange{Target: "botname", Nick: "testuser", Flag: domain.ModeOperator, Add: true, By: "ChanServ", At: now}},
		{Event: domain.TopicChange{Target: "botname", Topic: "x", By: "testuser", At: now}},
		{Event: domain.Message{Target: "botname", From: "bot", Body: "hello human", At: now}},
	}

	cv := components.NewChatView[testKind](staticEvents(events), "botname", domain.KindChannel, "testuser", "")
	m, _ := cv.Update(components.SetChannelMsg{
		Channel: "botname",
		Kind:    domain.KindDM,
	})
	cv = m.(components.ChatView[testKind])

	v := ansi.Strip(cv.View(80, 24))

	require.Equal(t, []string{"Wed 01 Jan 2025 10:00:00 UTC <bot> hello human"}, chatSegments(v))
}

func TestChatView_dm_shows_quit_messages(t *testing.T) {
	now := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	events := []domain.StoredEvent{
		{Event: domain.Quit{Nick: "bot", Message: "goodbye", At: now}},
	}

	cv := components.NewChatView[testKind](staticEvents(events), "botname", domain.KindChannel, "testuser", "")
	m, _ := cv.Update(components.SetChannelMsg{
		Channel: "botname",
		Kind:    domain.KindDM,
	})
	cv = m.(components.ChatView[testKind])

	v := ansi.Strip(cv.View(80, 24))

	require.Equal(t, []string{"*** bot has quit (goodbye)"}, chatSegments(v))
}

func TestChatView_topic_bar_shown(t *testing.T) {
	cv := newChatViewWithEvents("#general", "testuser", "Welcome to general", testEvents)
	v := cv.View(80, 24)

	require.Equal(t, []string{
		"Welcome to general",
		"[10:00:00] <alice> hello",
		"[10:01:00] <bob> hi there",
		"[10:02:00] <alice> how are you?",
	}, chatSegments(v))
}

func TestChatView_no_topic_bar_when_empty(t *testing.T) {
	withTitle := newChatViewWithEvents("#general", "testuser", "some topic", testEvents)
	without := newChatViewWithEvents("#general", "testuser", "", testEvents)

	vWith := withTitle.View(80, 24)
	vWithout := without.View(80, 24)

	require.Equal(t, []string{
		"some topic",
		"[10:00:00] <alice> hello",
		"[10:01:00] <bob> hi there",
		"[10:02:00] <alice> how are you?",
	}, chatSegments(vWith))
	require.Equal(t, []string{
		"[10:00:00] <alice> hello",
		"[10:01:00] <bob> hi there",
		"[10:02:00] <alice> how are you?",
	}, chatSegments(vWithout))
}

func TestChatView_topic_bar_reduces_message_area(t *testing.T) {
	msgs := make([]domain.Message, 30)
	for i := range msgs {
		msgs[i] = domain.Message{
			Target: "#general",
			From:   "user",
			Body:   fmt.Sprintf("msg %d", i),
		}
	}

	events := messagesToEvents(msgs)

	withTitle := newChatViewWithEvents("#general", "testuser", "A topic", events)
	without := newChatViewWithEvents("#general", "testuser", "", events)

	vWith := withTitle.View(80, 24)
	vWithout := without.View(80, 24)

	withLines := lipgloss.Height(vWith)
	withoutLines := lipgloss.Height(vWithout)

	// Both should fill the same total height.
	require.Equal(t, withoutLines, withLines)
}

func TestChatView_TopicUpdatedMsg_updates_topic_bar(t *testing.T) {
	var m ui.Model = newChatViewWithEvents("#general", "testuser", "", testEvents)

	// No topic initially.
	v := m.View(80, 24)
	require.Equal(t, []string{
		"[10:00:00] <alice> hello",
		"[10:01:00] <bob> hi there",
		"[10:02:00] <alice> how are you?",
	}, chatSegments(ansi.Strip(v)))

	// Send TopicUpdatedMsg.
	m, _ = m.Update(components.TopicUpdatedMsg{Topic: "new topic"})

	v = m.View(80, 24)
	require.Equal(t, []string{
		"new topic",
		"[10:00:00] <alice> hello",
		"[10:01:00] <bob> hi there",
		"[10:02:00] <alice> how are you?",
	}, chatSegments(ansi.Strip(v)))

	// Clear topic.
	m, _ = m.Update(components.TopicUpdatedMsg{Topic: ""})

	v = m.View(80, 24)
	require.Equal(t, []string{
		"[10:00:00] <alice> hello",
		"[10:01:00] <bob> hi there",
		"[10:02:00] <alice> how are you?",
	}, chatSegments(ansi.Strip(v)))
}

func TestChatView_pending_indicator(t *testing.T) {
	cv := newChatViewWithEvents("#general", "testuser", "", testEvents)
	var m ui.Model = cv

	// Initially no pending indicator.
	v := m.View(80, 24)
	require.Equal(t, "", pendingIndicatorLine(v))

	// Set pending.
	m, _ = m.Update(components.PendingResponseMsg{Pending: true})

	v = m.View(80, 24)
	require.True(t, strings.HasSuffix(pendingIndicatorLine(v), "responding…"))
	require.Equal(t, []string{
		"[10:00:00] <alice> hello",
		"[10:01:00] <bob> hi there",
		"[10:02:00] <alice> how are you?",
	}, visibleEventLines(v))

	// Clear pending.
	m, _ = m.Update(components.PendingResponseMsg{Pending: false})

	v = m.View(80, 24)
	require.Equal(t, "", pendingIndicatorLine(v))
}

func TestChatView_pending_indicator_reduces_message_area(t *testing.T) {
	msgs := make([]domain.Message, 30)
	for i := range msgs {
		msgs[i] = domain.Message{
			Target: "#general",
			From:   "user",
			Body:   fmt.Sprintf("msg %d", i),
		}
	}

	cv := newChatViewWithEvents("#general", "testuser", "", messagesToEvents(msgs))
	var m ui.Model = cv

	m, _ = m.Update(components.PendingResponseMsg{Pending: true})

	v := m.View(80, 24)
	withLines := lipgloss.Height(v)

	// Total height stays the same; the pending indicator pushes
	// the visible window up by one line, so the most-recent 22
	// messages (8..29) remain in view.
	require.Equal(t, 24, withLines)
	require.Equal(t, numberedUserMessages("msg", 8, 22), visibleEventsWithoutTimestamps(v))
	require.NotEmpty(t, pendingIndicatorLine(v))
}

func renderSingleEvent(event domain.StoredEvent) string {
	return renderSingleEventWithHighlight(event, nil, "testuser")
}

func renderSingleEventWithHighlight(event domain.StoredEvent, words []string, nick domain.Nick) string {
	cv := components.NewChatView[testKind](
		staticEvents([]domain.StoredEvent{event}),
		"#test", domain.KindChannel, nick, "",
	)
	var m ui.Model = cv
	topicFormat := "2006-01-02 15:04"

	m, _ = m.Update(components.CommandsMsg[testKind]{
		Commands: []*command.Node[testKind]{
			{Name: "join", Help: "Join or create a channel", Positionals: []command.Positional[testKind]{{Name: "channel"}}},
			{Name: "help", Help: "Show available commands."},
		},
	})
	m, _ = m.Update(components.TimestampFormatMsg{
		Format: &topicFormat,
		Locale: language.BritishEnglish,
	})

	if len(words) > 0 {
		m, _ = m.Update(components.HighlightWordsMsg{Words: words, UserNick: nick})
	}

	v := m.View(200, 24)

	return ansi.Strip(v)
}

func TestRenderLine_IRC_events(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name  string
		event domain.StoredEvent
		want  string
	}{
		{
			"join",
			domain.StoredEvent{Event: domain.Join{Target: "#general", Nick: "alice", At: now}},
			"*** alice has joined #general",
		},
		{
			"join_created",
			domain.StoredEvent{Event: domain.Join{Target: "#general", Nick: "alice", Created: true, At: now}},
			"*** Created channel #general",
		},
		{
			"part",
			domain.StoredEvent{Event: domain.Part{Target: "#general", Nick: "alice", At: now}},
			"*** alice has left #general",
		},
		{
			"part_with_message",
			domain.StoredEvent{Event: domain.Part{Target: "#general", Nick: "alice", Message: "see you later", At: now}},
			"*** alice has left #general (see you later)",
		},
		{
			"quit",
			domain.StoredEvent{Event: domain.Quit{Nick: "alice", At: now}},
			"*** alice has quit",
		},
		{
			"quit_with_message",
			domain.StoredEvent{Event: domain.Quit{Nick: "alice", Message: "shutting down", At: now}},
			"*** alice has quit (shutting down)",
		},
		{
			"nick_change",
			domain.StoredEvent{Event: domain.NickChange{OldNick: "alice", NewNick: "bob", At: now}},
			"*** alice is now known as bob",
		},
		{
			"topic_set_with_author",
			domain.StoredEvent{Event: domain.TopicChange{Target: "#general", Topic: "cool topic", By: "alice", At: now}},
			"*** topic for #general set by alice: cool topic",
		},
		{
			"topic_set_no_author",
			domain.StoredEvent{Event: domain.TopicChange{Target: "#general", Topic: "cool topic", At: now}},
			"*** topic for #general set to: cool topic",
		},
		{
			"topic_cleared",
			domain.StoredEvent{Event: domain.TopicChange{Target: "#general", Topic: "", By: "alice", At: now}},
			"*** topic for #general cleared by alice",
		},
		{
			"topic_info_with_metadata",
			domain.StoredEvent{Event: domain.TopicInfo{
				Target: "#general", Topic: "cool topic",
				TopicSetBy: "alice", TopicSetAt: time.Date(2026, 4, 4, 23, 30, 0, 0, time.UTC),
				At: now,
			}},
			"*** topic for #general: cool topic (set by alice on 2026-04-04 23:30)",
		},
		{
			"topic_info_no_topic",
			domain.StoredEvent{Event: domain.TopicInfo{Target: "#general", At: now}},
			"*** No topic set for #general",
		},
		{
			"mode_change",
			domain.StoredEvent{Event: domain.ModeChange{
				Target: "#general", Nick: "botty", Flag: domain.ModeChannelVoice, Add: true, By: "ChanServ", At: now,
			}},
			"*** ChanServ sets mode +v botty",
		},
		{
			"model_invited",
			domain.StoredEvent{Event: domain.ModelInvited{
				Target: "#general", Nick: "botty", By: "alice", At: now,
			}},
			"*** botty has joined #general",
		},
		{
			"model_kicked",
			domain.StoredEvent{Event: domain.ModelKicked{Target: "#general", Nick: "botty", By: "someone", At: now}},
			"*** botty was kicked from #general by someone",
		},
		{
			"action_message",
			domain.StoredEvent{Event: domain.Message{Target: "#test", From: "alice", Body: "waves", Action: true, At: now}},
			"* alice waves",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderSingleEvent(tt.event)
			want := tt.want
			if event, ok := tt.event.Event.(domain.Message); ok && event.Action {
				want = fmt.Sprintf("%s * alice waves", event.At.Format("2006-01-02 15:04"))
			}

			require.Equal(t, []string{want}, chatSegments(got))
		})
	}
}

func TestRenderLine_topic_info_omits_timestamp_when_disabled(t *testing.T) {
	events := []domain.StoredEvent{{
		Event: domain.TopicInfo{
			Target:     "#general",
			Topic:      "cool topic",
			TopicSetBy: "alice",
			TopicSetAt: time.Date(2026, 4, 4, 23, 30, 0, 0, time.UTC),
			At:         time.Now(),
		},
	}}

	cv := components.NewChatView[testKind](staticEvents(events), "#test", domain.KindChannel, "testuser", "")
	var m ui.Model = cv
	disabled := ""

	m, _ = m.Update(components.TimestampFormatMsg{
		Format: &disabled,
		Locale: language.BritishEnglish,
	})

	v := ansi.Strip(m.View(200, 24))

	require.Equal(t, []string{"*** topic for #general: cool topic (set by alice)"}, chatSegments(v))
}

func TestRenderLine_application_feedback(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name  string
		event domain.StoredEvent
		want  []string
	}{
		{
			"help",
			domain.StoredEvent{Event: domain.Help{Target: "#test", At: now}},
			[]string{
				"*** /join <channel>                  Join or create a channel",
				"*** /help                            Show available commands.",
				"*** formatting                      M-B/M-I/M-U/M-R/M-S toggle styles",
				"*** formatting                      M-C colours, M-O clears formatting",
			},
		},
		{
			"list_reply",
			domain.StoredEvent{Event: domain.ListReply{Channel: "#general", Members: 3, At: now}},
			[]string{"*** #general (3)"},
		},
		{
			"list_reply_with_topic",
			domain.StoredEvent{Event: domain.ListReply{Channel: "#random", Members: 5, Topic: "cool", At: now}},
			[]string{"*** #random (5) — cool"},
		},
		{
			"list_end",
			domain.StoredEvent{Event: domain.ListEnd{At: now}},
			[]string{"*** End of /list"},
		},
		{
			"api_key_saved",
			domain.StoredEvent{Event: domain.SystemNotice{Target: "#test", Text: "OpenRouter API key saved and activated.", At: now}},
			[]string{"✓ OpenRouter API key saved and activated."},
		},
		{
			"poke_interval_set",
			domain.StoredEvent{Event: domain.SystemNotice{Target: "#test", Text: "Poke interval set to 10m0s.", At: now}},
			[]string{"✓ Poke interval set to 10m0s."},
		},
		{
			"usage_hint_config",
			domain.StoredEvent{Event: domain.UsageHint{
				Target:  "#test",
				Command: "config",
				Usage:   "/config api-key <value> | /config small-model <model-id> | /config poke-interval <duration>",
				At:      now,
			}},
			[]string{"⚠ usage: /config api-key <value> | /config small-model <model-id> | /config poke-interval <duration>"},
		},
		{
			"usage_hint_invite",
			domain.StoredEvent{Event: domain.UsageHint{
				Target:  "#test",
				Command: "add-model",
				Usage:   "/add-model <model-id> [--persona <text>]",
				At:      now,
			}},
			[]string{"⚠ usage: /add-model <model-id> [--persona <text>]"},
		},
		{
			"no_channel",
			domain.StoredEvent{Event: domain.UsageHint{Usage: "join a channel first", At: now}},
			[]string{"⚠ join a channel first"},
		},
		{
			"command_error",
			domain.StoredEvent{Event: domain.CommandError{Target: "#test", Err: "unknown command: /foo", At: now}},
			[]string{"✗ unknown command: /foo"},
		},
		{
			"unknown_nick_error",
			domain.StoredEvent{Event: domain.CommandError{Target: "#test", Err: "no such nick: ghost", At: now}},
			[]string{"✗ no such nick: ghost"},
		},
		{
			"config_changed",
			domain.StoredEvent{Event: domain.SystemNotice{Target: "#test", Text: "API key saved", At: now}},
			[]string{"✓ API key saved"},
		},
		{
			"backend_error",
			domain.StoredEvent{Event: domain.CommandError{Target: "#test", Err: "model invocation: connection refused", At: now}},
			[]string{"✗ model invocation: connection refused"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderSingleEvent(tt.event)

			require.Equal(t, tt.want, chatSegments(got))
		})
	}
}

func TestNewMessagesDivider_fills_width(t *testing.T) {
	events := make([]domain.StoredEvent, 30)
	for i := range events {
		events[i] = domain.StoredEvent{
			Event: domain.Message{
				Target: "#test",
				From:   "user",
				Body:   fmt.Sprintf("message %d", i),
				At:     time.Now(),
			},
		}
	}

	cv := components.NewChatView[testKind](
		func() []domain.StoredEvent { return events },
		"#test", domain.KindChannel, "testuser", "",
	)
	var m ui.Model = cv
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 24}})

	// Scroll up, then grow the events slice to trigger the divider.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	events = append(events, domain.StoredEvent{
		Event: domain.Message{Target: "#test", From: "other", Body: "new arrival", At: time.Now()},
	})

	// Scroll back towards the bottom to bring the divider into view.
	for range 5 {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	}

	v := m.View(80, 24)
	stripped := ansi.Strip(v)

	divider := dividerLine(stripped)
	require.NotEmpty(t, divider)
	require.GreaterOrEqual(t, len([]rune(divider)), 40,
		"divider should span a significant portion of the width")
	require.Equal(t, "───────────────────────────────── new messages ─────────────────────────────────", divider)
}

func TestChatView_command_popover_renders_and_completes(t *testing.T) {
	var m ui.Model = components.NewChatView[testKind](nilEvents, "#general", domain.KindChannel, "testuser", "")
	nodes := []*command.Node[testKind]{
		{
			Name: "join",
			Help: "Join a channel",
			Positionals: []command.Positional[testKind]{
				{Name: "channel", Source: command.LiteralSource[testKind](
					command.Suggestion{Value: "#general", Label: "#general"},
					command.Suggestion{Value: "#random", Label: "#random"},
				)},
			},
		},
	}
	m, _ = m.Update(components.CommandsMsg[testKind]{Commands: nodes})
	m, _ = m.Update(components.CompleterMsg{
		Completer: command.CompletionSet[testKind]{Set: command.Set[testKind]{Commands: nodes}, Ctx: testKindChannel},
	})

	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{X: 20, Y: 0, Width: 60, Height: 24}})
	m = typeText(t, m, "/jo")

	v := m.View(60, 24)
	require.Equal(t, []string{"/join <channel>  Join a channel"}, popoverLines(v))
	require.Equal(t, []string{"testuser", ">", "/jo"}, chatInputTokens(v))

	var cmd tea.Cmd
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyTab})

	require.NotNil(t, cmd, "Tab should produce a cmd")
	m, _ = m.Update(cmd())

	m = typeText(t, m, "#random")
	_, cmd = enter(t, m)

	require.NotNil(t, cmd)
	sub := cmd().(components.CommandSubmitMsg)
	require.Equal(t, "/join #random", sub.Raw)
}

func TestChatView_popover_arrow_keys_do_not_fall_through(t *testing.T) {
	var m ui.Model = components.NewChatView[testKind](nilEvents, "#general", domain.KindChannel, "testuser", "")
	nodes := []*command.Node[testKind]{
		{Name: "join", Help: "Join a channel"},
		{Name: "part", Help: "Part from the current channel"},
		{Name: "quit", Help: "Exit modeloff"},
	}
	m, _ = m.Update(components.CommandsMsg[testKind]{Commands: nodes})
	m, _ = m.Update(components.CompleterMsg{
		Completer: command.CompletionSet[testKind]{Set: command.Set[testKind]{Commands: nodes}, Ctx: testKindChannel},
	})

	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{X: 0, Y: 0, Width: 60, Height: 24}})

	// Seed input history so Up would recall it if it fell through.
	m = typeText(t, m, "previous input")
	m, _ = enter(t, m)

	m = typeText(t, m, "/")

	// The popover is now visible with suggestions. Down should
	// navigate the popover, not recall input history.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})

	// Type Tab to accept whatever is selected, then complete and submit.
	var cmd tea.Cmd
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyTab})

	require.NotNil(t, cmd, "Tab should produce a cmd")
	m, _ = m.Update(cmd())

	_, cmd = enter(t, m)

	require.NotNil(t, cmd)
	sub := cmd().(components.CommandSubmitMsg)

	// If Down fell through to input history, the input would contain
	// "previous input" instead of a command. The second suggestion
	// (/part) should be selected after one Down press.
	require.Equal(t, "/part", sub.Raw)
}

func TestChatView_popover_renders_usage_in_suggestions(t *testing.T) {
	var m ui.Model = components.NewChatView[testKind](nilEvents, "#general", domain.KindChannel, "testuser", "")
	nodes := []*command.Node[testKind]{
		{Name: "join", Help: "Join a channel", Positionals: []command.Positional[testKind]{{Name: "channel"}}},
		{Name: "part", Help: "Part from the current channel"},
		{Name: "quit", Help: "Exit modeloff"},
	}
	m, _ = m.Update(components.CommandsMsg[testKind]{Commands: nodes})
	m, _ = m.Update(components.CompleterMsg{
		Completer: command.CompletionSet[testKind]{Set: command.Set[testKind]{Commands: nodes}, Ctx: testKindChannel},
	})

	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{X: 0, Y: 0, Width: 60, Height: 24}})
	m = typeText(t, m, "/")

	v := m.View(60, 24)
	stripped := ansi.Strip(v)

	require.Equal(t, []string{
		"/join <channel>  Join a channel",
		"/part  Part from the current channel",
		"/quit  Exit modeloff",
	}, popoverLines(stripped))
	require.Equal(t, []string{"testuser", ">", "/"}, chatInputTokens(stripped))
}

func TestChatView_popover_collapses_aliases_onto_single_row(t *testing.T) {
	// Regression guard for the `/j oin (/j) <channel>` garble that
	// appeared when each alias was rendered as its own popover row
	// with its Label trimmed against the canonical Usage. Aliases
	// must be collapsed into the single parenthesised group after the
	// canonical name, followed by positional args and the help text.
	var m ui.Model = components.NewChatView[testKind](nilEvents, "#general", domain.KindChannel, "testuser", "")
	nodes := []*command.Node[testKind]{
		{
			Name:        "join",
			Aliases:     []string{"j", "jo"},
			Help:        "Join a channel",
			Positionals: []command.Positional[testKind]{{Name: "channel"}},
		},
		{Name: "quit", Aliases: []string{"q"}, Help: "Exit modeloff"},
	}
	m, _ = m.Update(components.CommandsMsg[testKind]{Commands: nodes})
	m, _ = m.Update(components.CompleterMsg{
		Completer: command.CompletionSet[testKind]{Set: command.Set[testKind]{Commands: nodes}, Ctx: testKindChannel},
	})

	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{X: 0, Y: 0, Width: 80, Height: 24}})
	m = typeText(t, m, "/")

	v := m.View(80, 24)
	stripped := ansi.Strip(v)

	require.Equal(t, []string{
		"/join (/j, /jo) <channel>  Join a channel",
		"/quit (/q)  Exit modeloff",
	}, popoverLines(stripped))
	require.Equal(t, []string{"testuser", ">", "/"}, chatInputTokens(stripped))
}

func TestChatView_mouse_click_positions_input_cursor(t *testing.T) {
	cv := components.NewChatView[testKind](nilEvents, "#general", domain.KindChannel, "testuser", "")
	var m ui.Model = cv

	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{X: 20, Y: 0, Width: 60, Height: 24}})
	m = typeText(t, m, "hello")
	m, _ = m.Update(tea.MouseMsg{
		X:      32,
		Y:      23,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	m = typeText(t, m, "X")

	var cmd tea.Cmd
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)

	sub := cmd().(components.MessageSubmitMsg)
	require.Equal(t, "hXello", sub.Text)
}

func TestChatView_divider_inserted_when_scrolled_up(t *testing.T) {
	events := make([]domain.StoredEvent, 30)
	for i := range events {
		events[i] = domain.StoredEvent{
			Event: domain.Message{
				Target: "#general",
				From:   "user",
				Body:   fmt.Sprintf("message %d", i),
				At:     time.Now(),
			},
		}
	}

	cv := components.NewChatView[testKind](
		func() []domain.StoredEvent { return events },
		"#general", domain.KindChannel, "testuser", "",
	)
	timestampFmt := "[15:04:05]"
	updated, _ := cv.Update(components.TimestampFormatMsg{
		Format: &timestampFmt,
		Locale: language.BritishEnglish,
	})
	var m ui.Model = updated.(components.ChatView[testKind])
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 24}})

	// Scroll up so we're no longer at the bottom.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})

	v := m.View(80, 24)
	require.Equal(t, numberedUserMessages("message", 0, 22), visibleEventsWithoutTimestamps(v))

	// Append new events to the closure-backed slice while scrolled
	// up. A ScrollbackUpdatedMsg after each append signals the
	// message list to re-read the getter — the divider arms on
	// growth observed while not at the bottom.
	for i := 30; i < 33; i++ {
		events = append(events, domain.StoredEvent{
			Event: domain.Message{
				Target: "#general",
				From:   "other",
				Body:   fmt.Sprintf("new message %d", i),
				At:     time.Now(),
			},
		})
		m, _ = m.Update(components.ScrollbackUpdatedMsg{Channel: "#general"})
	}

	vScrolledUp := ansi.Strip(m.View(80, 24))
	t.Logf("while scrolled up:\n%s", vScrolledUp)

	// Scroll to bottom to see the divider.
	for range 5 {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	}

	v = m.View(80, 24)
	stripped := ansi.Strip(v)

	require.Equal(t, expectedDivider(80), dividerLine(stripped))
}

func TestChatView_no_divider_when_at_bottom(t *testing.T) {
	events := make([]domain.StoredEvent, 5)
	for i := range events {
		events[i] = domain.StoredEvent{
			Event: domain.Message{
				Target: "#general",
				From:   "user",
				Body:   fmt.Sprintf("message %d", i),
				At:     time.Now(),
			},
		}
	}

	cv := newChatViewWithEvents("#general", "testuser", "", events)
	var m ui.Model = cv
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 24}})

	// Add more events while at bottom.
	for i := 5; i < 8; i++ {
		m, _ = m.Update(domain.StoredEvent{
			Event: domain.Message{
				Target: "#general",
				From:   "other",
				Body:   fmt.Sprintf("new message %d", i),
				At:     time.Now(),
			},
		})
	}

	v := m.View(80, 24)
	stripped := ansi.Strip(v)

	require.Equal(t, "", dividerLine(stripped))
}

func TestChatView_stored_events_insert_divider_when_scrolled_up(t *testing.T) {
	events := make([]domain.StoredEvent, 30)
	for i := range 30 {
		events[i] = domain.StoredEvent{
			Event: domain.Message{
				Target: "#general",
				From:   "user",
				Body:   fmt.Sprintf("message %d", i),
			},
		}
	}

	cv := components.NewChatView[testKind](
		func() []domain.StoredEvent { return events },
		"#general", domain.KindChannel, "testuser", "",
	)
	timestampFmt := "[15:04:05]"
	updated, _ := cv.Update(components.TimestampFormatMsg{
		Format: &timestampFmt,
		Locale: language.BritishEnglish,
	})
	var m ui.Model = updated.(components.ChatView[testKind])
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 24}})

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})

	for i := 30; i < 33; i++ {
		events = append(events, domain.StoredEvent{
			Event: domain.Message{
				Target: "#general",
				From:   "user",
				Body:   fmt.Sprintf("new message %d", i),
			},
		})
		m, _ = m.Update(components.ScrollbackUpdatedMsg{Channel: "#general"})
	}

	for range 5 {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	}

	v := ansi.Strip(m.View(80, 24))

	require.Equal(t, expectedDivider(80), dividerLine(v))
	require.Equal(t, []string{
		"[00:00:00] <user> message 11",
		"[00:00:00] <user> message 12",
		"[00:00:00] <user> message 13",
		"[00:00:00] <user> message 14",
		"[00:00:00] <user> message 15",
		"[00:00:00] <user> message 16",
		"[00:00:00] <user> message 17",
		"[00:00:00] <user> message 18",
		"[00:00:00] <user> message 19",
		"[00:00:00] <user> message 20",
		"[00:00:00] <user> message 21",
		"[00:00:00] <user> message 22",
		"[00:00:00] <user> message 23",
		"[00:00:00] <user> message 24",
		"[00:00:00] <user> message 25",
		"[00:00:00] <user> message 26",
		"[00:00:00] <user> message 27",
		"[00:00:00] <user> message 28",
		"[00:00:00] <user> message 29",
		"[00:00:00] <user> new message 30",
		"[00:00:00] <user> new message 31",
		"[00:00:00] <user> new message 32",
	}, visibleEventLines(v))
}

func TestChatView_stored_events_keep_divider_when_more_arrive_during_catch_up(t *testing.T) {
	events := make([]domain.StoredEvent, 30)
	for i := range 30 {
		events[i] = domain.StoredEvent{
			Event: domain.Message{
				Target: "#general",
				From:   "user",
				Body:   fmt.Sprintf("message %d", i),
			},
		}
	}

	cv := components.NewChatView[testKind](
		func() []domain.StoredEvent { return events },
		"#general", domain.KindChannel, "testuser", "",
	)
	timestampFmt := "[15:04:05]"
	updated, _ := cv.Update(components.TimestampFormatMsg{
		Format: &timestampFmt,
		Locale: language.BritishEnglish,
	})
	var m ui.Model = updated.(components.ChatView[testKind])
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{Width: 80, Height: 24}})

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})

	// First new event while scrolled up — divider is armed.
	events = append(events, domain.StoredEvent{
		Event: domain.Message{
			Target: "#general",
			From:   "user",
			Body:   "new message 30",
		},
	})
	m, _ = m.Update(components.ScrollbackUpdatedMsg{Channel: "#general"})

	// Second new event while still scrolled up — divider stays.
	events = append(events, domain.StoredEvent{
		Event: domain.Message{
			Target: "#general",
			From:   "user",
			Body:   "new message 31",
		},
	})
	m, _ = m.Update(components.ScrollbackUpdatedMsg{Channel: "#general"})

	// Scroll to bottom to see both new events and the divider.
	for range 5 {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	}

	v := ansi.Strip(m.View(80, 24))

	require.Equal(t, expectedDivider(80), dividerLine(v))
	require.Equal(t, []string{
		"[00:00:00] <user> message 10",
		"[00:00:00] <user> message 11",
		"[00:00:00] <user> message 12",
		"[00:00:00] <user> message 13",
		"[00:00:00] <user> message 14",
		"[00:00:00] <user> message 15",
		"[00:00:00] <user> message 16",
		"[00:00:00] <user> message 17",
		"[00:00:00] <user> message 18",
		"[00:00:00] <user> message 19",
		"[00:00:00] <user> message 20",
		"[00:00:00] <user> message 21",
		"[00:00:00] <user> message 22",
		"[00:00:00] <user> message 23",
		"[00:00:00] <user> message 24",
		"[00:00:00] <user> message 25",
		"[00:00:00] <user> message 26",
		"[00:00:00] <user> message 27",
		"[00:00:00] <user> message 28",
		"[00:00:00] <user> message 29",
		"[00:00:00] <user> new message 30",
		"[00:00:00] <user> new message 31",
	}, visibleEventLines(v))
}

func TestChatView_mouse_wheel_scrolls_messages(t *testing.T) {
	msgs := make([]domain.Message, 30)
	for i := range msgs {
		msgs[i] = domain.Message{
			Target: "#general",
			From:   "user",
			Body:   fmt.Sprintf("message %d", i),
		}
	}

	cv := newChatViewWithEvents("#general", "testuser", "", messagesToEvents(msgs))
	var m ui.Model = cv
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{X: 20, Y: 0, Width: 60, Height: 24}})

	m, _ = m.Update(tea.MouseMsg{
		X:      25,
		Y:      10,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonWheelUp,
	})

	v := m.View(60, 24)
	require.Equal(t, numberedUserMessages("message", 4, 22), visibleEventsWithoutTimestamps(v))
	require.Equal(t, "(57%)", scrollIndicatorLine(v))
}

func TestChatView_mouse_click_accepts_popover_suggestion(t *testing.T) {
	var m ui.Model = components.NewChatView[testKind](nilEvents, "#general", domain.KindChannel, "testuser", "")
	nodes := []*command.Node[testKind]{
		{
			Name: "join",
			Help: "Join a channel",
			Positionals: []command.Positional[testKind]{
				{Name: "channel", Source: command.LiteralSource[testKind](
					command.Suggestion{Value: "#general", Label: "#general"},
					command.Suggestion{Value: "#random", Label: "#random"},
				)},
			},
		},
	}
	m, _ = m.Update(components.CommandsMsg[testKind]{Commands: nodes})
	m, _ = m.Update(components.CompleterMsg{
		Completer: command.CompletionSet[testKind]{Set: command.Set[testKind]{Commands: nodes}, Ctx: testKindChannel},
	})

	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{X: 20, Y: 0, Width: 60, Height: 24}})
	m = typeText(t, m, "/jo")

	var cmd tea.Cmd
	m, cmd = m.Update(tea.MouseMsg{
		X:      24,
		Y:      22,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})

	require.NotNil(t, cmd, "mouse click should produce a cmd")
	m, _ = m.Update(cmd())

	m = typeText(t, m, "#general")
	_, cmd = enter(t, m)

	require.NotNil(t, cmd)
	sub := cmd().(components.CommandSubmitMsg)
	require.Equal(t, "/join #general", sub.Raw)
}

func TestContainsHighlightWord(t *testing.T) {
	tests := []struct {
		name   string
		words  []string
		nick   domain.Nick
		body   string
		expect bool
	}{
		{
			name:   "dollar_nick_matches_user_nick",
			words:  []string{"$nick"},
			nick:   "testuser",
			body:   "hey testuser check this out",
			expect: true,
		},
		{
			name:   "literal_word_matches",
			words:  []string{"check"},
			nick:   "testuser",
			body:   "hey testuser check this out",
			expect: true,
		},
		{
			name:   "case_insensitive",
			words:  []string{"CHECK"},
			nick:   "testuser",
			body:   "hey testuser check this out",
			expect: true,
		},
		{
			name:   "no_match",
			words:  []string{"foobar"},
			nick:   "testuser",
			body:   "hey testuser check this out",
			expect: false,
		},
		{
			name:   "no_highlight_words_configured",
			words:  nil,
			nick:   "testuser",
			body:   "hey testuser check this out",
			expect: false,
		},
		{
			name:   "empty_nick_placeholder_ignored",
			words:  []string{"$nick"},
			nick:   "",
			body:   "hey testuser check this out",
			expect: false,
		},
		{
			name:   "multiple_words_any_matches",
			words:  []string{"nope", "check"},
			nick:   "testuser",
			body:   "hey testuser check this out",
			expect: true,
		},
		{
			name:   "formatting codes are ignored for matching",
			words:  []string{"testuser"},
			nick:   "testuser",
			body:   "hey \x02testuser\x02 check this out",
			expect: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := components.ContainsHighlightWord(tt.body, tt.words, tt.nick)
			require.Equal(t, tt.expect, got)
		})
	}
}

func TestRenderLine_preserves_irc_formatting_in_plain_output(t *testing.T) {
	rendered := renderSingleEvent(domain.StoredEvent{
		Event: domain.Message{
			Target: "#test",
			From:   "alice",
			Body:   "hello \x02bold\x02 \x1funder\x1f \x1estrike\x1e",
			At:     time.Date(2026, 4, 12, 11, 0, 0, 0, time.UTC),
		},
	})

	require.Equal(t, []string{"2026-04-12 11:00 <alice> hello bold under strike"}, chatSegments(rendered))
}
