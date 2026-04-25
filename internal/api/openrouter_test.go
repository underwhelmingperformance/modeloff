package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

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

func TestOpenRouterClient_ListModels_error_branches(t *testing.T) {
	tests := []struct {
		name         string
		serverHandle func() (url string, cleanup func())
		wantErrMsg   string
		wantLogMsg   string
	}{
		{
			name: "transport failure returns single-line error",
			serverHandle: func() (string, func()) {
				srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				url := srv.URL
				srv.Close()
				return url, func() {}
			},
			wantErrMsg: "list models: network error",
			wantLogMsg: "openrouter list models transport failure",
		},
		{
			name: "non-2xx returns shaped status error",
			serverHandle: func() (string, func()) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":"upstream"}`))
				}))
				return srv.URL, srv.Close
			},
			wantErrMsg: "list models: status 503",
			wantLogMsg: "openrouter list models non-2xx",
		},
		{
			name: "decode failure returns single-line error",
			serverHandle: func() (string, func()) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`not actually json`))
				}))
				return srv.URL, srv.Close
			},
			wantErrMsg: "list models: invalid response",
			wantLogMsg: "openrouter list models decode failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf apiLogBuffer
			handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
			slog.SetDefault(slog.New(handler))
			t.Cleanup(func() { slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) })

			url, cleanup := tt.serverHandle()
			t.Cleanup(cleanup)

			client := NewOpenRouterClient("test-key", url, &http.Client{Timeout: time.Second})

			_, err := client.ListModels(t.Context())
			require.Error(t, err)
			require.Equal(t, tt.wantErrMsg, err.Error())
			require.NotContains(t, err.Error(), "\n", "user-facing error must be single line")

			record := buf.find(tt.wantLogMsg)
			require.NotNil(t, record, "expected %q log entry", tt.wantLogMsg)
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

func messageContents(msgs []openai.ChatCompletionMessageParamUnion) []string {
	contents := make([]string, len(msgs))

	for i, m := range msgs {
		switch {
		case m.OfSystem != nil:
			contents[i] = m.OfSystem.Content.OfString.Value
		case m.OfAssistant != nil:
			contents[i] = m.OfAssistant.Content.OfString.Value
		case m.OfUser != nil:
			contents[i] = m.OfUser.Content.OfString.Value
		case m.OfDeveloper != nil:
			contents[i] = m.OfDeveloper.Content.OfString.Value
		case m.OfTool != nil:
			contents[i] = m.OfTool.Content.OfString.Value
		}
	}

	return contents
}

func TestBuildMessages_self_messages_are_assistant_role_in_history(t *testing.T) {
	const selfID = "inst-abc123"

	history := []protocol.IRCMessage{
		{Kind: protocol.KindPrivMsg, From: "botty", InstanceID: selfID, Target: "#test", Body: "I said this"},
		{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "alice said this"},
	}
	events := []protocol.IRCMessage{
		{Kind: protocol.KindPrivMsg, From: "bob", Target: "#test", Body: "bob said this"},
	}

	msgs := buildMessages("system prompt", selfID, history, events)

	require.Equal(t, []string{"system", "assistant", "user", "user"}, messageRoles(msgs))
}

func TestBuildMessages_self_events_are_excluded(t *testing.T) {
	const selfID = "inst-abc123"

	events := []protocol.IRCMessage{
		{Kind: protocol.KindPrivMsg, From: "bob", Target: "#test", Body: "bob said this"},
		{Kind: protocol.KindPrivMsg, From: "botty", InstanceID: selfID, Target: "#test", Body: "I said this too"},
		{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "alice chiming in"},
	}

	msgs := buildMessages("system prompt", selfID, nil, events)

	require.Equal(t, []string{"system", "user", "user"}, messageRoles(msgs))
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

	expectedEvent := events[0]
	expectedEvent.InstanceID = ""

	expectedJSON, err := json.Marshal(expectedEvent)
	require.NoError(t, err)

	require.Equal(t, []string{
		"system prompt",
		string(expectedJSON),
	}, messageContents(msgs))
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

	require.Equal(t, []string{"write_memory", "delete_memory", "search_memory"}, toolNames)
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

	require.Equal(t, []string{"write_memory", "delete_memory"}, toolNames)
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
	require.EqualError(t, err, "model returned no response and no tool calls")
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

// --- Log capture ---

type apiLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (lb *apiLogBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	return lb.buf.Write(p)
}

func (lb *apiLogBuffer) find(msg string) map[string]any {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	for line := range bytes.SplitSeq(lb.buf.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}

		var record map[string]any
		if json.Unmarshal(line, &record) != nil {
			continue
		}

		if record["msg"] == msg {
			return record
		}
	}

	return nil
}

