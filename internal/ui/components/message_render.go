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

// renderChannelEvent renders a domain.PersistableEvent into a styled
// string at the given width. kind discriminates channel/DM from
// status rendering — see the SystemNotice case for the
// status-channel variant.
func renderChannelEvent[C command.KindProvider](
	event domain.PersistableEvent,
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
		ts := formatTimestampPrefix(e.At, timestampFormat, locale)
		highlighted := ContainsHighlightWord(e.Body, highlightWords, userNick)
		body := renderIRCBody(e.Body)

		seed := string(e.InstanceID)

		if e.Action {
			nick := theme.NickStyle(seed).Render(string(e.From))
			prefix := fmt.Sprintf("%s* %s", ts, nick)
			if highlighted {
				prefix = theme.Highlight.Render(strings.TrimSpace(prefix))
			}

			return wrap.Render(strings.TrimSpace(fmt.Sprintf("%s %s", prefix, body)))
		}

		nick := theme.NickStyle(seed).
			Render(fmt.Sprintf("<%s>", string(e.From)))
		prefix := ts + nick
		if highlighted {
			prefix = theme.Highlight.Render(strings.TrimSpace(prefix))
		}

		return wrap.Render(strings.TrimSpace(fmt.Sprintf("%s %s", prefix, body)))

	case domain.Join:
		text := fmt.Sprintf("%s has joined %s", e.Nick, e.Target)
		if e.Created {
			text = fmt.Sprintf("Created channel %s", e.Target)
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

	case domain.Part:
		text := fmt.Sprintf("%s has left %s", e.Nick, e.Target)
		if e.Message != "" {
			text += fmt.Sprintf(" (%s)", e.Message)
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

	case domain.Quit:
		text := fmt.Sprintf("%s has quit", e.Nick)
		if e.Message != "" {
			text += fmt.Sprintf(" (%s)", e.Message)
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

	case domain.TopicChange:
		var text string

		if e.Topic == "" {
			text = fmt.Sprintf("topic for %s cleared by %s", e.Target, e.By)
		} else if e.By != "" {
			text = fmt.Sprintf("topic for %s set by %s: %s", e.Target, e.By, e.Topic)
		} else {
			text = fmt.Sprintf("topic for %s set to: %s", e.Target, e.Topic)
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

	case domain.ModeChange:
		issuer := string(e.By)
		if e.ServerIssued() {
			issuer = "server"
		}

		target := string(e.Nick)
		if e.ChannelMode() && e.Param == "" {
			// Member-mode change (+o/+v on a nick in a channel) — the
			// nick is the target; channel context is implicit.
			target = string(e.Nick)
		} else if e.ChannelMode() {
			// Parametric attribute mode (+l 10, +k key) — the param
			// is the value, channel is the context.
			target = e.Param + " " + string(e.Target)
		} else if !e.ChannelMode() {
			// User-mode change — nick is the target.
			target = string(e.Nick)
		}

		return wrap.Render(theme.SystemEvent.Render(
			fmt.Sprintf("*** %s sets mode %s %s", issuer, e.Flag.IRCString(e.Add), target)))

	case domain.ModelInvited:
		return wrap.Render(theme.SystemEvent.Render(
			fmt.Sprintf("*** %s has joined %s", e.Nick, e.Target)))

	case domain.ModelKicked:
		return wrap.Render(theme.SystemEvent.Render(
			fmt.Sprintf("*** %s was kicked from %s by %s", e.Nick, e.Target, e.By)))

	case domain.NickChange:
		return wrap.Render(theme.SystemEvent.Render(
			fmt.Sprintf("*** %s is now known as %s", e.OldNick, e.NewNick)))

	case domain.TopicInfo:
		if e.Topic == "" {
			return wrap.Render(theme.SystemEvent.Render(
				fmt.Sprintf("*** No topic set for %s", e.Target)))
		}

		text := fmt.Sprintf("topic for %s: %s", e.Target, e.Topic)
		if e.TopicSetBy != "" {
			topicTime := timestamp.Format(e.TopicSetAt, timestampFormat, locale)
			if topicTime == "" {
				text += fmt.Sprintf(" (set by %s)", e.TopicSetBy)
			} else {
				text += fmt.Sprintf(" (set by %s on %s)", e.TopicSetBy, topicTime)
			}
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

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
		if e.Command != "" {
			return wrap.Render(theme.Warning.Render("⚠ usage: " + e.Usage))
		}

		return wrap.Render(theme.Warning.Render("⚠ " + e.Usage))

	case domain.SystemNotice:
		// On the status channel, system notices are operational
		// narration (connection events, config confirmations as
		// background chatter). They render in the shared server-event
		// class — the same "*** <text>" shape join/part/quit/mode/topic
		// use — so every line the server narrates reads as one visual
		// class, with no directional arrows or per-variant glyphs. On
		// regular channels and DMs the same notice is a direct
		// confirmation of a user action, so it keeps the ✓ tick; that
		// green glyph is reserved exclusively for user-action feedback.
		// System notices are always server-authored, so no kind carries
		// a nick prefix.
		switch kind {
		case domain.KindStatus:
			return wrap.Render(theme.SystemEvent.Render("*** " + e.Text))
		default:
			return wrap.Render(theme.Success.Render("✓ " + e.Text))
		}

	default:
		return ""
	}
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
		fmt.Sprintf("%s is %s", w.Nick, w.ModelID),
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
		"formatting                      M-B/M-I/M-U/M-R/M-S toggle styles",
		"formatting                      M-C colours, M-O clears formatting",
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
