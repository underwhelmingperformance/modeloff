// Package api provides the interface and types for communicating with
// the OpenRouter API (OpenAI-compatible) and OpenRouter-specific
// endpoints such as model listing.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	openai "github.com/openai/openai-go/v3"
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

// ErrClientNotConfigured reports that no API client has been built,
// which is the state until an OpenRouter key is configured. Every
// upstream call is refused on it, so a caller can tell a missing key
// from a request the provider answered badly.
var ErrClientNotConfigured = errors.New("api client not configured")

// ErrPromptTooLong indicates the provider refused the request because
// its prompt exceeds the model's context window. It is not retryable
// on its own: the same request produces the same refusal. What
// answers it is a smaller request, which is why the dispatch path
// records the refusal against the model's token ratio before it tries
// again.
var ErrPromptTooLong = errors.New("prompt exceeds the model context window")

// PromptTokenEstimator is the optional provider capability that
// estimates what a rendered request costs in prompt tokens for one
// model.
//
// [RenderedEventRequest.EstimatedPromptTokens] applies OpenRouter's
// four-bytes-per-token normalising rule, which is not the native
// count a provider enforces its context window with. OpenRouter's
// April 2026 measurements of the Opus 4.7 tokeniser put native counts
// 32 to 45 per cent above it for identical text, and the JSON
// punctuation and RFC 3339 timestamps in this app's transcript push
// the same way. A client that has seen the provider's own counts for
// a model answers from those instead.
type PromptTokenEstimator interface {
	// EstimatePromptTokens returns the prompt tokens `request` is
	// expected to cost under `modelID`.
	EstimatePromptTokens(modelID domain.ModelID, request RenderedEventRequest) int

	// RecordPromptRejection records that the provider refused a
	// request of `requestBytes` bytes under `modelID` for exceeding
	// the context window, so every later estimate puts a request that
	// size above `limitTokens`.
	RecordPromptRejection(modelID domain.ModelID, requestBytes, limitTokens int)
}

// ErrModelRefused indicates the model refused to respond.
type ErrModelRefused struct {
	Reason string
}

func (e *ErrModelRefused) Error() string {
	return fmt.Sprintf("model refused: %s", e.Reason)
}

// validateChoice checks the first choice in a completion response for
// refusal, content filtering, or truncation. It returns nil when the
// choice looks healthy.
func validateChoice(choice openai.ChatCompletionChoice) error {
	if choice.Message.Refusal != "" {
		return &ErrModelRefused{Reason: choice.Message.Refusal}
	}

	switch choice.FinishReason {
	case "content_filter":
		return ErrContentFiltered
	case "length":
		return ErrResponseTruncated
	}

	return nil
}

// ModelInfo holds metadata about an available model from OpenRouter.
type ModelInfo struct {
	ID                  domain.ModelID `json:"id"`
	Name                string         `json:"name"`
	Description         string         `json:"description"`
	ContextLen          int            `json:"context_length"`
	SupportedParameters []string       `json:"supported_parameters"`
}

// SupportsTools reports whether OpenRouter advertises tool-calling
// support for this model, via `"tools"` in its `supported_parameters`
// list. The app's chat-completion protocol drives every model turn
// through tool calls (`msg`, `me`, `pass`, the channel-management and
// memory tools), so a model without this parameter fails every turn
// upstream even though it validates fine at invite time.
func (m ModelInfo) SupportsTools() bool {
	return slices.Contains(m.SupportedParameters, "tools")
}

// SupportsStructuredOutputs reports whether OpenRouter advertises
// strict JSON-schema structured-output support for this model, via
// `"structured_outputs"` in its `supported_parameters` list.
// OpenRouter tracks this separately from `"tools"`: the two are
// independent capabilities, not usually-correlated ones, so a model
// can have either, both, or neither. GenerateNick and
// GeneratePersonaTemplates set a strict `json_schema` `ResponseFormat` and
// never set `Tools`, so this is the parameter their calls depend on.
func (m ModelInfo) SupportsStructuredOutputs() bool {
	return slices.Contains(m.SupportedParameters, "structured_outputs")
}

// Usage contains token and cost metadata returned by OpenRouter.
type Usage struct {
	PromptTokens          int64   `json:"prompt_tokens"`
	CompletionTokens      int64   `json:"completion_tokens"`
	TotalTokens           int64   `json:"total_tokens"`
	ReasoningTokens       int64   `json:"reasoning_tokens"`
	CachedTokens          int64   `json:"cached_tokens"`
	CacheWriteTokens      int64   `json:"cache_write_tokens"`
	CostCredits           float64 `json:"cost_credits"`
	UpstreamInferenceCost float64 `json:"upstream_inference_cost"`
}

