package components

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/text/language"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/theme"
	"github.com/laney/modeloff/internal/ui/timestamp"
)

// renderChannelEvent renders a domain.ChannelEvent into a styled
// string at the given width.
func renderChannelEvent(
	event domain.ChannelEvent,
	width int,
	highlightWords []string,
	userNick domain.Nick,
	commands command.Set,
	timestampFormat *string,
	locale language.Tag,
) string {
	wrap := lipgloss.NewStyle().Width(width)

	switch e := event.(type) {
	case domain.ChannelMessage:
		ts := formatTimestampPrefix(e.At, timestampFormat, locale)
		highlighted := ContainsHighlightWord(e.Body, highlightWords, userNick)

		body := e.Body
		if highlighted {
			body = theme.Highlight.Render(body)
		}

		if e.Action {
			nick := theme.NickStyle(string(e.From)).Render(string(e.From))
			return wrap.Render(strings.TrimSpace(fmt.Sprintf("%s* %s %s", ts, nick, body)))
		}

		nick := theme.NickStyle(string(e.From)).
			Render(fmt.Sprintf("<%s>", string(e.From)))

		return wrap.Render(strings.TrimSpace(fmt.Sprintf("%s%s %s", ts, nick, body)))

	case domain.ChannelJoin:
		text := fmt.Sprintf("%s has joined %s", e.Nick, e.Channel)
		if e.Created {
			text = fmt.Sprintf("Created channel %s", e.Channel)
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

	case domain.ChannelPart:
		text := fmt.Sprintf("%s has left %s", e.Nick, e.Channel)
		if e.Message != "" {
			text += fmt.Sprintf(" (%s)", e.Message)
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

	case domain.ChannelQuit:
		text := fmt.Sprintf("%s has quit", e.Nick)
		if e.Message != "" {
			text += fmt.Sprintf(" (%s)", e.Message)
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

	case domain.ChannelTopicChange:
		var text string

		if e.Topic == "" {
			text = fmt.Sprintf("topic for %s cleared by %s", e.Channel, e.By)
		} else if e.By != "" {
			text = fmt.Sprintf("topic for %s set by %s: %s", e.Channel, e.By, e.Topic)
		} else {
			text = fmt.Sprintf("topic for %s set to: %s", e.Channel, e.Topic)
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

	case domain.ChannelModeChange:
		return wrap.Render(theme.SystemEvent.Render(
			fmt.Sprintf("*** %s sets mode %s %s", e.Channel, e.Mode, e.Nick)))

	case domain.ChannelModelInvited:
		text := fmt.Sprintf("%s (%s) has joined %s", e.Nick, e.ModelID, e.Channel)
		if e.Persona != "" {
			text = fmt.Sprintf("%s with persona %q", text, e.Persona)
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

	case domain.ChannelModelKicked:
		return wrap.Render(theme.SystemEvent.Render(
			fmt.Sprintf("*** %s has been kicked from %s", e.Nick, e.Channel)))

	case domain.ChannelNickChange:
		return wrap.Render(theme.SystemEvent.Render(
			fmt.Sprintf("*** %s is now known as %s", e.OldNick, e.NewNick)))

	case domain.ChannelTopicInfo:
		if e.Topic == "" {
			return wrap.Render(theme.SystemEvent.Render(
				fmt.Sprintf("*** No topic set for %s", e.Channel)))
		}

		text := fmt.Sprintf("topic for %s: %s", e.Channel, e.Topic)
		if e.TopicSetBy != "" {
			topicTime := timestamp.Format(e.TopicSetAt, timestampFormat, locale)
			if topicTime == "" {
				text += fmt.Sprintf(" (set by %s)", e.TopicSetBy)
			} else {
				text += fmt.Sprintf(" (set by %s on %s)", e.TopicSetBy, topicTime)
			}
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

	case domain.ChannelHelp:
		return wrap.Render(renderHelp(commands))

	case domain.ChannelWhois:
		return wrap.Render(renderWhoisEvent(e))

	case domain.ChannelListOutput:
		return wrap.Render(renderChannelListEvent(e))

	case domain.ChannelPersonasList:
		return wrap.Render(renderPersonasListEvent(e))

	case domain.ChannelCommandError:
		return wrap.Render(theme.Error.Render("✗ " + e.Err))

	case domain.ChannelUsageHint:
		if e.Command != "" {
			return wrap.Render(theme.Warning.Render("⚠ usage: " + e.Usage))
		}

		return wrap.Render(theme.Warning.Render("⚠ " + e.Usage))

	case domain.ChannelSystemNotice:
		return wrap.Render(theme.Success.Render("✓ " + e.Text))

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

func renderWhoisEvent(w domain.ChannelWhois) string {
	lines := []string{
		fmt.Sprintf("%s is %s", w.Instance.Nick, w.Instance.ModelID),
	}

	if w.Instance.Persona != "" {
		lines = append(lines, fmt.Sprintf("  persona: %s", w.Instance.Persona))
	}

	if w.Instance.Channels != nil && w.Instance.Channels.Len() > 0 {
		var chStrs []string
		for pair := w.Instance.Channels.Oldest(); pair != nil; pair = pair.Next() {
			chStrs = append(chStrs, string(pair.Key))
		}

		lines = append(lines, fmt.Sprintf("  channels: %s", strings.Join(chStrs, ", ")))
	}

	var parts []string
	for _, line := range lines {
		parts = append(parts, theme.SystemEvent.Render("*** "+line))
	}

	return strings.Join(parts, "\n")
}

func renderChannelListEvent(cl domain.ChannelListOutput) string {
	if len(cl.Channels) == 0 {
		return theme.SystemEvent.Render("*** no channels")
	}

	var parts []string
	for _, ch := range cl.Channels {
		line := string(ch.Name)
		if ch.Topic != "" {
			line += " — " + ch.Topic
		}

		parts = append(parts, theme.SystemEvent.Render("*** "+line))
	}

	return strings.Join(parts, "\n")
}

func renderPersonasListEvent(pl domain.ChannelPersonasList) string {
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

func renderHelp(commands command.Set) string {
	lines := make([]string, 0, len(commands.Commands))
	for _, node := range commands.Commands {
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

	lower := strings.ToLower(body)

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
