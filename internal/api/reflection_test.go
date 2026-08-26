package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

type reflectionWireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type reflectionWireSchema struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

type reflectionWireFormat struct {
	Type       string               `json:"type"`
	JSONSchema reflectionWireSchema `json:"json_schema"`
}

type reflectionWireRequest struct {
	Model               string                  `json:"model"`
	Messages            []reflectionWireMessage `json:"messages"`
	Tools               []any                   `json:"tools"`
	ParallelToolCalls   bool                    `json:"parallel_tool_calls"`
	PromptCacheKey      string                  `json:"prompt_cache_key"`
	MaxCompletionTokens int64                   `json:"max_completion_tokens"`
	ResponseFormat      reflectionWireFormat    `json:"response_format"`
}

type reflectionProviderEffect struct {
	Request reflectionWireRequest
	Result  ReflectionResult
}

func TestOpenRouterClient_ReflectPersona_uses_strict_structured_output(t *testing.T) {
	input := ReflectionInput{
		Baseline:     "careful and curious",
		Participants: []ReflectionParticipant{{Token: "p1", Nick: "alice"}},
		Experiences:  []ReflectionInputExperience{},
		Amendments:   []ReflectionInputAmendment{},
		Events: []ReflectionInputEvent{{
			Sequence: 12, WindowKind: domain.KindChannel, Window: "#dev",
			Participant: "p1",
			Message: protocol.IRCMessage{
				Kind:   protocol.KindPrivMsg,
				Source: domain.LegacyClientSource("alice"),
				Target: "#dev", Body: "the reproduction is in the issue",
			},
			Substantive: true,
		}},
	}
	proposal := ReflectionProposal{
		Experiences: []ReflectionExperienceProposal{{
			Key: "alice-reproduction", Kind: domain.ExperienceRelationship,
			Summary: "Alice supplied a reproduction for a technical correction.",
			Subject: "p1", Confidence: domain.ConfidenceHigh,
			Sources: []domain.ReflectionSequence{12},
		}},
		Amendments: []ReflectionAmendmentProposal{{
			Scope: domain.AmendmentRelationship, Counterpart: "p1",
			Tendency:     "Usually trusts Alice's corrections when they include a reproduction.",
			Confidence:   domain.ConfidenceMedium,
			EvidenceKeys: []string{"alice-reproduction"},
		}},
		Retract: []string{},
	}
	response, err := json.Marshal(proposal)
	require.NoError(t, err)

	var received reflectionWireRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		require.NoError(t, readErr)
		require.NoError(t, json.Unmarshal(body, &received))
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(structuredChatResponse(string(response))))
	}))
	t.Cleanup(server.Close)
	client := NewOpenRouterClient("test-key", server.URL+"/api/v1", server.Client())

	result, err := client.ReflectPersona(
		t.Context(), "test/reflection", "inst-botty", input,
	)
	require.NoError(t, err)
	encodedInput, err := json.Marshal(input)
	require.NoError(t, err)

	require.Equal(t, reflectionProviderEffect{
		Request: reflectionWireRequest{
			Model: "test/reflection",
			Messages: []reflectionWireMessage{
				{Role: "system", Content: reflectionPrompt},
				{Role: "user", Content: string(encodedInput)},
			},
			Tools: []any{}, ParallelToolCalls: false,
			PromptCacheKey:      "inst-botty",
			MaxCompletionTokens: reflectionCompletionTokens,
			ResponseFormat: reflectionWireFormat{
				Type: "json_schema",
				JSONSchema: reflectionWireSchema{
					Name: "persona_reflection", Strict: true,
					Schema: reflectionSchema,
				},
			},
		},
		Result: ReflectionResult{
			Proposal: proposal, RequestID: "chatcmpl_test",
			Usage: Usage{
				PromptTokens: 10, CompletionTokens: 5,
				TotalTokens: 15, CostCredits: 0.125,
			},
		},
	}, reflectionProviderEffect{Request: received, Result: result})
}
