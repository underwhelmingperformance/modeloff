package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/laney/modeloff/internal/domain"
)

const reflectionRunRetentionHeadroom = 100

// ErrPersonaLineageChanged reports that a reflection result was based on an
// older revision or checkpoint.
var ErrPersonaLineageChanged = errors.New("persona lineage changed")

// ErrReflectionSourceOwnership reports a cited event belonging to another
// instance's private candidate stream.
var ErrReflectionSourceOwnership = errors.New("reflection source belongs to another instance")

// ErrReflectionSourceMissing reports a cited event the stream no longer
// holds. Retention trims the stream on every append, keeping what an
// accepted experience cites, and a run's own citations do not exist yet
// while it is deciding on them. A run whose evidence is trimmed away
// before it commits has lost a race with its instance's own traffic.
var ErrReflectionSourceMissing = errors.New("reflection source no longer exists")

// PersonaExperienceDraft is one validated experience awaiting durable IDs.
type PersonaExperienceDraft struct {
	Key        string
	Kind       domain.ExperienceKind
	Summary    string
	SubjectID  *domain.InstanceID
	Confidence domain.Confidence
	OccurredAt time.Time
	Sources    []domain.ReflectionEventRef
}

// PersonaAmendmentDraft is one validated amendment awaiting durable IDs.
type PersonaAmendmentDraft struct {
	Scope        domain.AmendmentScope
	Counterpart  *domain.InstanceID
	Tendency     string
	Confidence   domain.Confidence
	EvidenceKeys []string
	EvidenceIDs  []domain.ExperienceID
	ExpiresAt    *time.Time
	SupersedesID *domain.PersonaAmendmentID
}

// PersonaReflectionAcceptance is one fully validated reflection result. The
// store still checks ownership, the base revision, and the checkpoint inside
// its transaction.
//
// Description is the replacement persona text. A nil Description means the run
// proposed no persona change, and the next revision keeps the parent's
// description and the citations that came with it.
//
// DescriptionEvidenceKeys names the experiences this run proposed that the
// description was built from. The store records them as given: whether a
// description has to cite anything at all is the reflection contract's rule,
// checked before the acceptance reaches here.
//
// Consolidate names the tendencies the new description absorbs. Each one
// leaves the active set and keeps its row and its evidence, so an operator
// can still read what the description was built from. Leaving them active
// would count them against the amendment cap, which refuses a run once it
// is full.
type PersonaReflectionAcceptance struct {
	RunID                   domain.ReflectionRunID
	InstanceID              domain.InstanceID
	BaseRevisionID          domain.PersonaRevisionID
	PriorCheckpoint         domain.ReflectionSequence
	HighWaterMark           domain.ReflectionSequence
	ModelID                 domain.ModelID
	StartedAt               time.Time
	FinishedAt              time.Time
	Description             *string
	DescriptionEvidenceKeys []string
	Experiences             []PersonaExperienceDraft
	Amendments              []PersonaAmendmentDraft
	Retract                 []domain.PersonaAmendmentID
	Consolidate             []domain.PersonaAmendmentID

	// RecalledSources is every event the run read back through its recall
	// tools. The experiences those events are behind have their salience
	// refreshed, so an instance that keeps returning to an episode keeps
	// it live.
	RecalledSources []domain.ReflectionSequence
}

// PersonaReflectionCommit is the complete durable state produced by one
// accepted or no-change reflection.
type PersonaReflectionCommit struct {
	Lineage     domain.PersonaLineage
	Revision    domain.PersonaRevision
	Experiences []domain.Experience
	Amendments  []domain.PersonaAmendment
	Run         domain.ReflectionRun
}

// PersonaSnapshot is one coherent read of the active persona revision and its
// bounded state.
type PersonaSnapshot struct {
	Lineage     domain.PersonaLineage
	Revision    domain.PersonaRevision
	Experiences []domain.Experience
	Amendments  []domain.PersonaAmendment
}

// PersonaSnapshot returns one transactionally coherent view of an instance's
// active persona lineage.
func (s *SQLiteStore) PersonaSnapshot(
	ctx context.Context,
	instanceID domain.InstanceID,
) (PersonaSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PersonaSnapshot{}, fmt.Errorf("begin persona snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	return personaSnapshotTx(ctx, tx, instanceID)
}

func personaSnapshotTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID domain.InstanceID,
) (PersonaSnapshot, error) {
	state, err := personaLineageTx(ctx, tx, instanceID)
	if err != nil {
		return PersonaSnapshot{}, err
	}
	revision, err := personaRevisionTx(ctx, tx, state.CurrentRevisionID)
	if err != nil {
		return PersonaSnapshot{}, err
	}
	experiences, err := personaExperiencesTx(ctx, tx, revision.ExperienceIDs)
	if err != nil {
		return PersonaSnapshot{}, err
	}
	amendments, err := personaAmendmentsTx(ctx, tx, revision.AmendmentIDs)
	if err != nil {
		return PersonaSnapshot{}, err
	}

	return PersonaSnapshot{
		Lineage: state, Revision: revision,
		Experiences: experiences, Amendments: amendments,
	}, nil
}

