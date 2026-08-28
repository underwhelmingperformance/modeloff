package modelmanager

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/store"
)

type reflectionRecallEffect struct {
	History []api.ReflectionInputEvent
	Cited   []api.ReflectionInputEvent
	Refusal modelclient.ToolResultPayload
}

// reflectionToolPast is how many events the fixture leaves behind the
// run's checkpoint. It is more than [maxReflectionRecallEvents] so a
// recall asking for everything is answered by the cap and not by the
// size of the instance's past.
const reflectionToolPast = 3 * maxReflectionRecallEvents

// reflectionToolFixture stores an instance's past and advances its
// checkpoint over the first [reflectionToolPast] events, so the run's
// own event range starts partway through and the tools have something
// older to read.
func reflectionToolFixture(
	t *testing.T,
	at time.Time,
) (*reflectionTools, store.PendingReflectionSnapshot) {
	t.Helper()

	stored, instance := reflectionSchedulerStore(t)
	require.NoError(t, stored.AppendReflectionEvents(
		t.Context(), instance.ID(),
		reflectionCandidates(1, reflectionToolPast+reflectionSubstantiveThreshold, at), at,
	))
	_, err := stored.CommitPersonaReflection(
		t.Context(), store.PersonaReflectionAcceptance{
			RunID: "seed-1", InstanceID: instance.ID(),
			BaseRevisionID: 1, PriorCheckpoint: 0,
			HighWaterMark: reflectionToolPast,
			ModelID:       "test/seed", StartedAt: at, FinishedAt: at,
			Experiences: []store.PersonaExperienceDraft{},
			Amendments:  []store.PersonaAmendmentDraft{},
			Retract:     []domain.PersonaAmendmentID{},
		},
	)
	require.NoError(t, err)
	snapshot, err := stored.PendingReflectionSnapshot(
		t.Context(), instance.ID(), reflectionInputEventLimit,
	)
	require.NoError(t, err)
	_, aliases := reflectionAPIInput(snapshot)

	return newReflectionTools(stored, snapshot, aliases), snapshot
}

func TestReflectionTools_read_an_instances_past_under_the_run_tokens(t *testing.T) {
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	tools, snapshot := reflectionToolFixture(t, at)

	results := tools.execute(t.Context(), []api.PendingToolCall{
		{
			ID: "call_history", Name: "recall_history",
			Args: json.RawMessage(`{"window":"#dev","before":0,"limit":2}`),
		},
		{
			ID: "call_cited", Name: "recall_events",
			Args: json.RawMessage(`{"sequences":[1]}`),
		},
		{
			ID: "call_unknown", Name: "no_such_tool",
			Args: json.RawMessage(`{}`),
		},
	})

	require.Equal(t, []string{"call_history", "call_cited", "call_unknown"},
		[]string{results[0].ToolCallID, results[1].ToolCallID, results[2].ToolCallID})
	checkpoint := snapshot.Range.Checkpoint

	require.Equal(t, reflectionRecallEffect{
		History: []api.ReflectionInputEvent{
			recalledEvent(checkpoint-1, at),
			recalledEvent(checkpoint, at),
		},
		Cited:   []api.ReflectionInputEvent{recalledEvent(1, at)},
		Refusal: modelclient.ToolResultPayload{Error: `unknown tool "no_such_tool"`},
	}, reflectionRecallEffect{
		History: decodeRecalledEvents(t, results[0].Content),
		Cited:   decodeRecalledEvents(t, results[1].Content),
		Refusal: decodeToolPayload(t, results[2].Content),
	})
}

func TestReflectionTools_bound_one_recall_to_the_result_cap(t *testing.T) {
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	tools, snapshot := reflectionToolFixture(t, at)

	results := tools.execute(t.Context(), []api.PendingToolCall{{
		ID: "call_history", Name: "recall_history",
		Args: json.RawMessage(`{"window":"","before":0,"limit":500}`),
	}})

	checkpoint := snapshot.Range.Checkpoint
	want := make([]api.ReflectionInputEvent, 0, maxReflectionRecallEvents)
	for sequence := checkpoint - maxReflectionRecallEvents + 1; sequence <= checkpoint; sequence++ {
		want = append(want, recalledEvent(sequence, at))
	}
	require.Equal(t, want, decodeRecalledEvents(t, results[0].Content))
}

