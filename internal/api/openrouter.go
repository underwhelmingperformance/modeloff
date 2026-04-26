package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/invopop/jsonschema"
	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// Default per-call timeouts. The chat timeout bounds how long a model
// may take to produce a completion; the meta timeout bounds the
// smaller OpenRouter-specific endpoints (model listing, nickname and
// persona generation). Both apply only when the caller has not
// already set a deadline on the context.
const (
	defaultChatTimeout = 60 * time.Second
	defaultMetaTimeout = 30 * time.Second
)

// OpenRouterClient implements Client using openai-go for chat
// completions and direct HTTP for OpenRouter-specific endpoints.
type OpenRouterClient struct {
	oai            openai.Client
	baseURL        string
	apiKey         string
	http           *http.Client
	chatTimeout    time.Duration
	metaTimeout    time.Duration
	tracerProvider trace.TracerProvider
}

// NewOpenRouterClient creates a client configured to talk to an
// OpenAI-compatible API at baseURL. The client guards each call with
// a default timeout so a hung model or stalled network cannot block
// indefinitely; callers may still pass a context with a tighter
// deadline, which always wins.
func NewOpenRouterClient(apiKey, baseURL string, httpClient *http.Client) *OpenRouterClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	oai := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
		option.WithHTTPClient(httpClient),
	)

	return &OpenRouterClient{
		oai:            oai,
		baseURL:        baseURL,
		apiKey:         apiKey,
		http:           httpClient,
		chatTimeout:    defaultChatTimeout,
		metaTimeout:    defaultMetaTimeout,
		tracerProvider: otel.GetTracerProvider(),
	}
}

// WithTimeouts overrides the per-call chat and meta timeouts on the
// client. It is intended for tests that need shorter deadlines than
// the defaults; production code should rely on the defaults. The
// receiver is mutated in place; the return value is provided only
// for chaining at construction time, not for builder-style cloning.
func (c *OpenRouterClient) WithTimeouts(chat, meta time.Duration) *OpenRouterClient {
	c.chatTimeout = chat
	c.metaTimeout = meta

	return c
}

// WithTracerProvider overrides the OTel `TracerProvider` the client
// uses for its spans. Tests inject a per-test recorder so span
// recordings stay scoped to a single test rather than relying on the
// global provider's swap-and-restore. Production code does not need
// to call this — the default global provider is already correct.
func (c *OpenRouterClient) WithTracerProvider(tp trace.TracerProvider) *OpenRouterClient {
	c.tracerProvider = tp

	return c
}

func (c *OpenRouterClient) tracer() trace.Tracer {
	return c.tracerProvider.Tracer("github.com/laney/modeloff/internal/api")
}

// ensureDeadline wraps ctx with the given timeout if it has no
// deadline of its own, otherwise it leaves the caller's deadline in
// place. The returned cancel must always be deferred by the caller.
func ensureDeadline(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}

	return context.WithTimeout(ctx, timeout)
}

type openRouterUsageExtras struct {
	Cost                float64 `json:"cost"`
	PromptTokensDetails struct {
		CacheWriteTokens    int64 `json:"cache_write_tokens"`
		CacheCreationTokens int64 `json:"cache_creation_tokens"`
	} `json:"prompt_tokens_details"`
	CostDetails struct {
		UpstreamInferenceCost float64 `json:"upstream_inference_cost"`
	} `json:"cost_details"`
}

type completionParseError struct {
	target string
	err    error
}

func (e *completionParseError) Error() string {
	return fmt.Sprintf("parse %s: %v", e.target, e.err)
}

func (e *completionParseError) Unwrap() error {
	return e.err
}

// generateSchema reflects a Go type into a JSON Schema map suitable
// for the OpenAI API. It uses invopop/jsonschema with inlining enabled
// so that all definitions are expanded in place.
func generateSchema[T any]() map[string]any {
	reflector := jsonschema.Reflector{
		DoNotReference: true,
	}

	var v T
	schema := reflector.Reflect(v)

	data, _ := json.Marshal(schema)

	var result map[string]any
	_ = json.Unmarshal(data, &result)

	return result
}

