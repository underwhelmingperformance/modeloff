package modelclient

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

type recordingContextStore struct {
	summaries []store.ContextSummary
	updates   []store.ContextSummaryUpdate
}

type contextSummaryRequest struct {
	Previous []string
	Sources  []protocol.IRCMessage
}

type compactedPlanState struct {
	Summaries []protocol.IRCMessage
	Recent    []protocol.IRCMessage
	Events    []protocol.IRCMessage
}

type compactionState struct {
	SummaryInputs []contextSummaryRequest
	Updates       []store.ContextSummaryUpdate
	Plan          compactedPlanState
}

type recoveredCompactionState struct {
	Recovered []protocol.IRCMessage
	Plan      compactedPlanState
}

type contextSummaryTrace struct {
	Previous        [][]string
	SourceFragments []protocol.IRCMessage
	AssimilatedBody string
}

type oversizedCompactionState struct {
	Trace              contextSummaryTrace
	SummaryRequestsFit bool
	Updates            []store.ContextSummaryUpdate
	Plan               compactedPlanState
	FinalProviderFits  bool
}

var contextCompactionTime = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

func (s *recordingContextStore) ContextSummaries(
	context.Context,
	protocol.WindowGuard,
) ([]store.ContextSummary, error) {
	return s.summaries, nil
}

func (s *recordingContextStore) CommitContextSummary(
	_ context.Context,
	_ protocol.WindowGuard,
	update store.ContextSummaryUpdate,
) (store.ContextSummary, error) {
	s.updates = append(s.updates, update)
	sources := make([]protocol.IRCMessage, 0)
	for _, summary := range s.summaries {
		sources = append(sources, summary.Sources...)
	}
	sources = append(sources, update.Sources...)
	committed := store.ContextSummary{
		ID: store.ContextSummaryID(len(s.updates)), InstanceID: update.InstanceID,
		Window: update.Window, Summary: update.Summary, Sources: sources, CreatedAt: update.CreatedAt,
	}
	s.summaries = []store.ContextSummary{committed}

	return committed, nil
}

func traceContextSummaries(inputs []contextSummaryRequest) contextSummaryTrace {
	trace := contextSummaryTrace{Previous: make([][]string, 0, len(inputs))}
	var body strings.Builder
	for _, input := range inputs {
		trace.Previous = append(trace.Previous, input.Previous)
		for _, source := range input.Sources {
			body.WriteString(source.Body)
			source.Body = ""
			trace.SourceFragments = append(trace.SourceFragments, source)
		}
	}
	trace.AssimilatedBody = body.String()

	return trace
}

func TestCompactContextPlan_preserves_raw_context_until_the_request_needs_compaction(t *testing.T) {
	instanceID := domain.InstanceID("inst-botty")
	window := testChannelContext(domain.NewChannelWindow("#dev", contextCompactionTime))
	recent := []protocol.IRCMessage{
		{Kind: protocol.KindPrivMsg, Body: strings.Repeat("a", 4000), At: contextCompactionTime},
		{Kind: protocol.KindPrivMsg, Body: strings.Repeat("b", 4000), At: contextCompactionTime.Add(1)},
		{Kind: protocol.KindPrivMsg, Body: strings.Repeat("c", 4000), At: contextCompactionTime.Add(2)},
	}
	events := []protocol.IRCMessage{{
		Kind: protocol.KindPrivMsg, Body: "current trigger", At: contextCompactionTime.Add(3),
	}}

	var summaryInputs []contextSummaryRequest
	client := &apitest.Fake{SummarizeContextFn: func(
		_ context.Context,
		_ domain.ModelID,
		_ domain.InstanceID,
		previous []string,
		sources []protocol.IRCMessage,
	) (api.ContextSummaryResult, error) {
		summaryInputs = append(summaryInputs, contextSummaryRequest{
			Previous: previous, Sources: sources,
		})

		return api.ContextSummaryResult{Summary: "The earlier discussion covered a and b."}, nil
	}}
	contexts := &recordingContextStore{}
	plan, err := compactContextPlan(t.Context(), contextCompactionRequest{
		plan: contextPlanRequest{
			renderer: client, modelID: "test/model", instanceID: instanceID,
			prompt: api.SystemPrompt{Fixed: "fixed", Dynamic: "state"},
			recent: recent, events: events, contextLength: 6500,
		},
		rawRecent: recent, rawEvents: events, summaries: nil,
		window: window, contexts: contexts, guard: validWindowGuard{window: window},
		summarizer: client, now: func() time.Time { return contextCompactionTime.Add(4) },
	})
	require.NoError(t, err)
	require.True(t, plan.Budget.Fits)
	require.Equal(t, compactionState{
		SummaryInputs: []contextSummaryRequest{{Previous: nil, Sources: recent[:2]}},
		Updates: []store.ContextSummaryUpdate{{
			InstanceID: instanceID, Window: protocol.ChannelWindowTarget("#dev"),
			Summary: "The earlier discussion covered a and b.", Sources: recent[:2],
			CreatedAt: contextCompactionTime.Add(4),
		}},
		Plan: compactedPlanState{
			Summaries: []protocol.IRCMessage{{
				Kind: protocol.KindServerReply, Source: domain.ServerSource("modeloff"), Target: "#dev",
				Body: "summary of earlier context: The earlier discussion covered a and b.", At: contextCompactionTime.Add(4),
			}},
			Recent: recent[2:], Events: events,
		},
	}, compactionState{
		SummaryInputs: summaryInputs,
		Updates:       contexts.updates,
		Plan: compactedPlanState{
			Summaries: plan.Summaries, Recent: plan.Recent, Events: plan.Events,
		},
	})
}

