package modelmanager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

// ReflectionMode is the configured persona-reflection mode.
type ReflectionMode = config.ReflectionMode

const (
	// ReflectionDisabled records candidates and schedules no provider work.
	ReflectionDisabled = config.ReflectionDisabled
	// ReflectionShadow validates and records proposals without changing state.
	ReflectionShadow = config.ReflectionShadow
	// ReflectionActive commits proposals that pass deterministic validation.
	ReflectionActive = config.ReflectionActive
)

// InvalidReflectionModeError reports an unsupported configured mode.
type InvalidReflectionModeError = config.InvalidReflectionModeError

// ParseReflectionMode parses the persisted and command-line spelling of a mode.
func ParseReflectionMode(value string) (ReflectionMode, error) {
	return config.ParseReflectionMode(value)
}

// SetReflectionMode changes whether reflection work is scheduled and whether
// validated proposals may mutate persona lineage.
func (m *Manager) SetReflectionMode(ctx context.Context, mode ReflectionMode) error {
	mode = mode.Resolved()

	m.mu.Lock()
	previous := m.reflections
	if mode == ReflectionDisabled {
		m.reflections = nil
	} else if m.reflections == nil {
		stored, ok := m.store.(reflectionStateStore)
		if !ok {
			m.mu.Unlock()
			return errors.New("persona reflection store is unavailable")
		}
		m.reflections = newReflectionScheduler(
			m.lifecycleContext, stored, m.now, m.runReflection,
		)
	}
	m.reflectionMode = mode
	scheduler := m.reflections
	m.mu.Unlock()

	if mode == ReflectionDisabled {
		if previous != nil {
			return previous.stop(ctx)
		}

		return nil
	}

	return m.wakeStoredInstances(ctx, scheduler)
}

// wakeStoredInstances starts a worker for every model instance the store
// holds. It runs when reflection is enabled and when the manager starts,
// which are the two moments a backlog can already be waiting.
//
// The candidate stream is recorded whatever the mode, so an operator
// turning reflection on has history to work from. A worker exists only
// once something wakes it, and nothing else does until the instance's
// next delivery, so without this an instance already over its threshold
// would sit there until somebody spoke to it. An instance on no channel
// has no delivery coming at all.
func (m *Manager) wakeStoredInstances(ctx context.Context, scheduler *reflectionScheduler) error {
	if scheduler == nil {
		return nil
	}

	instances, err := m.store.ListInstances(ctx)
	if err != nil {
		return fmt.Errorf("list instances to schedule reflection: %w", err)
	}

	for _, instance := range instances {
		if instance.IsModel() {
			scheduler.notify(instance.ID())
		}
	}

	return nil
}

// SetReflectionModel changes the model used by future reflection attempts.
// Empty selects the configured small model at the start of each attempt.
func (m *Manager) SetReflectionModel(modelID domain.ModelID) {
	m.mu.Lock()
	m.reflectionModel = domain.ModelID(strings.TrimSpace(string(modelID)))
	m.mu.Unlock()
}

// ReflectionSettings returns the current mode and the configured model. An
// empty model means reflection follows the small-model setting.
func (m *Manager) ReflectionSettings() (ReflectionMode, domain.ModelID) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.reflectionMode, m.reflectionModel
}

const (
	reflectionFailureNoClient     = "api_client_unavailable"
	reflectionFailureNoCapability = "reflection_capability_unavailable"
	reflectionFailureModel        = "reflection_model_unavailable"
	reflectionFailureUpstream     = "upstream_failed"
	reflectionFailureResponse     = "response_unreadable"
	reflectionFailureStore        = "store_failed"
	reflectionDiscardDisabled     = "reflection_disabled"
)

// reflectionRecordTimeout bounds the write that records a terminal
// outcome. Disabling reflection and draining the manager both cancel the
// context a run is executing under, so the record is written under a
// context detached from it.
const reflectionRecordTimeout = 5 * time.Second

