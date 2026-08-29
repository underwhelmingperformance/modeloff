// Package apitest provides a configurable [api.Client] test double.
// Every package that dispatches a model turn, arbitrates a persona,
// or prepares an instance needs an [api.Client] to hand its code
// under test, and Fake is the one implementation they share.
package apitest

import (
	"context"
	"fmt"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// Fake is a hand-configurable [api.Client]. Each method routes
// through its matching optional field; a caller sets only the ones a
// test cares about; every method left nil answers with an empty
// result and a nil error, cheap enough that a test exercising an
// unrelated path needs no setup at all.
type Fake struct {
	ListModelsFn func(context.Context) ([]api.ModelInfo, error)
	SendEventsFn func(
		ctx context.Context,
		modelID domain.ModelID,
		selfInstanceID domain.InstanceID,
		systemPrompt api.SystemPrompt,
		history []protocol.IRCMessage,
		events []protocol.IRCMessage,
	) (api.CompletionResult, error)
	ContinueWithToolResultsFn func(
		ctx context.Context,
		conv *api.Conversation,
		results []api.ToolResult,
	) (api.CompletionResult, error)
	RenderToolResultRequestFn func(
		conv *api.Conversation,
		results []api.ToolResult,
	) (api.RenderedEventRequest, error)
	SummarizeContextFn func(
		ctx context.Context,
		modelID domain.ModelID,
		selfInstanceID domain.InstanceID,
		previous []string,
		sources []protocol.IRCMessage,
	) (api.ContextSummaryResult, error)
	ReflectPersonaFn func(
		ctx context.Context,
		modelID domain.ModelID,
		instanceID domain.InstanceID,
		input api.ReflectionInput,
		tools ...api.ToolDefinition,
	) (api.ReflectionExploration, error)
	ContinueReflectionFn func(
		ctx context.Context,
		conv *api.Conversation,
		results []api.ToolResult,
		tools ...api.ToolDefinition,
	) (api.ReflectionExploration, error)
	ProposeReflectionFn func(
		ctx context.Context,
		conv *api.Conversation,
		results []api.ToolResult,
		support api.StructuredOutputSupport,
	) (api.ReflectionResult, error)
	GenerateNickFn             func(ctx context.Context, smallModel domain.ModelID, persona string, exclude []domain.Nick) (domain.Nick, error)
	GeneratePersonaTemplatesFn func(ctx context.Context, smallModel domain.ModelID) ([]domain.PersonaTemplate, error)
}

var _ api.Client = (*Fake)(nil)
var _ api.ContextSummarizer = (*Fake)(nil)
var _ api.ReflectionGenerator = (*Fake)(nil)

// ListModels answers through [Fake.ListModelsFn], or nil results
// with no error.
func (f *Fake) ListModels(ctx context.Context) ([]api.ModelInfo, error) {
	if f.ListModelsFn != nil {
		return f.ListModelsFn(ctx)
	}

	return nil, nil
}

// RenderEventRequest returns the standard OpenRouter request shape
// used by the test client.
func (f *Fake) RenderEventRequest(
	modelID domain.ModelID,
	selfInstanceID domain.InstanceID,
	systemPrompt api.SystemPrompt,
	history api.TurnHistory,
	events []protocol.IRCMessage,
	tools ...api.ToolDefinition,
) (api.RenderedEventRequest, error) {
	return api.RenderEventRequest(
		modelID, selfInstanceID, systemPrompt, history, events, tools...,
	)
}

// RenderToolResultRequest answers through
// [Fake.RenderToolResultRequestFn], or returns empty request evidence.
func (f *Fake) RenderToolResultRequest(
	conv *api.Conversation,
	results []api.ToolResult,
	_ ...api.ToolDefinition,
) (api.RenderedEventRequest, error) {
	if f.RenderToolResultRequestFn != nil {
		return f.RenderToolResultRequestFn(conv, results)
	}

	return api.RenderedEventRequest{}, nil
}

// RenderContextSummaryRequest returns the standard OpenRouter request
// shape used by the test client.
func (f *Fake) RenderContextSummaryRequest(
	modelID domain.ModelID,
	selfInstanceID domain.InstanceID,
	previous []string,
	sources []protocol.IRCMessage,
) (api.RenderedEventRequest, error) {
	client := api.NewOpenRouterClient("", "https://example.invalid/v1", nil)

	return client.RenderContextSummaryRequest(
		modelID, selfInstanceID, previous, sources,
	)
}

// SummarizeContext answers through [Fake.SummarizeContextFn], or a
// fixed compact summary when no hook is configured.
func (f *Fake) SummarizeContext(
	ctx context.Context,
	modelID domain.ModelID,
	selfInstanceID domain.InstanceID,
	previous []string,
	sources []protocol.IRCMessage,
) (api.ContextSummaryResult, error) {
	if f.SummarizeContextFn != nil {
		return f.SummarizeContextFn(ctx, modelID, selfInstanceID, previous, sources)
	}

	return api.ContextSummaryResult{Summary: "compacted context"}, nil
}

// ReflectPersona answers through [Fake.ReflectPersonaFn], or opens a
// reflection the instance explores nothing in. The exploration carries no
// [api.Conversation]: only the real client can build one, and the manager
// passes whatever it receives straight to the next call, so a fake that
// answers both ends never needs one.
func (f *Fake) ReflectPersona(
	ctx context.Context,
	modelID domain.ModelID,
	instanceID domain.InstanceID,
	input api.ReflectionInput,
	tools ...api.ToolDefinition,
) (api.ReflectionExploration, error) {
	if f.ReflectPersonaFn != nil {
		return f.ReflectPersonaFn(ctx, modelID, instanceID, input, tools...)
	}

	return api.ReflectionExploration{}, nil
}

// ContinueReflection answers through [Fake.ContinueReflectionFn], or ends
// the exploration by calling no further tools.
func (f *Fake) ContinueReflection(
	ctx context.Context,
	conv *api.Conversation,
	results []api.ToolResult,
	tools ...api.ToolDefinition,
) (api.ReflectionExploration, error) {
	if f.ContinueReflectionFn != nil {
		return f.ContinueReflectionFn(ctx, conv, results, tools...)
	}

	return api.ReflectionExploration{}, nil
}

// ProposeReflection answers through [Fake.ProposeReflectionFn], or
// returns an empty no-change proposal.
func (f *Fake) ProposeReflection(
	ctx context.Context,
	conv *api.Conversation,
	results []api.ToolResult,
	support api.StructuredOutputSupport,
) (api.ReflectionResult, error) {
	if f.ProposeReflectionFn != nil {
		return f.ProposeReflectionFn(ctx, conv, results, support)
	}

	return api.ReflectionResult{Proposal: api.ReflectionProposal{
		Experiences: []api.ReflectionExperienceProposal{},
		Amendments:  []api.ReflectionAmendmentProposal{},
		Retract:     []string{},
	}}, nil
}

// SendEvents answers through [Fake.SendEventsFn], or an empty
// [api.CompletionResult]. `tools` is not forwarded to the hook: no
// caller across the test suite this double serves has needed to
// inspect the tool list a dispatch turn offered. The hook receives
// the transcript flattened into provider order, which is what a test
// asserting on the prompt reads.
func (f *Fake) SendEvents(
	ctx context.Context,
	modelID domain.ModelID,
	selfInstanceID domain.InstanceID,
	systemPrompt api.SystemPrompt,
	history api.TurnHistory,
	events []protocol.IRCMessage,
	_ ...api.ToolDefinition,
) (api.CompletionResult, error) {
	if f.SendEventsFn != nil {
		return f.SendEventsFn(ctx, modelID, selfInstanceID, systemPrompt, history.Messages(), events)
	}

	return api.CompletionResult{}, nil
}

// ContinueWithToolResults answers through
// [Fake.ContinueWithToolResultsFn], or an empty [api.CompletionResult].
func (f *Fake) ContinueWithToolResults(
	ctx context.Context,
	conv *api.Conversation,
	results []api.ToolResult,
	_ ...api.ToolDefinition,
) (api.CompletionResult, error) {
	if f.ContinueWithToolResultsFn != nil {
		return f.ContinueWithToolResultsFn(ctx, conv, results)
	}

	return api.CompletionResult{}, nil
}

// GenerateNick answers through [Fake.GenerateNickFn] when set.
// Otherwise it returns "fakenick", or "fakenick<N>" once the caller
// has already excluded N suggestions, so a test that drives a nick
// collision and retries (ADDMODEL run twice, a taken nick already in
// the store) gets a distinct nick on each attempt without wiring its
// own counter.
func (f *Fake) GenerateNick(ctx context.Context, smallModel domain.ModelID, persona string, exclude []domain.Nick) (api.NicknameResult, error) {
	if f.GenerateNickFn != nil {
		nick, err := f.GenerateNickFn(ctx, smallModel, persona, exclude)
		return api.NicknameResult{Nick: nick}, err
	}

	nick := domain.Nick("fakenick")
	if len(exclude) > 0 {
		nick = domain.Nick(fmt.Sprintf("fakenick%d", len(exclude)))
	}

	return api.NicknameResult{Nick: nick}, nil
}

// GeneratePersonaTemplates answers through
// [Fake.GeneratePersonaTemplatesFn], or with no templates.
func (f *Fake) GeneratePersonaTemplates(ctx context.Context, smallModel domain.ModelID) ([]domain.PersonaTemplate, error) {
	if f.GeneratePersonaTemplatesFn != nil {
		return f.GeneratePersonaTemplatesFn(ctx, smallModel)
	}

	return nil, nil
}

// ReasonAware wraps [Fake] and additionally implements
// [api.NickReasonGenerator], as its own type. Keeping the capability
// on a separate type is what lets a test choose which of the two
// paths a Client is asked through: a bare Fake satisfies only plain
// GenerateNick, and a ReasonAware also satisfies
// GenerateNickWithReasons.
//
// GenerateNickWithReasonsFn has no nil fallback: a test reaches for
// ReasonAware specifically to exercise this path, so a caller that
// constructs one always has an answer to give.
type ReasonAware struct {
	Fake

	GenerateNickWithReasonsFn func(ctx context.Context, smallModel domain.ModelID, persona string, excluded []api.RejectedNick) (domain.Nick, error)
}

var _ api.NickReasonGenerator = (*ReasonAware)(nil)

// GenerateNickWithReasons implements [api.NickReasonGenerator] via
// [ReasonAware.GenerateNickWithReasonsFn].
func (f *ReasonAware) GenerateNickWithReasons(
	ctx context.Context,
	smallModel domain.ModelID,
	persona string,
	excluded []api.RejectedNick,
) (api.NicknameResult, error) {
	nick, err := f.GenerateNickWithReasonsFn(ctx, smallModel, persona, excluded)

	return api.NicknameResult{Nick: nick}, err
}
