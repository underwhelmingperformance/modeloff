package components

import (
	"fmt"
	"strings"
	"time"

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
func renderMessage(e domain.Message, highlightWords []string, userNick domain.Nick, timestampFormat *string, locale language.Tag) string {
	ts := formatTimestampPrefix(e.At, timestampFormat, locale)
	highlighted := ContainsHighlightWord(e.Body, highlightWords, userNick)
	body := renderIRCBody(e.Body)
	style := nickStyleFor(e)

	var prefix string
	if e.Action {
		nick := style.Render(string(e.From))
		prefix = fmt.Sprintf("%s* %s", ts, nick)
	} else {
		nick := style.Render(fmt.Sprintf("<%s>", string(e.From)))
		prefix = ts + nick
	}

	if highlighted {
		prefix = theme.Highlight.Render(strings.TrimSpace(prefix))
	}

	return strings.TrimSpace(fmt.Sprintf("%s %s", prefix, body))
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

func formatTimestampPrefix(at time.Time, format *string, locale language.Tag) string {
	rendered := timestamp.Format(at, format, locale)
	if rendered == "" {
		return ""
	}

	return theme.Dim.Render(rendered + " ")
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
	label := theme.Warning.Render(" new messages ")
	labelWidth := lipgloss.Width(label)

	leftWidth := (width - labelWidth) / 2
	rightWidth := width - leftWidth - labelWidth

	left := strings.Repeat("─", max(0, leftWidth))
	right := strings.Repeat("─", max(0, rightWidth))

	return theme.Dim.Render(left) + label + theme.Dim.Render(right)
}

// ContainsHighlightWord reports whether body contains any of the
// given highlight words. The placeholder "$nick" is expanded to the
// provided userNick. Matching is case-insensitive.
func ContainsHighlightWord(body string, words []string, userNick domain.Nick) bool {
	if len(words) == 0 {
		return false
	}

	lower := strings.ToLower(ircfmt.Strip(body))

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
