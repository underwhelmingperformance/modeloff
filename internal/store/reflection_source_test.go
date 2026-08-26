package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

type reflectionSourceOwnershipEffect struct {
	Rejected bool
	Persona  storemod.PersonaSnapshot
}

func TestCommitPersonaReflection_rejects_another_instances_source(t *testing.T) {
	stored := storetest.NewMemoryStore(t)
	botty := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	)
	alice := domain.NewModelInstance(
		"inst-alice", "alice", "test/model", "methodical and direct", nil,
	)
	require.NoError(t, stored.SaveInstance(t.Context(), botty))
	require.NoError(t, stored.SaveInstance(t.Context(), alice))
	at := time.Date(2026, 8, 26, 20, 30, 0, 0, time.UTC)
	require.NoError(t, stored.AppendReflectionEvents(
		t.Context(), alice.ID(), []storemod.ReflectionEventCandidate{{
			Source: protocol.ChannelHistoryRef(1, "#dev"),
			Message: protocol.IRCMessage{
				Kind:   protocol.KindPrivMsg,
				Source: domain.ClientSource("inst-peer", "peer"),
				Target: "#dev", Body: "another instance observed this", At: at,
			},
			Substantive: true,
		}}, at,
	))
	before, err := stored.PersonaSnapshot(t.Context(), botty.ID())
	require.NoError(t, err)

	_, commitErr := stored.CommitPersonaReflection(
		t.Context(), storemod.PersonaReflectionAcceptance{
			RunID: "foreign-source", InstanceID: botty.ID(),
			BaseRevisionID: before.Revision.ID, HighWaterMark: 1,
			ModelID: "test/reflection", StartedAt: at, FinishedAt: at,
			Experiences: []storemod.PersonaExperienceDraft{{
				Key: "foreign", Kind: domain.ExperienceObservation,
				Summary:    "An event from another private stream was observed.",
				Confidence: domain.ConfidenceHigh, OccurredAt: at,
				Sources: []domain.ReflectionEventRef{{Sequence: 1}},
			}},
		},
	)
	after, err := stored.PersonaSnapshot(t.Context(), botty.ID())
	require.NoError(t, err)

	require.Equal(t, reflectionSourceOwnershipEffect{
		Rejected: true, Persona: before,
	}, reflectionSourceOwnershipEffect{
		Rejected: errors.Is(commitErr, storemod.ErrReflectionSourceOwnership),
		Persona:  after,
	})
}