// modelResponseBody is the discriminated union for the model's
// reply/pass decision. It implements jsonschema.customSchemaImpl to
// produce the anyOf schema with const discriminators.
type modelResponseBody struct{}

func (modelResponseBody) JSONSchema() *jsonschema.Schema {
	reflector := jsonschema.Reflector{DoNotReference: true}
	replySpanSchema := reflector.Reflect(protocol.ReplySpan{})
	replyProps := jsonschema.NewProperties()
	replyProps.Set("kind", &jsonschema.Schema{Type: "string", Const: "reply"})
	replyProps.Set("messages", &jsonschema.Schema{
		Type:        "array",
		Description: "One or more messages to send.",
		Items: &jsonschema.Schema{
			Type:                 "object",
			AdditionalProperties: jsonschema.FalseSchema,
			Properties: func() *orderedmap.OrderedMap[string, *jsonschema.Schema] {
				p := jsonschema.NewProperties()
				p.Set("type", &jsonschema.Schema{
					Type:        "string",
					Enum:        []any{"message", "action"},
					Description: `"message" for a regular message, "action" for a /me action.`,
				})
				// Keep the message item schema flat. Runtime validation still
				// enforces exactly one of body or spans, but avoiding a nested
				// anyOf here keeps the schema friendlier to stricter providers.
				p.Set("body", &jsonschema.Schema{
					Type:        "string",
					Description: "The plain message text. For actions, just the action body without /me. Provide either body or spans, not both.",
				})
				p.Set("spans", &jsonschema.Schema{
					Type:        "array",
					Description: "Optional styled spans. Prefer this over raw IRC control characters when you want formatting. Provide either spans or body, not both.",
					Items:       replySpanSchema,
				})
				return p
			}(),
			Required: []string{"type"},
		},
	})

	passProps := jsonschema.NewProperties()
	passProps.Set("kind", &jsonschema.Schema{Type: "string", Const: "pass"})
	passProps.Set("reason", &jsonschema.Schema{
		Type:        "string",
		Description: "A brief reason for not replying.",
	})

	return &jsonschema.Schema{
		AnyOf: []*jsonschema.Schema{
			{
				Type:                 "object",
				Properties:           replyProps,
				Required:             []string{"kind", "messages"},
				AdditionalProperties: jsonschema.FalseSchema,
			},
			{
				Type:                 "object",
				Properties:           passProps,
				Required:             []string{"kind", "reason"},
				AdditionalProperties: jsonschema.FalseSchema,
			},
		},
	}
}

type modelResponseWrapper struct {
	Response modelResponseBody `json:"response"`
}

var modelResponseSchemaMap = generateSchema[modelResponseWrapper]()

func responseFormat() openai.ChatCompletionNewParamsResponseFormatUnion {
	return openai.ChatCompletionNewParamsResponseFormatUnion{
		OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
			JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
				Name:   "model_response",
				Schema: modelResponseSchemaMap,
				Strict: openai.Bool(true),
			},
		},
	}
}

func toolParams(definitions []ToolDefinition) []openai.ChatCompletionToolUnionParam {
	tools := make([]openai.ChatCompletionToolUnionParam, 0, len(definitions))

	for _, definition := range definitions {
		tools = append(tools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        definition.Name,
			Description: openai.String(definition.Description),
			Strict:      openai.Bool(true),
			Parameters:  definition.Parameters,
		}))
	}

	return tools
}