// TurnHistory is the transcript one turn sends, split at the point a
// provider may cache up to. Cacheable holds the summaries of what has
// been compacted and the recent transcript, which grow only at their
// tail, so a provider matching a cached prompt prefix reuses all of
// it. Current holds the server state as it stands for this turn: the
// member list, the topic, the memories selected against the turn's
// traffic and the per-participant persona records, none of which a
// later turn can be assumed to repeat.
//
// The request carries a prompt-cache breakpoint at the end of
// Cacheable, so a change in Current invalidates only what follows it.
type TurnHistory struct {
	Cacheable []protocol.IRCMessage
	Current   []protocol.IRCMessage
}

// Messages returns the whole transcript in the order the provider
// receives it.
func (h TurnHistory) Messages() []protocol.IRCMessage {
	return slices.Concat(h.Cacheable, h.Current)
}

// ToolDefinition describes a model-callable tool.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// SystemPrompt separates reusable application instructions from
// instance-specific state. Providers can cache Fixed at an explicit
// content-block boundary. Dynamic follows in a later message so providers
// that normalise system content can still reuse the fixed instructions.
type SystemPrompt struct {
	Fixed   string `json:"fixed"`
	Dynamic string `json:"dynamic"`
}

// Text returns the prompt as providers without content-block caching read it.
func (p SystemPrompt) Text() string {
	return p.Fixed + p.Dynamic
}

// PendingToolCall represents a tool call from the model that requires
// execution before the conversation can continue.
type PendingToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// ToolResult carries the outcome of executing a pending tool call,
// ready to be sent back to the model as a tool response message.
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
}

// Conversation is an opaque handle to the accumulated messages in a
// multi-turn tool-calling exchange. It is returned inside
// CompletionResult when the model calls intermediate tools.
type Conversation struct {
	modelID        domain.ModelID
	promptCacheKey domain.InstanceID
	messages       []openai.ChatCompletionMessageParamUnion
}

