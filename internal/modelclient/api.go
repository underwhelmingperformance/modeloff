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

const maxToolLoopTurns = 5

// terminalTool is the tool that ended a turn. Most tools leave the
// turn open — the model gets their results back and decides what to
// do next — so the zero value is [terminalNone] and the two named
// members are the whole set that closes it.
type terminalTool string

const (
	// terminalNone is a batch of tools that all left the turn open.
	terminalNone terminalTool = ""

	// terminalPass is the model declining to say anything. The turn
	// is over because it said so.
	terminalPass terminalTool = "pass"

	// terminalQuit is the model ending its own connection. The turn
	// is over because the client issuing it no longer exists.
	terminalQuit terminalTool = "quit"
)

// terminalToolFor classifies a tool name. Anything the turn survives
// is [terminalNone].
func terminalToolFor(name string) terminalTool {
	switch t := terminalTool(name); t {
	case terminalPass, terminalQuit:
		return t
	default:
		return terminalNone
	}
}

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
// Two tools end the turn: `pass`, the model saying it has nothing to
// add, and `quit`, the model ending its own connection. Both stop
// the batch where they appear, so anything the model sequenced after
// a `quit` is never run — the client it would run as has already
// gone, and executing it could only produce a tool error for a
// conversation that is over. A goodbye the model wants said has to
// come before the `quit` in the batch, which is the order it would
// take on a real connection too.
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

		toolResults, terminal, wErr := executeTools(ctx, sess, ToolContext{
			Session: sess,
			Actor:   inst,
			Channel: channelName,
			Client:  caller,
		}, registry, result.PendingToolCalls, pacer)
		if wErr != nil {
			return outcome, wErr
		}
		outcome.toolTurnCount++

		switch terminal {
		case terminalPass:
			outcome.passReason = observability.PassReasonModelPass
			return outcome, nil
		case terminalQuit:
			return outcome, nil
		case terminalNone:
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

// executeTools runs pending tool calls in order and returns the
// results to feed back to the model, along with the [terminalTool]
// that closed the turn if one did.
//
// A terminal tool stops the batch where it sits, so the results
// cover the calls up to and including it. That is what keeps the
// calls a model sequenced after its own `quit` from running as a
// client that no longer exists. A `pass` mixed with other tools is
// rejected wholesale under the pass-exclusivity rule, which ends the
// turn the same way. The rich reason text the model supplied to
// `pass` lands on the per-call execute_tool span as `pass.reason`;
// the dispatch-turn span carries the stable enum.
func executeTools(
	ctx context.Context,
	sess Session,
	toolCtx ToolContext,
	registry *ToolRegistry,
	calls []api.PendingToolCall,
	pacer *Pacer,
) ([]api.ToolResult, terminalTool, error) {
	if reject := rejectMixedPass(calls); reject != nil {
		return reject, terminalPass, nil
	}

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

		if body, ok := pacingBody(toolName, call.Args); ok {
			if err := pacer.Wait(callCtx, body); err != nil {
				callSpan.End()
				return nil, terminalNone, err
			}
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

		data, _ := json.Marshal(payload)
		results = append(results, api.ToolResult{ToolCallID: call.ID, Content: string(data)})

		if terminal := terminalToolFor(toolName); terminal != terminalNone {
			return results, terminal, nil
		}
	}

	return results, terminalNone, nil
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
		if terminalToolFor(call.Name) == terminalPass {
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
