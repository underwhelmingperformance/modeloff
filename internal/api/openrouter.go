package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/laney/modeloff/internal/domain"
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

// replyTool defines the "reply" function tool that models call when
// they want to respond.
func replyTool() openai.ChatCompletionToolParam {
	return openai.ChatCompletionToolParam{
		Function: shared.FunctionDefinitionParam{
			Name:        "reply",
			Description: param.NewOpt("Send one or more messages to the channel. Each message is either a regular message or an action (/me). Keep each message short — one thought per line, like IRC."),
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
									"description": `"message" for a regular message, "action" for a /me action (e.g. * nick waves).`,
								},
								"body": map[string]any{
									"type":        "string",
									"description": "The message text. For actions, just the action text without /me.",
								},
							},
							"required":             []string{"type", "body"},
							"additionalProperties": false,
						},
						"description": "One or more messages to send. Use multiple items for separate thoughts.",
					},
				},
				"required":             []string{"messages"},
				"additionalProperties": false,
			},
		},
	}
}

// passTool defines the "pass" function tool that models call when
// they choose not to respond.
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

// toolChoice forces the model to call one of our tools.
func toolChoice() openai.ChatCompletionToolChoiceOptionUnionParam {
	return openai.ChatCompletionToolChoiceOptionUnionParam{
		OfAuto: param.NewOpt("required"),
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
) (protocol.ModelResponse, error) {
	messages := buildMessages(systemPrompt, history, events)

	resp, err := c.oai.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:      openai.ChatModel(string(modelID)),
		Messages:   messages,
		Tools:      []openai.ChatCompletionToolParam{replyTool(), passTool()},
		ToolChoice: toolChoice(),
	})
	if err != nil {
		return protocol.ModelResponse{}, fmt.Errorf("chat completion: %w", err)
	}

	return parseResponse(resp)
}

func buildMessages(
	systemPrompt string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
) []openai.ChatCompletionMessageParamUnion {
	var msgs []openai.ChatCompletionMessageParamUnion

	msgs = append(msgs, openai.SystemMessage(systemPrompt))

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

func parseResponse(resp *openai.ChatCompletion) (protocol.ModelResponse, error) {
	if len(resp.Choices) == 0 {
		return protocol.ModelResponse{}, fmt.Errorf("no choices in response")
	}

	msg := resp.Choices[0].Message

	if len(msg.ToolCalls) == 0 {
		return protocol.ModelResponse{
			Kind:   protocol.ResponseSilence,
			Reason: "model did not call a tool",
		}, nil
	}

	call := msg.ToolCalls[0]

	switch call.Function.Name {
	case "reply":
		var args struct {
			Messages []protocol.ReplyPart `json:"messages"`
		}

		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return protocol.ModelResponse{}, fmt.Errorf("parse reply args: %w", err)
		}

		return protocol.ModelResponse{
			Kind:     protocol.ResponseReply,
			Messages: args.Messages,
		}, nil

	case "pass":
		var args struct {
			Reason string `json:"reason"`
		}

		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return protocol.ModelResponse{}, fmt.Errorf("parse pass args: %w", err)
		}

		return protocol.ModelResponse{
			Kind:   protocol.ResponseSilence,
			Reason: args.Reason,
		}, nil

	default:
		return protocol.ModelResponse{}, fmt.Errorf("unknown tool call: %q", call.Function.Name)
	}
}

// modelsResponse matches the OpenRouter /models endpoint shape.
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list models: status %d: %s", resp.StatusCode, body)
	}

	var mr modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}

	models := make([]ModelInfo, len(mr.Data))
	for i, m := range mr.Data {
		models[i] = ModelInfo{
			ID:          domain.ModelID(m.ID),
			Name:        m.Name,
			Description: m.Description,
			ContextLen:  m.ContextLength,
		}
	}

	return models, nil
}

// GenerateNick asks a model to generate an IRC-style nickname for a
// given model ID. The nickModel parameter selects which model
// performs the generation.
func (c *OpenRouterClient) GenerateNick(ctx context.Context, nickModel domain.ModelID, modelID domain.ModelID) (domain.Nick, error) {
	prompt := fmt.Sprintf(
		"Generate a short, creative IRC-style nickname (lowercase, no spaces, max 12 chars) for an AI model called %q. "+
			"The nickname should be fun and personality-driven, NOT derived from the model name or ID. "+
			"Think of it like a human IRC handle — playful, memorable, and unique. "+
			"Reply with ONLY the nickname, nothing else.",
		string(modelID),
	)

	resp, err := c.oai.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: openai.ChatModel(string(nickModel)),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(prompt),
		},
	})
	if err != nil {
		return "", fmt.Errorf("generate nick: %w", err)
	}

	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("generate nick: no choices in response")
	}

	nick := sanitizeNick(resp.Choices[0].Message.Content)
	if nick == "" {
		return "", fmt.Errorf("generate nick: model returned empty or unsalvageable response")
	}

	return domain.Nick(nick), nil
}

const maxNickLen = 12

// sanitizeNick cleans a raw model response into a valid IRC-style
// nickname: lowercase, alphanumeric plus underscores and hyphens,
// max 12 characters.
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