// CommitPersonaReflection atomically records accepted experiences and
// amendments, creates the next revision when needed, and advances the
// reflection checkpoint.
//
// Repeating one RunID returns the first committed result, for as long as
// that run is one of the newest `reflectionRunRetentionHeadroom` this
// instance has recorded. Retention removes the row the answer is read
// from, so a retry arriving after that many later attempts is a new
// commit against a lineage that has moved, and is refused with
// [ErrPersonaLineageChanged]. A retry follows its own attempt by
// seconds, so what bounds the guarantee is the instance's own reflection
// rate.
func (s *SQLiteStore) CommitPersonaReflection(
	ctx context.Context,
	acceptance PersonaReflectionAcceptance,
) (PersonaReflectionCommit, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PersonaReflectionCommit{}, fmt.Errorf("begin persona reflection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, found, err := personaReflectionCommitTx(ctx, tx, acceptance.RunID)
	if err != nil {
		return PersonaReflectionCommit{}, err
	}
	if found {
		if existing.Run.InstanceID != acceptance.InstanceID {
			return PersonaReflectionCommit{}, fmt.Errorf(
				"commit persona reflection: run %q belongs to another instance",
				acceptance.RunID,
			)
		}
		return existing, nil
	}

	state, err := personaLineageTx(ctx, tx, acceptance.InstanceID)
	if err != nil {
		return PersonaReflectionCommit{}, err
	}
	if state.CurrentRevisionID != acceptance.BaseRevisionID ||
		state.Checkpoint != acceptance.PriorCheckpoint {
		return PersonaReflectionCommit{}, ErrPersonaLineageChanged
	}
	if acceptance.HighWaterMark < acceptance.PriorCheckpoint {
		return PersonaReflectionCommit{}, fmt.Errorf(
			"commit persona reflection: high-water mark %d precedes checkpoint %d",
			acceptance.HighWaterMark,
			acceptance.PriorCheckpoint,
		)
	}

	experiences, experienceByKey, err := insertPersonaExperiencesTx(
		ctx, tx, acceptance,
	)
	if err != nil {
		return PersonaReflectionCommit{}, err
	}
	amendments, err := insertPersonaAmendmentsTx(
		ctx, tx, acceptance, experienceByKey,
	)
	if err != nil {
		return PersonaReflectionCommit{}, err
	}

	if err := refreshRecalledSalienceTx(ctx, tx, acceptance); err != nil {
		return PersonaReflectionCommit{}, err
	}

	revision, err := nextPersonaRevisionTx(
		ctx, tx, acceptance, experiences, experienceByKey, amendments,
	)
	if err != nil {
		return PersonaReflectionCommit{}, err
	}
	outcome := domain.ReflectionNoChange
	if revision.ID != acceptance.BaseRevisionID {
		outcome = domain.ReflectionAccepted
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO persona_transitions
				(instance_id, from_revision_id, to_revision_id, kind, at)
			VALUES (?, ?, ?, ?, ?)
		`, acceptance.InstanceID, acceptance.BaseRevisionID, revision.ID,
			domain.PersonaTransitionReflection,
			formatTime(acceptance.FinishedAt)); err != nil {
			return PersonaReflectionCommit{}, fmt.Errorf("insert persona transition: %w", err)
		}
	}
	stateUpdate, err := tx.ExecContext(ctx, `
		UPDATE persona_lineages
		SET current_revision_id = ?, checkpoint = ?, reflected_at = ?
		WHERE instance_id = ? AND current_revision_id = ? AND checkpoint = ?
	`,
		revision.ID,
		acceptance.HighWaterMark,
		formatTime(acceptance.FinishedAt),
		acceptance.InstanceID,
		acceptance.BaseRevisionID,
		acceptance.PriorCheckpoint,
	)
	if err != nil {
		return PersonaReflectionCommit{}, fmt.Errorf("advance persona lineage: %w", err)
	}
	updated, err := stateUpdate.RowsAffected()
	if err != nil {
		return PersonaReflectionCommit{}, fmt.Errorf("read persona lineage update: %w", err)
	}
	if updated != 1 {
		return PersonaReflectionCommit{}, ErrPersonaLineageChanged
	}

	run := domain.ReflectionRun{
		ID: acceptance.RunID, InstanceID: acceptance.InstanceID,
		BaseRevisionID:   acceptance.BaseRevisionID,
		PriorCheckpoint:  acceptance.PriorCheckpoint,
		HighWaterMark:    acceptance.HighWaterMark,
		ResultRevisionID: revision.ID, ModelID: acceptance.ModelID,
		Outcome:             outcome,
		ProposedExperiences: len(acceptance.Experiences),
		AcceptedExperiences: len(experiences),
		ProposedAmendments:  len(acceptance.Amendments),
		AcceptedAmendments:  len(amendments),
		StartedAt:           acceptance.StartedAt, FinishedAt: acceptance.FinishedAt,
	}
	if err := insertReflectionRunTx(ctx, tx, run); err != nil {
		return PersonaReflectionCommit{}, err
	}
	revisionExperiences, err := personaExperiencesTx(ctx, tx, revision.ExperienceIDs)
	if err != nil {
		return PersonaReflectionCommit{}, err
	}
	activeAmendments, err := personaAmendmentsTx(ctx, tx, revision.AmendmentIDs)
	if err != nil {
		return PersonaReflectionCommit{}, err
	}
	if err := tx.Commit(); err != nil {
		return PersonaReflectionCommit{}, fmt.Errorf("commit persona reflection: %w", err)
	}

	state.CurrentRevisionID = revision.ID
	state.Checkpoint = acceptance.HighWaterMark
	reflectedAt := acceptance.FinishedAt
	state.ReflectedAt = &reflectedAt

	return PersonaReflectionCommit{
		Lineage: state, Revision: revision,
		Experiences: revisionExperiences, Amendments: activeAmendments, Run: run,
	}, nil
}

func insertPersonaExperiencesTx(
	ctx context.Context,
	tx *sql.Tx,
	acceptance PersonaReflectionAcceptance,
) ([]domain.Experience, map[string]domain.ExperienceID, error) {
	experiences := make([]domain.Experience, 0, len(acceptance.Experiences))
	byKey := make(map[string]domain.ExperienceID, len(acceptance.Experiences))
	for _, draft := range acceptance.Experiences {
		if draft.Key == "" {
			return nil, nil, fmt.Errorf("commit persona reflection: experience key is empty")
		}
		if _, exists := byKey[draft.Key]; exists {
			return nil, nil, fmt.Errorf("commit persona reflection: duplicate experience key %q", draft.Key)
		}
		if len(draft.Sources) == 0 {
			return nil, nil, fmt.Errorf(
				"commit persona reflection: experience %q has no sources",
				draft.Key,
			)
		}
		for _, source := range draft.Sources {
			if source.Sequence <= acceptance.PriorCheckpoint ||
				source.Sequence > acceptance.HighWaterMark {
				return nil, nil, fmt.Errorf(
					"commit persona reflection: source %d is outside (%d, %d]",
					source.Sequence,
					acceptance.PriorCheckpoint,
					acceptance.HighWaterMark,
				)
			}
			if err := requireReflectionSourceOwnershipTx(
				ctx, tx, acceptance.InstanceID, source.Sequence,
			); err != nil {
				return nil, nil, err
			}
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO persona_experiences
				(instance_id, kind, summary, subject_id, confidence, occurred_at,
				 created_at, last_cited_at, salience_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			acceptance.InstanceID, draft.Kind, draft.Summary, draft.SubjectID,
			draft.Confidence, formatTime(draft.OccurredAt),
			formatTime(acceptance.FinishedAt),
			formatTime(acceptance.FinishedAt),
			formatTime(domain.SalienceAt(draft.Confidence, acceptance.FinishedAt)),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("insert persona experience: %w", err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return nil, nil, fmt.Errorf("read persona experience id: %w", err)
		}
		experienceID := domain.ExperienceID(id)
		byKey[draft.Key] = experienceID
		for _, source := range draft.Sources {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO persona_experience_sources (experience_id, sequence)
				VALUES (?, ?)
			`, experienceID, source.Sequence); err != nil {
				return nil, nil, fmt.Errorf("insert persona experience source: %w", err)
			}
		}
		experiences = append(experiences, domain.Experience{
			ID: experienceID, InstanceID: acceptance.InstanceID,
			Kind: draft.Kind, Summary: draft.Summary, SubjectID: draft.SubjectID,
			Confidence: draft.Confidence, OccurredAt: draft.OccurredAt,
			CreatedAt:   acceptance.FinishedAt,
			LastCitedAt: acceptance.FinishedAt,
			SalienceAt:  domain.SalienceAt(draft.Confidence, acceptance.FinishedAt),
			Sources:     slices.Clone(draft.Sources),
		})
	}

	return experiences, byKey, nil
}

func requireReflectionSourceOwnershipTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID domain.InstanceID,
	sequence domain.ReflectionSequence,
) error {
	var owner domain.InstanceID
	err := tx.QueryRowContext(ctx, `
		SELECT instance_id FROM reflection_events WHERE sequence = ?
	`, sequence).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrReflectionSourceMissing
	}
	if err != nil {
		return fmt.Errorf("check reflection source ownership: %w", err)
	}
	if owner != instanceID {
		return ErrReflectionSourceOwnership
	}

	return nil
}

func insertPersonaAmendmentsTx(
	ctx context.Context,
	tx *sql.Tx,
	acceptance PersonaReflectionAcceptance,
	experienceByKey map[string]domain.ExperienceID,
) ([]domain.PersonaAmendment, error) {
	amendments := make([]domain.PersonaAmendment, 0, len(acceptance.Amendments))
	for _, draft := range acceptance.Amendments {
		if draft.Scope == domain.AmendmentGlobal && draft.Counterpart != nil {
			return nil, fmt.Errorf("commit persona reflection: global amendment has a counterpart")
		}
		if draft.Scope == domain.AmendmentRelationship && draft.Counterpart == nil {
			return nil, fmt.Errorf("commit persona reflection: relationship amendment has no counterpart")
		}
		evidence := slices.Clone(draft.EvidenceIDs)
		for _, key := range draft.EvidenceKeys {
			id, ok := experienceByKey[key]
			if !ok {
				return nil, fmt.Errorf("commit persona reflection: unknown experience key %q", key)
			}
			evidence = append(evidence, id)
		}
		slices.Sort(evidence)
		evidence = slices.Compact(evidence)
		if len(evidence) == 0 {
			return nil, fmt.Errorf("commit persona reflection: amendment has no evidence")
		}
		if err := requirePersonaExperienceOwnershipTx(ctx, tx, acceptance.InstanceID, evidence); err != nil {
			return nil, err
		}
		if err := requirePersonaAmendmentOwnershipTx(
			ctx, tx, acceptance.InstanceID, draft.SupersedesID,
		); err != nil {
			return nil, err
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO persona_amendments
				(instance_id, scope, counterpart_id, tendency, confidence,
				 created_at, expires_at, supersedes_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`,
			acceptance.InstanceID, draft.Scope, draft.Counterpart, draft.Tendency,
			draft.Confidence, formatTime(acceptance.FinishedAt),
			formatOptionalTime(draft.ExpiresAt), draft.SupersedesID,
		)
		if err != nil {
			return nil, fmt.Errorf("insert persona amendment: %w", err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("read persona amendment id: %w", err)
		}
		amendmentID := domain.PersonaAmendmentID(id)
		for _, experienceID := range evidence {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO persona_amendment_evidence (amendment_id, experience_id)
				VALUES (?, ?)
			`, amendmentID, experienceID); err != nil {
				return nil, fmt.Errorf("insert persona amendment evidence: %w", err)
			}
		}
		amendments = append(amendments, domain.PersonaAmendment{
			ID: amendmentID, InstanceID: acceptance.InstanceID,
			Scope: draft.Scope, Counterpart: draft.Counterpart,
			Tendency: draft.Tendency, Confidence: draft.Confidence,
			Evidence: evidence, CreatedAt: acceptance.FinishedAt,
			ExpiresAt: draft.ExpiresAt, SupersedesID: draft.SupersedesID,
		})
	}

	return amendments, nil
}