// SendEvents sends protocol events to a model and returns its typed
// response. The model replies via structured JSON output (reply or
// pass) and may optionally call memory tools.
func (c *OpenRouterClient) SendEvents(
	ctx context.Context,
	modelID domain.ModelID,
	selfInstanceID domain.InstanceID,
	systemPrompt string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
	tools ...ToolDefinition,
) (CompletionResult, error) {
	logger := slog.Default().With("component", "api.openrouter", "model_id", modelID)
	tracer := c.tracer()

	ctx, span := tracer.Start(ctx, "api.openrouter.send_events")
	span.SetAttributes(
		attribute.String(observability.AttrOperation, "api.openrouter.send_events"),
		attribute.String(observability.AttrModelID, string(modelID)),
	)
	defer span.End()

	msgs := buildMessages(systemPrompt, selfInstanceID, history, events)
	resp, rawResp, err := c.chatCompletion(ctx, modelID, openai.ChatCompletionNewParams{ //nolint:bodyclose // SDK reads and closes the body.
		Model:          shared.ChatModel(string(modelID)),
		Messages:       msgs,
		Tools:          toolParams(tools),
		ResponseFormat: responseFormat(),
	})
	if err != nil {
		markSpanError(span, observability.ErrorKindTransport, 0, err)
		logger.ErrorContext(ctx, "openrouter send events failed", "error", err)
		return CompletionResult{}, err
	}

	result, assistantMsg, err := parseCompletionResponse(resp, rawResp)
	if err != nil {
		markSpanError(span, completionParseErrorKind(err), 0, err)
		logger.ErrorContext(ctx, "openrouter response parse failed", "error", err)
		return CompletionResult{}, err
	}

	if len(result.PendingToolCalls) > 0 {
		result.Conversation = &Conversation{
			modelID:  modelID,
			messages: append(msgs, assistantMsg),
		}
	}

	result.Usage.SetSpanAttributes(span, result.RequestID)
	span.SetAttributes(attribute.String(observability.AttrResult, ResponseResultKind(result.Response)))

	logger.InfoContext(
		ctx,
		"openrouter send events completed",
		"request_id", result.RequestID,
		"result", ResponseResultKind(result.Response),
		"prompt_tokens", result.Usage.PromptTokens,
		"completion_tokens", result.Usage.CompletionTokens,
		"cost_credits", result.Usage.CostCredits,
		"event_count", len(events),
		"history_count", len(history),
	)

	return result, nil
}

// ContinueWithToolResults appends tool result messages to the
// conversation and sends the next request.
func (c *OpenRouterClient) ContinueWithToolResults(
	ctx context.Context,
	conv *Conversation,
	results []ToolResult,
	tools ...ToolDefinition,
) (CompletionResult, error) {
	logger := slog.Default().With("component", "api.openrouter", "model_id", conv.modelID)
	tracer := c.tracer()

	ctx, span := tracer.Start(ctx, "api.openrouter.continue_with_tool_results")
	span.SetAttributes(
		attribute.String(observability.AttrOperation, "api.openrouter.continue_with_tool_results"),
		attribute.String(observability.AttrModelID, string(conv.modelID)),
	)
	defer span.End()

	msgs := conv.messages
	for _, r := range results {
		msgs = append(msgs, openai.ToolMessage(r.Content, r.ToolCallID))
	}

	resp, rawResp, err := c.chatCompletion(ctx, conv.modelID, openai.ChatCompletionNewParams{ //nolint:bodyclose // SDK reads and closes the body.
		Model:          shared.ChatModel(string(conv.modelID)),
		Messages:       msgs,
		Tools:          toolParams(tools),
		ResponseFormat: responseFormat(),
	})
	if err != nil {
		markSpanError(span, observability.ErrorKindTransport, 0, err)
		logger.ErrorContext(ctx, "openrouter continue failed", "error", err)
		return CompletionResult{}, err
	}

	result, assistantMsg, err := parseCompletionResponse(resp, rawResp)
	if err != nil {
		markSpanError(span, completionParseErrorKind(err), 0, err)
		logger.ErrorContext(ctx, "openrouter continue parse failed", "error", err)
		return CompletionResult{}, err
	}

	if len(result.PendingToolCalls) > 0 {
		// Append tool results and the new assistant message for the
		// next iteration.
		nextMsgs := make([]openai.ChatCompletionMessageParamUnion, len(msgs), len(msgs)+1)
		copy(nextMsgs, msgs)
		nextMsgs = append(nextMsgs, assistantMsg)

		result.Conversation = &Conversation{
			modelID:  conv.modelID,
			messages: nextMsgs,
		}
	}

	result.Usage.SetSpanAttributes(span, result.RequestID)
	span.SetAttributes(attribute.String(observability.AttrResult, ResponseResultKind(result.Response)))

	logger.InfoContext(
		ctx,
		"openrouter continue completed",
		"request_id", result.RequestID,
		"result", ResponseResultKind(result.Response),
		"prompt_tokens", result.Usage.PromptTokens,
		"completion_tokens", result.Usage.CompletionTokens,
		"cost_credits", result.Usage.CostCredits,
	)

	return result, nil
}