func TestCompactContextPlan_assimilates_an_oversized_current_batch_in_order(t *testing.T) {
	instanceID := domain.InstanceID("inst-botty")
	window := testChannelContext(domain.NewChannelWindow("#dev", contextCompactionTime))
	events := []protocol.IRCMessage{
		{Kind: protocol.KindPrivMsg, Body: strings.Repeat("a", 4000), At: contextCompactionTime},
		{Kind: protocol.KindPrivMsg, Body: strings.Repeat("b", 4000), At: contextCompactionTime.Add(1)},
		{Kind: protocol.KindPrivMsg, Body: strings.Repeat("c", 4000), At: contextCompactionTime.Add(2)},
	}
	client := &apitest.Fake{SummarizeContextFn: func(
		context.Context,
		domain.ModelID,
		domain.InstanceID,
		[]string,
		[]protocol.IRCMessage,
	) (api.ContextSummaryResult, error) {
		return api.ContextSummaryResult{Summary: "The first two new messages were assimilated."}, nil
	}}
	contexts := &recordingContextStore{}
	plan, err := compactContextPlan(t.Context(), contextCompactionRequest{
		plan: contextPlanRequest{
			renderer: client, modelID: "test/model", instanceID: instanceID,
			prompt: api.SystemPrompt{Fixed: "fixed", Dynamic: "state"},
			events: events, contextLength: 6500,
		},
		rawEvents: events, window: window, contexts: contexts,
		guard: validWindowGuard{window: window}, summarizer: client,
		now: func() time.Time { return contextCompactionTime.Add(3) },
	})
	require.NoError(t, err)
	require.True(t, plan.Budget.Fits)

	recovered := append([]protocol.IRCMessage{}, contexts.summaries[0].Sources...)
	recovered = append(recovered, plan.Events...)
	require.Equal(t, recoveredCompactionState{
		Recovered: events,
		Plan:      compactedPlanState{Recent: nil, Events: events[2:]},
	}, recoveredCompactionState{
		Recovered: recovered,
		Plan:      compactedPlanState{Recent: plan.Recent, Events: plan.Events},
	})
}

