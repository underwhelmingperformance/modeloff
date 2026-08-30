package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

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

var nicknameSchema = generateSchema[nicknameResponse]()

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
				Schema: nicknameSchema,
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
				resp, rawResp, err = c.chatCompletion(ctx, openai.ChatCompletionNewParams{ //nolint:bodyclose // SDK reads and closes the body.
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
					return &CompletionParseError{Target: "nickname", Err: err}
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

// personaProposal is used twice: [generateSchema] reflects it into the
// schema the request declares, and the response is decoded back into
// it.
//
// Only Description is read. A model generates its response a token at
// a time, each token conditioned on what it has already written, and a
// strict schema lets it write nothing but the fields the schema
// declares. The other six fields are where the model works out who this
// person is, and Description is declared last so that it is generated
// after them. Deleting the six as unused, or moving Description above
// them, would have the description generated first, from the prompt
// alone.
//
// The six fields are not stored. Storing them would keep a second
// account of the character beside the description, and the first
// accepted reflection would change the description and leave that
// account describing the instance as it was created.
type personaProposal struct {
	CaresAbout string `json:"cares_about" jsonschema_description:"What this person cares about, in a few words."`
	BotheredBy string `json:"bothered_by" jsonschema_description:"What reliably irritates them."`
	SeeksOut   string `json:"seeks_out" jsonschema_description:"The kind of exchange they go looking for."`
	Avoids     string `json:"avoids" jsonschema_description:"The kind of exchange they stay out of."`
	Tension    string `json:"tension" jsonschema_description:"Something in them that pulls two ways."`
	Anchor     string `json:"anchor" jsonschema_description:"A corner of their life, inside the terminal or outside it, that they mention when the talk turns idle."`

	Description string `json:"description" jsonschema_description:"A one-line description of the person, drawn from the fields above."`
}

var personaSchema = generateSchema[personaProposal]()

const personaGenerationPrompt = `Invent one regular participant in an IRC channel.

Answer the fields in order. Say what this person cares about, what irritates them, what they go looking for and what they stay out of, something in them that pulls two ways, and a corner of their life they mention when the talk turns idle. Only then write the one-line description, drawn from what you have just said.

Describe a plausible person with room for context-dependent behaviour. Not a role, a mascot, a catchphrase, or a single exaggerated trait. Do not prescribe a fixed speaking gimmick, a repeated phrase, an accent, a formatting habit, or a response to every situation. Do not mention AI systems or assistants.`

const personaPresentPrompt = `These characters are already here. Invent somebody none of them is:

%s`

// RejectedPersona is one description GeneratePersona already produced,
// with the reason it was turned down. The reason reaches the model as
// the operator wrote it, or as the validator's refusal.
type RejectedPersona struct {
	Description string
	Reason      string
}

// PersonaRequest is what the request shows the model besides the
// prompt.
type PersonaRequest struct {
	// Present holds the description of each character already
	// connected. The prompt asks for a character unlike every one of
	// them.
	Present []string

	// Rejected holds each description already turned down, with the
	// reason given for it.
	Rejected []RejectedPersona
}

func personaResponseFormat() openai.ChatCompletionNewParamsResponseFormatUnion {
	return openai.ChatCompletionNewParamsResponseFormatUnion{
		OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
			JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
				Name:   "persona",
				Schema: personaSchema,
				Strict: openai.Bool(true),
			},
		},
	}
}

// GeneratePersona asks a model to invent one character for an instance
// being added, and returns its description. Each entry in req.Rejected
// is sent back as an assistant message, wrapped in the object the
// schema asks for, followed by a user message giving the reason, so
// the model reads what it wrote and why it was turned down.
func (c *OpenRouterClient) GeneratePersona(
	ctx context.Context,
	smallModel domain.ModelID,
	req PersonaRequest,
) (string, error) {
	ctx, cancel := ensureDeadline(ctx, c.metaTimeout)
	defer cancel()

	logger := slog.Default().With(
		"component", "api.openrouter",
		"small_model", smallModel,
		"attempt", len(req.Rejected)+1,
	)

	var description string
	err := c.inSpan(ctx, "api.openrouter.generate_persona",
		[]attribute.KeyValue{attribute.String(observability.AttrModelID, string(smallModel))},
		func(ctx context.Context, span trace.Span) error {
			messages := []openai.ChatCompletionMessageParamUnion{
				openai.UserMessage(personaGenerationPrompt),
			}

			if len(req.Present) > 0 {
				messages = append(messages,
					openai.UserMessage(fmt.Sprintf(personaPresentPrompt, strings.Join(req.Present, "\n"))))
			}

			for _, rejected := range req.Rejected {
				messages = append(messages,
					openai.AssistantMessage(fmt.Sprintf(`{"description":%q}`, rejected.Description)),
					openai.UserMessage(rejected.Reason))
			}

			resp, rawResp, err := c.chatCompletion(ctx, openai.ChatCompletionNewParams{ //nolint:bodyclose // SDK reads and closes the body.
				Model:          shared.ChatModel(string(smallModel)),
				Messages:       messages,
				ResponseFormat: personaResponseFormat(),
			})
			if err != nil {
				markSpanError(span, observability.ErrorKindTransport, 0, err)
				logger.ErrorContext(ctx, "openrouter generate persona failed", "error", err)

				return err
			}

			if len(resp.Choices) == 0 {
				err := fmt.Errorf("generate persona: no choices in response")
				markSpanError(span, observability.ErrorKindInvalidResponse, 0, err)

				return err
			}

			choice := resp.Choices[0]

			if err := validateChoice(choice); err != nil {
				markSpanError(span, observability.ErrorKindInvalidResponse, 0, err)

				return err
			}

			var proposal personaProposal
			if err := json.Unmarshal([]byte(choice.Message.Content), &proposal); err != nil {
				markSpanError(span, observability.ErrorKindResponseParse, 0, err)

				return &CompletionParseError{Target: "persona", Err: err}
			}

			description = proposal.Description

			usage := usageFromResponse(resp.Usage)
			requestID := requestIDFromChatCompletion(resp, rawResp)
			usage.SetSpanAttributes(span, requestID)
			span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

			logger.InfoContext(ctx, "openrouter generate persona completed", "request_id", requestID)

			return nil
		})
	if err != nil {
		return "", err
	}

	return description, nil
}
