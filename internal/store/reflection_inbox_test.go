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

type reflectionInboxSnapshot struct {
	Events []storemod.ReflectionEvent
	Status storemod.ReflectionInboxStatus
}

type pendingReflectionSnapshot struct {
	Persona storemod.PersonaSnapshot
	Events  []storemod.ReflectionEvent
	Range   storemod.ReflectionRange
	Status  storemod.ReflectionInboxStatus
}

type deletedReflectionInboxState struct {
	Events         []storemod.ReflectionEvent
	StateIsMissing bool
}

const testReflectionEventRetention = 2000

func TestSQLiteStore_bounds_uncited_reflection_events_and_preserves_accepted_sources(t *testing.T) {
	stored := storetest.NewMemoryStore(t)
	uncited := domain.NewModelInstance(
		"inst-uncited", "uncited", "test/model", "careful", nil,
	)
	cited := domain.NewModelInstance(
		"inst-cited", "cited", "test/model", "curious", nil,
	)
	recordedAt := time.Date(2026, 8, 27, 17, 0, 0, 0, time.UTC)
	require.NoError(t, stored.SaveInstance(t.Context(), uncited))
	require.NoError(t, stored.SaveInstance(t.Context(), cited))

	uncitedCandidates := reflectionRetentionCandidates(
		1, testReflectionEventRetention+1, "#uncited", recordedAt,
	)
	require.NoError(t, stored.AppendReflectionEvents(
		t.Context(), uncited.ID(), uncitedCandidates, recordedAt,
	))
	uncitedStatus, err := stored.ReflectionInboxStatus(t.Context(), uncited.ID())
	require.NoError(t, err)
	uncitedEvents, err := stored.ReflectionEvents(
		t.Context(), uncited.ID(), 0, uncitedStatus.HighWaterMark,
		testReflectionEventRetention+10,
	)
	require.NoError(t, err)
	require.Equal(t, reflectionRetentionEvents(
		uncited.ID(), 2, testReflectionEventRetention+1,
		"#uncited", recordedAt, 2,
	), uncitedEvents)

	firstSource := reflectionRetentionCandidates(1, 1, "#cited", recordedAt)
	require.NoError(t, stored.AppendReflectionEvents(
		t.Context(), cited.ID(), firstSource, recordedAt,
	))
	citedFirstStatus, err := stored.ReflectionInboxStatus(t.Context(), cited.ID())
	require.NoError(t, err)
	state, err := stored.PersonaLineage(t.Context(), cited.ID())
	require.NoError(t, err)
	_, err = stored.CommitPersonaReflection(t.Context(), storemod.PersonaReflectionAcceptance{
		RunID: "accepted-source", InstanceID: cited.ID(),
		BaseRevisionID: state.CurrentRevisionID,
		HighWaterMark:  citedFirstStatus.HighWaterMark, ModelID: "test/reflection",
		StartedAt: recordedAt, FinishedAt: recordedAt,
		Experiences: []storemod.PersonaExperienceDraft{{
			Key: "source", Kind: domain.ExperienceObservation,
			Summary: "The first event mattered.", Confidence: domain.ConfidenceHigh,
			OccurredAt: recordedAt,
			Sources: []domain.ReflectionEventRef{{
				Sequence: citedFirstStatus.HighWaterMark,
			}},
		}},
	})
	require.NoError(t, err)
	citedCandidates := reflectionRetentionCandidates(
		2, testReflectionEventRetention+2, "#cited", recordedAt,
	)
	require.NoError(t, stored.AppendReflectionEvents(
		t.Context(), cited.ID(), citedCandidates, recordedAt,
	))
	citedStatus, err := stored.ReflectionInboxStatus(t.Context(), cited.ID())
	require.NoError(t, err)
	citedEvents, err := stored.ReflectionEvents(
		t.Context(), cited.ID(), 0, citedStatus.HighWaterMark,
		testReflectionEventRetention+10,
	)
	require.NoError(t, err)
	wantCited := reflectionRetentionEvents(
		cited.ID(), 1, 1, "#cited", recordedAt,
		citedFirstStatus.HighWaterMark,
	)
	wantCited = append(wantCited, reflectionRetentionEvents(
		cited.ID(), 3, testReflectionEventRetention+2,
		"#cited", recordedAt, citedFirstStatus.HighWaterMark+2,
	)...)
	require.Equal(t, wantCited, citedEvents)
}

func reflectionRetentionCandidates(
	first, last int,
	channel domain.ChannelName,
	at time.Time,
) []storemod.ReflectionEventCandidate {
	candidates := make([]storemod.ReflectionEventCandidate, 0, last-first+1)
	for id := first; id <= last; id++ {
		candidates = append(candidates, storemod.ReflectionEventCandidate{
			Source: protocol.ChannelHistoryRef(int64(id), channel),
			Message: protocol.IRCMessage{
				Kind: protocol.KindPrivMsg, Source: domain.ClientSource("inst-alice", "alice"),
				Target: string(channel), Body: "candidate", At: at,
			},
			Substantive: true,
		})
	}

	return candidates
}

