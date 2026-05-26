package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

// nicknameResponse is the structured output the model returns. The
// schema enforces shape (length and allowed characters) so callers do
// not need to sanitise free-form text.
type nicknameResponse struct {
	// The pattern must not contain a comma: the invopop/jsonschema tag parser splits on it.
	Nick string `json:"nick" jsonschema:"minLength=1,maxLength=12,pattern=^[a-z0-9_-]+$" jsonschema_description:"Exactly one IRC nickname suggestion."`
}

var nicknameSchemaMap = generateSchema[nicknameResponse]()

func nicknameResponseFormat() openai.ChatCompletionNewParamsResponseFormatUnion {
	return openai.ChatCompletionNewParamsResponseFormatUnion{
		OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
			JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
				Name:   "nickname_response",
				Schema: nicknameSchemaMap,
				Strict: openai.Bool(true),
			},
		},
	}
}

const nicknamePrompt = `Generate exactly one short, fun IRC-style nickname for an IRC regular.

Constraints:
- return JSON only and match the schema exactly
- do not explain the choice
- produce one nickname, not a list
- do not use words based on assistant names, model names, or generic AI terms unless the persona strongly implies them
- prefer something that sounds like a handle a human would pick on IRC
- do not treat the persona as the person's whole identity
- avoid simply turning the persona description into a literal label
- prefer something a real user might have chosen years ago: suggest habits, interests, in-jokes, tone, or history rather than job-title summaries
- a slightly indirect or playful nick is better than an obvious descriptor
- prefer nicks that feel personally chosen and lived-in
- avoid obviously symbolic or overly neat compositions

Persona: %s`

// GenerateNick asks a model to suggest one IRC-style nickname guided
// by the persona description. Rejected suggestions from prior calls
// are folded into the conversation as a follow-up turn so the model
// avoids repeating them; the caller's authoritative nick list is
// never sent.
func (c *OpenRouterClient) GenerateNick(
	ctx context.Context,
	smallModel domain.ModelID,
	persona string,
	excludePreviousSuggestions []domain.Nick,
) (NicknameResult, error) {
	ctx, cancel := ensureDeadline(ctx, c.metaTimeout)
	defer cancel()

	logger := slog.Default().With(
		"component", "api.openrouter",
		"small_model", smallModel,
		"attempt", len(excludePreviousSuggestions)+1,
	)

	var result NicknameResult
	err := c.inSpan(ctx, "api.openrouter.generate_nick",
		[]attribute.KeyValue{attribute.String(observability.AttrModelID, string(smallModel))},
		func(ctx context.Context, span trace.Span) error {
			messages := []openai.ChatCompletionMessageParamUnion{
				openai.UserMessage(fmt.Sprintf(nicknamePrompt, persona)),
			}

			for _, rejected := range excludePreviousSuggestions {
				messages = append(messages,
					openai.AssistantMessage(fmt.Sprintf(`{"nick":%q}`, string(rejected))),
					openai.UserMessage(fmt.Sprintf(
						"That nick is already taken. Suggest a different one. Avoid: %s",
						string(rejected),
					)),
				)
			}

			resp, rawResp, err := c.chatCompletion(ctx, smallModel, openai.ChatCompletionNewParams{ //nolint:bodyclose // SDK reads and closes the body.
				Model:          shared.ChatModel(string(smallModel)),
				Messages:       messages,
				ResponseFormat: nicknameResponseFormat(),
			})
			if err != nil {
				markSpanError(span, observability.ErrorKindTransport, 0, err)
				logger.ErrorContext(ctx, "openrouter generate nick failed", "error", err)
				return err
			}

			if len(resp.Choices) == 0 {
				err := fmt.Errorf("generate nick: no choices in response")
				markSpanError(span, observability.ErrorKindInvalidResponse, 0, err)
				return err
			}

			choice := resp.Choices[0]

			if err := validateChoice(choice); err != nil {
				markSpanError(span, observability.ErrorKindInvalidResponse, 0, err)
				return err
			}

			var parsed nicknameResponse
			if err := json.Unmarshal([]byte(choice.Message.Content), &parsed); err != nil {
				markSpanError(span, observability.ErrorKindResponseParse, 0, err)
				return &completionParseError{target: "nickname", err: err}
			}

			if parsed.Nick == "" {
				err := fmt.Errorf("generate nick: schema-valid response carried an empty nick")
				markSpanError(span, observability.ErrorKindInvalidResponse, 0, err)
				return err
			}

			result = NicknameResult{
				Nick:      domain.Nick(parsed.Nick),
				RequestID: requestIDFromChatCompletion(resp, rawResp),
				Usage:     usageFromResponse(resp.Usage),
			}

			result.Usage.SetSpanAttributes(span, result.RequestID)
			span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

			logger.InfoContext(ctx, "openrouter generate nick completed",
				"request_id", result.RequestID,
				"nick", parsed.Nick,
			)

			return nil
		})
	if err != nil {
		return NicknameResult{}, err
	}

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
	ctx, cancel := ensureDeadline(ctx, c.metaTimeout)
	defer cancel()

	logger := slog.Default().With("component", "api.openrouter", "model_id", smallModel)

	var personas []domain.Persona
	err := c.inSpan(ctx, "api.openrouter.generate_personas",
		[]attribute.KeyValue{attribute.String(observability.AttrModelID, string(smallModel))},
		func(ctx context.Context, span trace.Span) error {
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
				return err
			}

			if len(resp.Choices) == 0 {
				err := fmt.Errorf("generate personas: no choices in response")
				markSpanError(span, observability.ErrorKindInvalidResponse, 0, err)
				return err
			}

			choice := resp.Choices[0]

			if err := validateChoice(choice); err != nil {
				markSpanError(span, observability.ErrorKindInvalidResponse, 0, err)
				return err
			}

			var wrapper personaListWrapper
			if err := json.Unmarshal([]byte(choice.Message.Content), &wrapper); err != nil {
				markSpanError(span, observability.ErrorKindResponseParse, 0, err)
				return &completionParseError{target: "persona list", err: err}
			}

			personas = make([]domain.Persona, len(wrapper.Personas))
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

			return nil
		})
	if err != nil {
		return nil, err
	}

	return personas, nil
}