func buildMessages(
	systemPrompt string,
	selfInstanceID domain.InstanceID,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
) []openai.ChatCompletionMessageParamUnion {
	msgs := []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(systemPrompt)}

	appendMsg := func(m protocol.IRCMessage) {
		isSelf := selfInstanceID != "" && m.InstanceID == selfInstanceID

		// Strip the internal instance ID before marshalling so it
		// never appears in the prompt sent to the model.
		m.InstanceID = ""

		data, _ := json.Marshal(m)
		if isSelf {
			msgs = append(msgs, openai.AssistantMessage(string(data)))
		} else {
			msgs = append(msgs, openai.UserMessage(string(data)))
		}
	}

	for _, h := range history {
		appendMsg(h)
	}

	for _, e := range events {
		if selfInstanceID != "" && e.InstanceID == selfInstanceID {
			continue
		}

		appendMsg(e)
	}

	return msgs
}

type structuredModelResponse struct {
	Response struct {
		Kind     string               `json:"kind"`
		Messages []protocol.ReplyPart `json:"messages,omitempty"`
		Reason   string               `json:"reason,omitempty"`
	} `json:"response"`
}

// parseCompletionResponse extracts the model's structured response and
// any tool calls from an API response. It returns the
// CompletionResult plus the raw assistant message (needed to build
// the next turn in multi-turn exchanges).
//
// The model's reply/pass decision arrives as structured JSON in the
// message content. Tool calls arrive as tool_calls and are returned
// as PendingToolCalls. When
// pending calls are present, the caller must continue the
// conversation.
func parseCompletionResponse(resp *openai.ChatCompletion, rawResp *http.Response) (CompletionResult, openai.ChatCompletionMessageParamUnion, error) {
	if resp == nil || len(resp.Choices) == 0 {
		return CompletionResult{}, openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("no choices in response")
	}

	choice := resp.Choices[0]
	msg := choice.Message
	result := CompletionResult{
		RequestID: requestIDFromChatCompletion(resp, rawResp),
		Usage:     usageFromResponse(resp.Usage),
	}

	if err := validateChoice(choice); err != nil {
		return CompletionResult{}, openai.ChatCompletionMessageParamUnion{}, err
	}

	assistantMsg := msg.ToParam()

	for _, call := range msg.ToolCalls {
		result.PendingToolCalls = append(result.PendingToolCalls, PendingToolCall{
			ID:   call.ID,
			Name: call.Function.Name,
			Args: json.RawMessage(call.Function.Arguments),
		})
	}

	if msg.Content != "" {
		var structured structuredModelResponse

		if err := json.Unmarshal([]byte(msg.Content), &structured); err != nil {
			return CompletionResult{}, openai.ChatCompletionMessageParamUnion{}, &completionParseError{target: "structured response", err: err}
		}

		switch structured.Response.Kind {
		case "reply":
			result.Response = protocol.ModelResponse{
				Kind:     protocol.ResponseReply,
				Messages: structured.Response.Messages,
			}
		case "pass":
			result.Response = protocol.ModelResponse{
				Kind:   protocol.ResponseSilence,
				Reason: structured.Response.Reason,
			}
		default:
			return CompletionResult{}, openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("unknown response kind: %q", structured.Response.Kind)
		}
	}

	if result.Response.Kind == "" && len(result.PendingToolCalls) == 0 {
		return CompletionResult{}, openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("model returned no response and no tool calls")
	}

	return result, assistantMsg, nil
}