func TestSendEvents_logs_event_and_history_counts(t *testing.T) {
	var buf apiLogBuffer

	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) })

	srv := newStructuredChatServer(t, `{"response":{"kind":"pass","reason":"nothing"}}`)

	client := NewOpenRouterClient("test-key", srv.URL, srv.Client())

	history := []protocol.IRCMessage{
		{Kind: protocol.KindJoin, From: "bob", Target: "#test"},
		{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "earlier"},
	}
	events := []protocol.IRCMessage{
		{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "hello"},
		{Kind: protocol.KindPrivMsg, From: "bob", Target: "#test", Body: "world"},
		{Kind: protocol.KindJoin, From: "charlie", Target: "#test"},
	}

	_, err := client.SendEvents(t.Context(), "test/model", "", "system", history, events)
	require.NoError(t, err)

	record := buf.find("openrouter send events completed")
	require.NotNil(t, record, "expected 'openrouter send events completed' log entry")

	require.Equal(t, float64(3), record["event_count"])
	require.Equal(t, float64(2), record["history_count"])
}

func TestContinueWithToolResults_logs_token_counts(t *testing.T) {
	var buf apiLogBuffer

	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) })

	// Server that returns tool calls on the first request.
	toolSrv := newToolCallServer(t, toolCallFixture{
		name: "write_memory",
		args: `{"key":"k","value":"v"}`,
	})

	client := NewOpenRouterClient("test-key", toolSrv.URL, toolSrv.Client())

	result, err := client.SendEvents(
		t.Context(), "test/model", "", "system", nil,
		[]protocol.IRCMessage{{Kind: protocol.KindPrivMsg, From: "alice", Target: "#test", Body: "hi"}},
		testMemoryTools(false)...,
	)
	require.NoError(t, err)
	require.NotNil(t, result.Conversation)

	// For the continuation, use a server that returns a structured response.
	continueSrv := newStructuredChatServer(t, `{"response":{"kind":"pass","reason":"done"}}`)
	continueClient := NewOpenRouterClient("test-key", continueSrv.URL, continueSrv.Client())

	// Transplant the conversation to the new client's server.
	_, err = continueClient.ContinueWithToolResults(
		t.Context(),
		result.Conversation,
		[]ToolResult{{ToolCallID: "call-1", Content: "ok"}},
		testMemoryTools(false)...,
	)
	require.NoError(t, err)

	record := buf.find("openrouter continue completed")
	require.NotNil(t, record, "expected 'openrouter continue completed' log entry")

	require.Equal(t, float64(10), record["prompt_tokens"])
	require.Equal(t, float64(5), record["completion_tokens"])
	require.Equal(t, 0.125, record["cost_credits"])
}

