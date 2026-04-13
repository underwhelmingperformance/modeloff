package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	openai "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
)

func testMemoryTools(includeSearch bool) []ToolDefinition {
	tools := []ToolDefinition{
		{
			Name:        "write_memory",
			Description: "write",
			Parameters: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
		{
			Name:        "delete_memory",
			Description: "delete",
			Parameters: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
	}

	if includeSearch {
		tools = append(tools, ToolDefinition{
			Name:        "search_memory",
			Description: "search",
			Parameters: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		})
	}

	return tools
}

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
		name    string
		content string
		want    CompletionResult
		wantErr bool
	}{
		{
			name:    "model replies",
			content: `{"response":{"kind":"reply","messages":[{"type":"message","body":"Hello there!"}]}}`,
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
			name:    "model passes",
			content: `{"response":{"kind":"pass","reason":"Nothing to add"}}`,
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
			name:    "unknown response kind",
			content: `{"response":{"kind":"shout","text":"HELLO"}}`,
			wantErr: true,
		},
		{
			name:    "model replies with multiple messages including action",
			content: `{"response":{"kind":"reply","messages":[{"type":"message","body":"hey"},{"type":"action","body":"waves"}]}}`,
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
			srv := newStructuredChatServer(t, tt.content)

			client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

			got, err := client.SendEvents(
				t.Context(),
				"test/model",
				"",
				"You are a test bot.",
				nil,
				[]protocol.IRCMessage{
					{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "hi"},
				},
				testMemoryTools(true)...,
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

func TestCompletionParseErrorKind(t *testing.T) {
	require.Equal(
		t,
		observability.ErrorKindResponseParse,
		completionParseErrorKind(&completionParseError{target: "structured response", err: errors.New("bad json")}),
	)
}

func messageRoles(msgs []openai.ChatCompletionMessageParamUnion) []string {
	roles := make([]string, len(msgs))

	for i, m := range msgs {
		switch {
		case m.OfSystem != nil:
			roles[i] = "system"
		case m.OfAssistant != nil:
			roles[i] = "assistant"
		case m.OfUser != nil:
			roles[i] = "user"
		case m.OfTool != nil:
			roles[i] = "tool"
		case m.OfDeveloper != nil:
			roles[i] = "developer"
		}
	}

	return roles
}

func TestBuildMessages_self_messages_are_assistant_role(t *testing.T) {
	const selfID = "inst-abc123"

	history := []protocol.IRCMessage{
		{Kind: protocol.KindPrivMsg, From: "botty", InstanceID: selfID, Target: "#test", Body: "I said this"},
		{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "alice said this"},
	}
	events := []protocol.IRCMessage{
		{Kind: protocol.KindPrivMsg, From: "bob", Target: "#test", Body: "bob said this"},
		{Kind: protocol.KindPrivMsg, From: "botty", InstanceID: selfID, Target: "#test", Body: "I said this too"},
	}

	msgs := buildMessages("system prompt", selfID, history, events)

	require.Equal(t, []string{"system", "assistant", "user", "user", "assistant"}, messageRoles(msgs))
}

func TestBuildMessages_survives_nick_rename(t *testing.T) {
	const selfID = "inst-stable"

	history := []protocol.IRCMessage{
		{Kind: protocol.KindPrivMsg, From: "old-nick", InstanceID: selfID, Target: "#test", Body: "before rename"},
		{Kind: protocol.KindPrivMsg, From: "new-nick", InstanceID: selfID, Target: "#test", Body: "after rename"},
		{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "other user"},
	}

	msgs := buildMessages("system prompt", selfID, history, nil)

	require.Equal(t, []string{"system", "assistant", "assistant", "user"}, messageRoles(msgs))
}

func TestBuildMessages_instance_id_stripped_from_json(t *testing.T) {
	events := []protocol.IRCMessage{
		{Kind: protocol.KindPrivMsg, From: "botty", InstanceID: "inst-xyz", Target: "#test", Body: "hello"},
	}

	msgs := buildMessages("system prompt", "", events, nil)

	for _, m := range msgs {
		data, err := json.Marshal(m)
		require.NoError(t, err)
		require.NotContains(t, string(data), "inst-xyz")
	}
}

func TestOpenRouterClient_SendEventsWithHistory(t *testing.T) {
	var receivedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/chat/completions", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&receivedBody))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(structuredChatResponse(`{"response":{"kind":"pass","reason":"just checking"}}`))
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
		"",
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
						"content": `{"response":{"kind":"pass","reason":"done"}}`,
					},
					"finish_reason": "stop",
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
		"",
		"You are a test bot.",
		nil,
		[]protocol.IRCMessage{
			{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "hi"},
		},
		testMemoryTools(true)...,
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
	srv := newToolCallServer(t, toolCallFixture{
		name: "write_memory",
		args: `{"key": "mood", "content": "happy"}`,
	})

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	got, err := client.SendEvents(
		t.Context(),
		"test/model",
		"",
		"You are a test bot.",
		nil,
		[]protocol.IRCMessage{
			{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "hi"},
		},
		testMemoryTools(false)...,
	)

	require.NoError(t, err)
	require.Equal(t, []PendingToolCall{
		{
			ID:   "call_123",
			Name: "write_memory",
			Args: json.RawMessage(`{"key": "mood", "content": "happy"}`),
		},
	}, got.PendingToolCalls)
	require.NotNil(t, got.Conversation)
}

func TestOpenRouterClient_SendEvents_delete_memory(t *testing.T) {
	srv := newToolCallServer(t, toolCallFixture{
		name: "delete_memory",
		args: `{"key": "old_stuff"}`,
	})

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	got, err := client.SendEvents(
		t.Context(),
		"test/model",
		"",
		"You are a test bot.",
		nil,
		[]protocol.IRCMessage{
			{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "hi"},
		},
		testMemoryTools(true)...,
	)

	require.NoError(t, err)
	require.Equal(t, []PendingToolCall{
		{
			ID:   "call_123",
			Name: "delete_memory",
			Args: json.RawMessage(`{"key": "old_stuff"}`),
		},
	}, got.PendingToolCalls)
	require.NotNil(t, got.Conversation)
}

func TestOpenRouterClient_SendEvents_search_memory(t *testing.T) {
	srv := newToolCallServer(t, toolCallFixture{
		name: "search_memory",
		args: `{"query": "favourite colour", "limit": 3}`,
	})

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	got, err := client.SendEvents(
		t.Context(),
		"test/model",
		"",
		"You are a test bot.",
		nil,
		[]protocol.IRCMessage{
			{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "hi"},
		},
		testMemoryTools(true)...,
	)

	require.NoError(t, err)
	require.Equal(t, []PendingToolCall{
		{
			ID:   "call_123",
			Name: "search_memory",
			Args: json.RawMessage(`{"query": "favourite colour", "limit": 3}`),
		},
	}, got.PendingToolCalls)
	require.NotNil(t, got.Conversation)
}

func TestOpenRouterClient_SendEvents_includes_explicit_search_tool(t *testing.T) {
	var receivedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&receivedBody))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(structuredChatResponse(
			`{"response":{"kind":"pass","reason":"done"}}`,
		))
	}))
	t.Cleanup(srv.Close)

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	_, err := client.SendEvents(
		t.Context(),
		"test/model",
		"",
		"You are a test bot.",
		nil,
		[]protocol.IRCMessage{
			{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "hi"},
		},
		testMemoryTools(true)...,
	)
	require.NoError(t, err)

	tools, ok := receivedBody["tools"].([]any)
	require.True(t, ok, "expected tools in request body")

	var toolNames []string
	for _, tool := range tools {
		toolMap := tool.(map[string]any)
		fn := toolMap["function"].(map[string]any)
		toolNames = append(toolNames, fn["name"].(string))
	}

	require.Contains(t, toolNames, "search_memory")
	require.Contains(t, toolNames, "write_memory")
	require.Contains(t, toolNames, "delete_memory")
}