// CompletionResult contains the model's tool calls and raw text (if
// any) alongside request metadata. AssistantText is retained for the
// turn journal but does not make text model output actionable. An
// empty PendingToolCalls slice with a nil Conversation signals
// silence. When PendingToolCalls is non-empty, Conversation carries
// the message state the next turn appends to.
type CompletionResult struct {
	PendingToolCalls []PendingToolCall
	AssistantText    string
	Refusal          string
	ResponseReceived bool
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

// RejectedNick pairs a nickname suggestion GenerateNick already tried
// with why it could not be used. NickReasonGenerator's retry prompt
// uses Reason to tell a grammar rejection ("must start with a letter
// or one of ...") from a plain collision ("already taken"), which
// GenerateNick's single fixed wording cannot.
type RejectedNick struct {
	Nick   domain.Nick
	Reason string
}

// NickReasonGenerator is an optional capability a Client can
// implement alongside GenerateNick: the same nickname generation,
// except each previously rejected suggestion carries the reason it
// was rejected. A caller that wants that distinction in the retry
// prompt probes for this interface with a type assertion; a Client
// that does not implement it is asked through GenerateNick instead,
// whose retry wording covers only a collision.
type NickReasonGenerator interface {
	GenerateNickWithReasons(
		ctx context.Context,
		smallModel domain.ModelID,
		persona string,
		excluded []RejectedNick,
	) (NicknameResult, error)
}

// ContextSummaryResult is one compact representation of projected
// transcript sources.
type ContextSummaryResult struct {
	Summary   string
	RequestID string
	Usage     Usage
}

// ContextSummarizer is the optional provider capability used when a
// complete turn does not fit the model's context window.
type ContextSummarizer interface {
	RenderContextSummaryRequest(
		modelID domain.ModelID,
		selfInstanceID domain.InstanceID,
		previous []string,
		sources []protocol.IRCMessage,
	) (RenderedEventRequest, error)
	SummarizeContext(
		ctx context.Context,
		modelID domain.ModelID,
		selfInstanceID domain.InstanceID,
		previous []string,
		sources []protocol.IRCMessage,
	) (ContextSummaryResult, error)
}

// ReflectionParticipant is one other client a reflection request names.
// Token is the handle a proposal uses to scope a relationship experience
// or amendment to this participant. Nick is the display name, present
// where the request carries an event this participant authored.
type ReflectionParticipant struct {
	Token string      `json:"token"`
	Nick  domain.Nick `json:"nick,omitempty"`
}

// ReflectionWindowKind is the JSON label for the conversation a
// reflection event belongs to. It is a word because the reflection
// request is what a model reads, and nothing in that request defines
// [domain.ChannelKind]'s integer encoding.
type ReflectionWindowKind string

const (
	// ReflectionWindowChannel is a named channel, and Window is its name.
	ReflectionWindowChannel ReflectionWindowKind = "channel"
	// ReflectionWindowDirect is a direct message, and Window is the
	// counterpart's participant token.
	ReflectionWindowDirect ReflectionWindowKind = "direct"
)

// ReflectionWindowKindFor labels the conversation `target` names: a
// channel target is a channel, and anything else is a direct message.
//
// It takes the sealed target because [domain.ChannelKind] also holds the
// status window, which has no label here. A nil target is the same
// unlabelled case, and no reflection candidate carries one: the store
// refuses a candidate with no window before it is filed.
func ReflectionWindowKindFor(target protocol.WindowTarget) ReflectionWindowKind {
	if _, ok := protocol.ChannelWindowName(target); ok {
		return ReflectionWindowChannel
	}

	return ReflectionWindowDirect
}

// ReflectionInputEvent is one sequenced candidate supplied to private persona
// reflection. Participant is the token of the client that authored the
// message, empty for a server line or for the reflecting instance itself.
// Window groups events by conversation: a channel by name, a direct message
// by the counterpart's token.
type ReflectionInputEvent struct {
	Sequence    domain.ReflectionSequence `json:"sequence"`
	WindowKind  ReflectionWindowKind      `json:"window_kind"`
	Window      string                    `json:"window"`
	Participant string                    `json:"participant,omitempty"`
	Message     protocol.IRCMessage       `json:"message"`
	Substantive bool                      `json:"substantive"`
}

// ReflectionInputExperience is one active experience as the reflecting
// model reads it. Subject is a participant token.
type ReflectionInputExperience struct {
	Kind       domain.ExperienceKind       `json:"kind"`
	Summary    string                      `json:"summary"`
	Subject    string                      `json:"subject,omitempty"`
	Confidence domain.Confidence           `json:"confidence"`
	OccurredAt time.Time                   `json:"occurred_at"`
	Sources    []domain.ReflectionSequence `json:"sources"`
}

// ReflectionInputAmendment is one active tendency as the reflecting model
// reads it. Token is what a proposal names to retract or supersede it, and
// Counterpart is a participant token.
type ReflectionInputAmendment struct {
	Token       string                `json:"token"`
	Scope       domain.AmendmentScope `json:"scope"`
	Counterpart string                `json:"counterpart,omitempty"`
	Tendency    string                `json:"tendency"`
	Confidence  domain.Confidence     `json:"confidence"`
	CreatedAt   time.Time             `json:"created_at"`
	ExpiresAt   *time.Time            `json:"expires_at,omitempty"`
}

// ReflectionInput is the closed set of persona information and the event
// range supplied to one reflection request. Description is the persona in
// force, which is the active revision's; Baseline is revision zero's,
// which a reset returns to.
type ReflectionInput struct {
	Description  string                      `json:"description"`
	Baseline     string                      `json:"baseline"`
	Participants []ReflectionParticipant     `json:"participants"`
	Experiences  []ReflectionInputExperience `json:"active_experiences"`
	Amendments   []ReflectionInputAmendment  `json:"active_amendments"`
	Events       []ReflectionInputEvent      `json:"events"`
}

// ReflectionExperienceProposal is one bounded experience proposed from the
// supplied event range. Sources contains reflection sequence numbers and
// Subject a participant token.
type ReflectionExperienceProposal struct {
	Key        string                      `json:"key"`
	Kind       domain.ExperienceKind       `json:"kind"`
	Summary    string                      `json:"summary"`
	Subject    string                      `json:"subject"`
	Confidence domain.Confidence           `json:"confidence"`
	Sources    []domain.ReflectionSequence `json:"sources"`
}

// ReflectionAmendmentProposal is one tendency proposed from experiences in
// the same response. Counterpart is a participant token, and Supersedes the
// token of the active amendment this one replaces, empty when it replaces
// nothing.
type ReflectionAmendmentProposal struct {
	Scope        domain.AmendmentScope `json:"scope"`
	Counterpart  string                `json:"counterpart"`
	Tendency     string                `json:"tendency"`
	Confidence   domain.Confidence     `json:"confidence"`
	EvidenceKeys []string              `json:"evidence_keys"`
	Supersedes   string                `json:"supersedes"`
}

// ReflectionPersonaProposal is a replacement persona description drawn
// from experiences in the same response. An empty Description proposes no
// change and leaves the next revision on the parent's text.
//
// Consolidates holds the tokens of active amendments the description
// absorbs. Each leaves the active set and keeps its row, so the trail from
// a description back to the tendencies behind it survives.
type ReflectionPersonaProposal struct {
	Description  string   `json:"description"`
	EvidenceKeys []string `json:"evidence_keys"`
	Consolidates []string `json:"consolidates"`
}

// ReflectionProposal is the structured output of one private reflection.
// Empty slices and an empty description mean no change. Retract holds
// active amendment tokens.
type ReflectionProposal struct {
	Experiences []ReflectionExperienceProposal `json:"experiences"`
	Amendments  []ReflectionAmendmentProposal  `json:"amendments"`
	Persona     ReflectionPersonaProposal      `json:"persona"`
	Retract     []string                       `json:"retract_amendments"`
}

// ReflectionResult carries a structured proposal and its provider evidence.
type ReflectionResult struct {
	Proposal  ReflectionProposal
	RequestID string
	Usage     Usage
}

// ReflectionGenerator is the optional provider capability used by the
// manager-owned private reflection scheduler.
//
// A reflection is two phases. ReflectPersona and ContinueReflection
// explore: the instance reads its own past through read-only tools, with
// no response format set. ProposeReflection closes the run: the
// accumulated conversation is asked for its proposal, with the schema and
// no tools. Splitting them keeps any one request from needing tool
// support and structured output together, which OpenRouter's
// `supported_parameters` reports separately and cannot answer for.
type ReflectionGenerator interface {
	ReflectPersona(
		ctx context.Context,
		modelID domain.ModelID,
		instanceID domain.InstanceID,
		input ReflectionInput,
		tools ...ToolDefinition,
	) (ReflectionExploration, error)
	ContinueReflection(
		ctx context.Context,
		conv *Conversation,
		results []ToolResult,
		tools ...ToolDefinition,
	) (ReflectionExploration, error)
	ProposeReflection(
		ctx context.Context,
		conv *Conversation,
		results []ToolResult,
		support StructuredOutputSupport,
	) (ReflectionResult, error)
}

// Client defines the interface for all API interactions. Both the
// chat completion (via openai-go) and OpenRouter-specific calls are
// abstracted behind this interface to support testing with fakes.
type Client interface {
	// ListModels fetches available models from the OpenRouter API.
	ListModels(ctx context.Context) ([]ModelInfo, error)

	// RenderEventRequest returns the non-secret request evidence for a
	// SendEvents call without sending it.
	RenderEventRequest(
		modelID domain.ModelID,
		selfInstanceID domain.InstanceID,
		systemPrompt SystemPrompt,
		history TurnHistory,
		events []protocol.IRCMessage,
		tools ...ToolDefinition,
	) (RenderedEventRequest, error)

	// RenderToolResultRequest returns the non-secret request evidence
	// for a ContinueWithToolResults call without sending it.
	RenderToolResultRequest(
		conv *Conversation,
		results []ToolResult,
		tools ...ToolDefinition,
	) (RenderedEventRequest, error)

	// SendEvents sends a batch of protocol events to a model and
	// returns its response. The system prompt and conversation
	// history are managed by the caller.
	SendEvents(
		ctx context.Context,
		modelID domain.ModelID,
		selfInstanceID domain.InstanceID,
		systemPrompt SystemPrompt,
		history TurnHistory,
		events []protocol.IRCMessage,
		tools ...ToolDefinition,
	) (CompletionResult, error)

	// ContinueWithToolResults sends tool execution results back to
	// the model and returns the next response. The Conversation
	// carries the accumulated message state; ToolResults are appended
	// as tool-role messages before the next API call.
	ContinueWithToolResults(
		ctx context.Context,
		conv *Conversation,
		results []ToolResult,
		tools ...ToolDefinition,
	) (CompletionResult, error)

	// GenerateNick asks a model to suggest one IRC-style nickname
	// guided by `persona` (used as taste guidance, not a literal
	// label). `excludePreviousSuggestions` carries nicks the caller
	// has previously rejected — typically because of a session-side
	// uniqueness collision; the prompt mentions them verbatim and
	// asks for a different one. The caller's authoritative nick list
	// is intentionally never revealed to the model.
	GenerateNick(ctx context.Context, smallModel domain.ModelID, persona string, excludePreviousSuggestions []domain.Nick) (NicknameResult, error)

	// GeneratePersonaTemplates asks a model to write a set of persona
	// templates for the pool. Each carries Origin
	// domain.PersonaGenerated.
	GeneratePersonaTemplates(ctx context.Context, smallModel domain.ModelID) ([]domain.PersonaTemplate, error)
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
