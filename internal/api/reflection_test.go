package api

import (
	"encoding/json"
	"errors"
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

type reflectionWireFunction struct {
	Name   string `json:"name"`
	Strict bool   `json:"strict"`
}

type reflectionWireTool struct {
	Type     string                 `json:"type"`
	Function reflectionWireFunction `json:"function"`
}

type reflectionWireRequest struct {
	Model               string                  `json:"model"`
	Messages            []reflectionWireMessage `json:"messages"`
	Tools               []reflectionWireTool    `json:"tools"`
	ParallelToolCalls   bool                    `json:"parallel_tool_calls"`
	PromptCacheKey      string                  `json:"prompt_cache_key"`
	MaxCompletionTokens int64                   `json:"max_completion_tokens"`
	ResponseFormat      reflectionWireFormat    `json:"response_format"`
}

type reflectionExplorationEffect struct {
	Request   reflectionWireRequest
	ToolCalls []PendingToolCall
}

type reflectionProposalEffect struct {
	Request reflectionWireRequest
	Result  ReflectionResult
}

// reflectionServer answers each request from responses in order and
// records what it was sent. A reflection is several round trips, so a
// test drives one by scripting the whole run.
type reflectionServer struct {
	requests []reflectionWireRequest
}

func newReflectionServer(
	t *testing.T,
	responses ...map[string]any,
) (*reflectionServer, *OpenRouterClient) {
	t.Helper()

	recorder := &reflectionServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		require.NoError(t, readErr)
		var received reflectionWireRequest
		require.NoError(t, json.Unmarshal(body, &received))
		index := len(recorder.requests)
		recorder.requests = append(recorder.requests, received)
		require.Less(t, index, len(responses), "unscripted provider request")
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(responses[index]))
	}))
	t.Cleanup(server.Close)

	return recorder, NewOpenRouterClient("test-key", server.URL+"/api/v1", server.Client())
}

func reflectionTestInput() ReflectionInput {
	return ReflectionInput{
		Description:  "careful and curious, and slow to be convinced",
		Baseline:     "careful and curious",
		Participants: []ReflectionParticipant{{Token: "p1", Nick: "alice"}},
		Experiences:  []ReflectionInputExperience{},
		Amendments:   []ReflectionInputAmendment{},
		Events: []ReflectionInputEvent{{
			Sequence: 12, WindowKind: ReflectionWindowChannel, Window: "#dev",
			Participant: "p1",
			Message: protocol.IRCMessage{
				Kind:   protocol.KindPrivMsg,
				Source: domain.LegacyClientSource("alice"),
				Target: "#dev", Body: "the reproduction is in the issue",
			},
			Substantive: true,
		}},
	}
}

func reflectionTestProposal() ReflectionProposal {
	return ReflectionProposal{
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
		Persona: ReflectionPersonaProposal{
			Description:  "careful and curious, and readier to be convinced by working evidence",
			EvidenceKeys: []string{"alice-reproduction"},
		},
		Retract: []string{},
	}
}

func reflectionTestUsage() Usage {
	return Usage{
		PromptTokens: 10, CompletionTokens: 5,
		TotalTokens: 15, CostCredits: 0.125,
	}
}

func recallHistoryTool() ToolDefinition {
	return ToolDefinition{
		Name:        "recall_history",
		Description: "Read further back through what you saw.",
		Parameters: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"required":             []string{},
			"additionalProperties": false,
		},
	}
}

func TestOpenRouterClient_ReflectPersona_explores_with_tools_and_no_schema(t *testing.T) {
	input := reflectionTestInput()
	server, client := newReflectionServer(t, toolCallResponse(toolCallFixture{
		id: "call_1", name: "recall_history", args: `{"window":"#dev","before":0,"limit":10}`,
	}))

	exploration, err := client.ReflectPersona(
		t.Context(), "test/reflection", "inst-botty", input, recallHistoryTool(),
	)
	require.NoError(t, err)
	encodedInput, err := json.Marshal(input)
	require.NoError(t, err)

	require.Equal(t, reflectionExplorationEffect{
		Request: reflectionWireRequest{
			Model: "test/reflection",
			Messages: []reflectionWireMessage{
				{Role: "system", Content: reflectionPrompt},
				{Role: "user", Content: string(encodedInput)},
			},
			Tools: []reflectionWireTool{{
				Type: "function",
				Function: reflectionWireFunction{
					Name: "recall_history", Strict: true,
				},
			}},
			ParallelToolCalls:   false,
			PromptCacheKey:      "inst-botty",
			MaxCompletionTokens: reflectionExplorationTokens,
		},
		ToolCalls: []PendingToolCall{{
			ID: "call_1", Name: "recall_history",
			Args: json.RawMessage(`{"window":"#dev","before":0,"limit":10}`),
		}},
	}, reflectionExplorationEffect{
		Request:   server.requests[0],
		ToolCalls: exploration.PendingToolCalls,
	})
	require.NotNil(t, exploration.Conversation)
}