func TestOpenRouterClient_SendEvents_excludes_search_without_explicit_tool(t *testing.T) {
	var receivedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&receivedBody))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(structuredChatResponse(
			`{"response":{"kind":"pass","reason":"done"}}`,
		))
	}))
	t.Cleanup(srv.Close)

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	_, err := client.SendEvents(
		t.Context(),
		"test/model",
		"",
		"You are a test bot.",
		nil,
		[]protocol.IRCMessage{
			{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "hi"},
		},
		testMemoryTools(false)...,
	)
	require.NoError(t, err)

	tools, ok := receivedBody["tools"].([]any)
	require.True(t, ok, "expected tools in request body")

	var toolNames []string
	for _, tool := range tools {
		toolMap := tool.(map[string]any)
		fn := toolMap["function"].(map[string]any)
		toolNames = append(toolNames, fn["name"].(string))
	}

	require.NotContains(t, toolNames, "search_memory")
	require.Contains(t, toolNames, "write_memory")
	require.Contains(t, toolNames, "delete_memory")
}

func TestOpenRouterClient_SendEvents_contentFiltered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl_filtered",
			"choices": []map[string]any{
				{
					"message":       map[string]any{"role": "assistant", "content": ""},
					"finish_reason": "content_filter",
					"index":         0,
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	_, err := client.SendEvents(
		t.Context(), "test/model", "", "prompt", nil,
		[]protocol.IRCMessage{{Kind: protocol.KindPrivMsg, From: "a", Target: "#t", Body: "x"}},
	)
	require.ErrorIs(t, err, ErrContentFiltered)
}

func TestOpenRouterClient_SendEvents_truncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl_trunc",
			"choices": []map[string]any{
				{
					"message":       map[string]any{"role": "assistant", "content": ""},
					"finish_reason": "length",
					"index":         0,
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	_, err := client.SendEvents(
		t.Context(), "test/model", "", "prompt", nil,
		[]protocol.IRCMessage{{Kind: protocol.KindPrivMsg, From: "a", Target: "#t", Body: "x"}},
	)
	require.ErrorIs(t, err, ErrResponseTruncated)
}

func TestOpenRouterClient_SendEvents_refusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl_refuse",
			"choices": []map[string]any{
				{
					"message":       map[string]any{"role": "assistant", "content": "", "refusal": "I cannot do that"},
					"finish_reason": "stop",
					"index":         0,
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	_, err := client.SendEvents(
		t.Context(), "test/model", "", "prompt", nil,
		[]protocol.IRCMessage{{Kind: protocol.KindPrivMsg, From: "a", Target: "#t", Body: "x"}},
	)

	var refused *ErrModelRefused
	require.ErrorAs(t, err, &refused)
	require.Equal(t, "I cannot do that", refused.Reason)
}

func TestOpenRouterClient_SendEvents_emptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl_empty",
			"choices": []map[string]any{
				{
					"message":       map[string]any{"role": "assistant", "content": ""},
					"finish_reason": "stop",
					"index":         0,
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	_, err := client.SendEvents(
		t.Context(), "test/model", "", "prompt", nil,
		[]protocol.IRCMessage{{Kind: protocol.KindPrivMsg, From: "a", Target: "#t", Body: "x"}},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no response and no tool calls")
}

func TestOpenRouterClient_ContinueWithToolResults(t *testing.T) {
	firstCall := true

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))

		w.Header().Set("Content-Type", "application/json")

		if firstCall {
			firstCall = false

			require.NoError(t, json.NewEncoder(w).Encode(toolCallResponse(toolCallFixture{
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
			"content":      `{"ok":true,"summary":"stored memory \"mood\""}`,
			"tool_call_id": "call_123",
		}, lastMsg)

		require.NoError(t, json.NewEncoder(w).Encode(structuredChatResponse(
			`{"response":{"kind":"reply","messages":[{"type":"message","body":"stored it"}]}}`,
		)))
	}))
	t.Cleanup(srv.Close)

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	initial, err := client.SendEvents(
		t.Context(),
		"test/model",
		"",
		"You are a test bot.",
		nil,
		[]protocol.IRCMessage{
			{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "hi"},
		},
		testMemoryTools(false)...,
	)
	require.NoError(t, err)
	require.NotNil(t, initial.Conversation)
	require.Equal(t, []PendingToolCall{
		{
			ID:   "call_123",
			Name: "write_memory",
			Args: json.RawMessage(`{"key": "mood", "content": "happy"}`),
		},
	}, initial.PendingToolCalls)

	continued, err := client.ContinueWithToolResults(
		t.Context(),
		initial.Conversation,
		[]ToolResult{{ToolCallID: "call_123", Content: `{"ok":true,"summary":"stored memory \"mood\""}`}},
		testMemoryTools(false)...,
	)
	require.NoError(t, err)
	require.Equal(t, protocol.ModelResponse{
		Kind:     protocol.ResponseReply,
		Messages: []protocol.ReplyPart{{Kind: protocol.ReplyMessage, Body: "stored it"}},
	}, continued.Response)
}

func TestOpenRouterClient_GeneratePersonas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/chat/completions", r.URL.Path)
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(structuredChatResponse(
			`{"personas":[{"id":"grumpy-sysadmin","description":"Runs FreeBSD on everything and complains about systemd."},{"id":"lurker-larry","description":"Only speaks up to correct someone about an RFC."}]}`,
		))
	}))
	t.Cleanup(srv.Close)

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	got, err := client.GeneratePersonas(t.Context(), "anthropic/claude-haiku-4.5")
	require.NoError(t, err)
	require.Equal(t, []domain.Persona{
		{
			ID:          "grumpy-sysadmin",
			Description: "Runs FreeBSD on everything and complains about systemd.",
			Origin:      domain.PersonaGenerated,
		},
		{
			ID:          "lurker-larry",
			Description: "Only speaks up to correct someone about an RFC.",
			Origin:      domain.PersonaGenerated,
		},
	}, got)
}