type modelsResponse struct {
	Data []struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		Description   string `json:"description"`
		ContextLength int    `json:"context_length"`
	} `json:"data"`
}

// responseBodyLogLimit caps how much of an upstream non-2xx body we
// retain on spans and logs. Large JSON error payloads would otherwise
// bloat both the trace and the log stream without adding diagnostic
// value — the leading portion is almost always sufficient.
const responseBodyLogLimit = 4096

// shapedError carries a user-safe single-line message while preserving
// the original error in the chain so `errors.Is`/`errors.As` callers
// (timeouts, cancellation) keep working. Error renders only msg —
// callers that want the underlying detail must Unwrap.
type shapedError struct {
	msg   string
	cause error
}

func (e *shapedError) Error() string { return e.msg }
func (e *shapedError) Unwrap() error { return e.cause }

// truncateBody returns body as a string, capped at limit bytes and
// suffixed with a marker when truncation occurred. The cut point
// rewinds to the nearest UTF-8 rune boundary so the returned string
// is always valid UTF-8 — otherwise a multi-byte rune straddling
// the limit would produce mojibake that some log/trace exporters
// will drop or replace with U+FFFD.
func truncateBody(body []byte, limit int) string {
	if len(body) <= limit {
		return string(body)
	}

	end := limit
	for end > 0 && !utf8.RuneStart(body[end]) {
		end--
	}

	return string(body[:end]) + "…[truncated]"
}

// ListModels fetches available models from the OpenRouter API.
func (c *OpenRouterClient) ListModels(ctx context.Context) ([]ModelInfo, error) {
	ctx, cancel := ensureDeadline(ctx, c.metaTimeout)
	defer cancel()

	logger := slog.Default().With("component", "api.openrouter")
	tracer := c.tracer()

	ctx, span := tracer.Start(ctx, "api.openrouter.list_models")
	span.SetAttributes(attribute.String(observability.AttrOperation, "api.openrouter.list_models"))
	defer span.End()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		markSpanError(span, observability.ErrorKindTransport, 0, err)
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		shaped := &shapedError{msg: "list models: network error", cause: err}
		markSpanError(span, observability.ErrorKindTransport, 0, shaped)
		logger.ErrorContext(ctx, "openrouter list models transport failure", "error", err)
		return nil, shaped
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(resp.Body)
	truncated := truncateBody(body, responseBodyLogLimit)

	if readErr != nil {
		shaped := &shapedError{msg: "list models: response read failed", cause: readErr}
		span.SetAttributes(attribute.String(observability.AttrHTTPResponseBody, truncated))
		markSpanError(span, observability.ErrorKindTransport, resp.StatusCode, shaped)
		logger.ErrorContext(ctx, "openrouter list models read failed",
			"status", resp.StatusCode,
			"body_read_error", readErr,
		)
		return nil, shaped
	}

	if resp.StatusCode != http.StatusOK {
		span.SetAttributes(attribute.String(observability.AttrHTTPResponseBody, truncated))
		shaped := fmt.Errorf("list models: status %d", resp.StatusCode)
		markSpanError(span, observability.ErrorKindHTTPStatus, resp.StatusCode, shaped)
		logger.ErrorContext(ctx, "openrouter list models non-2xx",
			"status", resp.StatusCode,
			observability.AttrHTTPResponseBody, truncated,
		)
		return nil, shaped
	}

	var mr modelsResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		shaped := &shapedError{msg: "list models: invalid response", cause: err}
		span.SetAttributes(attribute.String(observability.AttrHTTPResponseBody, truncated))
		markSpanError(span, observability.ErrorKindResponseParse, 0, shaped)
		logger.ErrorContext(ctx, "openrouter list models decode failed",
			"decode_error", err,
			observability.AttrHTTPResponseBody, truncated,
		)
		return nil, shaped
	}

	models := make([]ModelInfo, len(mr.Data))
	for i, model := range mr.Data {
		models[i] = ModelInfo{
			ID:          domain.ModelID(model.ID),
			Name:        model.Name,
			Description: model.Description,
			ContextLen:  model.ContextLength,
		}
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
	logger.DebugContext(ctx, "openrouter list models completed", "count", len(models))

	return models, nil
}

