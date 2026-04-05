package api

import (
	"bytes"
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
)

const defaultBaseURL = "https://openrouter.ai/api/v1"

// OpenRouterClient implements Client using direct HTTP so that
// OpenRouter-specific usage metadata remains available to the app.
type OpenRouterClient struct {
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

	return &OpenRouterClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    httpClient,
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type toolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Strict      bool           `json:"strict"`
	Parameters  map[string]any `json:"parameters"`
}

type toolDefinition struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type chatCompletionRequest struct {
	Model      string           `json:"model"`
	Messages   []chatMessage    `json:"messages"`
	Tools      []toolDefinition `json:"tools,omitempty"`
	ToolChoice any              `json:"tool_choice,omitempty"`
}

type chatCompletionResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage usageResponse `json:"usage"`
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

func replyTool() toolDefinition {
	return toolDefinition{
		Type: "function",
		Function: toolFunction{
			Name:        "reply",
			Description: "Send one or more messages to the channel. Each message is either a regular message or an action (/me). Keep each message short, like IRC.",
			Strict:      true,
			Parameters: map[string]any{
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

func passTool() toolDefinition {
	return toolDefinition{
		Type: "function",
		Function: toolFunction{
			Name:        "pass",
			Description: "Explicitly choose not to reply. Use this when you have nothing to add.",
			Strict:      true,
			Parameters: map[string]any{
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

	resp, err := c.chatCompletion(ctx, chatCompletionRequest{
		Model:      string(modelID),
		Messages:   buildMessages(systemPrompt, history, events),
		Tools:      []toolDefinition{replyTool(), passTool()},
		ToolChoice: "required",
	})
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		logger.ErrorContext(ctx, "openrouter send events failed", "error", err)
		return CompletionResult{}, err
	}

	result, err := parseCompletionResponse(resp)
	if err != nil {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		span.SetStatus(codes.Error, err.Error())
		logger.ErrorContext(ctx, "openrouter response parse failed", "error", err)
		return CompletionResult{}, err
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

func buildMessages(
	systemPrompt string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
) []chatMessage {
	msgs := []chatMessage{{Role: "system", Content: systemPrompt}}

	for _, h := range history {
		data, _ := json.Marshal(h)
		msgs = append(msgs, chatMessage{Role: "user", Content: string(data)})
	}

	for _, e := range events {
		data, _ := json.Marshal(e)
		msgs = append(msgs, chatMessage{Role: "user", Content: string(data)})
	}

	return msgs
}

func parseCompletionResponse(resp chatCompletionResponse) (CompletionResult, error) {
	if len(resp.Choices) == 0 {
		return CompletionResult{}, fmt.Errorf("no choices in response")
	}

	msg := resp.Choices[0].Message
	result := CompletionResult{
		RequestID: resp.ID,
		Usage:     usageFromResponse(resp.Usage),
	}

	if len(msg.ToolCalls) == 0 {
		result.Response = protocol.ModelResponse{
			Kind:   protocol.ResponseSilence,
			Reason: "model did not call a tool",
		}

		return result, nil
	}

	call := msg.ToolCalls[0]

	switch call.Function.Name {
	case "reply":
		var args struct {
			Messages []protocol.ReplyPart `json:"messages"`
		}

		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return CompletionResult{}, fmt.Errorf("parse reply args: %w", err)
		}

		result.Response = protocol.ModelResponse{
			Kind:     protocol.ResponseReply,
			Messages: args.Messages,
		}

		return result, nil

	case "pass":
		var args struct {
			Reason string `json:"reason"`
		}

		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return CompletionResult{}, fmt.Errorf("parse pass args: %w", err)
		}

		result.Response = protocol.ModelResponse{
			Kind:   protocol.ResponseSilence,
			Reason: args.Reason,
		}

		return result, nil

	default:
		return CompletionResult{}, fmt.Errorf("unknown tool call: %q", call.Function.Name)
	}
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

	resp, err := c.chatCompletion(ctx, chatCompletionRequest{
		Model: string(nickModel),
		Messages: []chatMessage{
			{Role: "user", Content: prompt},
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
		RequestID: resp.ID,
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

func (c *OpenRouterClient) chatCompletion(ctx context.Context, payload chatCompletionRequest) (chatCompletionResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return chatCompletionResponse{}, fmt.Errorf("marshal chat completion request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return chatCompletionResponse{}, err
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return chatCompletionResponse{}, fmt.Errorf("chat completion: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		responseBody, _ := io.ReadAll(resp.Body)
		return chatCompletionResponse{}, fmt.Errorf("chat completion: status %d: %s", resp.StatusCode, responseBody)
	}

	var completion chatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&completion); err != nil {
		return chatCompletionResponse{}, fmt.Errorf("chat completion decode: %w", err)
	}

	return completion, nil
}

func usageFromResponse(response usageResponse) Usage {
	cacheWriteTokens := response.PromptTokensDetails.CacheWriteTokens
	if cacheWriteTokens == 0 {
		cacheWriteTokens = response.PromptTokensDetails.CacheCreationTokens
	}

	return Usage{
		PromptTokens:          response.PromptTokens,
		CompletionTokens:      response.CompletionTokens,
		TotalTokens:           response.TotalTokens,
		ReasoningTokens:       response.CompletionTokensDetails.ReasoningTokens,
		CachedTokens:          response.PromptTokensDetails.CachedTokens,
		CacheWriteTokens:      cacheWriteTokens,
		CostCredits:           response.Cost,
		UpstreamInferenceCost: response.CostDetails.UpstreamInferenceCost,
	}
}