func nextPersonaRevisionTx(
	ctx context.Context,
	tx *sql.Tx,
	acceptance PersonaReflectionAcceptance,
	experiences []domain.Experience,
	experienceByKey map[string]domain.ExperienceID,
	amendments []domain.PersonaAmendment,
) (domain.PersonaRevision, error) {
	parent, err := personaRevisionTx(ctx, tx, acceptance.BaseRevisionID)
	if err != nil {
		return domain.PersonaRevision{}, err
	}
	description := parent.Description
	descriptionEvidence := parent.DescriptionEvidence
	if acceptance.Description != nil && *acceptance.Description != parent.Description {
		description = *acceptance.Description
		descriptionEvidence, err = descriptionEvidenceIDs(acceptance, experienceByKey)
		if err != nil {
			return domain.PersonaRevision{}, err
		}
	}
	if len(experiences) == 0 && len(amendments) == 0 && len(acceptance.Retract) == 0 &&
		len(acceptance.Consolidate) == 0 && description == parent.Description {
		return parent, nil
	}

	active, err := nextRevisionAmendmentsTx(ctx, tx, acceptance, parent, amendments)
	if err != nil {
		return domain.PersonaRevision{}, err
	}
	experienceIDs := parent.ExperienceIDs
	for _, experience := range experiences {
		experienceIDs = append(experienceIDs, experience.ID)
	}
	slices.Sort(experienceIDs)
	experienceIDs = slices.Compact(experienceIDs)

	result, err := tx.ExecContext(ctx, `
		INSERT INTO persona_revisions
			(instance_id, parent_id, description, created_at)
		VALUES (?, ?, ?, ?)
	`, acceptance.InstanceID, acceptance.BaseRevisionID, description,
		formatTime(acceptance.FinishedAt))
	if err != nil {
		return domain.PersonaRevision{}, fmt.Errorf("insert persona revision: %w", err)
	}
	revisionID, err := result.LastInsertId()
	if err != nil {
		return domain.PersonaRevision{}, fmt.Errorf("read persona revision id: %w", err)
	}
	if err := insertRevisionLinksTx(ctx, tx, "revision experience", `
		INSERT INTO persona_revision_experiences (revision_id, experience_id)
		VALUES (?, ?)
	`, revisionID, experienceIDs); err != nil {
		return domain.PersonaRevision{}, err
	}
	if err := insertRevisionLinksTx(ctx, tx, "revision amendment", `
		INSERT INTO persona_revision_amendments (revision_id, amendment_id)
		VALUES (?, ?)
	`, revisionID, active); err != nil {
		return domain.PersonaRevision{}, err
	}
	if err := insertRevisionLinksTx(ctx, tx, "description evidence", `
		INSERT INTO persona_description_evidence (revision_id, experience_id)
		VALUES (?, ?)
	`, revisionID, descriptionEvidence); err != nil {
		return domain.PersonaRevision{}, err
	}
	parentID := acceptance.BaseRevisionID

	return domain.PersonaRevision{
		ID: domain.PersonaRevisionID(revisionID), InstanceID: acceptance.InstanceID,
		ParentID: &parentID, Description: description,
		DescriptionEvidence: descriptionEvidence,
		ExperienceIDs:       experienceIDs,
		AmendmentIDs:        active, CreatedAt: acceptance.FinishedAt,
	}, nil
}

