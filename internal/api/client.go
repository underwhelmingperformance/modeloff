// Package api provides the interface and types for communicating with
// the OpenRouter API (OpenAI-compatible) and OpenRouter-specific
// endpoints such as model listing.
package api

import (
	"context"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// ModelInfo holds metadata about an available model from OpenRouter.
type ModelInfo struct {
	ID          domain.ModelID `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	ContextLen  int            `json:"context_length"`
}

// Usage contains token and cost metadata returned by OpenRouter.
type Usage struct {
	PromptTokens         int64
	CompletionTokens     int64
	TotalTokens          int64
	ReasoningTokens      int64
	CachedTokens         int64
	CacheWriteTokens     int64
	CostCredits          float64
	UpstreamInferenceCost float64
}

// CompletionResult contains the model's typed response alongside
// request metadata.
type CompletionResult struct {
	Response  protocol.ModelResponse
	RequestID string
	Usage     Usage
}

// NicknameResult contains the generated nickname alongside request
// metadata.
type NicknameResult struct {
	Nick      domain.Nick
	RequestID string
	Usage     Usage
}

// Client defines the interface for all API interactions. Both the
// chat completion (via openai-go) and OpenRouter-specific calls are
// abstracted behind this interface to support testing with fakes.
type Client interface {
	// ListModels fetches available models from the OpenRouter API.
	ListModels(ctx context.Context) ([]ModelInfo, error)

	// SendEvents sends a batch of protocol events to a model and
	// returns its response. The system prompt and conversation
	// history are managed by the caller.
	SendEvents(
		ctx context.Context,
		modelID domain.ModelID,
		systemPrompt string,
		history []protocol.IRCMessage,
		events []protocol.IRCMessage,
	) (CompletionResult, error)

	// GenerateNick asks a model to generate a nickname for the given
	// model ID, returning the suggested nick. The nickModel parameter
	// selects which model performs the generation.
	GenerateNick(ctx context.Context, nickModel domain.ModelID, modelID domain.ModelID) (NicknameResult, error)
}