func TestOpenRouterClient_GeneratePersonas_empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(structuredChatResponse(`{"personas":[]}`))
	}))
	t.Cleanup(srv.Close)

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	got, err := client.GeneratePersonas(t.Context(), "anthropic/claude-haiku-4.5")
	require.NoError(t, err)
	require.Equal(t, []domain.Persona{}, got)
}

func TestOpenRouterClient_GeneratePersonas_invalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(structuredChatResponse(`not json`))
	}))
	t.Cleanup(srv.Close)

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	_, err := client.GeneratePersonas(t.Context(), "anthropic/claude-haiku-4.5")
	require.Error(t, err)

	var parseErr *completionParseError
	require.ErrorAs(t, err, &parseErr)
}

// --- Test helpers ---

type toolCallFixture struct {
	name string
	args string
}

func structuredChatResponse(content string) map[string]any {
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
					"content": content,
				},
				"finish_reason": "stop",
				"index":         0,
			},
		},
	}
}

func toolCallResponse(tc toolCallFixture) map[string]any {
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

func newStructuredChatServer(t *testing.T, content string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/chat/completions", r.URL.Path)
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(structuredChatResponse(content)))
	}))
	t.Cleanup(srv.Close)

	return srv
}

func newToolCallServer(t *testing.T, tc toolCallFixture) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/chat/completions", r.URL.Path)
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(toolCallResponse(tc)))
	}))
	t.Cleanup(srv.Close)

	return srv
}
