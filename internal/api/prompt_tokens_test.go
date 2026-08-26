package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

type promptEstimateCase struct {
	name        string
	observe     []promptTokenObservation
	reject      []promptTokenRejection
	requestSize int
	want        int
}

type promptTokenObservation struct {
	bytes  int
	tokens int64
}

type promptTokenRejection struct {
	bytes int
	limit int
}

type promptEstimateEffect struct {
	Tokens int
}

// TestPromptTokenRatios_estimate covers what the estimate rests on at
// each stage of a model's life: the four-byte rule before the
// provider has reported anything, the provider's own counts once it
// has, and the floor a refusal for length writes.
func TestPromptTokenRatios_estimate(t *testing.T) {
	tests := []promptEstimateCase{
		{
			name:        "no observation falls back on the four-byte rule",
			requestSize: 4000,
			want:        1000,
		},
		{
			name:        "an empty request costs nothing",
			requestSize: 0,
			want:        0,
		},
		{
			name:        "one observation sets the ratio",
			observe:     []promptTokenObservation{{bytes: 4000, tokens: 1400}},
			requestSize: 8000,
			want:        2800,
		},
		{
			name: "observations combine weighted by size",
			observe: []promptTokenObservation{
				{bytes: 1000, tokens: 400},
				{bytes: 9000, tokens: 1800},
			},
			requestSize: 10000,
			want:        2200,
		},
		{
			name:        "a refusal for length raises the estimate above the limit",
			observe:     []promptTokenObservation{{bytes: 4000, tokens: 1000}},
			reject:      []promptTokenRejection{{bytes: 4000, limit: 2000}},
			requestSize: 4000,
			want:        2001,
		},
		{
			name:        "a later refusal at a lower density leaves the floor alone",
			reject:      []promptTokenRejection{{bytes: 4000, limit: 2000}, {bytes: 8000, limit: 2000}},
			requestSize: 4000,
			want:        2001,
		},
		{
			name:        "an observation above the floor wins",
			observe:     []promptTokenObservation{{bytes: 1000, tokens: 900}},
			reject:      []promptTokenRejection{{bytes: 4000, limit: 2000}},
			requestSize: 4000,
			want:        3600,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ratios := newPromptTokenRatios()
			for _, observation := range tc.observe {
				ratios.observe("test/model", observation.bytes, observation.tokens)
			}
			for _, rejection := range tc.reject {
				ratios.reject("test/model", rejection.bytes, rejection.limit)
			}

			require.Equal(t, promptEstimateEffect{Tokens: tc.want}, promptEstimateEffect{
				Tokens: ratios.estimate("test/model", tc.requestSize),
			})
		})
	}
}

// TestPromptTokenRatios_are_kept_per_model proves one model's
// tokeniser does not decide another's estimate.
func TestPromptTokenRatios_are_kept_per_model(t *testing.T) {
	ratios := newPromptTokenRatios()
	ratios.observe("dense/model", 1000, 500)

	type perModelEffect struct {
		Dense int
		Other int
	}

	require.Equal(t, perModelEffect{Dense: 2000, Other: 1000}, perModelEffect{
		Dense: ratios.estimate("dense/model", 4000),
		Other: ratios.estimate("other/model", 4000),
	})
}

// TestOpenRouterClient_calibrates_the_estimate_from_reported_usage
// proves the estimate stops resting on the four-byte rule once the
// provider has reported a prompt-token count for the model. The count
// the fixture returns is deliberately far above the rule, which is
// the direction that matters: an under-estimate is what produces a
// refusal for length.
func TestOpenRouterClient_calibrates_the_estimate_from_reported_usage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(structuredChatResponse(`{"response":{"kind":"pass","reason":"ok"}}`))
	}))
	t.Cleanup(srv.Close)

	client := NewOpenRouterClient("test-key", srv.URL+"/api/v1", srv.Client())
	events := []protocol.IRCMessage{
		{Kind: protocol.KindPrivMsg, Source: domain.LegacyClientSource("alice"), Target: "#test", Body: "hi"},
	}
	rendered, err := client.RenderEventRequest("test/model", "", testSystemPrompt("prompt"), TurnHistory{}, events)
	require.NoError(t, err)

	before := client.EstimatePromptTokens("test/model", rendered)
	_, err = client.SendEvents(t.Context(), "test/model", "", testSystemPrompt("prompt"), TurnHistory{}, events)
	require.NoError(t, err)
	after := client.EstimatePromptTokens("test/model", rendered)

	type calibrationEffect struct {
		Before int
		After  int
	}

	// structuredChatResponse reports 10 prompt tokens, and the
	// rendered body is the same one SendEvents sent, so the observed
	// ratio answers 10 for it where the four-byte rule answered
	// len(body)/4.
	require.Equal(t, calibrationEffect{
		Before: (len(rendered.Body) + 3) / 4,
		After:  10,
	}, calibrationEffect{Before: before, After: after})
}

type lengthRefusalEffect struct {
	PromptTooLong bool
	Retryable     bool
}

// TestOpenRouterClient_marks_a_refusal_for_length covers how a
// provider says the prompt does not fit. OpenAI names the condition
// in the error code; OpenRouter puts it in the message. Either way
// the refusal has to reach the dispatch path as [ErrPromptTooLong]
// and must not be retried unchanged.
func TestOpenRouterClient_marks_a_refusal_for_length(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "OpenRouter names the limit in the message",
			body: `{"error":{"code":400,"message":"This endpoint's maximum context length is 8192 tokens. However, you requested about 9001 tokens."}}`,
		},
		{
			name: "OpenAI names the condition in the code",
			body: `{"error":{"code":"context_length_exceeded","message":"too long","type":"invalid_request_error"}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			client := NewOpenRouterClient("test-key", srv.URL+"/api/v1", srv.Client())
			_, err := client.SendEvents(
				t.Context(), "test/model", "", testSystemPrompt("prompt"), TurnHistory{},
				[]protocol.IRCMessage{{
					Kind: protocol.KindPrivMsg, Source: domain.LegacyClientSource("a"),
					Target: "#t", Body: "x",
				}},
			)
			require.Error(t, err)

			require.Equal(t, lengthRefusalEffect{PromptTooLong: true, Retryable: false}, lengthRefusalEffect{
				PromptTooLong: errors.Is(err, ErrPromptTooLong),
				Retryable:     Retryable(err),
			})
		})
	}
}

// TestOpenRouterClient_leaves_other_refusals_unmarked proves an
// ordinary bad request is not read as a length refusal, which would
// otherwise earn it a redispatch it cannot benefit from.
func TestOpenRouterClient_leaves_other_refusals_unmarked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"unknown model"}}`))
	}))
	t.Cleanup(srv.Close)

	client := NewOpenRouterClient("test-key", srv.URL+"/api/v1", srv.Client())
	_, err := client.SendEvents(
		t.Context(), "test/model", "", testSystemPrompt("prompt"), TurnHistory{},
		[]protocol.IRCMessage{{
			Kind: protocol.KindPrivMsg, Source: domain.LegacyClientSource("a"),
			Target: "#t", Body: "x",
		}},
	)
	require.Error(t, err)

	require.Equal(t, lengthRefusalEffect{PromptTooLong: false, Retryable: false}, lengthRefusalEffect{
		PromptTooLong: errors.Is(err, ErrPromptTooLong),
		Retryable:     Retryable(err),
	})
}