func (m *Manager) runReflection(
	ctx context.Context,
	snapshot store.PendingReflectionSnapshot,
) {
	stored, ok := m.store.(reflectionStateStore)
	if !ok {
		return
	}
	startMode, modelID := m.ReflectionSettings()
	if modelID == "" {
		modelID = m.SmallModel()
	}
	runID := m.reflectionRunID()
	startedAt := m.now()
	baseRun := reflectionRunFor(snapshot, runID, modelID, startedAt)

	client := m.APIClient()
	if client == nil {
		m.recordFailedReflection(ctx, stored, baseRun, reflectionFailureNoClient)
		return
	}
	generator, ok := client.(api.ReflectionGenerator)
	if !ok {
		m.recordFailedReflection(ctx, stored, baseRun, reflectionFailureNoCapability)
		return
	}
	if err := m.EnsureToolCapableModel(ctx, modelID); err != nil {
		m.recordFailedReflection(ctx, stored, baseRun, reflectionFailureModel)
		return
	}

	input, aliases := reflectionAPIInput(snapshot)
	tools := newReflectionTools(stored, snapshot, aliases)
	result, err := m.reflect(ctx, generator, modelID, snapshot, input, tools)
	finishedAt := m.now()
	baseRun.FinishedAt = finishedAt
	if err != nil {
		reason := reflectionFailureUpstream
		var parseErr *api.CompletionParseError
		if errors.As(err, &parseErr) {
			reason = reflectionFailureResponse
		}
		m.recordFailedReflection(ctx, stored, baseRun, reason)
		return
	}
	baseRun.ProposedExperiences = len(result.Proposal.Experiences)
	baseRun.ProposedAmendments = len(result.Proposal.Amendments)
	validation := reflectionValidationInput{
		RunID: runID, ModelID: modelID,
		StartedAt: startedAt, FinishedAt: finishedAt,
		Snapshot: snapshot, Proposal: result.Proposal, Aliases: aliases,
	}
	acceptance, err := validateReflectionProposal(validation)
	if err != nil {
		var validationErr *ReflectionValidationError
		if errors.As(err, &validationErr) {
			baseRun.Outcome = domain.ReflectionRejected
			baseRun.RejectionReason = fmt.Sprintf(
				"%s:%s", validationErr.Reason, validationErr.Field,
			)
			m.recordReflectionRun(ctx, stored, baseRun)
			return
		}
		m.recordFailedReflection(ctx, stored, baseRun, reflectionFailureStore)
		return
	}

	currentMode, _ := m.ReflectionSettings()
	if startMode == ReflectionShadow || currentMode == ReflectionShadow {
		baseRun.Outcome = domain.ReflectionShadow
		m.recordReflectionRun(ctx, stored, baseRun)
		return
	}
	if startMode != ReflectionActive || currentMode != ReflectionActive {
		baseRun.Outcome = domain.ReflectionDiscarded
		baseRun.RejectionReason = reflectionDiscardDisabled
		m.recordReflectionRun(ctx, stored, baseRun)
		return
	}
	commit, err := stored.CommitPersonaReflection(ctx, acceptance)
	if err != nil {
		if errors.Is(err, store.ErrPersonaLineageChanged) {
			baseRun.Outcome = domain.ReflectionStale
			baseRun.RejectionReason = "persona_lineage_changed"
			m.recordReflectionRun(ctx, stored, baseRun)

			return
		}
		// The instance's own traffic trimmed the evidence out of the
		// stream while the run was deciding on it. That is the state the
		// next run starts from, which is what a stale outcome says.
		if errors.Is(err, store.ErrReflectionSourceMissing) {
			baseRun.Outcome = domain.ReflectionStale
			baseRun.RejectionReason = "reflection_source_trimmed"
			m.recordReflectionRun(ctx, stored, baseRun)

			return
		}
		m.recordFailedReflection(ctx, stored, baseRun, reflectionFailureStore)

		return
	}

	m.noticeReflectionRun(ctx, commit.Run)
}

// reflect runs one reflection end to end: the instance explores its own
// past through the recall tools, then proposes.
//
// Exploration and the proposal are separate requests. Exploration offers
// tools and sets no response format; the proposal sets the schema and
// offers no tools. A model advertising both capabilities need not honour
// them in one request, and `supported_parameters` reports each on its own
// and cannot say.
//
// The loop stops offering further turns at [maxReflectionToolTurns].
// Results from the batch it stopped on go to the proposal call, so the
// conversation never ends on a tool call nothing answered.
func (m *Manager) reflect(
	ctx context.Context,
	generator api.ReflectionGenerator,
	modelID domain.ModelID,
	snapshot store.PendingReflectionSnapshot,
	input api.ReflectionInput,
	tools *reflectionTools,
) (api.ReflectionResult, error) {
	definitions := tools.definitions()
	exploration, err := generator.ReflectPersona(
		ctx, modelID, snapshot.Persona.Lineage.InstanceID, input, definitions...,
	)
	if err != nil {
		return api.ReflectionResult{}, err
	}

	var results []api.ToolResult
	for turn := 1; len(exploration.PendingToolCalls) > 0; turn++ {
		results = tools.execute(ctx, exploration.PendingToolCalls)
		if turn == maxReflectionToolTurns {
			break
		}
		exploration, err = generator.ContinueReflection(
			ctx, exploration.Conversation, results, definitions...,
		)
		if err != nil {
			return api.ReflectionResult{}, err
		}
		results = nil
	}

	return generator.ProposeReflection(
		ctx, exploration.Conversation, results, m.reflectionSchemaTransport(modelID),
	)
}

