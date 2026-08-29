package screens

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
	uitimestamp "github.com/laney/modeloff/internal/ui/timestamp"
)

// routeConfigResults answers the results a `/config` change reports,
// plus the persona-pool results `/templates` and
// `/regenerate-templates` share with `/config persona`. Each renders a
// confirmation line in the window the command was issued from. Three
// of the settings are ones the running screen reads for itself, the
// API key, the highlight words and the timestamp format, and those
// also republish the state they moved.
func (s ChatScreen) routeConfigResults(
	issuingWindow domain.Window,
	msg tea.Msg,
) (ChatScreen, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case chatcmd.APIKeySetResult:
		next, cmd := s.handleAPIKeySet(issuingWindow, msg)
		return next, cmd, true

	case chatcmd.PokeIntervalSetResult:
		return s, s.notice(issuingWindow, settingNotice("Poke interval", humanDuration(msg.Interval), msg.Reset)), true

	case chatcmd.DrainTimeoutSetResult:
		return s, s.notice(issuingWindow, settingNotice("Drain timeout", humanDuration(msg.Timeout), msg.Reset)), true

	case chatcmd.SmallModelSetResult:
		return s, s.notice(issuingWindow, settingNotice("Small model", string(msg.ModelID), msg.Reset)), true

	case chatcmd.EmbeddingModelSetResult:
		return s, s.notice(issuingWindow, settingNotice("Embedding model", string(msg.ModelID), msg.Reset)), true

	case chatcmd.ReflectionModeSetResult:
		return s, s.notice(issuingWindow, settingNotice("Reflection mode", msg.Mode.String(), msg.Reset)), true

	case chatcmd.ReflectionModelSetResult:
		return s, s.notice(issuingWindow, settingNotice("Reflection model", reflectionModelNotice(msg.ModelID), msg.Reset)), true

	case chatcmd.BaseURLSetResult:
		return s, s.notice(issuingWindow, settingNotice("Base URL", msg.URL, msg.Reset)), true

	case chatcmd.HighlightWordsSetResult:
		next, cmd := s.handleHighlightWordsSet(issuingWindow, msg)
		return next, cmd, true

	case chatcmd.TimestampFormatSetResult:
		return s, s.handleTimestampFormatSet(issuingWindow, msg), true

	case chatcmd.PersonaTemplatesResult:
		templatesList := domain.PersonaTemplatesList{
			Templates: msg,
			At:        time.Now(),
		}

		return s, tea.Batch(
			s.logReplyEvent(issuingWindow, templatesList),
			s.recordReply(nil, templatesList),
		), true

	case chatcmd.PersonaTemplatesRegeneratedResult:
		return s, s.notice(issuingWindow,
			fmt.Sprintf("Replaced %d generated persona templates.", msg.Count)), true

	case chatcmd.PersonaTemplateSavedResult:
		return s, s.notice(issuingWindow, fmt.Sprintf("Persona %s saved.", msg.ID)), true

	case chatcmd.PersonaTemplatesResetResult:
		return s, s.notice(issuingWindow, fmt.Sprintf("Removed %d user-defined persona(s).", msg.Count)), true

	case chatcmd.PersonaResult:
		return s, s.notice(issuingWindow, formatPersonaResult(msg)), true
	}

	return s, nil, false
}

func reflectionModelNotice(modelID domain.ModelID) string {
	if modelID == "" {
		return "small-model"
	}

	return string(modelID)
}

// formatAmendmentDeparture renders the recorded removal kind and time.
// Schema v20 could backfill consolidations only, so a retraction or
// supersession recorded before that migration has no reason to show.
func formatAmendmentDeparture(departure *domain.AmendmentDeparture) string {
	if departure == nil {
		return " (departure not recorded)"
	}

	return fmt.Sprintf(" (%s %s)", departure.Kind, departure.At.Format(time.RFC3339))
}

