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
		systemPrompt string,
		history []protocol.IRCMessage,
		events []protocol.IRCMessage,
	) (api.CompletionResult, error)
	ContinueWithToolResultsFn func(
		ctx context.Context,
		conv *api.Conversation,
		results []api.ToolResult,
	) (api.CompletionResult, error)
	GenerateNickFn     func(ctx context.Context, smallModel domain.ModelID, persona string, exclude []domain.Nick) (domain.Nick, error)
	GeneratePersonasFn func(ctx context.Context, smallModel domain.ModelID) ([]domain.Persona, error)
}

var _ api.Client = (*Fake)(nil)

// ListModels answers through [Fake.ListModelsFn], or nil results
// with no error.
func (f *Fake) ListModels(ctx context.Context) ([]api.ModelInfo, error) {
	if f.ListModelsFn != nil {
		return f.ListModelsFn(ctx)
	}

	return nil, nil
}

// SendEvents answers through [Fake.SendEventsFn], or an empty
// [api.CompletionResult]. `tools` is not forwarded to the hook: no
// caller across the test suite this double serves has needed to
// inspect the tool list a dispatch turn offered.
func (f *Fake) SendEvents(
	ctx context.Context,
	modelID domain.ModelID,
	selfInstanceID domain.InstanceID,
	systemPrompt string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
	_ ...api.ToolDefinition,
) (api.CompletionResult, error) {
	if f.SendEventsFn != nil {
		return f.SendEventsFn(ctx, modelID, selfInstanceID, systemPrompt, history, events)
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

// GeneratePersonas answers through [Fake.GeneratePersonasFn], or no
// personas.
func (f *Fake) GeneratePersonas(ctx context.Context, smallModel domain.ModelID) ([]domain.Persona, error) {
	if f.GeneratePersonasFn != nil {
		return f.GeneratePersonasFn(ctx, smallModel)
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
