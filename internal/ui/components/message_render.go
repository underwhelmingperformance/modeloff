package components

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/text/language"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ircfmt"
	"github.com/laney/modeloff/internal/ui/theme"
	"github.com/laney/modeloff/internal/ui/timestamp"
)

// renderChannelEvent renders a domain.Event into a styled string at
// the given width. It accepts the renderable base so it can render
// both persisted channel events and the render-only UI feedback
// (`Help`, `UsageHint`) the chat-screen raises locally. kind
// discriminates channel/DM from status rendering — see
// [renderSystemNotice] for the status-channel variant.
func renderChannelEvent[C command.KindProvider](
	event domain.Event,
	kind domain.ChannelKind,
	width int,
	highlightWords []string,
	userNick domain.Nick,
	commands []*command.Node[C],
	timestampFormat *string,
	locale language.Tag,
) string {
	wrap := lipgloss.NewStyle().Width(width)

	switch e := event.(type) {
	case domain.Message:
		return wrap.Render(renderMessage(e, highlightWords, userNick, timestampFormat, locale))
	case domain.Join:
		return wrap.Render(theme.SystemEvent.Render("*** " + joinText(e)))
	case domain.Part:
		return wrap.Render(theme.SystemEvent.Render("*** " + partText(e)))
	case domain.Quit:
		return wrap.Render(theme.SystemEvent.Render("*** " + quitText(e)))
	case domain.TopicChange:
		return wrap.Render(theme.SystemEvent.Render("*** " + topicChangeText(e)))
	case domain.ChannelModeChange:
		return wrap.Render(theme.SystemEvent.Render("*** " + channelModeChangeText(e)))
	case domain.Invited:
		return wrap.Render(theme.SystemEvent.Render(
			fmt.Sprintf("*** %s invited %s to %s", e.By, e.Nick, e.Target)))
	case domain.Kicked:
		return wrap.Render(theme.SystemEvent.Render(
			fmt.Sprintf("*** %s was kicked from %s by %s", e.Nick, e.Target, e.By)))
	case domain.NickChange:
		return wrap.Render(theme.SystemEvent.Render(
			fmt.Sprintf("*** %s is now known as %s", e.OldNick, e.NewNick)))
	case domain.TopicInfo:
		return wrap.Render(theme.SystemEvent.Render("*** " + topicInfoText(e, timestampFormat, locale)))
	case domain.Help:
		return wrap.Render(renderHelp(commands))
	case domain.Whois:
		return wrap.Render(renderWhoisEvent(e))
	case domain.ListReply:
		return wrap.Render(renderListReplyEvent(e))
	case domain.ListEnd:
		return wrap.Render(theme.SystemEvent.Render("*** End of /list"))
	case domain.PersonasList:
		return wrap.Render(renderPersonasListEvent(e))
	case domain.CommandError:
		return wrap.Render(theme.Error.Render("✗ " + e.Err))
	case domain.UsageHint:
		return wrap.Render(renderUsageHint(e))
	case domain.SystemNotice:
		return wrap.Render(renderSystemNotice(e, kind))
	default:
		return ""
	}
}

// renderMessage builds the per-message line: timestamp prefix, nick
// in its theme colour, optional highlight ribbon, IRC-formatted body.
// The `* nick text` vs `<nick> text` shape switches on `e.Action`.
//
// The user's own instance carries no [domain.InstanceID] (the empty
// string is `protocol.UserClientID`'s sentinel), so `e.InstanceID ==
// ""` picks out the user's own messages and exempts them from
// highlight matching: the user should never see a ribbon or a
// mention on their own words.
func renderMessage(e domain.Message, highlightWords []string, userNick domain.Nick, timestampFormat *string, locale language.Tag) string {
	ts := timestampPrefixText(e.At, timestampFormat, locale)
	highlighted := e.InstanceID != "" && ContainsHighlightWord(e.Body, highlightWords, userNick)
	body := renderIRCBody(e.Body)
	style := nickStyleFor(e)

	nickText := fmt.Sprintf("<%s>", string(e.From))
	if e.Action {
		nickText = string(e.From)
	}

	if highlighted {
		// The raw timestamp and nick text are composed into one
		// string and styled with a single Render call. Styling each
		// fragment first and wrapping the already-rendered result
		// would embed an SGR reset from the inner style partway
		// through, cancelling the highlight colour for the rest of
		// the line.
		prefix := theme.Highlight.Render(messagePrefixText(ts, nickText, e.Action))
		return strings.TrimSpace(fmt.Sprintf("%s %s", prefix, body))
	}

	ts = theme.Dim.Render(ts)
	nick := style.Render(nickText)

	var prefix string
	if e.Action {
		prefix = fmt.Sprintf("%s* %s", ts, nick)
	} else {
		prefix = ts + nick
	}

	return strings.TrimSpace(fmt.Sprintf("%s %s", prefix, body))
}