func TestManager_reflection_answers_a_recall_call_before_proposing(t *testing.T) {
	stored, instance := reflectionSchedulerStore(t)
	fixed := time.Date(2026, 8, 26, 20, 0, 0, 0, time.UTC)
	require.NoError(t, stored.AppendReflectionEvents(
		t.Context(), instance.ID(),
		reflectionCandidates(1, reflectionSubstantiveThreshold, fixed), fixed,
	))
	snapshot, err := stored.PendingReflectionSnapshot(
		t.Context(), instance.ID(), reflectionInputEventLimit,
	)
	require.NoError(t, err)

	var answered []api.ToolResult
	var offered []string
	client := &apitest.Fake{
		ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
			return []api.ModelInfo{{
				ID: "test/reflection", SupportedParameters: []string{"tools"},
			}}, nil
		},
		ReflectPersonaFn: func(
			_ context.Context,
			_ domain.ModelID,
			_ domain.InstanceID,
			_ api.ReflectionInput,
			tools ...api.ToolDefinition,
		) (api.ReflectionExploration, error) {
			for _, tool := range tools {
				offered = append(offered, tool.Name)
			}

			return api.ReflectionExploration{
				PendingToolCalls: []api.PendingToolCall{{
					ID: "call_1", Name: "recall_events",
					Args: json.RawMessage(`{"sequences":[1]}`),
				}},
			}, nil
		},
		ContinueReflectionFn: func(
			_ context.Context,
			_ *api.Conversation,
			results []api.ToolResult,
			_ ...api.ToolDefinition,
		) (api.ReflectionExploration, error) {
			answered = results

			return api.ReflectionExploration{}, nil
		},
	}
	manager := New(Config{
		Store: stored, APIClient: client, InitialAPIKey: "configured",
		BaseContext: t.Context, Now: func() time.Time { return fixed },
		ReflectionMode: ReflectionActive, ReflectionModel: "test/reflection",
		ReflectionRunID: func() domain.ReflectionRunID { return "recall-1" },
	})

	manager.runReflection(t.Context(), snapshot)
	run, err := stored.ReflectionRun(t.Context(), "recall-1")
	require.NoError(t, err)
	require.NoError(t, manager.DetachAll(t.Context()))

	require.Equal(t, []string{"recall_history", "recall_events"}, offered)
	require.Equal(t, domain.ReflectionNoChange, run.Outcome)
	require.Equal(t, "call_1", answered[0].ToolCallID)
	require.Equal(t,
		[]api.ReflectionInputEvent{recalledEvent(1, fixed)},
		decodeRecalledEvents(t, answered[0].Content),
	)
}

func TestManager_reflection_stops_offering_tools_at_the_turn_limit(t *testing.T) {
	stored, instance := reflectionSchedulerStore(t)
	fixed := time.Date(2026, 8, 26, 20, 0, 0, 0, time.UTC)
	require.NoError(t, stored.AppendReflectionEvents(
		t.Context(), instance.ID(),
		reflectionCandidates(1, reflectionSubstantiveThreshold, fixed), fixed,
	))
	snapshot, err := stored.PendingReflectionSnapshot(
		t.Context(), instance.ID(), reflectionInputEventLimit,
	)
	require.NoError(t, err)

	call := api.ReflectionExploration{
		PendingToolCalls: []api.PendingToolCall{{
			ID: "call_1", Name: "recall_events",
			Args: json.RawMessage(`{"sequences":[1]}`),
		}},
	}
	explorations := 0
	proposals := 0
	var proposalResults []api.ToolResult
	client := &apitest.Fake{
		ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
			return []api.ModelInfo{{
				ID: "test/reflection", SupportedParameters: []string{"tools"},
			}}, nil
		},
		ReflectPersonaFn: func(
			context.Context,
			domain.ModelID,
			domain.InstanceID,
			api.ReflectionInput,
			...api.ToolDefinition,
		) (api.ReflectionExploration, error) {
			explorations++

			return call, nil
		},
		ContinueReflectionFn: func(
			context.Context,
			*api.Conversation,
			[]api.ToolResult,
			...api.ToolDefinition,
		) (api.ReflectionExploration, error) {
			explorations++

			return call, nil
		},
		ProposeReflectionFn: func(
			_ context.Context,
			_ *api.Conversation,
			results []api.ToolResult,
			_ api.StructuredOutputSupport,
		) (api.ReflectionResult, error) {
			proposals++
			proposalResults = results

			return api.ReflectionResult{Proposal: api.ReflectionProposal{
				Experiences: []api.ReflectionExperienceProposal{},
				Amendments:  []api.ReflectionAmendmentProposal{},
				Retract:     []string{},
			}}, nil
		},
	}
	manager := New(Config{
		Store: stored, APIClient: client, InitialAPIKey: "configured",
		BaseContext: t.Context, Now: func() time.Time { return fixed },
		ReflectionMode: ReflectionActive, ReflectionModel: "test/reflection",
		ReflectionRunID: func() domain.ReflectionRunID { return "bounded-1" },
	})

	manager.runReflection(t.Context(), snapshot)
	require.NoError(t, manager.DetachAll(t.Context()))

	require.Equal(t, maxReflectionToolTurns, explorations)
	require.Equal(t, 1, proposals)
	require.Equal(t, "call_1", proposalResults[0].ToolCallID)
}

// recalledEvent is one event of [reflectionCandidates] as a recall tool
// answers it: the run's participant token in place of the instance id,
// under the same shape the request's own events carry.
func recalledEvent(
	sequence domain.ReflectionSequence,
	at time.Time,
) api.ReflectionInputEvent {
	message := reflectionCandidates(1, 1, at)[0].Message
	message.Source = message.Source.WithoutInstanceID()

	return api.ReflectionInputEvent{
		Sequence:   sequence,
		WindowKind: domain.KindChannel, Window: "#dev",
		Participant: "p1",
		Message:     message, Substantive: true,
	}
}

func decodeToolPayload(t *testing.T, content string) modelclient.ToolResultPayload {
	t.Helper()

	var payload modelclient.ToolResultPayload
	require.NoError(t, json.Unmarshal([]byte(content), &payload))

	return payload
}

func decodeRecalledEvents(t *testing.T, content string) []api.ReflectionInputEvent {
	t.Helper()

	var payload struct {
		OK     bool                       `json:"ok"`
		Data   []api.ReflectionInputEvent `json:"data"`
		Errorf string                     `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(content), &payload))
	require.True(t, payload.OK, payload.Errorf)

	return payload.Data
}
