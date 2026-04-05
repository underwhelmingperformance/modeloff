package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

func TestOpenRouterClient_ListModels(t *testing.T) {
	tests := []struct {
		name       string
		response   string
		statusCode int
		want       []ModelInfo
		wantErr    bool
	}{
		{
			name:       "successful response",
			statusCode: http.StatusOK,
			response: `{
				"data": [
					{
						"id": "anthropic/claude-3-haiku",
						"name": "Claude 3 Haiku",
						"description": "Fast and compact",
						"context_length": 200000
					},
					{
						"id": "openai/gpt-4o",
						"name": "GPT-4o",
						"description": "Flagship model",
						"context_length": 128000
					}
				]
			}`,
			want: []ModelInfo{
				{
					ID:          "anthropic/claude-3-haiku",
					Name:        "Claude 3 Haiku",
					Description: "Fast and compact",
					ContextLen:  200000,
				},
				{
					ID:          "openai/gpt-4o",
					Name:        "GPT-4o",
					Description: "Flagship model",
					ContextLen:  128000,
				},
			},
		},
		{
			name:       "empty model list",
			statusCode: http.StatusOK,
			response:   `{"data": []}`,
			want:       []ModelInfo{},
		},
		{
			name:       "server error",
			statusCode: http.StatusInternalServerError,
			response:   `{"error": "internal"}`,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/models", r.URL.Path)
				require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.response))
			}))
			t.Cleanup(srv.Close)

			client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

			got, err := client.ListModels(t.Context())
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestOpenRouterClient_SendEvents(t *testing.T) {
	tests := []struct {
		name     string
		toolCall toolCallFixture
		want     CompletionResult
		wantErr  bool
	}{
		{
			name: "model replies",
			toolCall: toolCallFixture{
				name: "reply",
				args: `{"messages": [{"type": "message", "body": "Hello there!"}]}`,
			},
			want: CompletionResult{
				RequestID: "chatcmpl_test",
				Usage: Usage{
					PromptTokens:     10,
					CompletionTokens: 5,
					TotalTokens:      15,
					CostCredits:      0.125,
				},
				Response: protocol.ModelResponse{
					Kind: protocol.ResponseReply,
					Messages: []protocol.ReplyPart{
						{Kind: protocol.ReplyMessage, Body: "Hello there!"},
					},
				},
			},
		},
		{
			name: "model passes",
			toolCall: toolCallFixture{
				name: "pass",
				args: `{"reason": "Nothing to add"}`,
			},
			want: CompletionResult{
				RequestID: "chatcmpl_test",
				Usage: Usage{
					PromptTokens:     10,
					CompletionTokens: 5,
					TotalTokens:      15,
					CostCredits:      0.125,
				},
				Response: protocol.ModelResponse{
					Kind:   protocol.ResponseSilence,
					Reason: "Nothing to add",
				},
			},
		},
		{
			name: "unknown tool call",
			toolCall: toolCallFixture{
				name: "unknown",
				args: `{}`,
			},
			wantErr: true,
		},
		{
			name: "model replies with multiple messages including action",
			toolCall: toolCallFixture{
				name: "reply",
				args: `{"messages": [{"type": "message", "body": "hey"}, {"type": "action", "body": "waves"}]}`,
			},
			want: CompletionResult{
				RequestID: "chatcmpl_test",
				Usage: Usage{
					PromptTokens:     10,
					CompletionTokens: 5,
					TotalTokens:      15,
					CostCredits:      0.125,
				},
				Response: protocol.ModelResponse{
					Kind: protocol.ResponseReply,
					Messages: []protocol.ReplyPart{
						{Kind: protocol.ReplyMessage, Body: "hey"},
						{Kind: protocol.ReplyAction, Body: "waves"},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newChatServer(t, tt.toolCall)

			client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

			got, err := client.SendEvents(
				t.Context(),
				"test/model",
				"You are a test bot.",
				nil,
				[]protocol.IRCMessage{
					{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "hi"},
				},
			)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestOpenRouterClient_SendEventsWithHistory(t *testing.T) {
	var receivedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/chat/completions", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&receivedBody))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatResponse(toolCallFixture{
			name: "pass",
			args: `{"reason": "just checking"}`,
		}))
	}))
	t.Cleanup(srv.Close)

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	history := []protocol.IRCMessage{
		{Kind: protocol.KindJoin, From: "bob", Target: "#test"},
	}
	events := []protocol.IRCMessage{
		{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "hello"},
	}

	_, err := client.SendEvents(
		t.Context(),
		"test/model",
		"System prompt",
		history,
		events,
	)
	require.NoError(t, err)

	var wantMsgs []map[string]any

	wantMsgs = append(wantMsgs, map[string]any{
		"role":    "system",
		"content": "System prompt",
	})

	for _, h := range history {
		data, err := json.Marshal(h)
		require.NoError(t, err)
		wantMsgs = append(wantMsgs, map[string]any{
			"role":    "user",
			"content": string(data),
		})
	}

	for _, e := range events {
		data, err := json.Marshal(e)
		require.NoError(t, err)
		wantMsgs = append(wantMsgs, map[string]any{
			"role":    "user",
			"content": string(data),
		})
	}

	wantJSON, err := json.Marshal(wantMsgs)
	require.NoError(t, err)

	msgs, ok := receivedBody["messages"].([]any)
	require.True(t, ok, "expected messages in request body")

	gotJSON, err := json.Marshal(msgs)
	require.NoError(t, err)

	require.JSONEq(t, string(wantJSON), string(gotJSON))
}