func TestModelResponseSchema_inlines_all_definitions(t *testing.T) {
	want := map[string]any{
		"$id":                  "https://github.com/laney/modeloff/internal/api/model-response-wrapper",
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"additionalProperties": false,
		"type":                 "object",
		"required":             []any{"response"},
		"properties": map[string]any{
			"response": map[string]any{
				"anyOf": []any{
					map[string]any{
						"additionalProperties": false,
						"type":                 "object",
						"required":             []any{"kind", "messages"},
						"properties": map[string]any{
							"kind": map[string]any{
								"const": "reply",
								"type":  "string",
							},
							"messages": map[string]any{
								"description": "One or more messages to send.",
								"type":        "array",
								"items": map[string]any{
									"additionalProperties": false,
									"type":                 "object",
									"required":             []any{"type"},
									"properties": map[string]any{
										"type": map[string]any{
											"description": `"message" for a regular message, "action" for a /me action.`,
											"type":        "string",
											"enum":        []any{"message", "action"},
										},
										"body": map[string]any{
											"description": "The plain message text. For actions, just the action body without /me. Provide either body or spans, not both.",
											"type":        "string",
										},
										"spans": map[string]any{
											"description": "Optional styled spans. Prefer this over raw IRC control characters when you want formatting. Provide either spans or body, not both.",
											"type":        "array",
											"items": map[string]any{
												"$id":                  "https://github.com/laney/modeloff/internal/protocol/reply-span",
												"$schema":              "https://json-schema.org/draft/2020-12/schema",
												"additionalProperties": false,
												"type":                 "object",
												"required":             []any{"text"},
												"properties": map[string]any{
													"text": map[string]any{
														"type": "string",
													},
													"style": map[string]any{
														"additionalProperties": false,
														"type":                 "object",
														"properties": map[string]any{
															"bold":      map[string]any{"type": "boolean"},
															"italic":    map[string]any{"type": "boolean"},
															"underline": map[string]any{"type": "boolean"},
															"reverse":   map[string]any{"type": "boolean"},
															"strike":    map[string]any{"type": "boolean"},
															"fg":        map[string]any{"type": "integer"},
															"bg":        map[string]any{"type": "integer"},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
					map[string]any{
						"additionalProperties": false,
						"type":                 "object",
						"required":             []any{"kind", "reason"},
						"properties": map[string]any{
							"kind": map[string]any{
								"const": "pass",
								"type":  "string",
							},
							"reason": map[string]any{
								"description": "A brief reason for not replying.",
								"type":        "string",
							},
						},
					},
				},
			},
		},
	}

	require.Equal(t, want, modelResponseSchemaMap)
}

// hangingTransport is an http.RoundTripper that blocks every request
// until its context is cancelled, then returns the context error.
// Used inside a synctest bubble it lets us drive timeout tests off
// virtual time without any real network or real wall-clock waiting.
type hangingTransport struct{}

func (hangingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func newHangingClient(chat, meta time.Duration) *OpenRouterClient {
	httpClient := &http.Client{Transport: hangingTransport{}}

	return NewOpenRouterClient("test-key", "http://hang.invalid", httpClient).
		WithTimeouts(chat, meta)
}

func TestOpenRouterClient_perCallTimeouts(t *testing.T) {
	const (
		clientChat = 60 * time.Second
		clientMeta = 30 * time.Second
	)

	tests := []struct {
		name string
		call func(ctx context.Context, c *OpenRouterClient) error
	}{
		{
			name: "ListModels",
			call: func(ctx context.Context, c *OpenRouterClient) error {
				_, err := c.ListModels(ctx)
				return err
			},
		},
		{
			name: "GenerateNick",
			call: func(ctx context.Context, c *OpenRouterClient) error {
				_, err := c.GenerateNick(ctx, "anthropic/claude-haiku-4.5", "anthropic/claude-3-haiku")
				return err
			},
		},
		{
			name: "GeneratePersonas",
			call: func(ctx context.Context, c *OpenRouterClient) error {
				_, err := c.GeneratePersonas(ctx, "anthropic/claude-haiku-4.5")
				return err
			},
		},
		{
			name: "SendEvents",
			call: func(ctx context.Context, c *OpenRouterClient) error {
				_, err := c.SendEvents(
					ctx,
					"test/model",
					"",
					"prompt",
					nil,
					[]protocol.IRCMessage{{Kind: protocol.KindPrivMsg, From: "a", Target: "#t", Body: "x"}},
				)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := newHangingClient(clientChat, clientMeta)

				err := tt.call(t.Context(), client)

				require.Error(t, err)
				require.ErrorIs(t, err, context.DeadlineExceeded)
			})
		})
	}
}

func TestOpenRouterClient_callerDeadlineWins(t *testing.T) {
	const (
		callerDeadline = 25 * time.Millisecond
		clientChat     = 60 * time.Second
		clientMeta     = 60 * time.Second
	)

	synctest.Test(t, func(t *testing.T) {
		client := newHangingClient(clientChat, clientMeta)

		ctx, cancel := context.WithTimeout(t.Context(), callerDeadline)
		t.Cleanup(cancel)

		callerDeadlineTime, ok := ctx.Deadline()
		require.True(t, ok)

		_, err := client.SendEvents(
			ctx,
			"test/model",
			"",
			"prompt",
			nil,
			[]protocol.IRCMessage{{Kind: protocol.KindPrivMsg, From: "a", Target: "#t", Body: "x"}},
		)

		require.Error(t, err)
		require.ErrorIs(t, err, context.DeadlineExceeded)

		// The caller's deadline must fire long before the 60 s
		// client-side timeout would; under synctest the virtual clock
		// only advances to the next pending timer, so observing the
		// caller deadline confirms its `WithTimeout` shadowed ours.
		require.WithinDuration(t, callerDeadlineTime, time.Now(), time.Millisecond)
	})
}

func TestTruncateBody(t *testing.T) {
	// The pound sign is a two-byte UTF-8 rune (0xC2 0xA3) that lets us
	// exercise the rune-boundary rewind without depending on platform
	// locale.
	tests := map[string]struct {
		body       []byte
		limit      int
		want       string
		assertUTF8 bool
	}{
		"empty body":                {body: []byte{}, limit: 8, want: "", assertUTF8: true},
		"shorter than limit":        {body: []byte("hello"), limit: 8, want: "hello", assertUTF8: true},
		"equal to limit":            {body: []byte("abcdefgh"), limit: 8, want: "abcdefgh", assertUTF8: true},
		"ascii over limit":          {body: []byte("abcdefghij"), limit: 8, want: "abcdefgh…[truncated]", assertUTF8: true},
		"rune straddles cut point":  {body: []byte("abcdefg£hij"), limit: 8, want: "abcdefg…[truncated]", assertUTF8: true},
		"rune ends on cut boundary": {body: []byte("ab£cdefgh"), limit: 4, want: "ab£…[truncated]", assertUTF8: true},

		// Upstream already returned invalid UTF-8 and the body fits within
		// limit, so the short-circuit passes it through unchanged. We do
		// not repair upstream — only guarantee that truncation itself
		// introduces no mojibake.
		"all continuation bytes, under limit": {body: []byte{0xA3, 0xA3, 0xA3}, limit: 8, want: "\xA3\xA3\xA3"},

		// Rewind runs to end=0 because no byte is a rune start; the
		// returned string is just the truncation marker, which is valid
		// UTF-8 despite the pathological input.
		"continuation-only body over limit": {body: []byte{0xA3, 0xA3, 0xA3, 0xA3, 0xA3}, limit: 2, want: "…[truncated]", assertUTF8: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := truncateBody(tc.body, tc.limit)
			require.Equal(t, tc.want, got)
			if tc.assertUTF8 {
				require.True(t, utf8.ValidString(got), "truncated output must be valid UTF-8")
			}
		})
	}
}
