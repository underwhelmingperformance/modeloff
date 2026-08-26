package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// reflectionSourceTrimEffect is what the commit answered, and whether the
// cited event was still in the stream when it ran.
type reflectionSourceTrimEffect struct {
	Trimmed bool
	Missing bool
	Foreign bool
	Persona PersonaSnapshot
}

// TestCommitPersonaReflection_rejects_a_trimmed_source pins that a run
// whose evidence its own traffic trimmed away is refused, and refused as a
// missing source and not as somebody else's.
//
// Retention runs on every append and keeps what an accepted experience
// cites. A run's citations do not exist while it is still deciding on
// them, so a long enough burst during one run takes its evidence with it.
// The refusal is what keeps an accepted experience from naming evidence
// nobody can read back.
func TestCommitPersonaReflection_rejects_a_trimmed_source(t *testing.T) {
	ctx := t.Context()
	stored := newTestStore(t)
	botty := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	)
	require.NoError(t, stored.SaveInstance(ctx, botty))
	at := time.Date(2026, 8, 26, 20, 30, 0, 0, time.UTC)

	require.NoError(t, stored.AppendReflectionEvents(
		ctx, botty.ID(), reflectionBurst(1, 1, at), at,
	))
	snapshot, err := stored.PendingReflectionSnapshot(ctx, botty.ID(), 10)
	require.NoError(t, err)
	cited := snapshot.Events[0].Sequence

	// One burst past the stream's retention headroom, which is what takes
	// the cited event with it.
	require.NoError(t, stored.AppendReflectionEvents(
		ctx, botty.ID(), reflectionBurst(100, reflectionEventRetentionHeadroom+1, at), at,
	))
	remaining, err := stored.ReflectionEvents(ctx, botty.ID(), cited-1, cited, 1)
	require.NoError(t, err)

	before, err := stored.PersonaSnapshot(ctx, botty.ID())
	require.NoError(t, err)
	_, commitErr := stored.CommitPersonaReflection(
		ctx, PersonaReflectionAcceptance{
			RunID: "trimmed-source", InstanceID: botty.ID(),
			BaseRevisionID:  before.Revision.ID,
			PriorCheckpoint: snapshot.Range.Checkpoint,
			HighWaterMark:   snapshot.Range.Through,
			ModelID:         "test/reflection", StartedAt: at, FinishedAt: at,
			Experiences: []PersonaExperienceDraft{{
				Key: "trimmed", Kind: domain.ExperienceObservation,
				Summary:    "Something the stream no longer holds.",
				Confidence: domain.ConfidenceHigh, OccurredAt: at,
				Sources: []domain.ReflectionEventRef{{Sequence: cited}},
			}},
		},
	)
	after, err := stored.PersonaSnapshot(ctx, botty.ID())
	require.NoError(t, err)

	require.Equal(t, reflectionSourceTrimEffect{
		Trimmed: true, Missing: true, Persona: before,
	}, reflectionSourceTrimEffect{
		Trimmed: len(remaining) == 0,
		Missing: errors.Is(commitErr, ErrReflectionSourceMissing),
		Foreign: errors.Is(commitErr, ErrReflectionSourceOwnership),
		Persona: after,
	})
}

// reflectionBurst builds count candidates from one counterpart, numbered
// from first so separate bursts allocate separate history references.
func reflectionBurst(
	first, count int,
	at time.Time,
) []ReflectionEventCandidate {
	candidates := make([]ReflectionEventCandidate, 0, count)
	for index := range count {
		id := first + index
		candidates = append(candidates, ReflectionEventCandidate{
			Source: protocol.ChannelHistoryRef(int64(id), "#dev"),
			Message: protocol.IRCMessage{
				Kind:   protocol.KindPrivMsg,
				Source: domain.ClientSource("inst-alice", "alice"),
				Target: "#dev", Body: fmt.Sprintf("line %d", id),
				At: at.Add(time.Duration(id) * time.Second),
			},
			Substantive: true,
		})
	}

	return candidates
}