func TestOpenRouterClient_SendEvents_preservesOpenRouterUsageMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/chat/completions", r.URL.Path)
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl_usage",
			"usage": map[string]any{
				"prompt_tokens":     11,
				"completion_tokens": 7,
				"total_tokens":      18,
				"cost":              0.625,
				"prompt_tokens_details": map[string]any{
					"cached_tokens":      3,
					"cache_write_tokens": 2,
				},
				"completion_tokens_details": map[string]any{
					"reasoning_tokens": 4,
				},
				"cost_details": map[string]any{
					"upstream_inference_cost": 0.5,
				},
			},
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"role":    "assistant",
						"content": "",
						"tool_calls": []map[string]any{
							{
								"id":   "call_usage",
								"type": "function",
								"function": map[string]any{
									"name":      "pass",
									"arguments": `{"reason":"done"}`,
								},
							},
						},
					},
					"finish_reason": "tool_calls",
					"index":         0,
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	got, err := client.SendEvents(
		t.Context(),
		"test/model",
		"You are a test bot.",
		nil,
		[]protocol.IRCMessage{
			{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "hi"},
		},
	)
	require.NoError(t, err)
	require.Equal(t, Usage{
		PromptTokens:          11,
		CompletionTokens:      7,
		TotalTokens:           18,
		ReasoningTokens:       4,
		CachedTokens:          3,
		CacheWriteTokens:      2,
		CostCredits:           0.625,
		UpstreamInferenceCost: 0.5,
	}, got.Usage)
}

func TestOpenRouterClient_GenerateNick(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		wantNick domain.Nick
	}{
		{name: "clean response", content: "claud3", wantNick: "claud3"},
		{name: "response with surrounding whitespace", content: "  sparky\n", wantNick: "sparky"},
		{name: "response with quotes", content: `"zenbot"`, wantNick: "zenbot"},
		{name: "response with mixed case", content: "ZenBot", wantNick: "zenbot"},
		{name: "response longer than 12 chars truncated", content: "superlongnicknamehere", wantNick: "superlongnic"},
		{name: "response with spaces replaced by underscores", content: "zen bot", wantNick: "zen_bot"},
		{name: "response with non-IRC characters stripped", content: "zen!@#bot", wantNick: "zenbot"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "chatcmpl_nick",
					"choices": []map[string]any{
						{
							"message": map[string]any{
								"role":    "assistant",
								"content": tt.content,
							},
							"finish_reason": "stop",
							"index":         0,
						},
					},
				})
			}))
			t.Cleanup(srv.Close)

			client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

			got, err := client.GenerateNick(t.Context(), "anthropic/claude-haiku-4.5", "anthropic/claude-3-haiku")
			require.NoError(t, err)
			require.Equal(t, tt.wantNick, got.Nick)
		})
	}
}

