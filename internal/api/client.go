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
	) (protocol.ModelResponse, error)

	// GenerateNick asks a small model to generate a nickname for a
	// given model, returning the suggested nick.
	GenerateNick(ctx context.Context, modelID domain.ModelID) (domain.Nick, error)
}