func TestCompactContextPlan_assimilates_one_oversized_message_through_fitting_requests(t *testing.T) {
	instanceID := domain.InstanceID("inst-botty")
	window := testChannelContext(domain.NewChannelWindow("#dev", contextCompactionTime))
	event := protocol.IRCMessage{
		Kind: protocol.KindPrivMsg, Source: domain.ClientSource("inst-alice", "alice"),
		Target: "#dev", Body: strings.Repeat("abcdefghij", 400), At: contextCompactionTime,
	}
	var summaryInputs []contextSummaryRequest
	var summaryRequests []api.RenderedEventRequest
	client := &apitest.Fake{SummarizeContextFn: func(
		_ context.Context,
		_ domain.ModelID,
		_ domain.InstanceID,
		previous []string,
		sources []protocol.IRCMessage,
	) (api.ContextSummaryResult, error) {
		summaryInputs = append(summaryInputs, contextSummaryRequest{
			Previous: previous, Sources: sources,
		})

		return api.ContextSummaryResult{
			Summary: fmt.Sprintf("assimilated fragment %d", len(summaryInputs)),
		}, nil
	}}
	contexts := &recordingContextStore{}
	plan, err := compactContextPlan(t.Context(), contextCompactionRequest{
		plan: contextPlanRequest{
			renderer: client, modelID: "test/model", instanceID: instanceID,
			prompt: api.SystemPrompt{Fixed: "fixed", Dynamic: "state"},
			events: []protocol.IRCMessage{event}, contextLength: 1400,
		},
		rawEvents: []protocol.IRCMessage{event}, window: window, contexts: contexts,
		guard: validWindowGuard{window: window}, summarizer: client,
		now: func() time.Time { return contextCompactionTime.Add(1) },
		recordRequest: func(request api.RenderedEventRequest) {
			summaryRequests = append(summaryRequests, request)
		},
	})
	require.NoError(t, err)

	sourceFragment := event
	sourceFragment.Body = ""
	require.Equal(t, oversizedCompactionState{
		Trace: contextSummaryTrace{
			Previous:        [][]string{nil, {"assimilated fragment 1"}},
			SourceFragments: []protocol.IRCMessage{sourceFragment, sourceFragment},
			AssimilatedBody: event.Body,
		},
		SummaryRequestsFit: true,
		Updates: []store.ContextSummaryUpdate{{
			InstanceID: instanceID, Window: protocol.ChannelWindowTarget("#dev"),
			Summary: "assimilated fragment 2", Sources: []protocol.IRCMessage{event},
			CreatedAt: contextCompactionTime.Add(1),
		}},
		Plan: compactedPlanState{
			Summaries: []protocol.IRCMessage{{
				Kind: protocol.KindServerReply, Source: domain.ServerSource("modeloff"), Target: "#dev",
				Body: "summary of earlier context: assimilated fragment 2", At: contextCompactionTime.Add(1),
			}},
			Events: []protocol.IRCMessage{},
		},
		FinalProviderFits: true,
	}, oversizedCompactionState{
		Trace:              traceContextSummaries(summaryInputs),
		SummaryRequestsFit: summaryRequestsFit(summaryRequests, 1400-contextSummaryPromptAllowance),
		Updates:            contexts.updates,
		Plan: compactedPlanState{
			Summaries: plan.Summaries, Recent: plan.Recent, Events: plan.Events,
		},
		FinalProviderFits: plan.Budget.Fits,
	})
}

func summaryRequestsFit(requests []api.RenderedEventRequest, limit int) bool {
	if len(requests) == 0 {
		return false
	}
	for _, request := range requests {
		if request.EstimatedPromptTokens() > limit {
			return false
		}
	}

	return true
}

// countingRenderer sizes each rendered request in proportion to the
// number of transcript messages it carries, which puts the budget
// boundary at an exact message count, and counts the renders each
// prefix search spends reaching it.
type countingRenderer struct {
	*apitest.Fake

	bytesPerMessage int
	planRenders     int
	summaryRenders  int
}

func (c *countingRenderer) RenderEventRequest(
	_ domain.ModelID,
	_ domain.InstanceID,
	_ api.SystemPrompt,
	history api.TurnHistory,
	events []protocol.IRCMessage,
	_ ...api.ToolDefinition,
) (api.RenderedEventRequest, error) {
	c.planRenders++

	return c.sized(len(history.Messages()) + len(events)), nil
}

func (c *countingRenderer) RenderContextSummaryRequest(
	_ domain.ModelID,
	_ domain.InstanceID,
	_ []string,
	sources []protocol.IRCMessage,
) (api.RenderedEventRequest, error) {
	c.summaryRenders++

	return c.sized(len(sources)), nil
}

func (c *countingRenderer) sized(messages int) api.RenderedEventRequest {
	return api.RenderedEventRequest{
		Body: []byte(strings.Repeat("x", messages*c.bytesPerMessage)),
	}
}

type prefixSearchEffect struct {
	Prefix  int
	Renders int
}