// messagePrefixText composes the raw, unstyled prefix text (a
// timestamp followed by `* nick` or `<nick>`, per `action`) that
// [renderMessage] passes to a single [theme.Highlight] call.
func messagePrefixText(ts, nickText string, action bool) string {
	if action {
		return strings.TrimSpace(fmt.Sprintf("%s* %s", ts, nickText))
	}

	return strings.TrimSpace(ts + nickText)
}

// anonymousNickStyle is the one colour every `+a`-masked line renders
// in. `+a` (RFC 2811 §4.2.1) exists so members cannot tell senders
// apart; a colour that still varied by sender's instance id would
// leak that distinction back through the palette.
var anonymousNickStyle = theme.Dim.Bold(true)

// nickStyleFor picks the nick colour for a message. The session
// masks an anonymous channel's `From` to [domain.AnonymousNick] but
// leaves `InstanceID` carrying the real sender, since the stored
// event keeps the real origin for audit; the renderer must not seed
// the colour hash from it once the line is masked.
func nickStyleFor(e domain.Message) lipgloss.Style {
	if e.From == domain.AnonymousNick {
		return anonymousNickStyle
	}

	return theme.NickStyle(string(e.InstanceID))
}

func joinText(e domain.Join) string {
	if e.Created {
		return fmt.Sprintf("Created channel %s", e.Target)
	}
	return fmt.Sprintf("%s has joined %s", e.Nick, e.Target)
}

func partText(e domain.Part) string {
	text := fmt.Sprintf("%s has left %s", e.Nick, e.Target)
	if e.Message != "" {
		text += fmt.Sprintf(" (%s)", e.Message)
	}
	return text
}

func quitText(e domain.Quit) string {
	text := fmt.Sprintf("%s has quit", e.Nick)
	if e.Message != "" {
		text += fmt.Sprintf(" (%s)", e.Message)
	}
	return text
}

func topicChangeText(e domain.TopicChange) string {
	if e.Topic == "" {
		return fmt.Sprintf("topic for %s cleared by %s", e.Target, e.By)
	}
	if e.By != "" {
		return fmt.Sprintf("topic for %s set by %s: %s", e.Target, e.By, e.Topic)
	}
	return fmt.Sprintf("topic for %s set to: %s", e.Target, e.Topic)
}

func channelModeChangeText(e domain.ChannelModeChange) string {
	issuer := string(e.By)
	if e.ServerIssued() {
		issuer = "server"
	}

	flag := e.Flag.IRCString(e.Add)
	if operand := channelModeChangeOperand(e); operand != "" {
		flag += " " + operand
	}

	return fmt.Sprintf("%s sets mode %s on %s", issuer, flag, e.Target)
}

// channelModeChangeOperand formats the argument between the mode
// flag and the channel name on a rendered `MODE` line: the affected
// nick for a member mode (+o/+v), the parameter for a parametric
// attribute mode (+l, +k, +f), or nothing for a boolean attribute
// mode (+i, +m, ...).
func channelModeChangeOperand(e domain.ChannelModeChange) string {
	if e.Param != "" {
		return e.Param
	}
	return string(e.Nick)
}

