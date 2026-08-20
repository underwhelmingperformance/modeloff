package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"

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

// nicknamePattern mirrors the shape declared on nicknameResponse's
// `pattern`/`minLength`/`maxLength` JSON-schema tags. The schema
// asks a well-behaved provider to enforce this; GenerateNick
// re-checks it locally because the schema is a request, not a
// guarantee, and re-validates before trusting the result.
var nicknamePattern = regexp.MustCompile(`^[a-z0-9_-]{1,12}$`)

// maxNickFormatAttempts caps how many times GenerateNick asks the
// small model for a nick that matches nicknamePattern before giving
// up: the first response, plus one retry carrying the rejected nick
// as a follow-up turn.
const maxNickFormatAttempts = 2

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
// avoids repeating them, each with the fixed "already taken" wording;
// the caller's authoritative nick list is never sent.
// [OpenRouterClient.GenerateNickWithReasons] is the same generation
// with a distinct retry hint per rejection reason.
func (c *OpenRouterClient) GenerateNick(
	ctx context.Context,
	smallModel domain.ModelID,
	persona string,
	excludePreviousSuggestions []domain.Nick,
) (NicknameResult, error) {
	retries := make([]nickRetryHint, len(excludePreviousSuggestions))
	for i, rejected := range excludePreviousSuggestions {
		retries[i] = nickRetryHint{
			nick: rejected,
			hint: fmt.Sprintf("That nick is already taken. Suggest a different one. Avoid: %s", string(rejected)),
		}
	}

	return c.generateNick(ctx, smallModel, persona, retries)
}

// GenerateNickWithReasons implements [NickReasonGenerator]. It is the
// same generation as [OpenRouterClient.GenerateNick], except the
// retry hint for each rejected suggestion names the reason it was
// rejected, so a grammar failure ("must start with a letter or one
// of ...") reads differently from a plain collision ("already
// taken").
func (c *OpenRouterClient) GenerateNickWithReasons(
	ctx context.Context,
	smallModel domain.ModelID,
	persona string,
	excluded []RejectedNick,
) (NicknameResult, error) {
	retries := make([]nickRetryHint, len(excluded))
	for i, rejected := range excluded {
		retries[i] = nickRetryHint{
			nick: rejected.Nick,
			hint: fmt.Sprintf("%q was rejected: %s. Suggest a different one.", string(rejected.Nick), rejected.Reason),
		}
	}

	return c.generateNick(ctx, smallModel, persona, retries)
}

// nickRetryHint is one nickname suggestion GenerateNick already
// tried, together with the exact retry message to fold into the
// conversation for it.
type nickRetryHint struct {
	nick domain.Nick
	hint string
}

// generateNick is the shared implementation behind GenerateNick and
// GenerateNickWithReasons: they differ only in the wording of each
// retry's hint, which the caller has already resolved into retries.
func (c *OpenRouterClient) generateNick(
	ctx context.Context,
	smallModel domain.ModelID,
	persona string,
	retries []nickRetryHint,
) (NicknameResult, error) {
	ctx, cancel := ensureDeadline(ctx, c.metaTimeout)
	defer cancel()

	logger := slog.Default().With(
		"component", "api.openrouter",
		"small_model", smallModel,
		"attempt", len(retries)+1,
	)

	var result NicknameResult
	err := c.inSpan(ctx, "api.openrouter.generate_nick",
		[]attribute.KeyValue{attribute.String(observability.AttrModelID, string(smallModel))},
		func(ctx context.Context, span trace.Span) error {
			messages := []openai.ChatCompletionMessageParamUnion{
				openai.UserMessage(fmt.Sprintf(nicknamePrompt, persona)),
			}

			for _, retry := range retries {
				messages = append(messages,
					openai.AssistantMessage(fmt.Sprintf(`{"nick":%q}`, string(retry.nick))),
					openai.UserMessage(retry.hint),
				)
			}

			var (
				parsed  nicknameResponse
				resp    *openai.ChatCompletion
				rawResp *http.Response
			)

			for formatAttempt := 1; formatAttempt <= maxNickFormatAttempts; formatAttempt++ {
				var err error
				resp, rawResp, err = c.chatCompletion(ctx, smallModel, openai.ChatCompletionNewParams{ //nolint:bodyclose // SDK reads and closes the body.
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

				if err := json.Unmarshal([]byte(choice.Message.Content), &parsed); err != nil {
					markSpanError(span, observability.ErrorKindResponseParse, 0, err)
					return &completionParseError{target: "nickname", err: err}
				}

				if parsed.Nick == "" {
					err := fmt.Errorf("generate nick: schema-valid response carried an empty nick")
					markSpanError(span, observability.ErrorKindInvalidResponse, 0, err)
					return err
				}

				if nicknamePattern.MatchString(parsed.Nick) {
					break
				}

				logger.WarnContext(ctx, "generated nick violated the schema pattern",
					"nick", parsed.Nick,
					"format_attempt", formatAttempt,
				)

				if formatAttempt == maxNickFormatAttempts {
					err := fmt.Errorf("generate nick: %q does not match the required format after %d attempts", parsed.Nick, maxNickFormatAttempts)
					markSpanError(span, observability.ErrorKindInvalidResponse, 0, err)
					return err
				}

				messages = append(messages,
					openai.AssistantMessage(fmt.Sprintf(`{"nick":%q}`, parsed.Nick)),
					openai.UserMessage(fmt.Sprintf(
						"%q doesn't match the required format (lowercase letters, digits, underscore or hyphen, 1-12 characters). Suggest a different one.",
						parsed.Nick,
					)),
				)
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

			// A generated persona goes on to become the app's own
			// instruction in an instance's system prompt, so what
			// the small model returns is bounded here rather than
			// taken as written. One unusable persona does not spoil
			// the batch: the pool is drawn from whatever passed.
			personas = make([]domain.Persona, 0, len(wrapper.Personas))
			for _, p := range wrapper.Personas {
				if reason := domain.ValidatePersona(p.Description); reason != domain.PersonaAccepted {
					logger.WarnContext(ctx, "discarding generated persona",
						"persona_id", p.ID,
						"reason", reason,
					)

					continue
				}

				personas = append(personas, domain.Persona{
					ID:          p.ID,
					Description: p.Description,
					Origin:      domain.PersonaGenerated,
				})
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
