package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

// experienceRetentionEffect is how many experiences survived a retention
// pass, and whether the two the test named are among them.
type experienceRetentionEffect struct {
	Kept        int
	KeptCited   bool
	KeptUncited bool
}

// TestSQLiteStore_pruneExperiences_keeps_what_a_revision_cites pins what
// the storage bound may take.
//
// Nothing else removes an experience: consolidation marks a tendency and
// leaves its evidence in place, and an experience has no expiry, because
// the citations are the audit trail every accepted change rests on. So
// this pass takes only the least salient of the experiences nothing has
// ever built anything on, and an old one a description still cites stays
// however far down the ranking it has fallen.
func TestSQLiteStore_pruneExperiences_keeps_what_a_revision_cites(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	instance := domain.NewModelInstance("inst-botty", "botty", "test/model", "quiet", nil)
	require.NoError(t, s.SaveInstance(ctx, instance))
	lineage, err := s.PersonaLineage(ctx, instance.ID())
	require.NoError(t, err)

	// The lowest salience goes to the oldest, so ids 1 and 2 are what the
	// bound reaches first.
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	total := experienceRetentionHeadroom + 2
	for index := range total {
		citedAt := at.Add(time.Duration(index) * time.Minute)
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO persona_experiences
				(id, instance_id, kind, summary, subject_id, confidence,
				 occurred_at, created_at, last_cited_at, salience_at)
			VALUES (?, ?, 'observation', ?, NULL, 'low', ?, ?, ?, ?)
		`,
			index+1, instance.ID(), fmt.Sprintf("experience %d", index+1),
			formatTime(citedAt), formatTime(citedAt), formatTime(citedAt),
			formatTime(domain.SalienceAt(domain.ConfidenceLow, citedAt)),
		)
		require.NoError(t, err)
	}

	const (
		cited   = domain.ExperienceID(1)
		uncited = domain.ExperienceID(2)
	)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO persona_description_evidence (revision_id, experience_id)
		VALUES (?, ?)
	`, lineage.CurrentRevisionID, cited)
	require.NoError(t, err)

	require.NoError(t, s.pruneEvents(ctx))

	var kept int
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM persona_experiences WHERE instance_id = ?`,
		instance.ID(),
	).Scan(&kept))

	require.Equal(t, experienceRetentionEffect{
		Kept: experienceRetentionHeadroom + 1, KeptCited: true,
	}, experienceRetentionEffect{
		Kept:        kept,
		KeptCited:   experienceExists(ctx, t, s, instance.ID(), cited),
		KeptUncited: experienceExists(ctx, t, s, instance.ID(), uncited),
	})
}

func experienceExists(
	ctx context.Context,
	t *testing.T,
	s *SQLiteStore,
	instanceID domain.InstanceID,
	id domain.ExperienceID,
) bool {
	t.Helper()

	var present bool
	require.NoError(t, s.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM persona_experiences WHERE instance_id = ? AND id = ?
		)
	`, instanceID, id).Scan(&present))

	return present
}
