package screens

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
	uitimestamp "github.com/laney/modeloff/internal/ui/timestamp"
)

// routeConfigResults answers the results a `/config` change reports,
// plus the persona-pool results `/personas` and
// `/regenerate-personas` share with `/config persona`. Each renders a
// confirmation line in the window the command was issued from. Three
// of the settings are ones the running screen reads for itself, the
// API key, the highlight words and the timestamp format, and those
// also republish the state they moved.
func (s ChatScreen) routeConfigResults(msg tea.Msg) (ChatScreen, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case chatcmd.APIKeySetResult:
		next, cmd := s.handleAPIKeySet(msg)
		return next, cmd, true

	case chatcmd.PokeIntervalSetResult:
		return s, s.notice(settingNotice("Poke interval", humanDuration(msg.Interval), msg.Reset)), true

	case chatcmd.DrainTimeoutSetResult:
		return s, s.notice(settingNotice("Drain timeout", humanDuration(msg.Timeout), msg.Reset)), true

	case chatcmd.SmallModelSetResult:
		return s, s.notice(settingNotice("Small model", string(msg.ModelID), msg.Reset)), true

	case chatcmd.EmbeddingModelSetResult:
		return s, s.notice(settingNotice("Embedding model", string(msg.ModelID), msg.Reset)), true

	case chatcmd.BaseURLSetResult:
		return s, s.notice(settingNotice("Base URL", msg.URL, msg.Reset)), true

	case chatcmd.HighlightWordsSetResult:
		next, cmd := s.handleHighlightWordsSet(msg)
		return next, cmd, true

	case chatcmd.TimestampFormatSetResult:
		return s, s.handleTimestampFormatSet(msg), true

	case chatcmd.PersonasListResult:
		personasList := domain.PersonasList{
			Personas: msg,
			At:       time.Now(),
		}

		return s, tea.Batch(
			s.logAndShow(personasList),
			s.recordReply(personasList),
		), true

	case chatcmd.PersonasRegeneratedResult:
		return s, s.notice(fmt.Sprintf("Generated %d personas.", msg.Count)), true

	case chatcmd.PersonaSetResult:
		return s, s.notice(fmt.Sprintf("Persona %s saved.", msg.ID)), true

	case chatcmd.PersonaResetResult:
		return s, s.notice(fmt.Sprintf("Removed %d user-defined persona(s).", msg.Count)), true
	}

	return s, nil, false
}

// notice renders a one-line confirmation in the window the command was
// issued from, or in `&modeloff` when the user has no window open.
func (s ChatScreen) notice(text string) tea.Cmd {
	return s.logAndShow(domain.SystemNotice{
		Target: s.active,
		Text:   text,
		At:     time.Now(),
	})
}

// settingNotice renders the confirmation for one `/config` setting.
// `wasReset` picks the verb, so the user can tell a value they typed
// from the default `--reset` put back.
func settingNotice(subject, value string, wasReset bool) string {
	verb := "set"
	if wasReset {
		verb = "reset"
	}

	return fmt.Sprintf("%s %s to %s.", subject, verb, value)
}

// handleAPIKeySet takes the new key into the screen's own state: the
// welcome checklist tracks it, the model catalogue is dropped because
// it belongs to the key that fetched it, and a fresh load and persona
// seed run against the new one. With no channel open the user is on
// the welcome screen, where the checklist carries the confirmation and
// a notice would have nowhere to render.
func (s ChatScreen) handleAPIKeySet(msg chatcmd.APIKeySetResult) (ChatScreen, tea.Cmd) {
	text := "OpenRouter API key saved and activated."
	if msg.Reset {
		text = "OpenRouter API key cleared."
	}

	s.checklist.hasAPIKey = !msg.Reset

	var rebind tea.Cmd
	s, rebind = s.setLiveModels(nil, command.SuggestionStateReady)

	if s.realChannelCount() == 0 {
		return s, tea.Batch(
			rebind,
			s.loadLiveModels(),
			s.ensurePersonas(),
			msgCmd(components.SetPlaceholderMsg{
				Text: s.checklist.Render(),
			}),
		)
	}

	return s, tea.Batch(
		rebind,
		s.notice(text),
		s.loadLiveModels(),
		s.ensurePersonas(),
	)
}

// handleHighlightWordsSet caches the new highlight set on the screen,
// which is what the per-message mention check reads, and publishes it
// to the renderer.
func (s ChatScreen) handleHighlightWordsSet(msg chatcmd.HighlightWordsSetResult) (ChatScreen, tea.Cmd) {
	s.highlightWords = msg.Words

	text := fmt.Sprintf("Highlight words set to: %s.", humanWordList(msg.Words))
	if msg.Reset {
		text = fmt.Sprintf("Highlight words reset to: %s.", humanWordList(msg.Words))
	}

	return s, tea.Batch(
		s.notice(text),
		msgCmd(components.HighlightWordsMsg{
			Words:    msg.Words,
			UserNick: s.user.Nick(),
		}),
	)
}

// handleTimestampFormatSet confirms the new format and publishes it to
// the renderer. The three outcomes are a format the user typed, the
// default the `--reset` flag put back, and timestamps switched off
// with an empty format.
func (s ChatScreen) handleTimestampFormatSet(msg chatcmd.TimestampFormatSetResult) tea.Cmd {
	var text string

	switch {
	case msg.Reset:
		text = "Timestamp format reset to the default 24-hour clock."
	case msg.Format != nil && *msg.Format != "":
		text = fmt.Sprintf("Timestamp format set to %s.", *msg.Format)
	default:
		text = "Timestamps disabled."
	}

	return tea.Batch(
		s.notice(text),
		msgCmd(components.TimestampFormatMsg{
			Format: msg.Format,
			Locale: uitimestamp.CurrentLocale(),
		}),
	)
}

// humanDuration renders d for a `/config` confirmation without Go's
// trailing zero-value units: time.Duration.String() would print
// "1h0m0s" for an hour, where a person reads "1h". Only the
// hour/minute/second components that matter for the durations
// `/config` accepts (poke-interval, drain-timeout) are considered;
// a sub-second remainder falls back to Duration's own String.
func humanDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}

	sign := ""
	whole := d
	if d < 0 {
		sign = "-"
		whole = -d
	}

	hours := whole / time.Hour
	remainder := whole - hours*time.Hour
	minutes := remainder / time.Minute
	remainder -= minutes * time.Minute
	seconds := remainder / time.Second
	remainder -= seconds * time.Second

	if remainder != 0 {
		return d.String()
	}

	var parts []string
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if seconds > 0 {
		parts = append(parts, fmt.Sprintf("%ds", seconds))
	}

	return sign + strings.Join(parts, "")
}

// humanWordList renders a word list for a `/config` confirmation as
// a plain comma-separated sentence, not Go's `%v` slice rendering
// (e.g. "[alice bob $nick]").
func humanWordList(words []string) string {
	if len(words) == 0 {
		return "(none)"
	}

	return strings.Join(words, ", ")
}