func formatPersonaResult(result chatcmd.PersonaResult) string {
	inspection := result.Inspection
	var text strings.Builder
	fmt.Fprintf(&text, "Persona for %s", inspection.Nick)
	switch result.Action {
	case chatcmd.PersonaReset:
		text.WriteString(" reset")
	case chatcmd.PersonaRolledBack:
		text.WriteString(" rolled back")
	case chatcmd.PersonaDescribed:
		text.WriteString(" described")
	case chatcmd.PersonaInspected:
	}
	fmt.Fprintf(
		&text, ": revision %d; checkpoint %d.\nPersona: %s",
		inspection.Revision.ID, inspection.Lineage.Checkpoint,
		inspection.Revision.Description,
	)
	if len(inspection.Revision.DescriptionEvidence) > 0 {
		fmt.Fprintf(
			&text, "\nBuilt from experiences: %s",
			formatExperienceIDs(inspection.Revision.DescriptionEvidence),
		)
	}
	if parent := inspection.Parent; parent != nil &&
		parent.Description != inspection.Revision.Description {
		fmt.Fprintf(
			&text, "\nRevision %d said: %s", parent.ID, parent.Description,
		)
	}
	fmt.Fprintf(&text, "\nReset baseline: %s", inspection.Lineage.Baseline)
	if inspection.Lineage.Template == nil {
		// Not "none". A null provenance means the lineage predates the
		// column, or was written from operator-supplied text, or had no
		// persona available, and the row does not say which. An instance
		// migrated from before the column may well have come from a pool
		// row, so claiming it started from nothing would be false.
		text.WriteString("\nTemplate: not recorded")
	} else {
		fmt.Fprintf(
			&text, "\nTemplate: %s (%s; sha256 %s)",
			inspection.Lineage.Template.ID, inspection.Lineage.Template.Origin,
			inspection.Lineage.Template.DescriptionHash,
		)
	}

	text.WriteString("\nExperiences:")
	if len(inspection.Experiences) == 0 {
		text.WriteString(" none")
	}
	for _, experience := range inspection.Experiences {
		fmt.Fprintf(
			&text, "\n- #%d [%s/%s; sources %s] %s",
			experience.ID, formatExperienceKind(experience, inspection.Counterparts),
			experience.Confidence,
			formatReflectionSources(experience.Sources), experience.Summary,
		)
	}

	text.WriteString("\nTendencies:")
	if len(inspection.Amendments) == 0 {
		text.WriteString(" none")
	}
	for _, amendment := range inspection.Amendments {
		fmt.Fprintf(
			&text, "\n- #%d [%s/%s; evidence %s] %s",
			amendment.ID, formatAmendmentScope(amendment, inspection.Counterparts),
			amendment.Confidence, formatExperienceIDs(amendment.Evidence),
			amendment.Tendency,
		)
	}

	if len(inspection.Departed) > 0 {
		text.WriteString("\nTendencies this revision removed:")
	}
	for _, amendment := range inspection.Departed {
		fmt.Fprintf(
			&text, "\n- #%d [%s/%s; evidence %s] %s%s",
			amendment.ID, formatAmendmentScope(amendment, inspection.Counterparts),
			amendment.Confidence, formatExperienceIDs(amendment.Evidence),
			amendment.Tendency, formatAmendmentDeparture(amendment.Departure),
		)
	}

	text.WriteString("\nRecent reflections:")
	if len(inspection.RecentRuns) == 0 {
		text.WriteString(" none")
	}
	for _, run := range inspection.RecentRuns {
		experiences, tendencies := reflectionRunCounts(run)
		fmt.Fprintf(
			&text,
			"\n- %s: %s via %s, revision %d -> %d, %s, %s, finished %s",
			run.ID, run.Outcome, run.ModelID, run.BaseRevisionID,
			run.ResultRevisionID, experiences, tendencies,
			run.FinishedAt.Format(time.RFC3339),
		)
		if run.RejectionReason != "" {
			fmt.Fprintf(&text, " (%s)", run.RejectionReason)
		}
	}

	text.WriteString("\nRevision transitions:")
	if len(inspection.Transitions) == 0 {
		text.WriteString(" none")
	}
	for _, transition := range inspection.Transitions {
		fmt.Fprintf(
			&text, "\n- %s: %d -> %d at %s",
			transition.Kind, transition.FromRevisionID, transition.ToRevisionID,
			transition.At.Format(time.RFC3339),
		)
	}

	return text.String()
}

func formatReflectionSources(sources []domain.ReflectionEventRef) string {
	values := make([]string, 0, len(sources))
	for _, source := range sources {
		values = append(values, strconv.FormatInt(int64(source.Sequence), 10))
	}

	return strings.Join(values, ", ")
}

func formatExperienceIDs(ids []domain.ExperienceID) string {
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		values = append(values, strconv.FormatInt(int64(id), 10))
	}

	return strings.Join(values, ", ")
}