// nextRevisionAmendmentsTx settles which tendencies the next revision holds.
// A tendency leaves the active set three ways: a retraction drops it, a
// consolidation folds it into the new description, and a new tendency
// supersedes it. Each removal has to name a tendency this instance owns and
// one the base revision still holds, so a stale proposal is refused before
// any row is written. Each removal writes its kind and time to the row
// that left, and the row and its evidence stay.
func nextRevisionAmendmentsTx(
	ctx context.Context,
	tx *sql.Tx,
	acceptance PersonaReflectionAcceptance,
	parent domain.PersonaRevision,
	amendments []domain.PersonaAmendment,
) ([]domain.PersonaAmendmentID, error) {
	remove := make(
		map[domain.PersonaAmendmentID]domain.AmendmentDepartureKind,
		len(acceptance.Retract)+len(acceptance.Consolidate)+len(amendments),
	)
	for _, id := range acceptance.Retract {
		remove[id] = domain.AmendmentRetracted
	}
	for _, id := range acceptance.Consolidate {
		remove[id] = domain.AmendmentConsolidated
	}
	for _, amendment := range amendments {
		if amendment.SupersedesID != nil {
			remove[*amendment.SupersedesID] = domain.AmendmentSuperseded
		}
	}
	removedIDs := slices.Sorted(maps.Keys(remove))
	if err := requirePersonaAmendmentIDsOwnedTx(
		ctx, tx, acceptance.InstanceID, removedIDs,
	); err != nil {
		return nil, err
	}
	for _, id := range removedIDs {
		if !slices.Contains(parent.AmendmentIDs, id) {
			return nil, fmt.Errorf(
				"persona amendment %d is not active in revision %d",
				id,
				acceptance.BaseRevisionID,
			)
		}
	}
	for _, id := range removedIDs {
		if _, err := tx.ExecContext(ctx, `
			UPDATE persona_amendments SET departed_at = ?, departure = ?
			WHERE id = ? AND instance_id = ?
		`, formatTime(acceptance.FinishedAt), string(remove[id]),
			id, acceptance.InstanceID); err != nil {
			return nil, fmt.Errorf("record persona amendment %d departure: %w", id, err)
		}
	}
	active := make([]domain.PersonaAmendmentID, 0, len(parent.AmendmentIDs)+len(amendments))
	for _, id := range parent.AmendmentIDs {
		if _, removed := remove[id]; !removed {
			active = append(active, id)
		}
	}
	for _, amendment := range amendments {
		active = append(active, amendment.ID)
	}
	slices.Sort(active)

	return slices.Compact(active), nil
}

// insertRevisionLinksTx writes one revision's membership of a join table.
// insert is a constant statement taking the revision id and one member id;
// what names the row in a failure.
func insertRevisionLinksTx[ID ~int64](
	ctx context.Context,
	tx *sql.Tx,
	what string,
	insert string,
	revisionID int64,
	ids []ID,
) error {
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, insert, revisionID, id); err != nil {
			return fmt.Errorf("insert %s: %w", what, err)
		}
	}

	return nil
}

// descriptionEvidenceIDs resolves the experiences a proposed description
// cites. Each key names an experience the same run proposed, so the rows
// already exist and already belong to the reflecting instance.
func descriptionEvidenceIDs(
	acceptance PersonaReflectionAcceptance,
	experienceByKey map[string]domain.ExperienceID,
) ([]domain.ExperienceID, error) {
	evidence := make([]domain.ExperienceID, 0, len(acceptance.DescriptionEvidenceKeys))
	for _, key := range acceptance.DescriptionEvidenceKeys {
		id, known := experienceByKey[key]
		if !known {
			return nil, fmt.Errorf(
				"commit persona reflection: unknown description evidence key %q", key,
			)
		}
		evidence = append(evidence, id)
	}
	slices.Sort(evidence)

	return slices.Compact(evidence), nil
}

func personaLineageTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID domain.InstanceID,
) (domain.PersonaLineage, error) {
	var state domain.PersonaLineage
	var templateID, templateOrigin, templateHash sql.NullString
	var createdAt string
	var reflectedAt sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT instance_id, baseline, template_id, template_origin, template_hash,
		       current_revision_id, checkpoint,
		       created_at, reflected_at
		FROM persona_lineages WHERE instance_id = ?
	`, instanceID).Scan(
		&state.InstanceID, &state.Baseline,
		&templateID, &templateOrigin, &templateHash,
		&state.CurrentRevisionID,
		&state.Checkpoint, &createdAt, &reflectedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.PersonaLineage{}, ErrNoPersonaLineage
	}
	if err != nil {
		return domain.PersonaLineage{}, fmt.Errorf("read persona lineage: %w", err)
	}
	if templateID.Valid || templateOrigin.Valid || templateHash.Valid {
		if !templateID.Valid || !templateOrigin.Valid || !templateHash.Valid {
			return domain.PersonaLineage{}, errors.New("read persona lineage: incomplete template provenance")
		}
		state.Template = &domain.PersonaTemplateProvenance{
			ID: templateID.String, Origin: domain.PersonaOrigin(templateOrigin.String),
			DescriptionHash: templateHash.String,
		}
	}
	state.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return domain.PersonaLineage{}, fmt.Errorf("parse persona lineage creation time: %w", err)
	}
	if reflectedAt.Valid {
		at, err := parseTime(reflectedAt.String)
		if err != nil {
			return domain.PersonaLineage{}, fmt.Errorf("parse persona reflection time: %w", err)
		}
		state.ReflectedAt = &at
	}

	return state, nil
}

func personaRevisionTx(
	ctx context.Context,
	tx *sql.Tx,
	revisionID domain.PersonaRevisionID,
) (domain.PersonaRevision, error) {
	var revision domain.PersonaRevision
	var parentID sql.NullInt64
	var createdAt string
	err := tx.QueryRowContext(ctx, `
		SELECT id, instance_id, parent_id, description, created_at
		FROM persona_revisions WHERE id = ?
	`, revisionID).Scan(
		&revision.ID, &revision.InstanceID, &parentID,
		&revision.Description, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.PersonaRevision{}, ErrNoPersonaRevision
	}
	if err != nil {
		return domain.PersonaRevision{}, fmt.Errorf("read persona revision: %w", err)
	}
	if parentID.Valid {
		parent := domain.PersonaRevisionID(parentID.Int64)
		revision.ParentID = &parent
	}
	revision.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return domain.PersonaRevision{}, fmt.Errorf("parse persona revision creation time: %w", err)
	}
	revision.ExperienceIDs, err = personaRevisionExperienceIDsTx(ctx, tx, revision.ID)
	if err != nil {
		return domain.PersonaRevision{}, err
	}
	revision.AmendmentIDs, err = personaRevisionAmendmentIDsTx(ctx, tx, revision.ID)
	if err != nil {
		return domain.PersonaRevision{}, err
	}
	revision.DescriptionEvidence, err = personaDescriptionEvidenceTx(ctx, tx, revision.ID)
	if err != nil {
		return domain.PersonaRevision{}, err
	}

	return revision, nil
}

func personaDescriptionEvidenceTx(
	ctx context.Context,
	tx *sql.Tx,
	revisionID domain.PersonaRevisionID,
) ([]domain.ExperienceID, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT experience_id FROM persona_description_evidence
		WHERE revision_id = ? ORDER BY experience_id
	`, revisionID)
	if err != nil {
		return nil, fmt.Errorf("read description evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := []domain.ExperienceID{}
	for rows.Next() {
		var id domain.ExperienceID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan description evidence: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read description evidence: %w", err)
	}

	return ids, nil
}

func personaRevisionExperienceIDsTx(
	ctx context.Context,
	tx *sql.Tx,
	revisionID domain.PersonaRevisionID,
) ([]domain.ExperienceID, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT experience_id FROM persona_revision_experiences
		WHERE revision_id = ? ORDER BY experience_id
	`, revisionID)
	if err != nil {
		return nil, fmt.Errorf("read revision experiences: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := []domain.ExperienceID{}
	for rows.Next() {
		var id domain.ExperienceID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan revision experience: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read revision experiences: %w", err)
	}

	return ids, nil
}

func personaRevisionAmendmentIDsTx(
	ctx context.Context,
	tx *sql.Tx,
	revisionID domain.PersonaRevisionID,
) ([]domain.PersonaAmendmentID, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT amendment_id FROM persona_revision_amendments
		WHERE revision_id = ? ORDER BY amendment_id
	`, revisionID)
	if err != nil {
		return nil, fmt.Errorf("read revision amendments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := []domain.PersonaAmendmentID{}
	for rows.Next() {
		var id domain.PersonaAmendmentID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan revision amendment: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read revision amendments: %w", err)
	}

	return ids, nil
}

func requirePersonaExperienceOwnershipTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID domain.InstanceID,
	ids []domain.ExperienceID,
) error {
	for _, id := range ids {
		var owner domain.InstanceID
		if err := tx.QueryRowContext(ctx,
			`SELECT instance_id FROM persona_experiences WHERE id = ?`, id,
		).Scan(&owner); err != nil {
			return fmt.Errorf("read persona experience %d: %w", id, err)
		}
		if owner != instanceID {
			return fmt.Errorf("persona experience %d belongs to another instance", id)
		}
	}

	return nil
}

func requirePersonaAmendmentOwnershipTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID domain.InstanceID,
	id *domain.PersonaAmendmentID,
) error {
	if id == nil {
		return nil
	}

	return requirePersonaAmendmentIDsOwnedTx(ctx, tx, instanceID, []domain.PersonaAmendmentID{*id})
}

func requirePersonaAmendmentIDsOwnedTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID domain.InstanceID,
	ids []domain.PersonaAmendmentID,
) error {
	for _, id := range ids {
		var owner domain.InstanceID
		if err := tx.QueryRowContext(ctx,
			`SELECT instance_id FROM persona_amendments WHERE id = ?`, id,
		).Scan(&owner); err != nil {
			return fmt.Errorf("read persona amendment %d: %w", id, err)
		}
		if owner != instanceID {
			return fmt.Errorf("persona amendment %d belongs to another instance", id)
		}
	}

	return nil
}

