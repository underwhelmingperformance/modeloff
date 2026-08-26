package modelclient

import (
	"slices"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

const (
	// completionAllowanceTokens is what context planning holds back
	// from a model's window for the completion. The dispatch request
	// caps the completion at the same number, so the reserve is what
	// the provider is actually told.
	completionAllowanceTokens = api.DispatchCompletionTokens
	minimumPromptTokens       = 1000
)

// ContextBudget records the model window allocation and the measured
// size of the request selected for one turn.
type ContextBudget struct {
	ContextLength       int
	CompletionAllowance int
	PromptLimit         int
	EstimatedPrompt     int
	RequestBytes        int
	Fits                bool
}

// ContextPlan is the provider input selected for one model turn. The
// fields keep current server state, recent transcript and newly
// arrived events distinct even though the provider receives the
// first two sections through one history parameter.
type ContextPlan struct {
	Prompt       api.SystemPrompt
	CurrentState []protocol.IRCMessage
	Summaries    []protocol.IRCMessage
	Recent       []protocol.IRCMessage
	Events       []protocol.IRCMessage
	Tools        []api.ToolDefinition
	Request      api.RenderedEventRequest
	Budget       ContextBudget
}

// ProviderHistory returns the history parameter in provider order:
// the summaries of what has already been compacted, then the recent
// transcript, then the current server state, split at the point the
// provider may cache up to.
//
// Current state holds the member list, the topic, the memories
// selected against this turn's traffic and the per-participant
// persona records, so it differs on most turns. A provider caching a
// prompt matches a prefix of it and invalidates everything after the
// first block that changed, which is why current state goes past the
// breakpoint and the transcript stays in front of it.
func (p ContextPlan) ProviderHistory() api.TurnHistory {
	return api.TurnHistory{
		Cacheable: slices.Concat(p.Summaries, p.Recent),
		Current:   slices.Clone(p.CurrentState),
	}
}

type contextPlanRequest struct {
	renderer api.Client

	modelID    domain.ModelID
	instanceID domain.InstanceID
	prompt     api.SystemPrompt

	currentState []protocol.IRCMessage
	summaries    []protocol.IRCMessage
	recent       []protocol.IRCMessage
	events       []protocol.IRCMessage
	tools        []api.ToolDefinition

	contextLength int
}

func buildContextPlan(req contextPlanRequest) (ContextPlan, error) {
	promptLimit, completionAllowance := contextWindowAllocation(req.contextLength)
	plan := ContextPlan{
		Prompt:       req.prompt,
		CurrentState: slices.Clone(req.currentState),
		Summaries:    slices.Clone(req.summaries),
		Recent:       slices.Clone(req.recent),
		Events:       slices.Clone(req.events),
		Tools:        slices.Clone(req.tools),
		Budget: ContextBudget{
			ContextLength:       req.contextLength,
			CompletionAllowance: completionAllowance,
			PromptLimit:         promptLimit,
		},
	}

	if err := renderContextPlan(req.renderer, req.modelID, req.instanceID, &plan); err != nil {
		return ContextPlan{}, err
	}

	return plan, nil
}

func renderContextPlan(
	renderer api.Client,
	modelID domain.ModelID,
	instanceID domain.InstanceID,
	plan *ContextPlan,
) error {
	rendered, err := renderer.RenderEventRequest(
		modelID,
		instanceID,
		plan.Prompt,
		plan.ProviderHistory(),
		plan.Events,
		plan.Tools...,
	)
	if err != nil {
		return err
	}

	plan.Request = rendered
	plan.Budget.EstimatedPrompt = estimatePromptTokens(renderer, modelID, rendered)
	plan.Budget.RequestBytes = len(rendered.Body)
	plan.Budget.Fits = plan.Budget.PromptLimit <= 0 ||
		plan.Budget.EstimatedPrompt <= plan.Budget.PromptLimit

	return nil
}

// estimatePromptTokens asks the provider client what a rendered
// request costs under `modelID`. A client that keeps no prompt-token
// counts, which every test double is, answers through the four-byte
// rule [api.RenderedEventRequest.EstimatedPromptTokens] applies.
func estimatePromptTokens(
	client any,
	modelID domain.ModelID,
	request api.RenderedEventRequest,
) int {
	if estimator, ok := client.(api.PromptTokenEstimator); ok {
		return estimator.EstimatePromptTokens(modelID, request)
	}

	return request.EstimatedPromptTokens()
}

func contextWindowAllocation(contextLength int) (int, int) {
	if contextLength <= 0 {
		return 0, 0
	}

	completionAllowance := min(
		completionAllowanceTokens,
		max(contextLength-minimumPromptTokens, 0),
	)
	promptLimit := contextLength - completionAllowance

	return promptLimit, completionAllowance
}
