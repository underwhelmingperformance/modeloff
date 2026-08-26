package modelclient

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

func TestBuildContextPlan_measures_the_complete_request_without_mutating_sections(t *testing.T) {
	t.Parallel()

	at := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	prompt := api.SystemPrompt{
		Fixed:   "fixed instructions " + strings.Repeat("f", 600),
		Dynamic: "current instance state " + strings.Repeat("d", 200),
	}
	currentState := []protocol.IRCMessage{{
		Kind: protocol.KindServerReply, Source: domain.ServerSource("modeloff"),
		Target: "#dev", Body: "topic: context planning",
	}}
	recent := []protocol.IRCMessage{
		{
			Kind: protocol.KindPrivMsg, Source: domain.LegacyClientSource("alice"),
			Target: "#dev", Body: strings.Repeat("old ", 400), At: at,
		},
		{
			Kind: protocol.KindPrivMsg, Source: domain.LegacyClientSource("bob"),
			Target: "#dev", Body: "the recent line must remain", At: at.Add(time.Second),
		},
	}
	events := []protocol.IRCMessage{{
		Kind: protocol.KindPrivMsg, Source: domain.LegacyClientSource("carol"),
		Target: "#dev", Body: "the new event", At: at.Add(2 * time.Second),
	}}
	tools := []api.ToolDefinition{{
		Name: "large_tool", Description: strings.Repeat("tool description ", 200),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content": map[string]any{"type": "string"},
			},
		},
	}}

	renderer := &apitest.Fake{}
	withoutOldest, err := renderer.RenderEventRequest(
		"test/model", "inst-botty", prompt,
		api.TurnHistory{Cacheable: recent[1:], Current: currentState}, events, tools...,
	)
	require.NoError(t, err)
	withOldest, err := renderer.RenderEventRequest(
		"test/model", "inst-botty", prompt,
		api.TurnHistory{Cacheable: recent, Current: currentState}, events, tools...,
	)
	require.NoError(t, err)

	promptLimit := withoutOldest.EstimatedPromptTokens()
	require.Greater(t, withOldest.EstimatedPromptTokens(), promptLimit)

	got, err := buildContextPlan(contextPlanRequest{
		renderer: renderer,
		modelID:  "test/model", instanceID: "inst-botty",
		prompt: prompt, currentState: currentState, recent: recent,
		events: events, tools: tools,
		contextLength: promptLimit + completionAllowanceTokens,
	})
	require.NoError(t, err)

	require.Equal(t, ContextPlan{
		Prompt:       prompt,
		CurrentState: currentState,
		Recent:       recent,
		Events:       events,
		Tools:        tools,
		Request:      withOldest,
		Budget: ContextBudget{
			ContextLength:       promptLimit + completionAllowanceTokens,
			CompletionAllowance: completionAllowanceTokens,
			PromptLimit:         promptLimit,
			EstimatedPrompt:     withOldest.EstimatedPromptTokens(),
			RequestBytes:        len(withOldest.Body),
			Fits:                false,
		},
	}, got)
}

func TestBuildContextPlan_keeps_semantic_sections_in_provider_order(t *testing.T) {
	t.Parallel()
	type providerInput struct {
		History api.TurnHistory
		Events  []protocol.IRCMessage
	}

	state := []protocol.IRCMessage{{Kind: protocol.KindServerReply, Body: "current state"}}
	summaries := []protocol.IRCMessage{{Kind: protocol.KindServerReply, Body: "older summary"}}
	recent := []protocol.IRCMessage{{Kind: protocol.KindPrivMsg, Body: "recent"}}
	events := []protocol.IRCMessage{{Kind: protocol.KindPrivMsg, Body: "new"}}

	got, err := buildContextPlan(contextPlanRequest{
		renderer: &apitest.Fake{}, modelID: "test/model", instanceID: "inst-botty",
		prompt:       api.SystemPrompt{Fixed: "fixed", Dynamic: "instance"},
		currentState: state, summaries: summaries, recent: recent, events: events,
	})
	require.NoError(t, err)

	require.Equal(t, providerInput{
		History: api.TurnHistory{
			Cacheable: []protocol.IRCMessage{summaries[0], recent[0]},
			Current:   []protocol.IRCMessage{state[0]},
		},
		Events: events,
	}, providerInput{
		History: got.ProviderHistory(),
		Events:  got.Events,
	})
}

