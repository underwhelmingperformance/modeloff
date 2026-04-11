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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/invopop/jsonschema"
	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// OpenRouterClient implements Client using openai-go for chat
// completions and direct HTTP for OpenRouter-specific endpoints.
type OpenRouterClient struct {
	oai           openai.Client
	baseURL       string
	apiKey        string
	http          *http.Client
	searchEnabled bool
}

// NewOpenRouterClient creates a client configured to talk to an
// OpenAI-compatible API at baseURL. Set searchEnabled to true to
// offer the search_memory tool to models.
func NewOpenRouterClient(apiKey, baseURL string, httpClient *http.Client, searchEnabled bool) *OpenRouterClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	oai := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
		option.WithHTTPClient(httpClient),
	)

	return &OpenRouterClient{
		oai:           oai,
		baseURL:       baseURL,
		apiKey:        apiKey,
		http:          httpClient,
		searchEnabled: searchEnabled,
	}
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
	replyProps := jsonschema.NewProperties()
	replyProps.Set("kind", &jsonschema.Schema{Type: "string", Const: "reply"})
	replyProps.Set("messages", &jsonschema.Schema{
		Type:        "array",
		Description: "One or more messages to send.",
		Items: &jsonschema.Schema{
			Type:                 "object",
			Required:             []string{"type", "body"},
			AdditionalProperties: jsonschema.FalseSchema,
			Properties: func() *orderedmap.OrderedMap[string, *jsonschema.Schema] {
				p := jsonschema.NewProperties()
				p.Set("type", &jsonschema.Schema{
					Type:        "string",
					Enum:        []any{"message", "action"},
					Description: `"message" for a regular message, "action" for a /me action.`,
				})
				p.Set("body", &jsonschema.Schema{
					Type:        "string",
					Description: "The message text. For actions, just the action body without /me.",
				})
				return p
			}(),
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

type writeMemoryArgs struct {
	Key     string `json:"key" jsonschema_description:"A short identifier for this memory (e.g. favourite_topic, user_name)."`
	Content string `json:"content" jsonschema_description:"The content to remember."`
}

type deleteMemoryArgs struct {
	Key string `json:"key" jsonschema_description:"The key of the memory to remove."`
}

type searchMemoryArgs struct {
	Query string `json:"query" jsonschema_description:"A natural-language query describing what you want to recall."`
	Limit int    `json:"limit" jsonschema_description:"Maximum number of results to return (1-20)."`
}

func writeMemoryTool() openai.ChatCompletionToolUnionParam {
	return openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
		Name:        "write_memory",
		Description: openai.String("Store a memory. Use this to remember something for future conversations."),
		Strict:      openai.Bool(true),
		Parameters:  generateSchema[writeMemoryArgs](),
	})
}

func deleteMemoryTool() openai.ChatCompletionToolUnionParam {
	return openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
		Name:        "delete_memory",
		Description: openai.String("Remove a memory by key. Use this when a memory is no longer relevant."),
		Strict:      openai.Bool(true),
		Parameters:  generateSchema[deleteMemoryArgs](),
	})
}

func searchMemoryTool() openai.ChatCompletionToolUnionParam {
	return openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
		Name:        "search_memory",
		Description: openai.String("Search your memories for information relevant to a query. Returns the most similar memories."),
		Strict:      openai.Bool(true),
		Parameters:  generateSchema[searchMemoryArgs](),
	})
}

func memoryTools(searchEnabled bool) []openai.ChatCompletionToolUnionParam {
	tools := []openai.ChatCompletionToolUnionParam{
		writeMemoryTool(),
		deleteMemoryTool(),
	}

	if searchEnabled {
		tools = append(tools, searchMemoryTool())
	}

	return tools
}

// SendEvents sends protocol events to a model and returns its typed
// response. The model replies via structured JSON output (reply or
// pass) and may optionally call memory tools.
func (c *OpenRouterClient) SendEvents(
	ctx context.Context,
	modelID domain.ModelID,
	systemPrompt string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
) (CompletionResult, error) {
	logger := slog.Default().With("component", "api.openrouter", "model_id", modelID)
	tracer := otel.Tracer("github.com/laney/modeloff/internal/api")

	ctx, span := tracer.Start(ctx, "api.openrouter.send_events")
	span.SetAttributes(
		attribute.String(observability.AttrOperation, "api.openrouter.send_events"),
		attribute.String(observability.AttrModelID, string(modelID)),
	)
	defer span.End()

	msgs := buildMessages(systemPrompt, history, events)

	resp, rawResp, err := c.chatCompletion(ctx, modelID, openai.ChatCompletionNewParams{ //nolint:bodyclose // SDK reads and closes the body.
		Model:          shared.ChatModel(string(modelID)),
		Messages:       msgs,
		Tools:          memoryTools(c.searchEnabled),
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
	)

	return result, nil
}

// ContinueWithToolResults appends tool result messages to the
// conversation and sends the next request.
func (c *OpenRouterClient) ContinueWithToolResults(
	ctx context.Context,
	conv *Conversation,
	results []ToolResult,
) (CompletionResult, error) {
	logger := slog.Default().With("component", "api.openrouter", "model_id", conv.modelID)
	tracer := otel.Tracer("github.com/laney/modeloff/internal/api")

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
		Tools:          memoryTools(c.searchEnabled),
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
	)

	return result, nil
}