func topicInfoText(e domain.TopicInfo, timestampFormat *string, locale language.Tag) string {
	if e.Topic == "" {
		return fmt.Sprintf("No topic set for %s", e.Target)
	}

	text := fmt.Sprintf("topic for %s: %s", e.Target, e.Topic)
	if e.TopicSetBy == "" {
		return text
	}

	topicTime := timestamp.Format(e.TopicSetAt, timestampFormat, locale)
	if topicTime == "" {
		return text + fmt.Sprintf(" (set by %s)", e.TopicSetBy)
	}
	return text + fmt.Sprintf(" (set by %s on %s)", e.TopicSetBy, topicTime)
}

func renderUsageHint(e domain.UsageHint) string {
	if e.Command != "" {
		return theme.Warning.Render("⚠ usage: " + e.Usage)
	}
	return theme.Warning.Render("⚠ " + e.Usage)
}

// renderSystemNotice picks the visual class for a system notice.
// On the status channel, notices are operational narration
// (connection events, config confirmations as background chatter)
// and render in the shared server-event class — the same
// "*** <text>" shape join/part/quit/mode/topic use — so every line
// the server narrates reads as one visual class, with no directional
// arrows or per-variant glyphs. On regular channels and DMs the
// same notice is a direct confirmation of a user action, so it
// keeps the ✓ tick; that green glyph is reserved exclusively for
// user-action feedback. System notices are always server-authored,
// so no kind carries a nick prefix.
func renderSystemNotice(e domain.SystemNotice, kind domain.ChannelKind) string {
	if kind == domain.KindStatus {
		return theme.SystemEvent.Render("*** " + e.Text)
	}
	return theme.Success.Render("✓ " + e.Text)
}

// timestampPrefixText renders the raw, unstyled timestamp prefix for
// a message line: the formatted time followed by a space, or "" when
// timestamps are disabled. [renderMessage] styles it, either on its
// own (theme.Dim) or as part of a single highlighted-prefix Render
// call.
func timestampPrefixText(at time.Time, format *string, locale language.Tag) string {
	rendered := timestamp.Format(at, format, locale)
	if rendered == "" {
		return ""
	}

	return rendered + " "
}

func renderWhoisEvent(w domain.Whois) string {
	lines := []string{
		fmt.Sprintf("%s is %s", w.Nick, whoisSubject(w.ModelID)),
	}

	if w.Persona != "" {
		lines = append(lines, fmt.Sprintf("  persona: %s", w.Persona))
	}

	if len(w.Channels) > 0 {
		strs := make([]string, len(w.Channels))
		for i, ch := range w.Channels {
			strs[i] = string(ch)
		}

		lines = append(lines, fmt.Sprintf("  channels: %s", strings.Join(strs, ", ")))
	}

	var parts []string
	for _, line := range lines {
		parts = append(parts, theme.SystemEvent.Render("*** "+line))
	}

	return strings.Join(parts, "\n")
}

// whoisSubject names what a `/whois` reply's nick is: a model id, or
// the human user's instance, which carries no [domain.ModelID].
func whoisSubject(modelID domain.ModelID) string {
	if modelID == "" {
		return "the human user"
	}
	return string(modelID)
}

func renderListReplyEvent(r domain.ListReply) string {
	line := fmt.Sprintf("%s (%d)", r.Channel, r.Members)
	if r.Topic != "" {
		line += " — " + r.Topic
	}

	return theme.SystemEvent.Render("*** " + line)
}

func renderPersonasListEvent(pl domain.PersonasList) string {
	if len(pl.Personas) == 0 {
		return theme.SystemEvent.Render("*** No personas defined.")
	}

	var parts []string
	for _, p := range pl.Personas {
		line := fmt.Sprintf("%s (%s): %s", p.ID, p.Origin, p.Description)
		parts = append(parts, theme.SystemEvent.Render("*** "+line))
	}

	return strings.Join(parts, "\n")
}

