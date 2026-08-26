package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

type personaFoundationState struct {
	Lineage  domain.PersonaLineage
	Revision domain.PersonaRevision
}

func TestSQLiteStore_creates_immutable_revision_zero_with_a_model_instance(t *testing.T) {
	store := storetest.NewMemoryStore(t)
	ctx := t.Context()
	instance := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	)

	require.NoError(t, store.SaveInstance(ctx, instance))
	state, err := store.PersonaLineage(ctx, instance.ID())
	require.NoError(t, err)
	revision, err := store.PersonaRevision(ctx, state.CurrentRevisionID)
	require.NoError(t, err)

	want := personaFoundationState{
		Lineage: domain.PersonaLineage{
			InstanceID:        "inst-botty",
			Baseline:          "careful and curious",
			CurrentRevisionID: state.CurrentRevisionID,
			CreatedAt:         time.Time{},
		},
		Revision: domain.PersonaRevision{
			ID:            state.CurrentRevisionID,
			InstanceID:    "inst-botty",
			Description:   "careful and curious",
			ExperienceIDs: []domain.ExperienceID{},
			AmendmentIDs:  []domain.PersonaAmendmentID{},
			CreatedAt:     time.Time{},
		},
	}
	require.Equal(t, want, personaFoundationState{Lineage: state, Revision: revision})

	// Reading the lineage alone would pass against an implementation that
	// rewrote revision zero's own description, which is what the name
	// says cannot happen.
	instance.SetPersona("a later display value")
	require.NoError(t, store.SaveInstance(ctx, instance))
	unchangedLineage, err := store.PersonaLineage(ctx, instance.ID())
	require.NoError(t, err)
	unchangedRevision, err := store.PersonaRevision(ctx, state.CurrentRevisionID)
	require.NoError(t, err)

	require.Equal(t, want, personaFoundationState{
		Lineage: unchangedLineage, Revision: unchangedRevision,
	})
}

func TestSQLiteStore_instance_deletion_removes_persona_lineage(t *testing.T) {
	store := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	)
	require.NoError(t, store.SaveInstance(t.Context(), instance))
	require.NoError(t, store.DeleteInstanceByID(t.Context(), instance.ID()))

	_, stateErr := store.PersonaLineage(t.Context(), instance.ID())
	require.True(t, errors.Is(stateErr, storemod.ErrNoPersonaLineage))
}