func insertReflectionRunTx(
	ctx context.Context,
	tx *sql.Tx,
	run domain.ReflectionRun,
) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO reflection_runs
			(id, instance_id, base_revision_id, prior_checkpoint,
			 high_water_mark, result_revision_id, model_id, outcome,
			 rejection_reason, proposed_experiences, accepted_experiences,
			 proposed_amendments, accepted_amendments, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		run.ID, run.InstanceID, run.BaseRevisionID, run.PriorCheckpoint,
		run.HighWaterMark, run.ResultRevisionID, run.ModelID, run.Outcome,
		run.RejectionReason, run.ProposedExperiences, run.AcceptedExperiences,
		run.ProposedAmendments, run.AcceptedAmendments,
		formatTime(run.StartedAt), formatTime(run.FinishedAt),
	)
	if err != nil {
		return fmt.Errorf("insert reflection run: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		WITH ranked AS (
			SELECT id,
				ROW_NUMBER() OVER (
					PARTITION BY instance_id ORDER BY finished_at DESC, id DESC
				) AS position
			FROM reflection_runs
			WHERE instance_id = ?
		)
		DELETE FROM reflection_runs WHERE id IN (
			SELECT id FROM ranked WHERE position > ?
		)
	`, run.InstanceID, reflectionRunRetentionHeadroom); err != nil {
		return fmt.Errorf("trim reflection runs: %w", err)
	}

	return nil
}

func personaReflectionCommitTx(
	ctx context.Context,
	tx *sql.Tx,
	runID domain.ReflectionRunID,
) (PersonaReflectionCommit, bool, error) {
	run, found, err := reflectionRunTx(ctx, tx, runID)
	if err != nil || !found {
		return PersonaReflectionCommit{}, found, err
	}
	state, err := personaLineageTx(ctx, tx, run.InstanceID)
	if err != nil {
		return PersonaReflectionCommit{}, false, err
	}
	state.CurrentRevisionID = run.ResultRevisionID
	state.Checkpoint = run.HighWaterMark
	reflectedAt := run.FinishedAt
	state.ReflectedAt = &reflectedAt
	revision, err := personaRevisionTx(ctx, tx, run.ResultRevisionID)
	if err != nil {
		return PersonaReflectionCommit{}, false, err
	}
	experiences, err := personaExperiencesTx(ctx, tx, revision.ExperienceIDs)
	if err != nil {
		return PersonaReflectionCommit{}, false, err
	}
	amendments, err := personaAmendmentsTx(ctx, tx, revision.AmendmentIDs)
	if err != nil {
		return PersonaReflectionCommit{}, false, err
	}
	amendments = asOfReflectionCommit(amendments, run.FinishedAt)

	return PersonaReflectionCommit{
		Lineage: state, Revision: revision,
		Experiences: experiences, Amendments: amendments, Run: run,
	}, true, nil
}

// asOfReflectionCommit undoes what happened to an amendment after the run
// being replayed finished.
//
// The replay reads the amendment rows as they stand now, and a departure
// is written after the fact by the later run that retracted, consolidated
// or superseded the tendency. Reading it back would make a retry of the
// earlier commit answer with state that commit never produced, which is
// the one thing the run-id replay exists to rule out.
//
// The comparison is on time, so it cannot separate a departure written
// by a later run sharing this run's `FinishedAt` from one this run
// already saw. Telling those apart needs the departing run recorded on
// the row.
func asOfReflectionCommit(
	amendments []domain.PersonaAmendment,
	finishedAt time.Time,
) []domain.PersonaAmendment {
	for index, amendment := range amendments {
		if amendment.Departure != nil && amendment.Departure.At.After(finishedAt) {
			amendments[index].Departure = nil
		}
	}

	return amendments
}

func reflectionRunTx(
	ctx context.Context,
	tx *sql.Tx,
	runID domain.ReflectionRunID,
) (domain.ReflectionRun, bool, error) {
	run, err := scanReflectionRun(tx.QueryRowContext(ctx, `
		SELECT id, instance_id, base_revision_id, prior_checkpoint,
		       high_water_mark, result_revision_id, model_id, outcome,
		       rejection_reason, proposed_experiences, accepted_experiences,
		       proposed_amendments, accepted_amendments, started_at, finished_at
		FROM reflection_runs WHERE id = ?
	`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ReflectionRun{}, false, nil
	}
	if err != nil {
		return domain.ReflectionRun{}, false, err
	}

	return run, true, nil
}

type reflectionRunScanner interface {
	Scan(dest ...any) error
}

func scanReflectionRun(scanner reflectionRunScanner) (domain.ReflectionRun, error) {
	var run domain.ReflectionRun
	var startedAt, finishedAt string
	err := scanner.Scan(
		&run.ID, &run.InstanceID, &run.BaseRevisionID, &run.PriorCheckpoint,
		&run.HighWaterMark, &run.ResultRevisionID, &run.ModelID, &run.Outcome,
		&run.RejectionReason, &run.ProposedExperiences, &run.AcceptedExperiences,
		&run.ProposedAmendments, &run.AcceptedAmendments, &startedAt, &finishedAt,
	)
	if err != nil {
		return domain.ReflectionRun{}, fmt.Errorf("read reflection run: %w", err)
	}
	run.StartedAt, err = parseTime(startedAt)
	if err != nil {
		return domain.ReflectionRun{}, fmt.Errorf("parse reflection start: %w", err)
	}
	run.FinishedAt, err = parseTime(finishedAt)
	if err != nil {
		return domain.ReflectionRun{}, fmt.Errorf("parse reflection finish: %w", err)
	}

	return run, nil
}

func personaExperiencesTx(
	ctx context.Context,
	tx *sql.Tx,
	ids []domain.ExperienceID,
) ([]domain.Experience, error) {
	experiences := make([]domain.Experience, 0, len(ids))
	for _, id := range ids {
		var experience domain.Experience
		var subject sql.NullString
		var occurredAt, createdAt, lastCitedAt, salienceAt string
		if err := tx.QueryRowContext(ctx, `
			SELECT id, instance_id, kind, summary, subject_id, confidence,
			       occurred_at, created_at, last_cited_at, salience_at
			FROM persona_experiences WHERE id = ?
		`, id).Scan(
			&experience.ID, &experience.InstanceID, &experience.Kind,
			&experience.Summary, &subject, &experience.Confidence,
			&occurredAt, &createdAt, &lastCitedAt, &salienceAt,
		); err != nil {
			return nil, fmt.Errorf("read persona experience %d: %w", id, err)
		}
		if subject.Valid {
			subjectID := domain.InstanceID(subject.String)
			experience.SubjectID = &subjectID
		}
		var err error
		experience.LastCitedAt, err = parseTime(lastCitedAt)
		if err != nil {
			return nil, fmt.Errorf("parse persona experience citation: %w", err)
		}
		experience.SalienceAt, err = parseTime(salienceAt)
		if err != nil {
			return nil, fmt.Errorf("parse persona experience salience: %w", err)
		}
		experience.OccurredAt, err = parseTime(occurredAt)
		if err != nil {
			return nil, fmt.Errorf("parse persona experience occurrence: %w", err)
		}
		experience.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse persona experience creation: %w", err)
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT sequence FROM persona_experience_sources
			WHERE experience_id = ? ORDER BY sequence
		`, id)
		if err != nil {
			return nil, fmt.Errorf("read persona experience sources: %w", err)
		}
		experience.Sources = []domain.ReflectionEventRef{}
		for rows.Next() {
			var source domain.ReflectionEventRef
			if err := rows.Scan(&source.Sequence); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan persona experience source: %w", err)
			}
			experience.Sources = append(experience.Sources, source)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read persona experience sources: %w", err)
		}
		_ = rows.Close()
		experiences = append(experiences, experience)
	}

	return experiences, nil
}