// TestContextPrefixNeeded_bisects_the_transcript pins the search for
// the shortest prefix whose summary brings a turn inside its prompt
// limit. Dropping more of the transcript can only shrink the plan, so
// the search bisects; a linear scan would render one complete request
// per candidate, and the ring holds up to modelHistorySize of them.
//
// The first render is the probe that replacing everything fits at all,
// and the rest are the bisection over what is left.
func TestContextPrefixNeeded_bisects_the_transcript(t *testing.T) {
	// Four bytes per message makes one message cost one estimated
	// token. A 40-token window leaves a 40-token prompt limit, and
	// each candidate carries one summary in place of the prefix it
	// drops, so 66-count messages remain and 26 is the shortest
	// prefix that fits.
	renderer := &countingRenderer{Fake: &apitest.Fake{}, bytesPerMessage: 4}
	recent := make([]protocol.IRCMessage, 64)
	for i := range recent {
		recent[i] = protocol.IRCMessage{
			Kind: protocol.KindPrivMsg, Body: fmt.Sprintf("line %d", i),
			At: contextCompactionTime.Add(time.Duration(i)),
		}
	}
	events := []protocol.IRCMessage{{
		Kind: protocol.KindPrivMsg, Body: "current trigger",
		At: contextCompactionTime.Add(64),
	}}
	window := testChannelContext(domain.NewChannelWindow("#dev", contextCompactionTime))

	needed, err := contextPrefixNeeded(contextCompactionRequest{
		plan: contextPlanRequest{
			renderer: renderer, modelID: "test/model", instanceID: "inst-botty",
			recent: recent, events: events, contextLength: 40,
		},
		rawRecent: recent, rawEvents: events, window: window,
		now: func() time.Time { return contextCompactionTime },
	})
	require.NoError(t, err)

	require.Equal(t, prefixSearchEffect{Prefix: 26, Renders: 7}, prefixSearchEffect{
		Prefix: needed, Renders: renderer.planRenders,
	})
}

// TestContextPrefixNeeded_refuses_a_request_no_prefix_saves pins that a
// turn whose fixed material is over the limit on its own gives up before
// spending anything.
//
// The system prompt, the current state and the tools are in every
// candidate, so when they exceed the limit between them, every prefix
// fails. Returning the whole prefix anyway would send a summary request
// and commit a durable summary to arrive at the same refusal.
func TestContextPrefixNeeded_refuses_a_request_no_prefix_saves(t *testing.T) {
	// One message costs a thousand estimated tokens, so the summary that
	// replaces the whole transcript is already over a forty-token window.
	renderer := &countingRenderer{Fake: &apitest.Fake{}, bytesPerMessage: 4000}
	recent := make([]protocol.IRCMessage, 4)
	for i := range recent {
		recent[i] = protocol.IRCMessage{
			Kind: protocol.KindPrivMsg, Body: fmt.Sprintf("line %d", i),
			At: contextCompactionTime.Add(time.Duration(i)),
		}
	}
	window := testChannelContext(domain.NewChannelWindow("#dev", contextCompactionTime))

	_, err := contextPrefixNeeded(contextCompactionRequest{
		plan: contextPlanRequest{
			renderer: renderer, modelID: "test/model", instanceID: "inst-botty",
			recent: recent, contextLength: 40,
		},
		rawRecent: recent, window: window,
		now: func() time.Time { return contextCompactionTime },
	})

	require.ErrorIs(t, err, errContextPrefixInsufficient)
}

// TestContextSummaryBatch_bisects_the_summary_sources pins the search
// for the longest prefix one summary call can take. Adding sources can
// only grow that request, so the search bisects.
func TestContextSummaryBatch_bisects_the_summary_sources(t *testing.T) {
	// Forty bytes per message makes one source cost ten estimated
	// tokens, and a 1000-token window leaves 488 tokens for the
	// summary request, so 48 of the 64 candidates fit.
	renderer := &countingRenderer{Fake: &apitest.Fake{}, bytesPerMessage: 40}
	recent := make([]protocol.IRCMessage, 64)
	for i := range recent {
		recent[i] = protocol.IRCMessage{
			Kind: protocol.KindPrivMsg, Body: fmt.Sprintf("line %d", i),
			At: contextCompactionTime.Add(time.Duration(i)),
		}
	}
	window := testChannelContext(domain.NewChannelWindow("#dev", contextCompactionTime))

	batch, err := contextSummaryBatch(contextCompactionRequest{
		plan: contextPlanRequest{
			renderer: renderer, modelID: "test/model", instanceID: "inst-botty",
			recent: recent, contextLength: 1000,
		},
		rawRecent: recent, window: window, summarizer: renderer,
		now: func() time.Time { return contextCompactionTime },
	}, 64)
	require.NoError(t, err)

	require.Equal(t, prefixSearchEffect{Prefix: 48, Renders: 6}, prefixSearchEffect{
		Prefix: batch.recent, Renders: renderer.summaryRenders,
	})
}

type droppedCompactionState struct {
	SummaryCalls int
	Updates      []store.ContextSummaryUpdate
	Plan         compactedPlanState
	Fits         bool
}