func renderHelp[C command.KindProvider](commands []*command.Node[C]) string {
	lines := make([]string, 0, len(commands))
	for _, node := range commands {
		full := node.FullUsage()

		line := full
		if node.Help != "" {
			line = fmt.Sprintf("%-32s %s", full, node.Help)
		}

		lines = append(lines, strings.TrimRight(line, " "))
	}

	if len(lines) == 0 {
		lines = []string{"/help                            Show available commands."}
	}

	lines = append(lines,
		"formatting                      M-b/M-i/M-u/M-r/M-s toggle styles",
		"formatting                      M-c colours, M-o clears formatting",
	)

	var parts []string
	for _, line := range lines {
		parts = append(parts, theme.SystemEvent.Render("*** "+line))
	}

	return strings.Join(parts, "\n")
}

func renderNewMessagesDivider(width int) string {
	return centeredDivider(width, theme.Warning.Render(" new messages "))
}

// renderDayChangedDivider marks a date rollover between two
// consecutive events in a window's scrollback, irssi's convention
// for keeping the date visible without repeating it on every line
// now that [timestamp.DefaultFormat] shows only the time of day.
func renderDayChangedDivider(width int, at time.Time, locale language.Tag) string {
	return centeredDivider(width, theme.Dim.Render(" "+timestamp.FormatDate(at, locale)+" "))
}

// centeredDivider draws label centred on a horizontal rule of dashes
// spanning width, the shape both [renderNewMessagesDivider] and
// [renderDayChangedDivider] use.
func centeredDivider(width int, label string) string {
	labelWidth := lipgloss.Width(label)

	leftWidth := (width - labelWidth) / 2
	rightWidth := width - leftWidth - labelWidth

	left := strings.Repeat("─", max(0, leftWidth))
	right := strings.Repeat("─", max(0, rightWidth))

	return theme.Dim.Render(left) + label + theme.Dim.Render(right)
}

// ContainsHighlightWord reports whether body contains any of the
// given highlight words as a whole word: a highlight word matches
// only where it is bounded by a non-word character (or the start/end
// of the text) on each side, so "art" does not match inside "start".
// The placeholder "$nick" is expanded to the provided userNick.
// Matching is case-insensitive.
func ContainsHighlightWord(body string, words []string, userNick domain.Nick) bool {
	if len(words) == 0 {
		return false
	}

	lowerText := strings.ToLower(ircfmt.Strip(body))

	for _, word := range words {
		w := word
		if w == "$nick" {
			w = string(userNick)
		}

		if w == "" {
			continue
		}

		if containsWholeWord(lowerText, w) {
			return true
		}
	}

	return false
}

// containsWholeWord reports whether word occurs in text bounded by a
// non-word rune (or the start/end of text) on each side. Matching is
// case-insensitive and Unicode-aware: a word rune is a letter, digit,
// or underscore.
func containsWholeWord(text, word string) bool {
	lowerText := strings.ToLower(text)
	lowerWord := strings.ToLower(word)

	for start := 0; start <= len(lowerText); {
		idx := strings.Index(lowerText[start:], lowerWord)
		if idx < 0 {
			return false
		}

		idx += start

		before := rune(' ')
		if idx > 0 {
			before, _ = utf8.DecodeLastRuneInString(lowerText[:idx])
		}

		afterIdx := idx + len(lowerWord)

		after := rune(' ')
		if afterIdx < len(lowerText) {
			after, _ = utf8.DecodeRuneInString(lowerText[afterIdx:])
		}

		if !isWordRune(before) && !isWordRune(after) {
			return true
		}

		// This occurrence's boundary failed; retry from the next byte
		// so an overlapping later occurrence still gets a chance, e.g.
		// word "art" against text "xartart y" first finds "art" at 1
		// (bounded by 'x' and 'a', both word runes, so it fails) and
		// must still find the one starting at 4.
		start = idx + 1
	}

	return false
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

func renderIRCBody(body string) string {
	document := ircfmt.Parse(body)
	var builder strings.Builder

	for lineIndex := range document.LineCount() {
		line := document.Line(lineIndex)
		for _, span := range line.Spans {
			builder.WriteString(styleForAttrs(span.Attrs).Render(span.Text))
		}
		builder.WriteByte('\n')
	}

	return strings.TrimSuffix(builder.String(), "\n")
}
