package modelclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
)

const (
	maxNewlineRetries            = 2
	maxToolLoopTurns             = 5
	silenceReasonContentFiltered = "content filtered"
	silenceReasonNewlineRetries  = "response contained newlines after retries"
	silenceReasonFormatRetries   = "response contained invalid formatting after retries"
)

// sendOutcome bundles the result of a [sendWithRetry] call plus
// the per-attempt telemetry the dispatch turn records on its span.
type sendOutcome struct {
	result        api.CompletionResult
	retryCount    int
	toolTurnCount int
	passReason    string
}

// sendWithRetry sends events to a model and retries if the response
// contains newlines in any message body. After maxNewlineRetries
// retries, a silent pass is returned. Each attempt may involve
// multiple API turns if the model uses memory tools.
func sendWithRetry(
	ctx context.Context,
	apiClient api.Client,
	sess Session,
	caller protocol.Client,
	inst *domain.Instance,
	channelName domain.ChannelName,
	prompt string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
	registry *ToolRegistry,
) (sendOutcome, error) {
	lastRetryReason := silenceReasonNewlineRetries

	for attempt := range maxNewlineRetries + 1 {
		result, toolTurnCount, err := sendWithToolLoop(ctx, apiClient, sess, caller, inst, channelName, prompt, history, events, registry)
		if err != nil {
			if refused, ok := errors.AsType[*api.ErrModelRefused](err); ok {
				return sendOutcome{
					result: api.CompletionResult{
						Response: protocol.ModelResponse{
							Kind:   protocol.ResponseSilence,
							Reason: refused.Reason,
						},
					},
					retryCount:    attempt,
					toolTurnCount: toolTurnCount,
					passReason:    observability.PassReasonModelRefused,
				}, nil
			}

			if errors.Is(err, api.ErrContentFiltered) {
				return sendOutcome{
					result: api.CompletionResult{
						Response: protocol.ModelResponse{
							Kind:   protocol.ResponseSilence,
							Reason: silenceReasonContentFiltered,
						},
					},
					retryCount:    attempt,
					toolTurnCount: toolTurnCount,
					passReason:    observability.PassReasonContentFiltered,
				}, nil
			}

			return sendOutcome{}, err
		}

		if result.Response.Kind != protocol.ResponseReply || len(result.Response.Messages) == 0 {
			return sendOutcome{
				result:        result,
				retryCount:    attempt,
				toolTurnCount: toolTurnCount,
				passReason:    passReasonForResponse(result.Response),
			}, nil
		}

		hasNewlines := containsNewlines(result.Response)
		hasInvalidFormatting := containsInvalidFormatting(result.Response)
		if !hasNewlines && !hasInvalidFormatting {
			return sendOutcome{
				result:        result,
				retryCount:    attempt,
				toolTurnCount: toolTurnCount,
			}, nil
		}

		if hasInvalidFormatting {
			lastRetryReason = silenceReasonFormatRetries
		} else {
			lastRetryReason = silenceReasonNewlineRetries
		}
	}

	resp := protocol.ModelResponse{
		Kind:   protocol.ResponseSilence,
		Reason: lastRetryReason,
	}

	return sendOutcome{
		result:     api.CompletionResult{Response: resp},
		retryCount: maxNewlineRetries,
		passReason: passReasonForResponse(resp),
	}, nil
}

// sendWithToolLoop sends events to a model and handles tool calls in a
// loop until the model replies, passes, or exceeds the tool turn limit.
func sendWithToolLoop(
	ctx context.Context,
	apiClient api.Client,
	sess Session,
	caller protocol.Client,
	inst *domain.Instance,
	channelName domain.ChannelName,
	prompt string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
	registry *ToolRegistry,
) (api.CompletionResult, int, error) {
	definitions := registry.Definitions()

	result, err := apiClient.SendEvents(ctx, inst.ModelID, inst.ID(), prompt, history, events, definitions...)
	if err != nil {
		return api.CompletionResult{}, 0, err
	}

	toolTurnCount := 0
	for range maxToolLoopTurns {

		if len(result.PendingToolCalls) == 0 {
			return result, toolTurnCount, nil
		}

		if registry == nil {
			return result, toolTurnCount, nil
		}

		toolResults := executeTools(ctx, sess, ToolContext{
			Session: sess,
			Actor:   inst,
			Channel: channelName,
			Client:  caller,
		}, registry, result.PendingToolCalls)
		toolTurnCount++

		result, err = apiClient.ContinueWithToolResults(ctx, result.Conversation, toolResults, definitions...)
		if err != nil {
			return api.CompletionResult{}, toolTurnCount, err
		}
	}

	return result, toolTurnCount, nil
}

