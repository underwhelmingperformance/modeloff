package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/invopop/jsonschema"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
	"github.com/tidwall/sjson"
)

// Default per-call timeouts. The chat timeout bounds how long a model
// may take to produce a completion; the meta timeout bounds the
// smaller OpenRouter-specific endpoints (model listing, nickname and
// persona generation). Both apply only when the caller has not
// already set a deadline on the context.
const (
	defaultChatTimeout            = 60 * time.Second
	defaultMetaTimeout            = 30 * time.Second
	anthropicStructuredOutputBeta = "structured-outputs-2025-11-13"
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
	ratios         *promptTokenRatios

	// contextWindows answers a model's context window, so a request can
	// be capped by what its own prompt leaves. It is nil until a caller
	// supplies one through [OpenRouterClient.SetContextWindows].
	contextWindows atomic.Pointer[func(domain.ModelID) int]
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
		ratios:         newPromptTokenRatios(),
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

// inSpan brackets fn with a span on the client's tracer provider.
// `ManualResult` is on because the OpenRouter call paths do not fit
// the flat `ok`/`error` shape: the chat completions stamp result as
// `silence`/`reply`/`tool`, and the meta endpoints select between
// `transport`/`http_status`/`response_parse` error kinds depending
// on which step failed. The runner still sets `codes.Error` status
// on a non-nil error; everything else is the closure's job, via
// `markSpanError` for the error path and explicit `SetAttributes`
// for the success path.
func (c *OpenRouterClient) inSpan(
	ctx context.Context,
	op string,
	attrs []attribute.KeyValue,
	fn func(ctx context.Context, span trace.Span) error,
) error {
	return observability.SpanRunner{
		Tracer:       c.tracer(),
		ManualResult: true,
	}.Run(ctx, op, attrs, fn)
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

// CompletionParseError reports a provider response the client could not
// read as the structure the call asked for. A caller distinguishes it
// from a transport failure with `errors.As`, because a model that keeps
// answering in the wrong shape is a different problem from a network
// that keeps failing.
type CompletionParseError struct {
	// Target names the structure the call asked the response for.
	Target string
	Err    error
}

func (e *CompletionParseError) Error() string {
	return fmt.Sprintf("parse %s: %v", e.Target, e.Err)
}

func (e *CompletionParseError) Unwrap() error {
	return e.Err
}

// generateSchema reflects a Go type into a JSON Schema for the OpenAI
// API. Definitions are inlined, and a strict schema must also say that
// no other key is allowed, which is what both reflector settings are
// for.
//
// The result is the encoded document, so `properties` reaches the
// provider in the order the Go type declares its fields in.
// [personaProposal] depends on that order.
func generateSchema[T any]() json.RawMessage {
	reflector := jsonschema.Reflector{
		DoNotReference:            true,
		AllowAdditionalProperties: false,
	}

	var v T
	schema := reflector.Reflect(v)

	encoded, _ := json.Marshal(schema)

	return encoded
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

func eventRequestParams(
	modelID domain.ModelID,
	selfInstanceID domain.InstanceID,
	systemPrompt SystemPrompt,
	history TurnHistory,
	events []protocol.IRCMessage,
	tools []ToolDefinition,
) openai.ChatCompletionNewParams {
	return completionRequestParams(
		modelID,
		selfInstanceID,
		buildMessages(systemPrompt, selfInstanceID, history, events),
		tools,
	)
}

// DispatchCompletionTokens is the most a dispatch turn's completion may
// take. Context planning holds the same number back from the model's
// window when it decides how much transcript a turn may carry, so the
// two agree on what the completion is allowed to take.
const DispatchCompletionTokens = 4000

// CompletionAllowance is what `max_completion_tokens` may be for a prompt
// of promptTokens against a window of contextLength.
//
// [DispatchCompletionTokens] is the ceiling and the window is the bound.
// The planner reserves the ceiling before it knows what the prompt will
// cost, and a request whose prompt turned out larger has to ask for less:
// a small window would otherwise be handed a prompt the planner sized
// against a shrunken reserve and a cap that never shrank with it, and a
// tool exchange that fits when it is planned would stop fitting once the
// assistant message and the tool results are appended.
//
// A window this client has not been told about gives the ceiling, which
// is what a request asked for before there was anything to bound it with.
// A window with nothing left asks for one token: the prompt is already
// over the limit, and the provider's answer to that is the length error
// the turn compacts against.
func CompletionAllowance(contextLength, promptTokens int) int {
	if contextLength <= 0 {
		return DispatchCompletionTokens
	}

	return max(min(DispatchCompletionTokens, contextLength-promptTokens), 1)
}

// ContextWindowLookup is the optional capability a [Client] implements to
// be told each model's context window, so it can size a completion against
// what the prompt leaves. A client that is never told keeps asking for
// [DispatchCompletionTokens].
type ContextWindowLookup interface {
	SetContextWindows(lookup func(domain.ModelID) int)
}

// SetContextWindows tells the client where to look up a model's context
// window. The manager passes the catalogue cache it already keeps, so the
// completion cap and the planner's reserve read the same number.
func (c *OpenRouterClient) SetContextWindows(lookup func(domain.ModelID) int) {
	c.contextWindows.Store(&lookup)
}

// sizeCompletion caps `params` by what its own prompt leaves in the
// model's window.
func (c *OpenRouterClient) sizeCompletion(
	modelID domain.ModelID,
	params openai.ChatCompletionNewParams,
) openai.ChatCompletionNewParams {
	lookup := c.contextWindows.Load()
	if lookup == nil {
		return params
	}

	request, err := renderCompletionRequest("", params)
	if err != nil {
		return params
	}
	params.MaxCompletionTokens = openai.Int(int64(CompletionAllowance(
		(*lookup)(modelID), c.EstimatePromptTokens(modelID, request),
	)))

	return params
}

func completionRequestParams(
	modelID domain.ModelID,
	promptCacheKey domain.InstanceID,
	messages []openai.ChatCompletionMessageParamUnion,
	tools []ToolDefinition,
) openai.ChatCompletionNewParams {
	params := openai.ChatCompletionNewParams{
		Model:               shared.ChatModel(string(modelID)),
		Messages:            messages,
		Tools:               toolParams(tools),
		ParallelToolCalls:   openai.Bool(false),
		MaxCompletionTokens: openai.Int(DispatchCompletionTokens),
	}
	if promptCacheKey != "" {
		params.PromptCacheKey = openai.String(string(promptCacheKey))
	}

	return params
}

// RenderedEventRequest is the non-secret wire evidence retained for
// one provider request.
type RenderedEventRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

// EstimatedPromptTokens estimates the token cost of the complete
// rendered request body. The estimate uses the same four-byte rule
// as model context planning, but measures the final JSON so it also
// covers message roles, encoding, tool schemas and provider fields.
func (r RenderedEventRequest) EstimatedPromptTokens() int {
	if len(r.Body) == 0 {
		return 0
	}

	return (len(r.Body) + 3) / 4
}

type requestJSONField struct {
	path  string
	value any
}

func openRouterCompletionPolicy(
	params openai.ChatCompletionNewParams,
) (map[string]string, []requestJSONField) {
	headers := make(map[string]string)
	if params.PromptCacheKey.Valid() {
		headers["x-session-id"] = params.PromptCacheKey.Value
	}
	if isAnthropicModel(domain.ModelID(params.Model)) && len(params.Tools) > 0 {
		headers["x-anthropic-beta"] = anthropicStructuredOutputBeta
	}

	var fields []requestJSONField
	if len(params.Tools) > 0 {
		fields = append(fields, requestJSONField{
			path: "provider", value: map[string]any{"require_parameters": true},
		})
	}
	fields = append(fields, requestJSONField{
		path: "plugins",
		value: []map[string]any{{
			"id": "context-compression", "enabled": false,
		}},
	})

	return headers, fields
}

func isAnthropicModel(modelID domain.ModelID) bool {
	return strings.HasPrefix(string(modelID), "anthropic/")
}

// RenderEventRequest returns the non-secret request evidence for one
// SendEvents call. The turn journal uses the same parameter builder as
// the provider call, so the body contains the roles, recipient
// projection, cache boundary and tool schemas the provider received.
func RenderEventRequest(
	modelID domain.ModelID,
	selfInstanceID domain.InstanceID,
	systemPrompt SystemPrompt,
	history TurnHistory,
	events []protocol.IRCMessage,
	tools ...ToolDefinition,
) (RenderedEventRequest, error) {
	return renderEventRequest("/chat/completions", modelID, selfInstanceID,
		systemPrompt, history, events, tools...)
}

// RenderEventRequest returns the non-secret request evidence for one
// SendEvents call, including the path below this client's configured
// API base URL.
func (c *OpenRouterClient) RenderEventRequest(
	modelID domain.ModelID,
	selfInstanceID domain.InstanceID,
	systemPrompt SystemPrompt,
	history TurnHistory,
	events []protocol.IRCMessage,
	tools ...ToolDefinition,
) (RenderedEventRequest, error) {
	path, err := c.chatCompletionPath()
	if err != nil {
		return RenderedEventRequest{}, err
	}

	return renderEventRequest(path, modelID, selfInstanceID,
		systemPrompt, history, events, tools...)
}

// RenderToolResultRequest returns the non-secret request evidence for
// one ContinueWithToolResults call, including the path below this
// client's configured API base URL.
func (c *OpenRouterClient) RenderToolResultRequest(
	conv *Conversation,
	results []ToolResult,
	tools ...ToolDefinition,
) (RenderedEventRequest, error) {
	path, err := c.chatCompletionPath()
	if err != nil {
		return RenderedEventRequest{}, err
	}

	return renderToolResultRequest(path, conv, results, tools...)
}

// RenderToolResultRequest returns the standard OpenRouter request
// shape for one ContinueWithToolResults call.
func RenderToolResultRequest(
	conv *Conversation,
	results []ToolResult,
	tools ...ToolDefinition,
) (RenderedEventRequest, error) {
	return renderToolResultRequest("/chat/completions", conv, results, tools...)
}

func (c *OpenRouterClient) chatCompletionPath() (string, error) {
	endpoint, err := url.JoinPath(c.baseURL, "chat/completions")
	if err != nil {
		return "", fmt.Errorf("render chat completion request URL: %w", err)
	}
	requestURL, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse chat completion request URL: %w", err)
	}

	return requestURL.EscapedPath(), nil
}

func renderEventRequest(
	path string,
	modelID domain.ModelID,
	selfInstanceID domain.InstanceID,
	systemPrompt SystemPrompt,
	history TurnHistory,
	events []protocol.IRCMessage,
	tools ...ToolDefinition,
) (RenderedEventRequest, error) {
	return renderCompletionRequest(path, eventRequestParams(
		modelID,
		selfInstanceID,
		systemPrompt,
		history,
		events,
		tools,
	))
}

func renderToolResultRequest(
	path string,
	conv *Conversation,
	results []ToolResult,
	tools ...ToolDefinition,
) (RenderedEventRequest, error) {
	params, err := toolResultRequestParams(conv, results, tools)
	if err != nil {
		return RenderedEventRequest{}, err
	}

	return renderCompletionRequest(path, params)
}

func renderCompletionRequest(
	path string,
	params openai.ChatCompletionNewParams,
) (RenderedEventRequest, error) {
	headers, fields := openRouterCompletionPolicy(params)
	body, err := params.MarshalJSON()
	if err != nil {
		return RenderedEventRequest{}, fmt.Errorf("render event request: %w", err)
	}
	for _, field := range fields {
		body, err = sjson.SetBytes(body, field.path, field.value)
		if err != nil {
			return RenderedEventRequest{}, fmt.Errorf("render event request policy: %w", err)
		}
	}

	return RenderedEventRequest{
		Method:  http.MethodPost,
		Path:    path,
		Headers: headers,
		Body:    body,
	}, nil
}

func toolResultRequestParams(
	conv *Conversation,
	results []ToolResult,
	tools []ToolDefinition,
) (openai.ChatCompletionNewParams, error) {
	if conv == nil {
		return openai.ChatCompletionNewParams{}, errors.New("render tool result request: missing conversation")
	}

	messages := slices.Clone(conv.messages)
	for _, result := range results {
		messages = append(messages, openai.ToolMessage(result.Content, result.ToolCallID))
	}

	return completionRequestParams(conv.modelID, conv.promptCacheKey, messages, tools), nil
}

// SendEvents sends protocol events to a model and returns its typed
// response. The model replies via structured JSON output (reply or
// pass) and may optionally call memory tools.
func (c *OpenRouterClient) SendEvents(
	ctx context.Context,
	modelID domain.ModelID,
	selfInstanceID domain.InstanceID,
	systemPrompt SystemPrompt,
	history TurnHistory,
	events []protocol.IRCMessage,
	tools ...ToolDefinition,
) (CompletionResult, error) {
	logger := slog.Default().With("component", "api.openrouter", "model_id", modelID)

	var result CompletionResult
	err := c.inSpan(ctx, "api.openrouter.send_events",
		[]attribute.KeyValue{attribute.String(observability.AttrModelID, string(modelID))},
		func(ctx context.Context, span trace.Span) error {
			params := c.sizeCompletion(modelID, eventRequestParams(
				modelID,
				selfInstanceID,
				systemPrompt,
				history,
				events,
				tools,
			))
			msgs := params.Messages
			rendered, err := c.recordRequestSize(span, modelID, params)
			if err != nil {
				return err
			}

			resp, rawResp, err := c.chatCompletion( //nolint:bodyclose // SDK reads and closes the body.
				ctx,
				params,
				option.WithMaxRetries(0),
			)
			if err != nil {
				err = classifyCompletionError(err)
				markSpanError(span, observability.ErrorKindTransport, 0, err)
				logger.ErrorContext(ctx, "openrouter send events failed", "error", err)
				return err
			}

			parsed, assistantMsg, err := parseCompletionResponse(resp, rawResp)
			if err != nil {
				result = parsed
				markSpanError(span, completionParseErrorKind(err), 0, err)
				logger.ErrorContext(ctx, "openrouter response parse failed", "error", err)
				return err
			}
			c.ratios.observe(modelID, len(rendered.Body), parsed.Usage.PromptTokens)

			if len(parsed.PendingToolCalls) > 0 {
				parsed.Conversation = &Conversation{
					modelID:        modelID,
					promptCacheKey: selfInstanceID,
					messages:       append(msgs, assistantMsg),
				}
			}

			parsed.Usage.SetSpanAttributes(span, parsed.RequestID)
			span.SetAttributes(attribute.String(observability.AttrResult, completionResultKind(parsed)))

			logger.InfoContext(
				ctx,
				"openrouter send events completed",
				"request_id", parsed.RequestID,
				"result", completionResultKind(parsed),
				"prompt_tokens", parsed.Usage.PromptTokens,
				"completion_tokens", parsed.Usage.CompletionTokens,
				"cost_credits", parsed.Usage.CostCredits,
				"event_count", len(events),
				"history_count", len(history.Cacheable)+len(history.Current),
			)

			result = parsed
			return nil
		})
	if err != nil {
		return result, err
	}

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

	var result CompletionResult
	err := c.inSpan(ctx, "api.openrouter.continue_with_tool_results",
		[]attribute.KeyValue{attribute.String(observability.AttrModelID, string(conv.modelID))},
		func(ctx context.Context, span trace.Span) error {
			params, err := toolResultRequestParams(conv, results, tools)
			if err != nil {
				return err
			}
			params = c.sizeCompletion(conv.modelID, params)
			msgs := params.Messages
			rendered, err := c.recordRequestSize(span, conv.modelID, params)
			if err != nil {
				return err
			}

			resp, rawResp, err := c.chatCompletion( //nolint:bodyclose // SDK reads and closes the body.
				ctx,
				params,
				option.WithMaxRetries(0),
			)
			if err != nil {
				err = classifyCompletionError(err)
				markSpanError(span, observability.ErrorKindTransport, 0, err)
				logger.ErrorContext(ctx, "openrouter continue failed", "error", err)
				return err
			}

			parsed, assistantMsg, err := parseCompletionResponse(resp, rawResp)
			if err != nil {
				result = parsed
				markSpanError(span, completionParseErrorKind(err), 0, err)
				logger.ErrorContext(ctx, "openrouter continue parse failed", "error", err)
				return err
			}
			c.ratios.observe(conv.modelID, len(rendered.Body), parsed.Usage.PromptTokens)

			if len(parsed.PendingToolCalls) > 0 {
				// Append tool results and the new assistant message for
				// the next iteration.
				nextMsgs := make([]openai.ChatCompletionMessageParamUnion, len(msgs), len(msgs)+1)
				copy(nextMsgs, msgs)
				nextMsgs = append(nextMsgs, assistantMsg)

				parsed.Conversation = &Conversation{
					modelID:        conv.modelID,
					promptCacheKey: conv.promptCacheKey,
					messages:       nextMsgs,
				}
			}

			parsed.Usage.SetSpanAttributes(span, parsed.RequestID)
			span.SetAttributes(attribute.String(observability.AttrResult, completionResultKind(parsed)))

			logger.InfoContext(
				ctx,
				"openrouter continue completed",
				"request_id", parsed.RequestID,
				"result", completionResultKind(parsed),
				"prompt_tokens", parsed.Usage.PromptTokens,
				"completion_tokens", parsed.Usage.CompletionTokens,
				"cost_credits", parsed.Usage.CostCredits,
			)

			result = parsed
			return nil
		})
	if err != nil {
		return result, err
	}

	return result, nil
}

// recordRequestSize renders what `params` describes, records its
// measured size on the span, and returns it so the caller can compare
// the provider's own prompt-token count against the bytes it sent.
func (c *OpenRouterClient) recordRequestSize(
	span trace.Span,
	modelID domain.ModelID,
	params openai.ChatCompletionNewParams,
) (RenderedEventRequest, error) {
	request, err := renderCompletionRequest("", params)
	if err != nil {
		return RenderedEventRequest{}, err
	}

	span.SetAttributes(
		attribute.Int(observability.AttrPromptEstimate, c.EstimatePromptTokens(modelID, request)),
		attribute.Int(observability.AttrRequestBytes, len(request.Body)),
	)

	return request, nil
}

// messageRole is the openai role a coalesced transcript run emits
// under. The system role is not one of them: the system message is
// the app's own prompt, emitted once ahead of every run, so no
// transcript line can reach the privileged role.
type messageRole string

const (
	roleUser      messageRole = "user"
	roleAssistant messageRole = "assistant"
)

// messageRun is a contiguous sequence of content carrying the same
// role. `buildMessages` accumulates runs as it walks history and
// events, then emits each run as one openai message: single-part runs
// collapse to a plain-string body, multi-part runs become an array of
// text content parts.
type messageRun struct {
	role  messageRole
	parts []string
}

// cachePoint identifies the content part that closes the request's
// cacheable prefix, as a run index and a part index within that run.
type cachePoint struct {
	run  int
	part int
}

// partIn returns the index of the part in `run` carrying the
// breakpoint, or -1 when the breakpoint is in another run.
func (p cachePoint) partIn(run int) int {
	if p.run != run {
		return -1
	}

	return p.part
}

// ephemeralCacheControl is the `cache_control` extra field a provider
// reads to place a prompt-cache breakpoint at a content part.
func ephemeralCacheControl() map[string]any {
	return map[string]any{"cache_control": map[string]any{"type": "ephemeral"}}
}

// buildMessages renders a turn's input as openai chat messages: the
// app-authored fixed system prompt, lower-authority current instance
// state, then the transcript.
//
// Only Fixed takes the system role. Dynamic contains the instance's
// current nick, window and optional persona; it takes the user role
// because persona text may be operator-supplied or generated. The
// fixed instructions define how the model may use that record.
// Everything the instance itself said takes the assistant role, and
// everything else takes the user role as a JSON envelope naming the
// kind and sender: a peer's chat traffic, a join or a part, a poke,
// and a server reply the instance asked for (WHOIS, LIST) or the
// server pushed at it. A WHOIS answer quotes the target's persona and
// a LIST answer quotes each channel's topic. Both contain free text
// another actor supplied, so neither may reach the system role.
//
// The tool role would be the exact fit for the answer to a command
// the instance itself issued. A tool message has to name the
// `tool_call_id` of a tool call in a preceding assistant message,
// and the transcript replays earlier turns as text with no tool
// calls in it, so a replayed reply has no id to name. Tool results
// within a turn do keep the tool role, which
// [OpenRouterClient.ContinueWithToolResults] gives them.
//
// The request carries two prompt-cache breakpoints: one on the fixed
// system prompt and one on the last part built from
// [TurnHistory.Cacheable]. A provider reuses the longest cached
// prefix it can match, so the second one is what lets a turn reuse
// the transcript the turn before it sent.
func buildMessages(
	systemPrompt SystemPrompt,
	selfInstanceID domain.InstanceID,
	history TurnHistory,
	events []protocol.IRCMessage,
) []openai.ChatCompletionMessageParamUnion {
	var runs []messageRun

	addPart := func(role messageRole, text string) {
		if last := len(runs) - 1; last >= 0 && runs[last].role == role {
			runs[last].parts = append(runs[last].parts, text)
			return
		}
		runs = append(runs, messageRun{role: role, parts: []string{text}})
	}

	appendMsg := func(m protocol.IRCMessage) {
		sourceID, identified := m.Source.InstanceID()
		isSelf := selfInstanceID != "" && identified && sourceID == selfInstanceID

		m.Source = m.Source.WithoutInstanceID()

		data, _ := json.Marshal(m)
		if isSelf {
			addPart(roleAssistant, string(data))
			return
		}

		addPart(roleUser, string(data))
	}

	for _, h := range history.Cacheable {
		appendMsg(h)
	}

	cache := cachePoint{run: -1, part: -1}
	if last := len(runs) - 1; last >= 0 {
		cache = cachePoint{run: last, part: len(runs[last].parts) - 1}
	}

	for _, h := range history.Current {
		appendMsg(h)
	}

	for _, e := range events {
		appendMsg(e)
	}

	msgs := make([]openai.ChatCompletionMessageParamUnion, 0, len(runs)+2)
	fixed := openai.ChatCompletionContentPartTextParam{Text: systemPrompt.Fixed}
	fixed.SetExtraFields(ephemeralCacheControl())
	msgs = append(msgs, openai.SystemMessage([]openai.ChatCompletionContentPartTextParam{fixed}))

	if systemPrompt.Dynamic != "" {
		msgs = append(msgs, openai.UserMessage(systemPrompt.Dynamic))
	}

	for i, r := range runs {
		msgs = append(msgs, runToMessage(r, cache.partIn(i)))
	}

	return msgs
}

// runToMessage emits one coalesced run as an openai message.
// `cacheAt` names the part that closes the cacheable prefix, or -1
// when the breakpoint is elsewhere. A breakpoint has to sit on a
// content part, so a run carrying one keeps the array form whatever
// its length; every other single-part run collapses to a plain-string
// body.
func runToMessage(r messageRun, cacheAt int) openai.ChatCompletionMessageParamUnion {
	textPart := func(i int, text string) openai.ChatCompletionContentPartTextParam {
		part := openai.ChatCompletionContentPartTextParam{Text: text}
		if i == cacheAt {
			part.SetExtraFields(ephemeralCacheControl())
		}

		return part
	}

	switch r.role {
	case roleAssistant:
		if len(r.parts) == 1 && cacheAt < 0 {
			return openai.AssistantMessage(r.parts[0])
		}

		parts := make([]openai.ChatCompletionAssistantMessageParamContentArrayOfContentPartUnion, len(r.parts))
		for i, p := range r.parts {
			part := textPart(i, p)
			parts[i] = openai.ChatCompletionAssistantMessageParamContentArrayOfContentPartUnion{
				OfText: &part,
			}
		}

		return openai.AssistantMessage(parts)

	case roleUser:
		if len(r.parts) == 1 && cacheAt < 0 {
			return openai.UserMessage(r.parts[0])
		}

		parts := make([]openai.ChatCompletionContentPartUnionParam, len(r.parts))
		for i, p := range r.parts {
			part := textPart(i, p)
			parts[i] = openai.ChatCompletionContentPartUnionParam{OfText: &part}
		}

		return openai.UserMessage(parts)
	}

	return openai.ChatCompletionMessageParamUnion{}
}

// parseCompletionResponse extracts the model's tool calls from an
// API response. Returns the CompletionResult plus the raw assistant
// message (needed to build the next turn in multi-turn exchanges).
//
// The model communicates exclusively through tool calls — `msg` and
// `me` for chat traffic, `pass` for explicit silence-with-reason,
// memory tools for retrieval, and the channel-management tools
// (`join`, `part`, `topic`, etc.). A completion with no tool calls
// is silence. Text content is retained for the turn journal but does
// not enter the dispatch protocol.
func parseCompletionResponse(resp *openai.ChatCompletion, rawResp *http.Response) (CompletionResult, openai.ChatCompletionMessageParamUnion, error) {
	if resp == nil {
		return CompletionResult{}, openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("no response")
	}

	// Some providers (Grok via OpenRouter, after a tool-loop
	// continuation) return a well-formed envelope with no choices
	// when the model has nothing further to add. Surface as silence:
	// no tool calls, no conversation handle, dispatch loop terminates.
	if len(resp.Choices) == 0 {
		return CompletionResult{
			ResponseReceived: true,
			RequestID:        requestIDFromChatCompletion(resp, rawResp),
			Usage:            usageFromResponse(resp.Usage),
		}, openai.ChatCompletionMessageParamUnion{}, nil
	}

	choice := resp.Choices[0]
	msg := choice.Message
	result := CompletionResult{
		AssistantText:    msg.Content,
		Refusal:          msg.Refusal,
		ResponseReceived: true,
		RequestID:        requestIDFromChatCompletion(resp, rawResp),
		Usage:            usageFromResponse(resp.Usage),
	}
	for _, call := range msg.ToolCalls {
		result.PendingToolCalls = append(result.PendingToolCalls, PendingToolCall{
			ID:   call.ID,
			Name: call.Function.Name,
			Args: json.RawMessage(call.Function.Arguments),
		})
	}

	if err := validateChoice(choice); err != nil {
		return result, openai.ChatCompletionMessageParamUnion{}, err
	}

	assistantMsg := msg.ToParam()

	return result, assistantMsg, nil
}

// completionResultKind maps a parsed [CompletionResult] to its
// observability result string. A completion with tool calls is
// recorded as `tool`; everything else (no tool calls, model
// silence) reads as `pass`.
func completionResultKind(result CompletionResult) string {
	if len(result.PendingToolCalls) > 0 {
		return observability.ResultTool
	}

	return observability.ResultPass
}

type modelsResponse struct {
	Data []struct {
		ID                  string   `json:"id"`
		Name                string   `json:"name"`
		Description         string   `json:"description"`
		ContextLength       int      `json:"context_length"`
		SupportedParameters []string `json:"supported_parameters"`
	} `json:"data"`
}

type listModelsFailure uint8

const (
	listModelsRequestFailure listModelsFailure = iota + 1
	listModelsTransportFailure
	listModelsReadFailure
	listModelsStatusFailure
	listModelsDecodeFailure
)

type listModelsError struct {
	failure    listModelsFailure
	statusCode int
	cause      error
}

func (e *listModelsError) Error() string {
	switch e.failure {
	case listModelsRequestFailure:
		return "list models: invalid request"
	case listModelsTransportFailure:
		return "list models: network error"
	case listModelsReadFailure:
		return "list models: response read failed"
	case listModelsStatusFailure:
		return fmt.Sprintf("list models: status %d", e.statusCode)
	case listModelsDecodeFailure:
		return "list models: invalid response"
	default:
		return "list models: failed"
	}
}

func (e *listModelsError) Unwrap() error {
	return e.cause
}

// responseBodyLogLimit caps how much of an upstream non-2xx body we
// retain on spans and logs. Large JSON error payloads would otherwise
// bloat both the trace and the log stream without adding diagnostic
// value — the leading portion is almost always sufficient.
const responseBodyLogLimit = 4096

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

	var models []ModelInfo
	err := c.inSpan(ctx, "api.openrouter.list_models", nil, func(ctx context.Context, span trace.Span) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
		if err != nil {
			shaped := &listModelsError{failure: listModelsRequestFailure, cause: err}
			markSpanError(span, observability.ErrorKindTransport, 0, shaped)
			logger.ErrorContext(ctx, "openrouter list models request build failed", "error", err)
			return shaped
		}

		req.Header.Set("Authorization", "Bearer "+c.apiKey)

		resp, err := c.http.Do(req)
		if err != nil {
			shaped := &listModelsError{failure: listModelsTransportFailure, cause: err}
			markSpanError(span, observability.ErrorKindTransport, 0, shaped)
			logger.ErrorContext(ctx, "openrouter list models transport failure", "error", err)
			return shaped
		}
		defer func() { _ = resp.Body.Close() }()

		body, readErr := io.ReadAll(resp.Body)
		truncated := truncateBody(body, responseBodyLogLimit)

		if readErr != nil {
			shaped := &listModelsError{failure: listModelsReadFailure, cause: readErr}
			span.SetAttributes(attribute.String(observability.AttrHTTPResponseBody, truncated))
			markSpanError(span, observability.ErrorKindTransport, resp.StatusCode, shaped)
			logger.ErrorContext(ctx, "openrouter list models read failed",
				"status", resp.StatusCode,
				"body_read_error", readErr,
			)
			return shaped
		}

		if resp.StatusCode != http.StatusOK {
			span.SetAttributes(attribute.String(observability.AttrHTTPResponseBody, truncated))
			shaped := &listModelsError{
				failure:    listModelsStatusFailure,
				statusCode: resp.StatusCode,
			}
			markSpanError(span, observability.ErrorKindHTTPStatus, resp.StatusCode, shaped)
			logger.ErrorContext(ctx, "openrouter list models non-2xx",
				"status", resp.StatusCode,
				observability.AttrHTTPResponseBody, truncated,
			)
			return shaped
		}

		var mr modelsResponse
		if err := json.Unmarshal(body, &mr); err != nil {
			shaped := &listModelsError{failure: listModelsDecodeFailure, cause: err}
			span.SetAttributes(attribute.String(observability.AttrHTTPResponseBody, truncated))
			markSpanError(span, observability.ErrorKindResponseParse, 0, shaped)
			logger.ErrorContext(ctx, "openrouter list models decode failed",
				"decode_error", err,
				observability.AttrHTTPResponseBody, truncated,
			)
			return shaped
		}

		models = make([]ModelInfo, len(mr.Data))
		for i, model := range mr.Data {
			models[i] = ModelInfo{
				ID:                  domain.ModelID(model.ID),
				Name:                model.Name,
				Description:         model.Description,
				ContextLen:          model.ContextLength,
				SupportedParameters: model.SupportedParameters,
			}
		}

		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
		logger.DebugContext(ctx, "openrouter list models completed", "count", len(models))

		return nil
	})

	return models, err
}

func (c *OpenRouterClient) chatCompletion(
	ctx context.Context,
	payload openai.ChatCompletionNewParams,
	requestOptions ...option.RequestOption,
) (*openai.ChatCompletion, *http.Response, error) {
	ctx, cancel := ensureDeadline(ctx, c.chatTimeout)
	defer cancel()
	headers, fields := openRouterCompletionPolicy(payload)

	var rawResp *http.Response

	opts := append([]option.RequestOption{
		option.WithResponseInto(&rawResp),
	}, requestOptions...)
	for name, value := range headers {
		// OpenRouter uses the session ID to select the same provider
		// endpoint from the first successful request. The prompt cache
		// key still reaches providers that use it for cache bucketing.
		opts = append(opts, option.WithHeader(name, value))
	}
	for _, field := range fields {
		opts = append(opts, option.WithJSONSet(field.path, field.value))
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
	var parseErr *CompletionParseError
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