// nicknameResponse is the structured output the model returns. The
// schema enforces shape (length and allowed characters) so callers do
// not need to sanitise free-form text.
type nicknameResponse struct {
	Nick string `json:"nick" jsonschema:"minLength=1,maxLength=12,pattern=^[a-z0-9_-]{1,12}$" jsonschema_description:"Exactly one IRC nickname suggestion."`
}

var nicknameSchemaMap = generateSchema[nicknameResponse]()

func nicknameResponseFormat() openai.ChatCompletionNewParamsResponseFormatUnion {
	return openai.ChatCompletionNewParamsResponseFormatUnion{
		OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
			JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
				Name:   "nickname_response",
				Schema: nicknameSchemaMap,
				Strict: openai.Bool(true),
			},
		},
	}
}

const nicknamePrompt = `Generate exactly one short, fun IRC-style nickname for an IRC regular.

Constraints:
- return JSON only and match the schema exactly
- do not explain the choice
- produce one nickname, not a list
- do not use words based on assistant names, model names, or generic AI terms unless the persona strongly implies them
- prefer something that sounds like a handle a human would pick on IRC
- do not treat the persona as the person's whole identity
- avoid simply turning the persona description into a literal label
- prefer something a real user might have chosen years ago: suggest habits, interests, in-jokes, tone, or history rather than job-title summaries
- a slightly indirect or playful nick is better than an obvious descriptor
- prefer nicks that feel personally chosen and lived-in
- avoid obviously symbolic or overly neat compositions

Persona: %s`

// GenerateNick asks a model to suggest one IRC-style nickname guided
// by the persona description. Rejected suggestions from prior calls
// are folded into the conversation as a follow-up turn so the model
// avoids repeating them; the caller's authoritative nick list is
// never sent.
func (c *OpenRouterClient) GenerateNick(
	ctx context.Context,
	smallModel domain.ModelID,
	persona string,
	excludePreviousSuggestions []domain.Nick,
) (NicknameResult, error) {
	ctx, cancel := ensureDeadline(ctx, c.metaTimeout)
	defer cancel()

	logger := slog.Default().With(
		"component", "api.openrouter",
		"small_model", smallModel,
		"attempt", len(excludePreviousSuggestions)+1,
	)
	tracer := c.tracer()

	ctx, span := tracer.Start(ctx, "api.openrouter.generate_nick")
	span.SetAttributes(
		attribute.String(observability.AttrOperation, "api.openrouter.generate_nick"),
		attribute.String(observability.AttrModelID, string(smallModel)),
	)
	defer span.End()

	messages := []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage(fmt.Sprintf(nicknamePrompt, persona)),
	}

	for _, rejected := range excludePreviousSuggestions {
		messages = append(messages,
			openai.AssistantMessage(fmt.Sprintf(`{"nick":%q}`, string(rejected))),
			openai.UserMessage(fmt.Sprintf(
				"That nick is already taken. Suggest a different one. Avoid: %s",
				string(rejected),
			)),
		)
	}

	resp, rawResp, err := c.chatCompletion(ctx, smallModel, openai.ChatCompletionNewParams{ //nolint:bodyclose // SDK reads and closes the body.
		Model:          shared.ChatModel(string(smallModel)),
		Messages:       messages,
		ResponseFormat: nicknameResponseFormat(),
	})
	if err != nil {
		markSpanError(span, observability.ErrorKindTransport, 0, err)
		logger.ErrorContext(ctx, "openrouter generate nick failed", "error", err)
		return NicknameResult{}, err
	}

	if len(resp.Choices) == 0 {
		err := fmt.Errorf("generate nick: no choices in response")
		markSpanError(span, observability.ErrorKindInvalidResponse, 0, err)
		return NicknameResult{}, err
	}

	choice := resp.Choices[0]

	if err := validateChoice(choice); err != nil {
		markSpanError(span, observability.ErrorKindInvalidResponse, 0, err)
		return NicknameResult{}, err
	}

	var parsed nicknameResponse
	if err := json.Unmarshal([]byte(choice.Message.Content), &parsed); err != nil {
		markSpanError(span, observability.ErrorKindResponseParse, 0, err)
		return NicknameResult{}, &completionParseError{target: "nickname", err: err}
	}

	if parsed.Nick == "" {
		err := fmt.Errorf("generate nick: schema-valid response carried an empty nick")
		markSpanError(span, observability.ErrorKindInvalidResponse, 0, err)
		return NicknameResult{}, err
	}

	result := NicknameResult{
		Nick:      domain.Nick(parsed.Nick),
		RequestID: requestIDFromChatCompletion(resp, rawResp),
		Usage:     usageFromResponse(resp.Usage),
	}

	result.Usage.SetSpanAttributes(span, result.RequestID)
	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	logger.InfoContext(ctx, "openrouter generate nick completed",
		"request_id", result.RequestID,
		"nick", parsed.Nick,
	)

	return result, nil
}

