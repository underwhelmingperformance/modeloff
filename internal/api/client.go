// Package api provides the interface and types for communicating with
// the OpenRouter API (OpenAI-compatible) and OpenRouter-specific
// endpoints such as model listing.
package api

import (
	"context"
	"errors"
	"fmt"

	openai "github.com/openai/openai-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
)

// ErrContentFiltered indicates the response was blocked by a content filter.
var ErrContentFiltered = errors.New("response blocked by content filter")

// ErrResponseTruncated indicates the response was truncated due to token limits.
var ErrResponseTruncated = errors.New("response truncated: hit token limit")

// ErrModelRefused indicates the model refused to respond.
type ErrModelRefused struct {
	Reason string
}

func (e *ErrModelRefused) Error() string {
	return fmt.Sprintf("model refused: %s", e.Reason)
}

// ModelInfo holds metadata about an available model from OpenRouter.
type ModelInfo struct {
	ID          domain.ModelID `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	ContextLen  int            `json:"context_length"`
}

// Usage contains token and cost metadata returned by OpenRouter.
type Usage struct {
	PromptTokens          int64
	CompletionTokens      int64
	TotalTokens           int64
	ReasoningTokens       int64
	CachedTokens          int64
	CacheWriteTokens      int64
	CostCredits           float64
	UpstreamInferenceCost float64
}

// ToolCallKind identifies the type of memory tool call.
type ToolCallKind string

const (
	// ToolCallWriteMemory is a write_memory tool call.
	ToolCallWriteMemory ToolCallKind = "write_memory"

	// ToolCallDeleteMemory is a delete_memory tool call.
	ToolCallDeleteMemory ToolCallKind = "delete_memory"
)

// PendingToolCall represents a tool call from the model that requires
// execution before the conversation can continue.
type PendingToolCall struct {
	ID   string
	Kind ToolCallKind
	Key  string
	Body string
}

// ToolResult carries the outcome of executing a pending tool call,
// ready to be sent back to the model as a tool response message.
type ToolResult struct {
	ToolCallID string
	Content    string
}

// Conversation is an opaque handle to the accumulated messages in a
// multi-turn tool-calling exchange. It is returned inside
// CompletionResult when the model calls intermediate tools.
type Conversation struct {
	modelID  domain.ModelID
	messages []openai.ChatCompletionMessageParamUnion
}

// CompletionResult contains the model's typed response alongside
// request metadata. When the model calls memory tools, Response is
// empty and PendingToolCalls contains the calls to execute. The
// Conversation field carries the message state needed to continue.
type CompletionResult struct {
	Response         protocol.ModelResponse
	PendingToolCalls []PendingToolCall
	Conversation     *Conversation
	RequestID        string
	Usage            Usage
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

	// ContinueWithToolResults sends tool execution results back to
	// the model and returns the next response. The Conversation
	// carries the accumulated message state; ToolResults are appended
	// as tool-role messages before the next API call.
	ContinueWithToolResults(
		ctx context.Context,
		conv *Conversation,
		results []ToolResult,
	) (CompletionResult, error)

	// GenerateNick asks a model to generate a nickname for the given
	// model ID, returning the suggested nick. The nickModel parameter
	// selects which model performs the generation.
	GenerateNick(ctx context.Context, nickModel domain.ModelID, modelID domain.ModelID) (NicknameResult, error)
}

// SetSpanAttributes records usage and request metadata on a span.
func (u Usage) SetSpanAttributes(span trace.Span, requestID string) {
	span.SetAttributes(
		attribute.String(observability.AttrRequestID, requestID),
		attribute.Int64(observability.AttrPromptTokens, u.PromptTokens),
		attribute.Int64(observability.AttrCompletionTokens, u.CompletionTokens),
		attribute.Int64(observability.AttrTotalTokens, u.TotalTokens),
		attribute.Int64(observability.AttrReasoningTokens, u.ReasoningTokens),
		attribute.Int64(observability.AttrCachedTokens, u.CachedTokens),
		attribute.Int64(observability.AttrCacheWriteTokens, u.CacheWriteTokens),
		attribute.Float64(observability.AttrCostCredits, u.CostCredits),
	)
}

// ResponseResultKind maps a model response to an observability result string.
func ResponseResultKind(response protocol.ModelResponse) string {
	switch response.Kind {
	case protocol.ResponseReply:
		return observability.ResultReply
	case protocol.ResponseSilence:
		return observability.ResultPass
	default:
		return observability.ResultOK
	}
}
