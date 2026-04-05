package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	openai "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/shared"
)

const defaultBaseURL = "https://openrouter.ai/api/v1"

// OpenRouterClient implements Client using openai-go for chat
// completions and direct HTTP for OpenRouter-specific endpoints.
type OpenRouterClient struct {
	oai     openai.Client
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewOpenRouterClient creates a client configured to talk to
// OpenRouter. The baseURL can be overridden for testing.
func NewOpenRouterClient(apiKey, baseURL string, httpClient *http.Client) *OpenRouterClient {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	oai := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
		option.WithHTTPClient(httpClient),
	)

	return &OpenRouterClient{
		oai:     oai,
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    httpClient,
	}
}

type usageResponse struct {
	PromptTokens        int64   `json:"prompt_tokens"`
	CompletionTokens    int64   `json:"completion_tokens"`
	TotalTokens         int64   `json:"total_tokens"`
	Cost                float64 `json:"cost"`
	PromptTokensDetails struct {
		CachedTokens        int64 `json:"cached_tokens"`
		CacheWriteTokens    int64 `json:"cache_write_tokens"`
		CacheCreationTokens int64 `json:"cache_creation_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	CostDetails struct {
		UpstreamInferenceCost float64 `json:"upstream_inference_cost"`
	} `json:"cost_details"`
}

func replyTool() openai.ChatCompletionToolParam {
	return openai.ChatCompletionToolParam{
		Function: shared.FunctionDefinitionParam{
			Name:        "reply",
			Description: param.NewOpt("Send one or more messages to the channel. Each message is either a regular message or an action (/me). Keep each message short, like IRC."),
			Strict:      param.NewOpt(true),
			Parameters: shared.FunctionParameters{
				"type": "object",
				"properties": map[string]any{
					"messages": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"type": map[string]any{
									"type":        "string",
									"enum":        []string{"message", "action"},
									"description": `"message" for a regular message, "action" for a /me action.`,
								},
								"body": map[string]any{
									"type":        "string",
									"description": "The message text. For actions, just the action body without /me.",
								},
							},
							"required":             []string{"type", "body"},
							"additionalProperties": false,
						},
						"description": "One or more messages to send.",
					},
				},
				"required":             []string{"messages"},
				"additionalProperties": false,
			},
		},
	}
}

func writeMemoryTool() openai.ChatCompletionToolParam {
	return openai.ChatCompletionToolParam{
		Function: shared.FunctionDefinitionParam{
			Name:        "write_memory",
			Description: param.NewOpt("Store a memory. Use this to remember something for future conversations."),
			Strict:      param.NewOpt(true),
			Parameters: shared.FunctionParameters{
				"type": "object",
				"properties": map[string]any{
					"key": map[string]any{
						"type":        "string",
						"description": "A short identifier for this memory (e.g. 'favourite_topic', 'user_name').",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "The content to remember.",
					},
				},
				"required":             []string{"key", "content"},
				"additionalProperties": false,
			},
		},
	}
}

func deleteMemoryTool() openai.ChatCompletionToolParam {
	return openai.ChatCompletionToolParam{
		Function: shared.FunctionDefinitionParam{
			Name:        "delete_memory",
			Description: param.NewOpt("Remove a memory by key. Use this when a memory is no longer relevant."),
			Strict:      param.NewOpt(true),
			Parameters: shared.FunctionParameters{
				"type": "object",
				"properties": map[string]any{
					"key": map[string]any{
						"type":        "string",
						"description": "The key of the memory to remove.",
					},
				},
				"required":             []string{"key"},
				"additionalProperties": false,
			},
		},
	}
}

func passTool() openai.ChatCompletionToolParam {
	return openai.ChatCompletionToolParam{
		Function: shared.FunctionDefinitionParam{
			Name:        "pass",
			Description: param.NewOpt("Explicitly choose not to reply. Use this when you have nothing to add."),
			Strict:      param.NewOpt(true),
			Parameters: shared.FunctionParameters{
				"type": "object",
				"properties": map[string]any{
					"reason": map[string]any{
						"type":        "string",
						"description": "Brief reason for not replying.",
					},
				},
				"required":             []string{"reason"},
				"additionalProperties": false,
			},
		},
	}
}

func allTools() []openai.ChatCompletionToolParam {
	return []openai.ChatCompletionToolParam{
		replyTool(),
		passTool(),
		writeMemoryTool(),
		deleteMemoryTool(),
	}
}

func toolChoice() openai.ChatCompletionToolChoiceOptionUnionParam {
	return openai.ChatCompletionToolChoiceOptionUnionParam{
		OfAuto: param.NewOpt(string(openai.ChatCompletionToolChoiceOptionAutoRequired)),
	}
}

// SendEvents sends protocol events to a model and returns its typed
// response. The model must call either the "reply" or "pass" tool.
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
	tools := allTools()

	resp, rawResp, err := c.chatCompletion(ctx, openai.ChatCompletionNewParams{
		Model:      shared.ChatModel(string(modelID)),
		Messages:   msgs,
		Tools:      tools,
		ToolChoice: toolChoice(),
	})
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		logger.ErrorContext(ctx, "openrouter send events failed", "error", err)
		return CompletionResult{}, err
	}

	result, assistantMsg, err := parseCompletionResponse(resp, rawResp)
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
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

	tools := allTools()

	resp, rawResp, err := c.chatCompletion(ctx, openai.ChatCompletionNewParams{
		Model:      shared.ChatModel(string(conv.modelID)),
		Messages:   msgs,
		Tools:      tools,
		ToolChoice: toolChoice(),
	})
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		logger.ErrorContext(ctx, "openrouter continue failed", "error", err)
		return CompletionResult{}, err
	}

	result, assistantMsg, err := parseCompletionResponse(resp, rawResp)
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
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

// parseCompletionResponse extracts the model's tool calls from an API
// response. It returns the CompletionResult plus the raw assistant
// message (needed to build the next turn in multi-turn exchanges).
//
// The model may call memory tools (write_memory, delete_memory) and/or
// terminal tools (reply, pass) in a single response. Memory tool calls
// are returned as PendingToolCalls; the first terminal tool call
// finalises the Response. If only memory tools are present, the caller
// must continue the conversation.
func parseCompletionResponse(resp *openai.ChatCompletion, rawResp *http.Response) (CompletionResult, openai.ChatCompletionMessageParamUnion, error) {
	if resp == nil || len(resp.Choices) == 0 {
		return CompletionResult{}, openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("no choices in response")
	}

	msg := resp.Choices[0].Message
	result := CompletionResult{
		RequestID: requestIDFromChatCompletion(resp, rawResp),
		Usage:     usageFromResponse(resp.Usage),
	}

	assistantMsg := msg.ToParam()

	if len(msg.ToolCalls) == 0 {
		result.Response = protocol.ModelResponse{
			Kind:   protocol.ResponseSilence,
			Reason: "model did not call a tool",
		}

		return result, assistantMsg, nil
	}

	for _, call := range msg.ToolCalls {
		switch call.Function.Name {
		case "reply":
			var args struct {
				Messages []protocol.ReplyPart `json:"messages"`
			}

			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				return CompletionResult{}, openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("parse reply args: %w", err)
			}

			result.Response = protocol.ModelResponse{
				Kind:     protocol.ResponseReply,
				Messages: args.Messages,
			}

			return result, assistantMsg, nil

		case "pass":
			var args struct {
				Reason string `json:"reason"`
			}

			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				return CompletionResult{}, openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("parse pass args: %w", err)
			}

			result.Response = protocol.ModelResponse{
				Kind:   protocol.ResponseSilence,
				Reason: args.Reason,
			}

			return result, assistantMsg, nil

		case "write_memory":
			var args struct {
				Key     string `json:"key"`
				Content string `json:"content"`
			}

			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				return CompletionResult{}, openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("parse write_memory args: %w", err)
			}

			result.PendingToolCalls = append(result.PendingToolCalls, PendingToolCall{
				ID:   call.ID,
				Kind: ToolCallWriteMemory,
				Key:  args.Key,
				Body: args.Content,
			})

		case "delete_memory":
			var args struct {
				Key string `json:"key"`
			}

			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				return CompletionResult{}, openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("parse delete_memory args: %w", err)
			}

			result.PendingToolCalls = append(result.PendingToolCalls, PendingToolCall{
				ID:   call.ID,
				Kind: ToolCallDeleteMemory,
				Key:  args.Key,
			})

		default:
			return CompletionResult{}, openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("unknown tool call: %q", call.Function.Name)
		}
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
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		logger.ErrorContext(ctx, "openrouter list models failed", "error", err)
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		err := fmt.Errorf("list models: status %d: %s", resp.StatusCode, body)
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	var mr modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
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

	resp, rawResp, err := c.chatCompletion(ctx, openai.ChatCompletionNewParams{
		Model: shared.ChatModel(string(nickModel)),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(prompt),
		},
	})
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		logger.ErrorContext(ctx, "openrouter generate nick failed", "error", err)
		return NicknameResult{}, err
	}

	if len(resp.Choices) == 0 {
		err := fmt.Errorf("generate nick: no choices in response")
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		return NicknameResult{}, err
	}

	nick := sanitizeNick(resp.Choices[0].Message.Content)
	if nick == "" {
		err := fmt.Errorf("generate nick: model returned empty or unsalvageable response")
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
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
	payload openai.ChatCompletionNewParams,
) (*openai.ChatCompletion, *http.Response, error) {
	var rawResp *http.Response

	completion, err := c.oai.Chat.Completions.New(
		ctx,
		payload,
		option.WithResponseInto(&rawResp),
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
	var extra usageResponse

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