func TestSanitizeNick(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "clean", raw: "sparky", want: "sparky"},
		{name: "trim whitespace", raw: "  sparky\n\t", want: "sparky"},
		{name: "strip quotes", raw: `"sparky"`, want: "sparky"},
		{name: "strip single quotes", raw: "'sparky'", want: "sparky"},
		{name: "strip backticks", raw: "`sparky`", want: "sparky"},
		{name: "lowercase", raw: "SPARKY", want: "sparky"},
		{name: "spaces to underscores", raw: "zen bot", want: "zen_bot"},
		{name: "strip unsafe chars", raw: "zen!@#$%^&*()bot", want: "zenbot"},
		{name: "allow underscores", raw: "zen_bot", want: "zen_bot"},
		{name: "allow hyphens", raw: "zen-bot", want: "zen-bot"},
		{name: "allow digits", raw: "bot42", want: "bot42"},
		{name: "truncate to 12", raw: "abcdefghijklmnop", want: "abcdefghijkl"},
		{name: "empty after sanitize", raw: "!@#$%^", want: ""},
		{name: "mixed problems", raw: `  "Zen Bot 3000!"  `, want: "zen_bot_3000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeNick(tt.raw)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestOpenRouterClient_SendEvents_write_memory(t *testing.T) {
	srv := newChatServer(t, toolCallFixture{
		name: "write_memory",
		args: `{"key": "mood", "content": "happy"}`,
	})

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	got, err := client.SendEvents(
		t.Context(),
		"test/model",
		"You are a test bot.",
		nil,
		[]protocol.IRCMessage{
			{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "hi"},
		},
	)

	require.NoError(t, err)
	require.Equal(t, []PendingToolCall{
		{ID: "call_123", Kind: ToolCallWriteMemory, Key: "mood", Body: "happy"},
	}, got.PendingToolCalls)
	require.NotNil(t, got.Conversation)
}

func TestOpenRouterClient_SendEvents_delete_memory(t *testing.T) {
	srv := newChatServer(t, toolCallFixture{
		name: "delete_memory",
		args: `{"key": "old_stuff"}`,
	})

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	got, err := client.SendEvents(
		t.Context(),
		"test/model",
		"You are a test bot.",
		nil,
		[]protocol.IRCMessage{
			{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "hi"},
		},
	)

	require.NoError(t, err)
	require.Equal(t, []PendingToolCall{
		{ID: "call_123", Kind: ToolCallDeleteMemory, Key: "old_stuff"},
	}, got.PendingToolCalls)
	require.NotNil(t, got.Conversation)
}

func TestOpenRouterClient_ContinueWithToolResults(t *testing.T) {
	firstCall := true

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))

		w.Header().Set("Content-Type", "application/json")

		if firstCall {
			firstCall = false

			require.NoError(t, json.NewEncoder(w).Encode(chatResponse(toolCallFixture{
				name: "write_memory",
				args: `{"key": "mood", "content": "happy"}`,
			})))

			return
		}

		// The continuation request should end with a tool result
		// message confirming the write_memory execution.
		messages, ok := body["messages"].([]any)
		require.True(t, ok, "expected messages in request body")
		lastMsg, ok := messages[len(messages)-1].(map[string]any)
		require.True(t, ok, "expected final message to be an object")
		require.Equal(t, map[string]any{
			"role":         "tool",
			"content":      "ok",
			"tool_call_id": "call_123",
		}, lastMsg)

		require.NoError(t, json.NewEncoder(w).Encode(chatResponse(toolCallFixture{
			name: "reply",
			args: `{"messages": [{"type": "message", "body": "stored it"}]}`,
		})))
	}))
	t.Cleanup(srv.Close)

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	initial, err := client.SendEvents(
		t.Context(),
		"test/model",
		"You are a test bot.",
		nil,
		[]protocol.IRCMessage{
			{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "hi"},
		},
	)
	require.NoError(t, err)
	require.NotNil(t, initial.Conversation)
	require.Equal(t, []PendingToolCall{
		{ID: "call_123", Kind: ToolCallWriteMemory, Key: "mood", Body: "happy"},
	}, initial.PendingToolCalls)

	continued, err := client.ContinueWithToolResults(
		t.Context(),
		initial.Conversation,
		[]ToolResult{{ToolCallID: "call_123", Content: "ok"}},
	)
	require.NoError(t, err)
	require.Equal(t, protocol.ModelResponse{
		Kind:     protocol.ResponseReply,
		Messages: []protocol.ReplyPart{{Kind: protocol.ReplyMessage, Body: "stored it"}},
	}, continued.Response)
}

// --- Test helpers ---

type toolCallFixture struct {
	name string
	args string
}

func chatResponse(tc toolCallFixture) map[string]any {
	return map[string]any{
		"id": "chatcmpl_test",
		"usage": map[string]any{
			"prompt_tokens":     10,
			"completion_tokens": 5,
			"total_tokens":      15,
			"cost":              0.125,
		},
		"choices": []map[string]any{
			{
				"message": map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []map[string]any{
						{
							"id":   "call_123",
							"type": "function",
							"function": map[string]any{
								"name":      tc.name,
								"arguments": tc.args,
							},
						},
					},
				},
				"finish_reason": "tool_calls",
				"index":         0,
			},
		},
	}
}

func newChatServer(t *testing.T, tc toolCallFixture) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/chat/completions", r.URL.Path)
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(chatResponse(tc)))
	}))
	t.Cleanup(srv.Close)

	return srv
}
