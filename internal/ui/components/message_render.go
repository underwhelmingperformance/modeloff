package components

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/theme"
)

// renderLine renders a single chat line into a styled string at the
// given width.
func renderLine(line tea.Msg, width int, highlightWords []string, userNick domain.Nick, commands command.Set) string {
	wrap := lipgloss.NewStyle().Width(width)

	switch l := line.(type) {
	case MessageLine:
		ts := theme.Dim.Render(l.Message.SentAt.Format("[15:04:05]"))
		highlighted := ContainsHighlightWord(l.Message.Body, highlightWords, userNick)

		body := l.Message.Body
		if highlighted {
			body = theme.Highlight.Render(body)
		}

		if l.Message.Action {
			nick := theme.NickStyle(string(l.Message.From)).
				Render(string(l.Message.From))
			return wrap.Render(fmt.Sprintf("%s * %s %s", ts, nick, body))
		}

		nick := theme.NickStyle(string(l.Message.From)).
			Render(fmt.Sprintf("<%s>", string(l.Message.From)))

		return wrap.Render(fmt.Sprintf("%s %s %s", ts, nick, body))

	case Join:
		text := fmt.Sprintf("%s has joined %s", l.Nick, l.Channel)
		if l.Created {
			text = fmt.Sprintf("Created channel %s", l.Channel)
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

	case Part:
		return wrap.Render(theme.SystemEvent.Render(
			fmt.Sprintf("*** %s has left %s", l.Nick, l.Channel)))

	case NickChange:
		return wrap.Render(theme.SystemEvent.Render(
			fmt.Sprintf("*** %s is now known as %s", l.OldNick, l.NewNick)))

	case TopicChange:
		var text string

		if l.Topic == "" {
			text = fmt.Sprintf("topic for %s cleared by %s", l.Channel, l.By)
		} else if l.By != "" {
			text = fmt.Sprintf("topic for %s set by %s: %s", l.Channel, l.By, l.Topic)
		} else {
			text = fmt.Sprintf("topic for %s set to: %s", l.Channel, l.Topic)
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

	case TopicInfo:
		if l.Channel.Topic == "" {
			return wrap.Render(theme.SystemEvent.Render(
				fmt.Sprintf("*** No topic set for %s", l.Channel.Name)))
		}

		text := fmt.Sprintf("topic for %s: %s", l.Channel.Name, l.Channel.Topic)
		if l.Channel.TopicSetBy != "" {
			text += fmt.Sprintf(" (set by %s on %s)",
				l.Channel.TopicSetBy, l.Channel.TopicSetAt.Format("2006-01-02 15:04"))
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

	case ModelInvited:
		text := fmt.Sprintf("%s (%s) has joined %s",
			l.Instance.Nick, l.Instance.ModelID, l.Channel)
		if l.Instance.Persona != "" {
			text = fmt.Sprintf("%s with persona %q", text, l.Instance.Persona)
		}

		return wrap.Render(theme.SystemEvent.Render("*** " + text))

	case ModelKicked:
		return wrap.Render(theme.SystemEvent.Render(
			fmt.Sprintf("*** %s has been kicked from %s", l.Nick, l.Channel)))

	case Help:
		return wrap.Render(renderHelp(commands))

	case Whois:
		return wrap.Render(renderWhois(l))

	case ChannelList:
		return wrap.Render(renderChannelList(l))

	case APIKeySaved:
		return wrap.Render(theme.Success.Render(
			"✓ OpenRouter API key saved and activated."))

	case PokeIntervalSet:
		return wrap.Render(theme.Success.Render(
			fmt.Sprintf("✓ Poke interval set to %s.", l.Interval)))

	case NickModelSet:
		return wrap.Render(theme.Success.Render(
			fmt.Sprintf("✓ Nick generation model set to %s.", l.ModelID)))

	case DMOpened:
		return wrap.Render(theme.Success.Render(
			fmt.Sprintf("✓ Opened direct message with %s", l.Nick)))

	case UsageHint:
		return wrap.Render(theme.Warning.Render("⚠ usage: " + l.Usage))

	case NoChannel:
		return wrap.Render(theme.Warning.Render("⚠ join a channel first"))

	case CommandError:
		return wrap.Render(theme.Error.Render("✗ " + l.Err.Error()))

	case ConfigChanged:
		return wrap.Render(theme.Success.Render("✓ " + l.Operation))

	case BackendError:
		return wrap.Render(theme.Error.Render(
			fmt.Sprintf("✗ %s: %s", l.Operation, l.Err)))

	case NewMessagesDivider:
		return renderNewMessagesDivider(width)

	default:
		return ""
	}
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

func renderHelp(commands command.Set) string {
	lines := make([]string, 0, len(commands.Commands))
	for _, node := range commands.Commands {
		usage := node.Usage()

		line := usage
		if node.Help != "" {
			line = fmt.Sprintf("%-32s %s", usage, node.Help)
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

func renderWhois(w Whois) string {
	lines := []string{
		fmt.Sprintf("%s is %s", w.Nick, w.ModelID),
	}

	if w.Persona != "" {
		lines = append(lines, fmt.Sprintf("  persona: %s", w.Persona))
	}

	if len(w.Channels) > 0 {
		var chStrs []string
		for ch := range w.Channels.Sorted() {
			chStrs = append(chStrs, string(ch))
		}

		lines = append(lines, fmt.Sprintf("  channels: %s", strings.Join(chStrs, ", ")))
	}

	var parts []string
	for _, line := range lines {
		parts = append(parts, theme.SystemEvent.Render("*** "+line))
	}

	return strings.Join(parts, "\n")
}

func renderChannelList(cl ChannelList) string {
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

func renderNewMessagesDivider(width int) string {
	label := theme.Warning.Render(" new messages ")
	labelWidth := lipgloss.Width(label)

	leftWidth := (width - labelWidth) / 2
	rightWidth := width - leftWidth - labelWidth

	left := strings.Repeat("─", max(0, leftWidth))
	right := strings.Repeat("─", max(0, rightWidth))

	return theme.Dim.Render(left) + label + theme.Dim.Render(right)
}

