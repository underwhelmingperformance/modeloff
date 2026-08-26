package store

import (
	"database/sql"
	"testing"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

type reflectionTablesState struct {
	PersonaLineages    int
	Revisions          int
	Experiences        int
	ExperienceSources  int
	Amendments         int
	AmendmentEvidence  int
	RevisionExperience int
	RevisionAmendment  int
	Transitions        int
	Runs               int
	CurrentRevision    domain.PersonaRevisionID
	Checkpoint         domain.ReflectionSequence
}

type tableCountQuery struct {
	table string
	value *int
}

func readReflectionTablesState(t *testing.T, store *SQLiteStore, instanceID domain.InstanceID) reflectionTablesState {
	t.Helper()
	var state reflectionTablesState
	queries := []tableCountQuery{
		{"persona_lineages", &state.PersonaLineages},
		{"persona_revisions", &state.Revisions},
		{"persona_experiences", &state.Experiences},
		{"persona_experience_sources", &state.ExperienceSources},
		{"persona_amendments", &state.Amendments},
		{"persona_amendment_evidence", &state.AmendmentEvidence},
		{"persona_revision_experiences", &state.RevisionExperience},
		{"persona_revision_amendments", &state.RevisionAmendment},
		{"persona_transitions", &state.Transitions},
		{"reflection_runs", &state.Runs},
	}
	for _, query := range queries {
		require.NoError(t, store.db.QueryRowContext(t.Context(),
			"SELECT count(*) FROM "+query.table,
		).Scan(query.value))
	}
	require.NoError(t, store.db.QueryRowContext(t.Context(), `
		SELECT current_revision_id, checkpoint FROM persona_lineages
		WHERE instance_id = ?
	`, instanceID).Scan(&state.CurrentRevision, &state.Checkpoint))

	return state
}

func TestCommitPersonaReflection_rolls_back_every_record_when_the_run_cannot_commit(t *testing.T) {
	db, err := sql.Open("sqlite3", SQLitePragmaDSN(":memory:"))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	store, err := NewSQLiteStore(t.Context(), db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	instance := domain.NewModelInstance("inst-botty", "botty", "test/model", "quiet", nil)
	require.NoError(t, store.SaveInstance(t.Context(), instance))
	base, err := store.PersonaLineage(t.Context(), instance.ID())
	require.NoError(t, err)
	before := readReflectionTablesState(t, store, instance.ID())
	_, err = store.db.ExecContext(t.Context(), `
		CREATE TRIGGER fail_reflection_run
		BEFORE INSERT ON reflection_runs
		BEGIN
			SELECT RAISE(FAIL, 'injected reflection-run failure');
		END
	`)
	require.NoError(t, err)

	at := time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC)
	_, commitErr := store.CommitPersonaReflection(t.Context(), PersonaReflectionAcceptance{
		RunID: "reflection-fails", InstanceID: instance.ID(),
		BaseRevisionID:  base.CurrentRevisionID,
		PriorCheckpoint: 0, HighWaterMark: 1,
		ModelID: "test/reflection", StartedAt: at, FinishedAt: at,
		Experiences: []PersonaExperienceDraft{{
			Key: "event", Kind: domain.ExperienceObservation,
			Summary: "A substantive event occurred.", Confidence: domain.ConfidenceHigh,
			OccurredAt: at, Sources: []domain.ReflectionEventRef{{Sequence: 1}},
		}},
		Amendments: []PersonaAmendmentDraft{{
			Scope: domain.AmendmentGlobal, Tendency: "Asks one more question before deciding.",
			Confidence: domain.ConfidenceMedium, EvidenceKeys: []string{"event"},
		}},
	})
	require.Error(t, commitErr)
	require.Equal(t, before, readReflectionTablesState(t, store, instance.ID()))
}