func reflectionRetentionEvents(
	instanceID domain.InstanceID,
	first, last int,
	channel domain.ChannelName,
	at time.Time,
	firstSequence domain.ReflectionSequence,
) []storemod.ReflectionEvent {
	events := make([]storemod.ReflectionEvent, 0, last-first+1)
	for id := first; id <= last; id++ {
		events = append(events, storemod.ReflectionEvent{
			Sequence:   firstSequence + domain.ReflectionSequence(id-first),
			InstanceID: instanceID,
			Source:     protocol.ChannelHistoryRef(int64(id), channel),
			Message: protocol.IRCMessage{
				Kind: protocol.KindPrivMsg, Source: domain.ClientSource("inst-alice", "alice"),
				Target: string(channel), Body: "candidate", At: at,
			},
			Substantive: true, CreatedAt: at,
		})
	}

	return events
}

func TestSQLiteStore_records_reflection_events_once_across_live_and_replay(t *testing.T) {
	stored := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	)
	require.NoError(t, stored.SaveInstance(t.Context(), instance))

	at := time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)
	channel := storemod.ReflectionEventCandidate{
		Source: protocol.HistoryRef{
			Kind: protocol.HistorySourceChannelScrollback, ID: 41,
			Window: protocol.ChannelWindowTarget("#dev"),
		},
		Message: protocol.IRCMessage{
			Kind:   protocol.KindPrivMsg,
			Source: domain.ClientSource("inst-alice", "alice"),
			Target: "#dev", Body: "the migration is ready", At: at,
		},
		Substantive: true,
	}
	direct := storemod.ReflectionEventCandidate{
		Source: protocol.HistoryRef{
			Kind: protocol.HistorySourceEvent, ID: 73,
			Window: protocol.DirectWindowTarget("inst-alice"),
		},
		Message: protocol.IRCMessage{
			Kind:   protocol.KindNick,
			Source: domain.ClientSource("inst-alice", "alice"),
			Target: "ally", At: at.Add(time.Second),
		},
	}
	reusedRow := channel
	reusedRow.Message.Body = "the next interval is ready"
	reusedRow.Message.At = at.Add(2 * time.Second)

	require.NoError(t, stored.AppendReflectionEvents(
		t.Context(), instance.ID(), []storemod.ReflectionEventCandidate{channel, direct}, at.Add(2*time.Second),
	))
	require.NoError(t, stored.AppendReflectionEvents(
		t.Context(), instance.ID(), []storemod.ReflectionEventCandidate{channel, reusedRow}, at.Add(3*time.Second),
	))

	status, err := stored.ReflectionInboxStatus(t.Context(), instance.ID())
	require.NoError(t, err)
	events, err := stored.ReflectionEvents(
		t.Context(), instance.ID(), 0, status.HighWaterMark, 10,
	)
	require.NoError(t, err)

	want := reflectionInboxSnapshot{
		Events: []storemod.ReflectionEvent{
			{
				Sequence: events[0].Sequence, InstanceID: instance.ID(),
				Source: channel.Source, Message: channel.Message, Substantive: true,
				CreatedAt: at.Add(2 * time.Second),
			},
			{
				Sequence: events[1].Sequence, InstanceID: instance.ID(),
				Source: direct.Source, Message: direct.Message,
				CreatedAt: at.Add(2 * time.Second),
			},
			{
				Sequence: events[2].Sequence, InstanceID: instance.ID(),
				Source: reusedRow.Source, Message: reusedRow.Message, Substantive: true,
				CreatedAt: at.Add(3 * time.Second),
			},
		},
		Status: storemod.ReflectionInboxStatus{
			HighWaterMark: status.HighWaterMark,
			PendingEvents: 3, SubstantiveEvents: 2,
		},
	}
	require.Equal(t, want, reflectionInboxSnapshot{Events: events, Status: status})
}

func TestSQLiteStore_reflection_inbox_uses_the_persona_checkpoint(t *testing.T) {
	stored := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	)
	require.NoError(t, stored.SaveInstance(t.Context(), instance))

	at := time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC)
	candidates := []storemod.ReflectionEventCandidate{
		{
			Source: protocol.HistoryRef{
				Kind: protocol.HistorySourceEvent, ID: 1,
				Window: protocol.DirectWindowTarget("inst-alice"),
			},
			Message: protocol.IRCMessage{Kind: protocol.KindJoin, At: at},
		},
		{
			Source: protocol.HistoryRef{
				Kind: protocol.HistorySourceEvent, ID: 2,
				Window: protocol.DirectWindowTarget("inst-alice"),
			},
			Message:     protocol.IRCMessage{Kind: protocol.KindPrivMsg, Body: "hello", At: at.Add(time.Second)},
			Substantive: true,
		},
	}
	require.NoError(t, stored.AppendReflectionEvents(t.Context(), instance.ID(), candidates, at))
	status, err := stored.ReflectionInboxStatus(t.Context(), instance.ID())
	require.NoError(t, err)
	state, err := stored.PersonaLineage(t.Context(), instance.ID())
	require.NoError(t, err)

	finishedAt := at.Add(time.Minute)
	_, err = stored.CommitPersonaReflection(t.Context(), storemod.PersonaReflectionAcceptance{
		RunID: "reflection-1", InstanceID: instance.ID(),
		BaseRevisionID: state.CurrentRevisionID, PriorCheckpoint: state.Checkpoint,
		HighWaterMark: status.HighWaterMark, ModelID: "test/reflection",
		StartedAt: at, FinishedAt: finishedAt,
	})
	require.NoError(t, err)

	got, err := stored.ReflectionInboxStatus(t.Context(), instance.ID())
	require.NoError(t, err)
	require.Equal(t, storemod.ReflectionInboxStatus{
		Checkpoint: status.HighWaterMark, HighWaterMark: status.HighWaterMark,
		LastAttemptAt: &finishedAt,
	}, got)
}

