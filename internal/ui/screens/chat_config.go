package screens

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
	uitimestamp "github.com/laney/modeloff/internal/ui/timestamp"
	"golang.org/x/text/language"
)

// routeConfigResults handles the result a `/config` change reports,
// and the one `/persona` reports. Each renders into the window the
// command was issued from: a line naming the setting's new value for a
// `/config` change, and the persona diagnostics [formatPersonaResult]
// builds for `/persona`. Three of the settings reach the running
// screen as well as the store: setting the API key or the highlight
// words updates what the screen holds, and setting the API key or the
// timestamp format sends the components that render from it a message
// of their own.
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
		next, cmd := s.handleTimestampFormatSet(issuingWindow, msg)
		return next, cmd, true

	case chatcmd.PersonaResult:
		return s, s.notice(
			issuingWindow,
			formatPersonaResult(msg, s.timestampFormat, s.locale),
		), true
	}

	return s, nil, false
}

func reflectionModelNotice(modelID domain.ModelID) string {
	if modelID == "" {
		return "small-model"
	}

	return string(modelID)
}

// formatAmendmentDeparture renders why a tendency is no longer active,
// and the revision that removed it. Schema v20 could backfill
// consolidations only, so a retraction or a supersession recorded
// before that migration has no recorded departure, and the tendency
// renders with nothing after it.
func formatAmendmentDeparture(
	departure *domain.AmendmentDeparture,
	revision domain.PersonaRevisionID,
) string {
	if departure == nil {
		return ""
	}

	return fmt.Sprintf("  %s r%d", departure.Kind, revision)
}

// formatPersonaResult renders an instance's persona lineage as a
// header, the description in force under it, and one section for each
// list the lineage holds. Fields within a row are separated by two
// spaces and appear in the same order on every row, so an operator
// comparing confidences or revision numbers reads down a column.
//
// Times use the operator's configured format, so an inspection reads in
// the same clock as every other line in the window.
func formatPersonaResult(
	result chatcmd.PersonaResult,
	timestampFormat *string,
	locale language.Tag,
) string {
	inspection := result.Inspection
	at := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}

		return uitimestamp.Format(t, timestampFormat, locale)
	}

	var text strings.Builder

	kind, _ := revisionArrivedBy(result)
	fmt.Fprintf(&text, "%s\n\n  %s\n", columns(
		fmt.Sprintf("%s  r%d", inspection.Nick, inspection.Revision.ID),
		string(kind),
		at(inspection.Revision.CreatedAt),
	), inspection.Revision.Description)

	field := func(label, value string) {
		fmt.Fprintf(&text, "\n%-11s %s", label, value)
	}

	if len(inspection.Revision.DescriptionEvidence) > 0 {
		field("built from", formatExperienceIDs(inspection.Revision.DescriptionEvidence))
	}

	if parent := inspection.Parent; parent != nil &&
		parent.Description != inspection.Revision.Description {
		field(fmt.Sprintf("parent r%d", parent.ID), parent.Description)
	}

	field("baseline", inspection.Lineage.Baseline)

	text.WriteString("\n\nexperiences")
	if len(inspection.Experiences) == 0 {
		text.WriteString("\n  none")
	}

	for _, experience := range inspection.Experiences {
		fmt.Fprintf(
			&text, "\n  #%d  %s  %s  %s",
			experience.ID, formatExperienceKind(experience, inspection.Counterparts),
			experience.Confidence, experience.Summary,
		)
	}

	text.WriteString("\n\ntendencies")
	if len(inspection.Amendments) == 0 {
		text.WriteString("\n  none")
	}

	for _, amendment := range inspection.Amendments {
		fmt.Fprintf(
			&text, "\n  #%d  %s  %s  %s",
			amendment.ID, formatAmendmentScope(amendment, inspection.Counterparts),
			amendment.Confidence, amendment.Tendency,
		)
	}

	if len(inspection.Departed) > 0 {
		text.WriteString("\n\ntendencies removed")
	}

	for _, amendment := range inspection.Departed {
		fmt.Fprintf(
			&text, "\n  #%d  %s%s",
			amendment.ID, amendment.Tendency,
			formatAmendmentDeparture(amendment.Departure, inspection.Revision.ID),
		)
	}

	text.WriteString("\n\nreflections")
	if len(inspection.RecentRuns) == 0 {
		text.WriteString("\n  none")
	}

	for _, run := range inspection.RecentRuns {
		experiences, tendencies := reflectionRunCounts(run)
		fmt.Fprintf(&text, "\n  %s", columns(
			at(run.FinishedAt), string(run.Outcome),
			fmt.Sprintf("r%d -> r%d", run.BaseRevisionID, run.ResultRevisionID),
			experiences, tendencies, string(run.ModelID),
		))

		if run.RejectionReason != "" {
			fmt.Fprintf(&text, " (%s)", run.RejectionReason)
		}
	}

	// Every accepted reflection in the section above wrote a
	// transition, so listing all transitions here would report each one
	// twice. A revision pointer an operator moved by hand appears
	// nowhere else, and those are what remain.
	operatorMoves := make([]domain.PersonaTransition, 0, len(inspection.Transitions))
	for _, transition := range inspection.Transitions {
		if transition.Kind != domain.PersonaTransitionReflection {
			operatorMoves = append(operatorMoves, transition)
		}
	}

	if len(operatorMoves) > 0 {
		text.WriteString("\n\noperator changes")
	}

	for _, transition := range operatorMoves {
		fmt.Fprintf(&text, "\n  %s", columns(
			at(transition.At), string(transition.Kind),
			fmt.Sprintf("r%d -> r%d", transition.FromRevisionID, transition.ToRevisionID),
		))
	}

	return text.String()
}