// reflectionSchemaTransport decides how the proposal schema reaches the
// model. The catalogue answers whether the model takes a strict
// `response_format`; where it does not, the schema goes in the text of
// the proposal instruction. The proposal is validated either way, so a
// model without the capability is not held to a weaker contract.
func (m *Manager) reflectionSchemaTransport(
	modelID domain.ModelID,
) api.StructuredOutputSupport {
	info, known := m.catalogueLookup(modelID)
	if known && info.SupportsStructuredOutputs() {
		return api.WithStructuredOutput
	}

	return api.WithoutStructuredOutput
}

func reflectionRunFor(
	snapshot store.PendingReflectionSnapshot,
	runID domain.ReflectionRunID,
	modelID domain.ModelID,
	startedAt time.Time,
) domain.ReflectionRun {
	return domain.ReflectionRun{
		ID: runID, InstanceID: snapshot.Persona.Lineage.InstanceID,
		BaseRevisionID:   snapshot.Persona.Revision.ID,
		PriorCheckpoint:  snapshot.Range.Checkpoint,
		HighWaterMark:    snapshot.Range.Through,
		ResultRevisionID: snapshot.Persona.Revision.ID,
		ModelID:          modelID, StartedAt: startedAt, FinishedAt: startedAt,
	}
}

// reflectionAPIInput renders one snapshot as a provider request, and
// returns the token table the validator resolves the resulting proposal
// against. Every identifier the request would otherwise carry is replaced
// by a per-run token; see [reflectionAliases].
func reflectionAPIInput(
	snapshot store.PendingReflectionSnapshot,
) (api.ReflectionInput, *reflectionAliases) {
	aliases := newReflectionAliases(snapshot.Persona.Lineage.InstanceID)

	events := make([]api.ReflectionInputEvent, 0, len(snapshot.Events))
	for _, event := range snapshot.Events {
		events = append(events, reflectionInputEvent(event, aliases))
	}

	experiences := make(
		[]api.ReflectionInputExperience, 0, len(snapshot.Persona.Experiences),
	)
	for _, experience := range snapshot.Persona.Experiences {
		entry := api.ReflectionInputExperience{
			Kind: experience.Kind, Summary: experience.Summary,
			Confidence: experience.Confidence, OccurredAt: experience.OccurredAt,
			Sources: make([]domain.ReflectionSequence, 0, len(experience.Sources)),
		}
		if experience.SubjectID != nil {
			entry.Subject = aliases.participant(*experience.SubjectID, "")
		}
		for _, source := range experience.Sources {
			entry.Sources = append(entry.Sources, source.Sequence)
		}
		experiences = append(experiences, entry)
	}

	amendments := make(
		[]api.ReflectionInputAmendment, 0, len(snapshot.Persona.Amendments),
	)
	for _, amendment := range snapshot.Persona.Amendments {
		entry := api.ReflectionInputAmendment{
			Token: aliases.amendment(amendment.ID),
			Scope: amendment.Scope, Tendency: amendment.Tendency,
			Confidence: amendment.Confidence, CreatedAt: amendment.CreatedAt,
			ExpiresAt: amendment.ExpiresAt,
		}
		if amendment.Counterpart != nil {
			entry.Counterpart = aliases.participant(*amendment.Counterpart, "")
		}
		amendments = append(amendments, entry)
	}

	return api.ReflectionInput{
		Description:  snapshot.Persona.Revision.Description,
		Baseline:     snapshot.Persona.Lineage.Baseline,
		Participants: aliases.participants,
		Experiences:  experiences,
		Amendments:   amendments,
		Events:       events,
	}, aliases
}