// personaItem is the per-persona shape returned by the model.
type personaItem struct {
	ID          string `json:"id" jsonschema_description:"A short kebab-case identifier for this persona."`
	Description string `json:"description" jsonschema_description:"A one-line description of the persona."`
}

// personaListWrapper is the top-level structured output envelope.
type personaListWrapper struct {
	Personas []personaItem `json:"personas"`
}

var personaSchemaMap = generateSchema[personaListWrapper]()

func personaResponseFormat() openai.ChatCompletionNewParamsResponseFormatUnion {
	return openai.ChatCompletionNewParamsResponseFormatUnion{
		OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
			JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
				Name:   "persona_list",
				Schema: personaSchemaMap,
				Strict: openai.Bool(true),
			},
		},
	}
}

// GeneratePersonas asks a model to generate a set of IRC user personas
// using structured output, returning them with PersonaGenerated origin.
func (c *OpenRouterClient) GeneratePersonas(ctx context.Context, smallModel domain.ModelID) ([]domain.Persona, error) {
	ctx, cancel := ensureDeadline(ctx, c.metaTimeout)
	defer cancel()

	logger := slog.Default().With("component", "api.openrouter", "model_id", smallModel)
	tracer := c.tracer()

	ctx, span := tracer.Start(ctx, "api.openrouter.generate_personas")
	span.SetAttributes(
		attribute.String(observability.AttrOperation, "api.openrouter.generate_personas"),
		attribute.String(observability.AttrModelID, string(smallModel)),
	)
	defer span.End()

	prompt := "Generate 10 distinct IRC user personas. Each should have a short kebab-case ID " +
		"and a one-line description. Make them varied. No AI-isms. These are IRC regulars."

	resp, rawResp, err := c.chatCompletion(ctx, smallModel, openai.ChatCompletionNewParams{ //nolint:bodyclose // SDK reads and closes the body.
		Model: shared.ChatModel(string(smallModel)),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(prompt),
		},
		ResponseFormat: personaResponseFormat(),
	})
	if err != nil {
		markSpanError(span, observability.ErrorKindTransport, 0, err)
		logger.ErrorContext(ctx, "openrouter generate personas failed", "error", err)
		return nil, err
	}

	if len(resp.Choices) == 0 {
		err := fmt.Errorf("generate personas: no choices in response")
		markSpanError(span, observability.ErrorKindInvalidResponse, 0, err)
		return nil, err
	}

	choice := resp.Choices[0]

	if err := validateChoice(choice); err != nil {
		markSpanError(span, observability.ErrorKindInvalidResponse, 0, err)
		return nil, err
	}

	var wrapper personaListWrapper
	if err := json.Unmarshal([]byte(choice.Message.Content), &wrapper); err != nil {
		markSpanError(span, observability.ErrorKindResponseParse, 0, err)
		return nil, &completionParseError{target: "persona list", err: err}
	}

	personas := make([]domain.Persona, len(wrapper.Personas))
	for i, p := range wrapper.Personas {
		personas[i] = domain.Persona{
			ID:          p.ID,
			Description: p.Description,
			Origin:      domain.PersonaGenerated,
		}
	}

	usage := usageFromResponse(resp.Usage)
	requestID := requestIDFromChatCompletion(resp, rawResp)
	usage.SetSpanAttributes(span, requestID)
	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	logger.InfoContext(ctx, "openrouter generate personas completed",
		"request_id", requestID,
		"count", len(personas),
	)

	return personas, nil
}