// countingEstimator answers with a fixed number of tokens per request
// byte and records the refusals it is told about, standing in for a
// provider client that has seen the model's own counts.
type countingEstimator struct {
	*apitest.Fake

	tokensPerByte int
	rejections    []promptRejection
}

type promptRejection struct {
	ModelID domain.ModelID
	Bytes   int
	Limit   int
}

func (e *countingEstimator) EstimatePromptTokens(
	_ domain.ModelID,
	request api.RenderedEventRequest,
) int {
	return len(request.Body) * e.tokensPerByte
}

func (e *countingEstimator) RecordPromptRejection(
	modelID domain.ModelID,
	requestBytes, limitTokens int,
) {
	e.rejections = append(e.rejections, promptRejection{
		ModelID: modelID, Bytes: requestBytes, Limit: limitTokens,
	})
}

type planBudgetEffect struct {
	EstimatedPrompt int
	Fits            bool
}

// TestBuildContextPlan_measures_with_the_provider_estimate covers
// which tokeniser the budget is decided against. The four-byte rule
// is OpenRouter's normalising metric and not the count a provider
// enforces its window with, so a client that knows the model's own
// ratio is what the plan asks.
func TestBuildContextPlan_measures_with_the_provider_estimate(t *testing.T) {
	t.Parallel()

	events := []protocol.IRCMessage{{
		Kind: protocol.KindPrivMsg, Source: domain.LegacyClientSource("alice"),
		Target: "#dev", Body: strings.Repeat("x", 2600),
	}}
	renderer := &apitest.Fake{}
	rendered, err := renderer.RenderEventRequest(
		"test/model", "inst-botty", api.SystemPrompt{Fixed: "fixed"},
		api.TurnHistory{}, events,
	)
	require.NoError(t, err)

	// A 5000-token window leaves a 1000-token prompt limit. The
	// four-byte rule puts this request inside it and one token per
	// byte, which is the density a dense tokeniser reaches, puts it
	// well outside.
	const contextLength = 5000
	promptLimit := contextLength - completionAllowanceTokens
	require.LessOrEqual(t, rendered.EstimatedPromptTokens(), promptLimit)
	require.Greater(t, len(rendered.Body), promptLimit)

	estimator := &countingEstimator{Fake: renderer, tokensPerByte: 1}
	plan, err := buildContextPlan(contextPlanRequest{
		renderer: estimator, modelID: "test/model", instanceID: "inst-botty",
		prompt: api.SystemPrompt{Fixed: "fixed"}, events: events,
		contextLength: contextLength,
	})
	require.NoError(t, err)

	require.Equal(t, planBudgetEffect{
		EstimatedPrompt: len(rendered.Body),
		Fits:            false,
	}, planBudgetEffect{
		EstimatedPrompt: plan.Budget.EstimatedPrompt,
		Fits:            plan.Budget.Fits,
	})
}

// TestRecordPromptRejection covers what a refusal for length teaches
// the provider client. Only the turn's own dispatch request is
// reported: a refused tool-loop continuation carries the conversation
// so far and is larger than the budget measured.
func TestRecordPromptRejection(t *testing.T) {
	t.Parallel()

	estimator := &countingEstimator{Fake: &apitest.Fake{}, tokensPerByte: 1}
	recordPromptRejection(estimator, "test/model", ContextBudget{
		RequestBytes: 12000, PromptLimit: 2500,
	})
	recordPromptRejection(estimator, "test/model", ContextBudget{})
	recordPromptRejection(&apitest.Fake{}, "test/model", ContextBudget{
		RequestBytes: 12000, PromptLimit: 2500,
	})

	require.Equal(t, []promptRejection{{
		ModelID: "test/model", Bytes: 12000, Limit: 2500,
	}}, estimator.rejections)
}