// reflectionInputEvent renders one candidate under the run's tokens. The
// request and every recall tool result go through it, so an event the
// instance reads back is spelled the way the request spelled it.
func reflectionInputEvent(
	event store.ReflectionEvent,
	aliases *reflectionAliases,
) api.ReflectionInputEvent {
	message := event.Message
	participant := ""
	if id, identified := message.Source.InstanceID(); identified {
		participant = aliases.participant(id, message.Source.Nick())
	}
	message.Source = message.Source.WithoutInstanceID()

	return api.ReflectionInputEvent{
		Sequence:    event.Sequence,
		WindowKind:  protocol.WindowTargetKind(event.Source.Window),
		Window:      aliases.window(event.Source.Window),
		Participant: participant,
		Message:     message, Substantive: event.Substantive,
	}
}

func (m *Manager) recordFailedReflection(
	ctx context.Context,
	stored reflectionStateStore,
	run domain.ReflectionRun,
	reason string,
) {
	run.Outcome = domain.ReflectionFailed
	run.RejectionReason = reason
	run.FinishedAt = m.now()
	m.recordReflectionRun(ctx, stored, run)
}

func (m *Manager) recordReflectionRun(
	ctx context.Context,
	stored reflectionStateStore,
	run domain.ReflectionRun,
) {
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), reflectionRecordTimeout,
	)
	defer cancel()

	if err := stored.RecordReflectionRun(ctx, run); err != nil {
		slog.Default().ErrorContext(ctx, "record reflection run",
			"component", "modelmanager",
			"instance_id", run.InstanceID,
			"outcome", run.Outcome,
			"error", err,
		)
	}

	m.noticeReflectionRun(ctx, run)
}

// noticeReflectionRun reports one run's terminal outcome to the
// operators. `reflection_runs` is the durable record, and nothing
// reads it until somebody runs `/persona` against the instance, so
// without this an operator watching models change has no line to
// connect the change to.
func (m *Manager) noticeReflectionRun(ctx context.Context, run domain.ReflectionRun) {
	m.operatorNotices().NoticeOperators(ctx, domain.SystemNotice{
		Target: domain.StatusChannelName,
		Text:   reflectionRunNotice(m.reflectionActorName(ctx, run.InstanceID), run),
		At:     run.FinishedAt,
	})
}

// reflectionActorName is what the notice calls the reflecting
// instance. An instance that has quit since the run started has no
// row to read a nick from, so it is named by its identifier, which is
// what `reflection_runs` is keyed by.
func (m *Manager) reflectionActorName(ctx context.Context, id domain.InstanceID) string {
	instances, err := m.store.ListInstances(ctx)
	if err != nil {
		return string(id)
	}
	for _, instance := range instances {
		if instance.ID() == id {
			return string(instance.Nick())
		}
	}

	return string(id)
}

// reflectionRunNotice renders one terminal outcome for the operator's
// status window. It uses the vocabulary `/persona` renders a run
// under: revisions, experiences and tendencies.
func reflectionRunNotice(actor string, run domain.ReflectionRun) string {
	var text strings.Builder
	fmt.Fprintf(&text, "Reflection for %s %s", actor, reflectionOutcomeSummary(run.Outcome))
	switch run.Outcome {
	case domain.ReflectionAccepted:
		fmt.Fprintf(&text, ": revision %d -> %d, %d experience(s), %d tendency change(s)",
			run.BaseRevisionID, run.ResultRevisionID,
			run.AcceptedExperiences, run.AcceptedAmendments,
		)
	case domain.ReflectionShadow:
		fmt.Fprintf(&text, ": %d experience(s) and %d tendency change(s) proposed",
			run.ProposedExperiences, run.ProposedAmendments,
		)
	}
	if run.RejectionReason != "" {
		fmt.Fprintf(&text, " (%s)", run.RejectionReason)
	}
	text.WriteString(".")

	return text.String()
}

// reflectionOutcomeSummary describes what became of one run's
// proposal, in the operator's terms.
func reflectionOutcomeSummary(outcome domain.ReflectionOutcome) string {
	switch outcome {
	case domain.ReflectionAccepted:
		return "accepted a revision"
	case domain.ReflectionNoChange:
		return "found nothing to record"
	case domain.ReflectionShadow:
		return "validated a proposal in shadow mode"
	case domain.ReflectionRejected:
		return "had its proposal refused"
	case domain.ReflectionStale:
		return "lost its proposal to a concurrent persona change"
	case domain.ReflectionDiscarded:
		return "discarded its proposal"
	case domain.ReflectionFailed:
		return "failed"
	}

	return string(outcome)
}