func TestOpenRouterClient_ProposeReflection_sets_the_schema_as_a_response_format(t *testing.T) {
	proposal := reflectionTestProposal()
	encodedProposal, err := json.Marshal(proposal)
	require.NoError(t, err)
	server, client := newReflectionServer(t,
		structuredChatResponse("looking at what happened"),
		structuredChatResponse(string(encodedProposal)),
	)

	exploration, err := client.ReflectPersona(
		t.Context(), "test/reflection", "inst-botty", reflectionTestInput(),
		recallHistoryTool(),
	)
	require.NoError(t, err)
	result, err := client.ProposeReflection(
		t.Context(), exploration.Conversation, nil, WithStructuredOutput,
	)
	require.NoError(t, err)

	proposalRequest := server.requests[1]
	require.Equal(t, reflectionProposalEffect{
		Request: reflectionWireRequest{
			Model:               "test/reflection",
			Messages:            proposalRequest.Messages,
			Tools:               []reflectionWireTool{},
			ParallelToolCalls:   false,
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
			Usage: reflectionTestUsage(),
		},
	}, reflectionProposalEffect{Request: proposalRequest, Result: result})
	require.Equal(t, reflectionWireMessage{
		Role: "user", Content: reflectionProposalPrompt,
	}, proposalRequest.Messages[len(proposalRequest.Messages)-1])
}

func TestOpenRouterClient_ProposeReflection_carries_the_schema_in_the_prompt(t *testing.T) {
	proposal := reflectionTestProposal()
	encodedProposal, err := json.Marshal(proposal)
	require.NoError(t, err)
	server, client := newReflectionServer(t,
		structuredChatResponse("looking at what happened"),
		structuredChatResponse(string(encodedProposal)),
	)

	exploration, err := client.ReflectPersona(
		t.Context(), "test/reflection", "inst-botty", reflectionTestInput(),
	)
	require.NoError(t, err)
	result, err := client.ProposeReflection(
		t.Context(), exploration.Conversation, nil, WithoutStructuredOutput,
	)
	require.NoError(t, err)

	proposalRequest := server.requests[1]
	require.Equal(t, reflectionProposalEffect{
		Request: reflectionWireRequest{
			Model:               "test/reflection",
			Messages:            proposalRequest.Messages,
			Tools:               []reflectionWireTool{},
			ParallelToolCalls:   false,
			PromptCacheKey:      "inst-botty",
			MaxCompletionTokens: reflectionCompletionTokens,
		},
		Result: ReflectionResult{
			Proposal: proposal, RequestID: "chatcmpl_test",
			Usage: reflectionTestUsage(),
		},
	}, reflectionProposalEffect{Request: proposalRequest, Result: result})
	require.Equal(t, reflectionWireMessage{
		Role:    "user",
		Content: reflectionProposalPrompt + "\n\nThe schema:\n" + reflectionSchemaJSON,
	}, proposalRequest.Messages[len(proposalRequest.Messages)-1])
}

type reflectionDecodeCase struct {
	Name    string
	Content string
	Want    ReflectionProposal
}

func TestDecodeReflectionProposal_reads_a_proposal_a_model_wrapped(t *testing.T) {
	proposal := reflectionTestProposal()
	encoded, err := json.Marshal(proposal)
	require.NoError(t, err)
	cases := []reflectionDecodeCase{
		{Name: "bare object", Content: string(encoded), Want: proposal},
		{
			Name:    "fenced block",
			Content: "```json\n" + string(encoded) + "\n```",
			Want:    proposal,
		},
		{
			Name:    "prose either side",
			Content: "Here is what I make of it:\n" + string(encoded) + "\nThat is all.",
			Want:    proposal,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			got, err := decodeReflectionProposal(testCase.Content)
			require.NoError(t, err)
			require.Equal(t, testCase.Want, got)
		})
	}
}

func TestDecodeReflectionProposal_refuses_a_response_holding_no_object(t *testing.T) {
	got, err := decodeReflectionProposal("I would rather not say.")

	require.Equal(t, ReflectionProposal{}, got)
	var parseErr *CompletionParseError
	require.ErrorAs(t, err, &parseErr)
	require.Equal(t, &CompletionParseError{
		Target: reflectionParseTarget, Err: errNoJSONObject,
	}, parseErr)
}

// reflectionRefusalEffect is what the decoder answered for a response
// that is JSON but is not a proposal.
type reflectionRefusalEffect struct {
	Proposal ReflectionProposal
	Missing  bool
}

// TestDecodeReflectionProposal_refuses_a_response_that_is_not_a_proposal
// pins that a reply which parses into nothing is refused rather than read
// as a considered no-change result.
//
// The schema is requested only where the model supports one, so the
// decoder is what tells a proposal apart from anything else that happens
// to be JSON. Accepting a zero proposal would advance the checkpoint and
// discard the pending events on the strength of a reply that never
// answered the question.
func TestDecodeReflectionProposal_refuses_a_response_that_is_not_a_proposal(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{name: "an answer to another question", content: `{"answer":"nothing changed"}`},
		{name: "an empty object", content: `{}`},
		{
			name:    "a proposal missing one field",
			content: `{"experiences":[],"amendments":[],"retract_amendments":[]}`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := decodeReflectionProposal(testCase.content)

			require.Equal(t, reflectionRefusalEffect{Missing: true}, reflectionRefusalEffect{
				Proposal: got, Missing: errors.Is(err, errMissingProposalField),
			})
		})
	}
}

// TestDecodeReflectionProposal_reads_a_proposal_carrying_an_extra_field
// pins that a model adding a field of its own is still understood. The
// required fields are what make a proposal recognisable; the rest of the
// object is not the decoder's business.
func TestDecodeReflectionProposal_reads_a_proposal_carrying_an_extra_field(t *testing.T) {
	proposal := reflectionTestProposal()
	encoded, err := json.Marshal(proposal)
	require.NoError(t, err)
	withExtra := `{"reasoning":"I thought about it",` + string(encoded)[1:]

	got, err := decodeReflectionProposal(withExtra)

	require.NoError(t, err)
	require.Equal(t, proposal, got)
}
