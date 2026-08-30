package store

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

// modelInstanceAtomicityEffect is what survived a failed save.
type modelInstanceAtomicityEffect struct {
	Failed    bool
	Instances int
	Revisions int
}

// TestSaveModelInstance_leaves_nothing_behind_when_the_lineage_fails pins
// that the instance row and its persona lineage are written together.
//
// Asserting only the successful path leaves the claim untested: an
// implementation opening one transaction for the instance and another for
// the lineage passes every such assertion, and leaves an instance with no
// lineage behind whenever the second fails. Dropping the lineage table is
// what makes the second write fail after the first has succeeded.
func TestSaveModelInstance_leaves_nothing_behind_when_the_lineage_fails(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	_, err := s.db.ExecContext(ctx, `DROP TABLE persona_lineages`)
	require.NoError(t, err)

	instance := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	)
	saveErr := s.SaveModelInstance(ctx, instance, testTime)

	var instances, revisions int
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM instances WHERE instance_id = ?`, instance.ID(),
	).Scan(&instances))
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM persona_revisions WHERE instance_id = ?`, instance.ID(),
	).Scan(&revisions))

	require.Equal(t, modelInstanceAtomicityEffect{Failed: true}, modelInstanceAtomicityEffect{
		Failed: saveErr != nil, Instances: instances, Revisions: revisions,
	})
}
