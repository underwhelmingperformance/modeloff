package modelclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
)

const (
	maxToolLoopTurns = 5
	passToolName     = "pass"
)

// turnOutcome bundles the result of a [runTurn] call plus the
// per-turn telemetry the dispatch span records.
type turnOutcome struct {
	toolTurnCount int
	passReason    string
}

// runTurn drives a single dispatch turn end-to-end: the initial
// `SendEvents` call, the tool loop that executes any model-requested
// tools and feeds their results back, and termination when the model
// stops calling tools.
//
// The model's whole conversational surface is its tool calls — `msg`
// and `me` post chat traffic, `pass` records explicit silence-with-
// reason, memory and channel-management tools do their respective
// work. A turn that emits no tool calls (the model genuinely has
// nothing to do) is implicit silence; the loop exits without an API
// retry.
//
// `pass` is mutually exclusive with every other tool. If the model
// emits it alongside something else, every call in that turn is
// rejected back to the model with an explanation and the loop
// continues so the model can correct.
//
// Upstream-side silence (refusal, content filter) short-circuits
// the turn and surfaces a stable pass reason on the span.
func runTurn(
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
	pacer *Pacer,
) (turnOutcome, error) {
	definitions := registry.Definitions()

	result, err := apiClient.SendEvents(ctx, inst.ModelID, inst.ID(), prompt, history, events, definitions...)
	if err != nil {
		if outcome, ok := classifyUpstreamSilence(err); ok {
			return outcome, nil
		}

		return turnOutcome{}, err
	}

	outcome := turnOutcome{}

	for range maxToolLoopTurns {
		if len(result.PendingToolCalls) == 0 {
			outcome.passReason = observability.PassReasonModelPass
			return outcome, nil
		}

		if registry == nil {
			outcome.passReason = observability.PassReasonModelPass
			return outcome, nil
		}

		toolResults, sawPass := executeTools(ctx, sess, ToolContext{
			Session: sess,
			Actor:   inst,
			Channel: channelName,
			Client:  caller,
		}, registry, result.PendingToolCalls, pacer)
		outcome.toolTurnCount++

		if sawPass {
			outcome.passReason = observability.PassReasonModelPass
			return outcome, nil
		}

		result, err = apiClient.ContinueWithToolResults(ctx, result.Conversation, toolResults, definitions...)
		if err != nil {
			if next, ok := classifyUpstreamSilence(err); ok {
				next.toolTurnCount = outcome.toolTurnCount
				return next, nil
			}

			return outcome, err
		}
	}

	// The model kept emitting tool calls past the loop bound — the
	// session-side analogue of the old structured-reply retry
	// exhaustion. The final batch of tool calls has already executed;
	// we just don't ask the model for more.
	outcome.passReason = observability.PassReasonToolLoopExhausted
	return outcome, nil
}

// classifyUpstreamSilence maps known upstream-side failure modes
// (refusal, content filter) to a stable pass reason. Anything else
// propagates as a transport / parse error.
func classifyUpstreamSilence(err error) (turnOutcome, bool) {
	if _, ok := errors.AsType[*api.ErrModelRefused](err); ok {
		return turnOutcome{passReason: observability.PassReasonModelRefused}, true
	}

	if errors.Is(err, api.ErrContentFiltered) {
		return turnOutcome{passReason: observability.PassReasonContentFiltered}, true
	}

	return turnOutcome{}, false
}

// executeTools runs pending tool calls and returns the results to
// feed back to the model. The second return value reports whether
// the model called the `pass` tool — that ends the turn after the
// current batch executes (or, if mixed with other tools, results
// in a turn-wide rejection per the pass-exclusivity rule). The
// rich reason text the model supplied to `pass` lands on the
// per-call execute_tool span as `pass.reason`; the dispatch-turn
// span carries the stable enum.
func executeTools(
	ctx context.Context,
	sess Session,
	toolCtx ToolContext,
	registry *ToolRegistry,
	calls []api.PendingToolCall,
	pacer *Pacer,
) ([]api.ToolResult, bool) {
	if reject := rejectMixedPass(calls); reject != nil {
		return reject, true
	}

	results := make([]api.ToolResult, 0, len(calls))
	tracer := sess.TracerProvider().Tracer("github.com/laney/modeloff/internal/modelclient")

	var sawPass bool

	for _, call := range calls {
		toolName := call.Name

		callCtx, callSpan := tracer.Start(ctx, "modelclient.execute_tool",
			trace.WithAttributes(
				attribute.String(observability.AttrOperation, "modelclient.execute_tool"),
				attribute.String("tool.name", toolName),
			),
		)

		if body, ok := pacingBody(toolName, call.Args); ok {
			pacer.Wait(callCtx, body)
		}

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

		if toolName == passToolName {
			sawPass = true
		}

		data, _ := json.Marshal(payload)
		results = append(results, api.ToolResult{ToolCallID: call.ID, Content: string(data)})
	}

	return results, sawPass
}

// rejectMixedPass enforces the rule that `pass` is mutually
// exclusive with every other tool in the same turn. When violated,
// every call (including the pass itself) receives an error result
// explaining the rule. The caller treats the rejection as a turn-
// ending silence so the model gets a single retry opportunity — the
// next turn carries the rejection results as tool-role messages and
// the model can issue a corrected call.
func rejectMixedPass(calls []api.PendingToolCall) []api.ToolResult {
	hasPass := false
	hasOther := false

	for _, call := range calls {
		if call.Name == passToolName {
			hasPass = true
			continue
		}

		hasOther = true
	}

	if !hasPass || !hasOther {
		return nil
	}

	payload := ToolResultPayload{
		OK:    false,
		Error: "pass cannot be combined with any other tool in the same turn — call pass on its own, or omit it",
	}

	data, _ := json.Marshal(payload)
	rejected := make([]api.ToolResult, 0, len(calls))
	for _, call := range calls {
		rejected = append(rejected, api.ToolResult{ToolCallID: call.ID, Content: string(data)})
	}

	return rejected
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