// formatExperienceKind renders an experience's kind with the actor its subject
// names, and a preposition saying how that actor relates to the experience: an
// assertion is a claim the subject made, a relationship experience concerns
// the subject, and any other kind carrying a subject is about them. An
// experience with no subject renders its kind alone.
func formatExperienceKind(
	experience domain.Experience,
	counterparts []domain.PersonaCounterpart,
) string {
	if experience.SubjectID == nil {
		return string(experience.Kind)
	}

	preposition := "about"
	switch experience.Kind {
	case domain.ExperienceAssertion:
		preposition = "by"
	case domain.ExperienceRelationship:
		preposition = "with"
	case domain.ExperienceObservation, domain.ExperienceInterpretation:
	}

	return fmt.Sprintf(
		"%s %s %s", experience.Kind, preposition,
		counterpartNick(*experience.SubjectID, counterparts),
	)
}

// counterpartNick gives the current nick for an actor the persona lineage
// names. An actor that has quit has no instance row left, so the inspection
// carries no counterpart for it.
func counterpartNick(
	id domain.InstanceID,
	counterparts []domain.PersonaCounterpart,
) string {
	for _, counterpart := range counterparts {
		if counterpart.InstanceID == id {
			return string(counterpart.Nick)
		}
	}

	return "departed counterpart"
}

func formatAmendmentScope(
	amendment domain.PersonaAmendment,
	counterparts []domain.PersonaCounterpart,
) string {
	if amendment.Scope != domain.AmendmentRelationship || amendment.Counterpart == nil {
		return string(amendment.Scope)
	}

	return fmt.Sprintf(
		"relationship with %s",
		counterpartNick(*amendment.Counterpart, counterparts),
	)
}

// notice renders a one-line confirmation in the window the command was
// issued from, or in `&modeloff` when the user has no window open.
func (s ChatScreen) notice(window domain.Window, text string) tea.Cmd {
	return s.logReplyEvent(window, domain.SystemNotice{
		Target: issuingWindowName(window),
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
func (s ChatScreen) handleAPIKeySet(
	issuingWindow domain.Window,
	msg chatcmd.APIKeySetResult,
) (ChatScreen, tea.Cmd) {
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
			s.ensurePersonaTemplates(),
			msgCmd(components.SetPlaceholderMsg{
				Text: s.checklist.text(),
			}),
		)
	}

	return s, tea.Batch(
		rebind,
		s.notice(issuingWindow, text),
		s.loadLiveModels(),
		s.ensurePersonaTemplates(),
	)
}

// handleHighlightWordsSet caches the new highlight set on the screen,
// which is what the per-message mention check reads, and publishes it
// to the renderer.
func (s ChatScreen) handleHighlightWordsSet(
	issuingWindow domain.Window,
	msg chatcmd.HighlightWordsSetResult,
) (ChatScreen, tea.Cmd) {
	s.highlightWords = msg.Words

	text := fmt.Sprintf("Highlight words set to: %s.", humanWordList(msg.Words))
	if msg.Reset {
		text = fmt.Sprintf("Highlight words reset to: %s.", humanWordList(msg.Words))
	}

	return s, tea.Batch(
		s.notice(issuingWindow, text),
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
func (s ChatScreen) handleTimestampFormatSet(
	issuingWindow domain.Window,
	msg chatcmd.TimestampFormatSetResult,
) tea.Cmd {
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
		s.notice(issuingWindow, text),
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

// reflectionRunCounts describes what one run produced.
//
// A shadow run accepts nothing by design, so reporting its accepted
// counts shows an operator nothing happening. Shadow is the mode an
// operator picks to watch reflection before letting it commit, so a
// shadow run reports what it proposed.
func reflectionRunCounts(run domain.ReflectionRun) (string, string) {
	experiences, tendencies := run.AcceptedExperiences, run.AcceptedAmendments
	if run.Outcome == domain.ReflectionShadow {
		experiences, tendencies = run.ProposedExperiences, run.ProposedAmendments

		return pluralise(experiences, "experience proposed", "experiences proposed"),
			pluralise(tendencies, "tendency change proposed", "tendency changes proposed")
	}

	return pluralise(experiences, "experience", "experiences"),
		pluralise(tendencies, "tendency change", "tendency changes")
}

// pluralise renders a count with the word that agrees with it, so an
// operator never reads "1 experience(s)".
func pluralise(count int, singular, plural string) string {
	if count == 1 {
		return fmt.Sprintf("%d %s", count, singular)
	}

	return fmt.Sprintf("%d %s", count, plural)
}
