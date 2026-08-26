package modelmanager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

// ReflectionMode controls whether scheduled reflection is disabled, recorded
// without mutation, or committed after validation.
type ReflectionMode uint8

const (
	// ReflectionDisabled records candidates without scheduling provider work.
	ReflectionDisabled ReflectionMode = iota
	// ReflectionShadow validates and records proposals without changing state.
	ReflectionShadow
	// ReflectionActive commits proposals that pass deterministic validation.
	ReflectionActive
)

const (
	reflectionFailureNoClient     = "api_client_unavailable"
	reflectionFailureNoCapability = "reflection_capability_unavailable"
	reflectionFailureModel        = "reflection_model_unavailable"
	reflectionFailureUpstream     = "upstream_failed"
	reflectionFailureStore        = "store_failed"
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
	modelID := m.reflectionModel
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
	if err := m.EnsureStructuredOutputModel(ctx, modelID); err != nil {
		m.recordFailedReflection(ctx, stored, baseRun, reflectionFailureModel)
		return
	}

	input, aliases := reflectionAPIInput(snapshot)
	result, err := generator.ReflectPersona(
		ctx, modelID, snapshot.Persona.Lineage.InstanceID, input,
	)
	finishedAt := m.now()
	baseRun.FinishedAt = finishedAt
	if err != nil {
		m.recordFailedReflection(ctx, stored, baseRun, reflectionFailureUpstream)
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

	if m.reflectionMode == ReflectionShadow {
		baseRun.Outcome = domain.ReflectionShadow
		m.recordReflectionRun(ctx, stored, baseRun)
		return
	}
	if m.reflectionMode != ReflectionActive {
		return
	}
	if _, err := stored.CommitPersonaReflection(ctx, acceptance); err != nil {
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
	}
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
		message := event.Message
		participant := ""
		if id, identified := message.Source.InstanceID(); identified {
			participant = aliases.participant(id, message.Source.Nick())
		}
		message.Source = message.Source.WithoutInstanceID()
		events = append(events, api.ReflectionInputEvent{
			Sequence:    event.Sequence,
			WindowKind:  protocol.WindowTargetKind(event.Source.Window),
			Window:      aliases.window(event.Source.Window),
			Participant: participant,
			Message:     message, Substantive: event.Substantive,
		})
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
		Baseline:     snapshot.Persona.Lineage.Baseline,
		Participants: aliases.participants,
		Experiences:  experiences,
		Amendments:   amendments,
		Events:       events,
	}, aliases
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
}