func buildMessages(
	systemPrompt string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
) []openai.ChatCompletionMessageParamUnion {
	msgs := []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(systemPrompt)}

	for _, h := range history {
		data, _ := json.Marshal(h)
		msgs = append(msgs, openai.UserMessage(string(data)))
	}

	for _, e := range events {
		data, _ := json.Marshal(e)
		msgs = append(msgs, openai.UserMessage(string(data)))
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
// any memory tool calls from an API response. It returns the
// CompletionResult plus the raw assistant message (needed to build
// the next turn in multi-turn exchanges).
//
// The model's reply/pass decision arrives as structured JSON in the
// message content. Memory tool calls (write_memory, delete_memory)
// arrive as tool_calls and are returned as PendingToolCalls. When
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
		switch call.Function.Name {
		case "write_memory":
			var args writeMemoryArgs

			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				return CompletionResult{}, openai.ChatCompletionMessageParamUnion{}, &completionParseError{target: "write_memory args", err: err}
			}

			result.PendingToolCalls = append(result.PendingToolCalls, PendingToolCall{
				ID:   call.ID,
				Kind: ToolCallWriteMemory,
				Key:  args.Key,
				Body: args.Content,
			})

		case "delete_memory":
			var args deleteMemoryArgs

			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				return CompletionResult{}, openai.ChatCompletionMessageParamUnion{}, &completionParseError{target: "delete_memory args", err: err}
			}

			result.PendingToolCalls = append(result.PendingToolCalls, PendingToolCall{
				ID:   call.ID,
				Kind: ToolCallDeleteMemory,
				Key:  args.Key,
			})

		case "search_memory":
			var args searchMemoryArgs

			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				return CompletionResult{}, openai.ChatCompletionMessageParamUnion{}, &completionParseError{target: "search_memory args", err: err}
			}

			result.PendingToolCalls = append(result.PendingToolCalls, PendingToolCall{
				ID:    call.ID,
				Kind:  ToolCallSearchMemory,
				Body:  args.Query,
				Limit: args.Limit,
			})

		default:
			return CompletionResult{}, openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("unknown tool call: %q", call.Function.Name)
		}
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

// ListModels fetches available models from the OpenRouter API.
func (c *OpenRouterClient) ListModels(ctx context.Context) ([]ModelInfo, error) {
	logger := slog.Default().With("component", "api.openrouter")
	tracer := otel.Tracer("github.com/laney/modeloff/internal/api")

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
		markSpanError(span, observability.ErrorKindTransport, 0, err)
		logger.ErrorContext(ctx, "openrouter list models failed", "error", err)
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		err := fmt.Errorf("list models: status %d: %s", resp.StatusCode, body)
		markSpanError(span, observability.ErrorKindHTTPStatus, resp.StatusCode, err)
		return nil, err
	}

	var mr modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		markSpanError(span, observability.ErrorKindResponseParse, 0, err)
		return nil, fmt.Errorf("list models: %w", err)
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

// GenerateNick asks a model to generate an IRC-style nickname for a
// given model ID. The nickModel parameter selects which model
// performs the generation.
func (c *OpenRouterClient) GenerateNick(ctx context.Context, nickModel domain.ModelID, modelID domain.ModelID) (NicknameResult, error) {
	logger := slog.Default().With("component", "api.openrouter", "nick_model", nickModel, "model_id", modelID)
	tracer := otel.Tracer("github.com/laney/modeloff/internal/api")

	ctx, span := tracer.Start(ctx, "api.openrouter.generate_nick")
	span.SetAttributes(
		attribute.String(observability.AttrOperation, "api.openrouter.generate_nick"),
		attribute.String(observability.AttrModelID, string(modelID)),
	)
	defer span.End()

	prompt := fmt.Sprintf(
		"Generate a short, fun IRC-style nickname (lowercase, no spaces, max 12 chars) for an AI model called %q. "+
			"Reply with ONLY the nickname, nothing else.",
		string(modelID),
	)

	resp, rawResp, err := c.chatCompletion(ctx, nickModel, openai.ChatCompletionNewParams{ //nolint:bodyclose // SDK reads and closes the body.
		Model: shared.ChatModel(string(nickModel)),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(prompt),
		},
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

	nick := sanitizeNick(choice.Message.Content)
	if nick == "" {
		err := fmt.Errorf("generate nick: model returned empty or unsalvageable response")
		markSpanError(span, observability.ErrorKindInvalidResponse, 0, err)
		return NicknameResult{}, err
	}

	result := NicknameResult{
		Nick:      domain.Nick(nick),
		RequestID: requestIDFromChatCompletion(resp, rawResp),
		Usage:     usageFromResponse(resp.Usage),
	}

	result.Usage.SetSpanAttributes(span, result.RequestID)
	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	logger.InfoContext(ctx, "openrouter generate nick completed", "request_id", result.RequestID, "nick", nick)

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
	logger := slog.Default().With("component", "api.openrouter", "model_id", smallModel)
	tracer := otel.Tracer("github.com/laney/modeloff/internal/api")

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

const maxNickLen = 12

func sanitizeNick(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, `"'`+"`")
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "_")

	var b strings.Builder

	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}

	result := b.String()
	if len(result) > maxNickLen {
		result = result[:maxNickLen]
	}

	return result
}

func (c *OpenRouterClient) chatCompletion(
	ctx context.Context,
	modelID domain.ModelID,
	payload openai.ChatCompletionNewParams,
) (*openai.ChatCompletion, *http.Response, error) {
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