func personaAmendmentsTx(
	ctx context.Context,
	tx *sql.Tx,
	ids []domain.PersonaAmendmentID,
) ([]domain.PersonaAmendment, error) {
	amendments := make([]domain.PersonaAmendment, 0, len(ids))
	for _, id := range ids {
		var amendment domain.PersonaAmendment
		var counterpart sql.NullString
		var createdAt string
		var expiresAt sql.NullString
		var supersedes sql.NullInt64
		var departedAt, departure sql.NullString
		if err := tx.QueryRowContext(ctx, `
			SELECT id, instance_id, scope, counterpart_id, tendency, confidence,
			       created_at, expires_at, supersedes_id, departed_at, departure
			FROM persona_amendments WHERE id = ?
		`, id).Scan(
			&amendment.ID, &amendment.InstanceID, &amendment.Scope,
			&counterpart, &amendment.Tendency, &amendment.Confidence,
			&createdAt, &expiresAt, &supersedes, &departedAt, &departure,
		); err != nil {
			return nil, fmt.Errorf("read persona amendment %d: %w", id, err)
		}
		if counterpart.Valid {
			counterpartID := domain.InstanceID(counterpart.String)
			amendment.Counterpart = &counterpartID
		}
		var err error
		amendment.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse persona amendment creation: %w", err)
		}
		if expiresAt.Valid {
			at, err := parseTime(expiresAt.String)
			if err != nil {
				return nil, fmt.Errorf("parse persona amendment expiry: %w", err)
			}
			amendment.ExpiresAt = &at
		}
		if supersedes.Valid {
			supersedesID := domain.PersonaAmendmentID(supersedes.Int64)
			amendment.SupersedesID = &supersedesID
		}
		if departedAt.Valid {
			at, err := parseTime(departedAt.String)
			if err != nil {
				return nil, fmt.Errorf("parse persona amendment departure: %w", err)
			}
			amendment.Departure = &domain.AmendmentDeparture{
				Kind: domain.AmendmentDepartureKind(departure.String), At: at,
			}
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT experience_id FROM persona_amendment_evidence
			WHERE amendment_id = ? ORDER BY experience_id
		`, id)
		if err != nil {
			return nil, fmt.Errorf("read persona amendment evidence: %w", err)
		}
		amendment.Evidence = []domain.ExperienceID{}
		for rows.Next() {
			var experienceID domain.ExperienceID
			if err := rows.Scan(&experienceID); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan persona amendment evidence: %w", err)
			}
			amendment.Evidence = append(amendment.Evidence, experienceID)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read persona amendment evidence: %w", err)
		}
		_ = rows.Close()
		amendments = append(amendments, amendment)
	}

	return amendments, nil
}

func formatOptionalTime(at *time.Time) any {
	if at == nil {
		return nil
	}

	return formatTime(*at)
}