// columns joins a row's fields with two spaces, dropping any the caller
// left empty. An operator can switch timestamps off, which empties the
// leading field of most rows, so a row has to read correctly without
// it.
func columns(fields ...string) string {
	present := make([]string, 0, len(fields))
	for _, field := range fields {
		if field != "" {
			present = append(present, field)
		}
	}

	return strings.Join(present, "  ")
}

// revisionArrivedBy reports what made this revision the active one.
//
// When the command wrote a revision, the answer is that write, because
// an operator who has just reset or rolled back needs to see which of
// the two happened. When the command only inspected, the answer is the
// kind of the transition into the active revision. Revision zero was
// never moved to, and retention can trim an older transition, so this
// reports false when neither source has an answer.
func revisionArrivedBy(result chatcmd.PersonaResult) (domain.PersonaTransitionKind, bool) {
	switch result.Action {
	case chatcmd.PersonaReset:
		return domain.PersonaTransitionReset, true
	case chatcmd.PersonaRolledBack:
		return domain.PersonaTransitionRollback, true
	case chatcmd.PersonaDescribed:
		return domain.PersonaTransitionOperator, true
	case chatcmd.PersonaInspected:
	}

	for _, transition := range slices.Backward(result.Inspection.Transitions) {
		if transition.ToRevisionID == result.Inspection.Revision.ID {
			return transition.Kind, true
		}
	}

	return "", false
}

func formatExperienceIDs(ids []domain.ExperienceID) string {
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		values = append(values, "#"+strconv.FormatInt(int64(id), 10))
	}

	return strings.Join(values, " ")
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

	s, rebind := s.setLiveModels(nil, command.SuggestionStateReady)

	// Clearing the cached models publishes a completer, and so does the
	// load, when its upstream call comes back. The popover keeps
	// whichever reaches it last, so the cleared one has to be delivered
	// before the load starts.
	refresh := tea.Sequence(rebind, s.loadLiveModels())

	if s.realChannelCount() == 0 {
		return s, tea.Batch(
			refresh,
			msgCmd(components.SetPlaceholderMsg{
				Text: s.checklist.text(),
			}),
		)
	}

	return s, tea.Batch(refresh, s.notice(issuingWindow, text))
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
) (ChatScreen, tea.Cmd) {
	s.timestampFormat = msg.Format

	var text string

	switch {
	case msg.Reset:
		text = "Timestamp format reset to the default 24-hour clock."
	case msg.Format != nil && *msg.Format != "":
		text = fmt.Sprintf("Timestamp format set to %s.", *msg.Format)
	default:
		text = "Timestamps disabled."
	}

	return s, tea.Batch(
		s.notice(issuingWindow, text),
		msgCmd(components.TimestampFormatMsg{
			Format: s.timestampFormat,
			Locale: s.locale,
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