// TestCompactContextPlan_drops_the_oldest_context_when_a_summary_fails
// covers the floor under compaction. A provider that cannot summarise
// the oldest lines must not take the turn down with it: the lines are
// in the audit log either way, so the turn drops them and sends what
// fits.
func TestCompactContextPlan_drops_the_oldest_context_when_a_summary_fails(t *testing.T) {
	instanceID := domain.InstanceID("inst-botty")
	window := testChannelContext(domain.NewChannelWindow("#dev", contextCompactionTime))
	recent := []protocol.IRCMessage{
		{Kind: protocol.KindPrivMsg, Body: strings.Repeat("a", 4000), At: contextCompactionTime},
		{Kind: protocol.KindPrivMsg, Body: strings.Repeat("b", 4000), At: contextCompactionTime.Add(1)},
		{Kind: protocol.KindPrivMsg, Body: strings.Repeat("c", 4000), At: contextCompactionTime.Add(2)},
	}
	events := []protocol.IRCMessage{{
		Kind: protocol.KindPrivMsg, Body: "current trigger", At: contextCompactionTime.Add(3),
	}}

	calls := 0
	client := &apitest.Fake{SummarizeContextFn: func(
		context.Context,
		domain.ModelID,
		domain.InstanceID,
		[]string,
		[]protocol.IRCMessage,
	) (api.ContextSummaryResult, error) {
		calls++

		return api.ContextSummaryResult{}, fmt.Errorf("upstream refused the summary")
	}}
	contexts := &recordingContextStore{}
	plan, err := compactContextPlan(t.Context(), contextCompactionRequest{
		plan: contextPlanRequest{
			renderer: client, modelID: "test/model", instanceID: instanceID,
			prompt: api.SystemPrompt{Fixed: "fixed", Dynamic: "state"},
			recent: recent, events: events, contextLength: 6500,
		},
		rawRecent: recent, rawEvents: events,
		window: window, contexts: contexts, guard: validWindowGuard{window: window},
		summarizer: client, now: func() time.Time { return contextCompactionTime.Add(4) },
	})
	require.NoError(t, err)

	require.Equal(t, droppedCompactionState{
		SummaryCalls: 1,
		Updates:      nil,
		Plan:         compactedPlanState{Recent: recent[2:], Events: events},
		Fits:         true,
	}, droppedCompactionState{
		SummaryCalls: calls,
		Updates:      contexts.updates,
		Plan: compactedPlanState{
			Summaries: plan.Summaries, Recent: plan.Recent, Events: plan.Events,
		},
		Fits: plan.Budget.Fits,
	})
}

// TestCompactContextPlan_bounds_the_summary_round_trips covers the
// other half of the floor. A summariser whose output is as big as
// what it replaced never brings the turn inside its window, and
// nothing else stops the compaction loop asking it again.
func TestCompactContextPlan_bounds_the_summary_round_trips(t *testing.T) {
	instanceID := domain.InstanceID("inst-botty")
	window := testChannelContext(domain.NewChannelWindow("#dev", contextCompactionTime))
	recent := make([]protocol.IRCMessage, 30)
	for i := range recent {
		recent[i] = protocol.IRCMessage{
			Kind: protocol.KindPrivMsg, Body: strings.Repeat("a", 4000),
			At: contextCompactionTime.Add(time.Duration(i)),
		}
	}
	events := []protocol.IRCMessage{{
		Kind: protocol.KindPrivMsg, Body: "current trigger", At: contextCompactionTime.Add(30),
	}}

	calls := 0
	client := &apitest.Fake{SummarizeContextFn: func(
		context.Context,
		domain.ModelID,
		domain.InstanceID,
		[]string,
		[]protocol.IRCMessage,
	) (api.ContextSummaryResult, error) {
		calls++

		return api.ContextSummaryResult{Summary: strings.Repeat("s", 20000)}, nil
	}}
	contexts := &recordingContextStore{}
	_, err := compactContextPlan(t.Context(), contextCompactionRequest{
		plan: contextPlanRequest{
			renderer: client, modelID: "test/model", instanceID: instanceID,
			prompt: api.SystemPrompt{Fixed: "fixed", Dynamic: "state"},
			recent: recent, events: events, contextLength: 6500,
		},
		rawRecent: recent, rawEvents: events,
		window: window, contexts: contexts, guard: validWindowGuard{window: window},
		summarizer: client, now: func() time.Time { return contextCompactionTime.Add(31) },
	})

	var exceeded *ContextWindowExceededError
	require.ErrorAs(t, err, &exceeded)
	require.Equal(t, maxContextSummaryRounds, calls)
}