func TestSQLiteStore_captures_one_bounded_reflection_snapshot(t *testing.T) {
	stored := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	)
	recordedAt := time.Date(2026, 8, 26, 17, 0, 0, 0, time.UTC)
	recent := recordedAt.Add(-time.Minute)
	require.NoError(t, stored.SaveInstance(t.Context(), instance))
	require.NoError(t, stored.AppendReflectionEvents(
		t.Context(), instance.ID(), []storemod.ReflectionEventCandidate{
			{
				Source: protocol.ChannelHistoryRef(31, "#dev"),
				Message: protocol.IRCMessage{
					Kind:   protocol.KindPrivMsg,
					Source: domain.ClientSource("inst-alice", "alice"),
					Target: "#dev", Body: "first", At: recent,
				},
				Substantive: true,
			},
			{
				Source: protocol.ChannelHistoryRef(32, "#dev"),
				Message: protocol.IRCMessage{
					Kind:   protocol.KindPrivMsg,
					Source: domain.ClientSource("inst-bob", "bob"),
					Target: "#dev", Body: "second", At: recordedAt,
				},
				Substantive: true,
			},
		}, recordedAt,
	))

	got, err := stored.PendingReflectionSnapshot(t.Context(), instance.ID(), 1)
	require.NoError(t, err)

	want := pendingReflectionSnapshot{
		Persona: storemod.PersonaSnapshot{
			Lineage: domain.PersonaLineage{
				InstanceID: instance.ID(), Baseline: "careful and curious",
				CurrentRevisionID: 1,
			},
			Revision: domain.PersonaRevision{
				ID: 1, InstanceID: instance.ID(),
				Description:         "careful and curious",
				DescriptionEvidence: []domain.ExperienceID{},
				ExperienceIDs:       []domain.ExperienceID{},
				AmendmentIDs:        []domain.PersonaAmendmentID{},
			},
			Experiences: []domain.Experience{},
			Amendments:  []domain.PersonaAmendment{},
		},
		Events: []storemod.ReflectionEvent{
			{
				Sequence: 1, InstanceID: instance.ID(),
				Source: protocol.ChannelHistoryRef(31, "#dev"),
				Message: protocol.IRCMessage{
					Kind:   protocol.KindPrivMsg,
					Source: domain.ClientSource("inst-alice", "alice"),
					Target: "#dev", Body: "first", At: recent,
				},
				Substantive: true, CreatedAt: recordedAt,
			},
		},
		Range: storemod.ReflectionRange{Through: 1},
		Status: storemod.ReflectionInboxStatus{
			HighWaterMark: 2,
			PendingEvents: 2, SubstantiveEvents: 2,
		},
	}
	require.Equal(t, want, pendingReflectionSnapshot{
		Persona: got.Persona, Events: got.Events,
		Range: got.Range, Status: got.Status,
	})
}

func TestSQLiteStore_instance_deletion_removes_the_reflection_inbox(t *testing.T) {
	stored := storetest.NewMemoryStore(t)
	instance := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "careful and curious", nil,
	)
	require.NoError(t, stored.SaveInstance(t.Context(), instance))
	at := time.Date(2026, 8, 26, 16, 30, 0, 0, time.UTC)
	require.NoError(t, stored.AppendReflectionEvents(
		t.Context(), instance.ID(), []storemod.ReflectionEventCandidate{{
			Source:  protocol.ChannelHistoryRef(1, "#dev"),
			Message: protocol.IRCMessage{Kind: protocol.KindJoin, Target: "#dev", At: at},
		}}, at,
	))

	require.NoError(t, stored.DeleteInstanceByID(t.Context(), instance.ID()))
	events, err := stored.ReflectionEvents(t.Context(), instance.ID(), 0, 100, 10)
	require.NoError(t, err)
	_, statusErr := stored.ReflectionInboxStatus(t.Context(), instance.ID())

	require.Equal(t, deletedReflectionInboxState{
		Events: []storemod.ReflectionEvent{}, StateIsMissing: true,
	}, deletedReflectionInboxState{
		Events: events, StateIsMissing: errors.Is(statusErr, storemod.ErrNoPersonaLineage),
	})
}