func (c *OpenRouterClient) chatCompletion(
	ctx context.Context,
	modelID domain.ModelID,
	payload openai.ChatCompletionNewParams,
) (*openai.ChatCompletion, *http.Response, error) {
	ctx, cancel := ensureDeadline(ctx, c.chatTimeout)
	defer cancel()

	var rawResp *http.Response

	opts := []option.RequestOption{
		option.WithResponseInto(&rawResp),
	}

	if isAnthropicModel(modelID) {
		opts = append(opts, option.WithJSONSet("cache_control", map[string]string{"type": "ephemeral"}))
	}

	completion, err := c.oai.Chat.Completions.New(
		ctx,
		payload,
		opts...,
	)
	if err != nil {
		return nil, rawResp, fmt.Errorf("chat completion: %w", err)
	}

	return completion, rawResp, nil
}

func isAnthropicModel(modelID domain.ModelID) bool {
	return strings.HasPrefix(string(modelID), "anthropic/")
}

func requestIDFromChatCompletion(resp *openai.ChatCompletion, rawResp *http.Response) string {
	if resp != nil && resp.ID != "" {
		return resp.ID
	}

	if rawResp == nil {
		return ""
	}

	if requestID := rawResp.Header.Get("x-request-id"); requestID != "" {
		return requestID
	}

	return rawResp.Header.Get("request-id")
}

func usageFromResponse(response openai.CompletionUsage) Usage {
	var extra openRouterUsageExtras

	rawJSON := response.RawJSON()
	if rawJSON != "" {
		_ = json.Unmarshal([]byte(rawJSON), &extra)
	}

	cacheWriteTokens := extra.PromptTokensDetails.CacheWriteTokens
	if cacheWriteTokens == 0 {
		cacheWriteTokens = extra.PromptTokensDetails.CacheCreationTokens
	}

	return Usage{
		PromptTokens:          response.PromptTokens,
		CompletionTokens:      response.CompletionTokens,
		TotalTokens:           response.TotalTokens,
		ReasoningTokens:       response.CompletionTokensDetails.ReasoningTokens,
		CachedTokens:          response.PromptTokensDetails.CachedTokens,
		CacheWriteTokens:      cacheWriteTokens,
		CostCredits:           extra.Cost,
		UpstreamInferenceCost: extra.CostDetails.UpstreamInferenceCost,
	}
}

func markSpanError(span interface {
	SetAttributes(...attribute.KeyValue)
	SetStatus(codes.Code, string)
}, errorKind string, httpStatusCode int, err error) {
	attrs := []attribute.KeyValue{
		attribute.String(observability.AttrResult, observability.ResultError),
		attribute.String(observability.AttrErrorKind, errorKind),
	}
	if httpStatusCode > 0 {
		attrs = append(attrs, attribute.Int(observability.AttrHTTPStatusCode, httpStatusCode))
	}

	span.SetAttributes(attrs...)
	span.SetStatus(codes.Error, err.Error())
}

func completionParseErrorKind(err error) string {
	var refused *ErrModelRefused
	var parseErr *completionParseError
	switch {
	case errors.As(err, &refused):
		return observability.ErrorKindInvalidResponse
	case errors.Is(err, ErrContentFiltered), errors.Is(err, ErrResponseTruncated):
		return observability.ErrorKindInvalidResponse
	case errors.As(err, &parseErr):
		return observability.ErrorKindResponseParse
	default:
		return observability.ErrorKindInvalidResponse
	}
}