// executeTools runs pending tool calls and returns the results to feed
// back to the model.
func executeTools(
	ctx context.Context,
	sess Session,
	toolCtx ToolContext,
	registry *ToolRegistry,
	calls []api.PendingToolCall,
) []api.ToolResult {
	results := make([]api.ToolResult, 0, len(calls))
	tracer := sess.TracerProvider().Tracer("github.com/laney/modeloff/internal/modelclient")

	for _, call := range calls {
		toolName := call.Name

		callCtx, callSpan := tracer.Start(ctx, "modelclient.execute_tool",
			trace.WithAttributes(
				attribute.String(observability.AttrOperation, "modelclient.execute_tool"),
				attribute.String("tool.name", toolName),
			),
		)

		payload := ToolResultPayload{
			OK:    false,
			Error: fmt.Sprintf("unknown tool %q", toolName),
		}

		if spec, ok := registry.Find(toolName); ok {
			nextPayload, err := spec.Execute(callCtx, toolCtx, call.Args)
			if err != nil {
				payload = ToolResultPayload{OK: false, Error: err.Error()}
			} else {
				payload = nextPayload
			}
		}

		if payload.OK {
			callSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
		} else {
			callSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
			callSpan.SetStatus(codes.Error, payload.Error)
		}

		callSpan.End()

		data, _ := json.Marshal(payload)
		results = append(results, api.ToolResult{ToolCallID: call.ID, Content: string(data)})
	}

	return results
}

func passReasonForResponse(response protocol.ModelResponse) string {
	if response.Kind != protocol.ResponseSilence {
		return ""
	}

	switch response.Reason {
	case silenceReasonContentFiltered:
		return observability.PassReasonContentFiltered
	case silenceReasonNewlineRetries:
		return observability.PassReasonNewlineRetryExhausted
	case silenceReasonFormatRetries:
		return observability.PassReasonFormatRetryExhausted
	default:
		return observability.PassReasonModelPass
	}
}

// containsNewlines reports whether any reply part body contains a
// newline after trimming.
func containsNewlines(resp protocol.ModelResponse) bool {
	for _, part := range resp.Messages {
		if strings.Contains(strings.TrimSpace(part.Body), "\n") {
			return true
		}
	}

	return false
}

func containsInvalidFormatting(resp protocol.ModelResponse) bool {
	for _, part := range resp.Messages {
		if err := protocol.ValidateReplyPart(part); err != nil {
			return true
		}
	}

	return false
}

// classifyEnsureModelError maps the errors produced by
// `session.EnsureStructuredOutputModel` to the appropriate observability
// error kind. The cached short-circuit sentinels reflect session-layer
// state that forbade the call before any upstream attempt.
// `domain.UnsupportedModelError` reflects a user-supplied model ID
// the catalogue does not include — fixable by the user, not
// infrastructure. Everything else is wrapped around a real upstream
// attempt and stays as `ErrorKindDispatch`.
func classifyEnsureModelError(err error) string {
	if errors.Is(err, ErrModelListUnavailable) || errors.Is(err, ErrNoAPIKey) {
		return observability.ErrorKindClientState
	}

	if _, ok := errors.AsType[domain.UnsupportedModelError](err); ok {
		return observability.ErrorKindValidation
	}

	return observability.ErrorKindDispatch
}

func setSpanError(span trace.Span, err error, errorKind string) {
	span.SetAttributes(
		attribute.String(observability.AttrResult, observability.ResultError),
		attribute.String(observability.AttrErrorKind, errorKind),
	)
	span.SetStatus(codes.Error, err.Error())
}
