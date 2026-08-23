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

type runTurnRequest struct {
	apiClient    api.Client
	session      Session
	caller       protocol.Client
	instance     *domain.Instance
	target       protocol.MsgTarget
	projection   providerTargetProjection
	prompt       api.SystemPrompt
	history      []protocol.IRCMessage
	events       []protocol.IRCMessage
	registry     *ToolRegistry
	pacer        *Pacer
	guard        protocol.WindowGuard
	journal      *turnJournalWriter
	startJournal func() *turnJournalWriter
	retry        retryPolicy
}

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

// nonReplayableTurnError reports a turn failure after the model's
// tool calls started executing. The original batch cannot be sent to
// the model again because a tool may already have changed external or
// local state, even when the failure happened while reporting its
// result upstream.
type nonReplayableTurnError struct {
	Err error
}

func (e *nonReplayableTurnError) Error() string {
	return e.Err.Error()
}

func (e *nonReplayableTurnError) Unwrap() error {
	return e.Err
}

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
func runTurn(ctx context.Context, turn runTurnRequest) (turnOutcome, error) {
	definitions := turn.registry.Definitions()
	result, journal, err := sendTurnEvents(ctx, turn, definitions)
	turn.journal = journal
	if err != nil {
		if outcome, ok := classifyUpstreamSilence(err); ok {
			return outcome, nil
		}

		return turnOutcome{}, err
	}

	outcome := turnOutcome{}
	executedAnyTool := false

	for range maxToolLoopTurns {
		if windowRevoked(ctx, turn.guard) {
			return outcome, errDispatchWindowClosed
		}

		if len(result.PendingToolCalls) == 0 || turn.registry == nil {
			outcome.passReason = observability.PassReasonModelPass
			return outcome, nil
		}

		toolResults, terminal, executed, toolErr := executeToolBatch(
			ctx, turn, result.PendingToolCalls,
		)
		outcome.toolTurnCount++
		executedAnyTool = executedAnyTool || executed
		if toolErr != nil {
			return outcome, toolErr
		}
		if windowRevoked(ctx, turn.guard) {
			return outcome, errDispatchWindowClosed
		}

		switch terminal {
		case terminalPass:
			outcome.passReason = observability.PassReasonModelPass
			return outcome, nil
		case terminalQuit:
			return outcome, nil
		case terminalNone:
		}
		if outcome.toolTurnCount == maxToolLoopTurns {
			outcome.passReason = observability.PassReasonToolLoopExhausted
			return outcome, nil
		}

		result, err = continueTurn(ctx, turn, result.Conversation, toolResults, definitions)
		if err != nil {
			if next, ok := classifyUpstreamSilence(err); ok {
				next.toolTurnCount = outcome.toolTurnCount
				return next, nil
			}

			if executedAnyTool {
				return outcome, &nonReplayableTurnError{Err: err}
			}

			return outcome, err
		}
	}

	outcome.passReason = observability.PassReasonToolLoopExhausted
	return outcome, nil
}

func sendTurnEvents(
	ctx context.Context,
	turn runTurnRequest,
	definitions []api.ToolDefinition,
) (api.CompletionResult, *turnJournalWriter, error) {
	if windowRevoked(ctx, turn.guard) {
		return api.CompletionResult{}, turn.journal, errDispatchWindowClosed
	}

	journal := turn.journal
	if turn.startJournal != nil {
		journal = turn.startJournal()
	}

	result, err := turn.apiClient.SendEvents(
		ctx,
		turn.instance.ModelID,
		turn.instance.ID(),
		turn.prompt,
		turn.history,
		turn.events,
		definitions...,
	)
	appendAssistantEvidence(journal, result, err)
	if err != nil && windowRevoked(ctx, turn.guard) {
		return api.CompletionResult{}, journal, errDispatchWindowClosed
	}

	return result, journal, err
}

func continueTurn(
	ctx context.Context,
	turn runTurnRequest,
	conversation *api.Conversation,
	toolResults []api.ToolResult,
	definitions []api.ToolDefinition,
) (api.CompletionResult, error) {
	result, err := continueTurnAttempt(
		ctx, turn, conversation, toolResults, definitions,
	)
	if err == nil || !api.Retryable(err) {
		return result, err
	}
	if waitErr := turn.retry.wait(ctx, err); waitErr != nil {
		return result, fmt.Errorf("wait to retry continuation: %w", waitErr)
	}

	return continueTurnAttempt(ctx, turn, conversation, toolResults, definitions)
}

func continueTurnAttempt(
	ctx context.Context,
	turn runTurnRequest,
	conversation *api.Conversation,
	toolResults []api.ToolResult,
	definitions []api.ToolDefinition,
) (api.CompletionResult, error) {
	if windowRevoked(ctx, turn.guard) {
		return api.CompletionResult{}, errDispatchWindowClosed
	}

	if turn.journal != nil {
		request, err := turn.apiClient.RenderToolResultRequest(
			conversation, toolResults, definitions...,
		)
		if err != nil {
			return api.CompletionResult{}, &nonReplayableTurnError{Err: err}
		}
		turn.journal.request(request)
	}

	result, err := turn.apiClient.ContinueWithToolResults(
		ctx, conversation, toolResults, definitions...,
	)
	appendAssistantEvidence(turn.journal, result, err)
	if err != nil && windowRevoked(ctx, turn.guard) {
		return api.CompletionResult{}, errDispatchWindowClosed
	}

	return result, err
}

func executeToolBatch(
	ctx context.Context,
	turn runTurnRequest,
	calls []api.PendingToolCall,
) ([]api.ToolResult, terminalTool, bool, error) {
	toolCtx := NewToolContext(turn.guard, turn.session, turn.caller, turn.target)
	toolCtx.projection = turn.projection

	batch, executionErr := executeTools(
		ctx, turn.session, toolCtx, turn.registry, calls, turn.pacer,
	)

	if len(batch.results) > 0 {
		appendToolEvidence(turn.journal, batch.results)
	}
	if executionErr != nil {
		return batch.results, batch.terminal, batch.executed, &nonReplayableTurnError{
			Err: executionErr,
		}
	}

	return batch.results, batch.terminal, batch.executed, nil
}

func windowRevoked(ctx context.Context, guard protocol.WindowGuard) bool {
	return guard != nil && !guard.Valid(ctx)
}

func appendAssistantEvidence(
	journal *turnJournalWriter,
	result api.CompletionResult,
	responseErr error,
) {
	if responseErr != nil && !result.ResponseReceived {
		return
	}

	journal.assistant(result)
}

func appendToolEvidence(journal *turnJournalWriter, results []api.ToolResult) {
	journal.toolResults(results)
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

type toolBatchOutcome struct {
	results  []api.ToolResult
	terminal terminalTool
	executed bool
}

// executeTools runs pending tool calls in order and returns the
// results to feed back to the model, along with the [terminalTool]
// that closed the turn if one did.
//
// A successful terminal tool stops the batch where it sits, so the
// results cover the calls up to and including it. That is what keeps
// calls a model sequenced after its own `quit` from running as a
// client that no longer exists. A rejected terminal call and a mixed
// `pass` batch return their error results to the model so it can issue
// a corrected call in the same tool loop. The rich reason text from a
// successful `pass` lands on the per-call execute_tool span as
// `pass.reason`; the dispatch-turn span carries the stable enum.
func executeTools(
	ctx context.Context,
	sess Session,
	toolCtx ToolContext,
	registry *ToolRegistry,
	calls []api.PendingToolCall,
	pacer *Pacer,
) (toolBatchOutcome, error) {
	if reject := rejectMixedPass(calls); reject != nil {
		return toolBatchOutcome{results: reject}, nil
	}

	outcome := toolBatchOutcome{results: make([]api.ToolResult, 0, len(calls))}
	tracer := sess.TracerProvider().Tracer("github.com/laney/modeloff/internal/modelclient")
	if pacer != nil {
		toolCtx.Pace = pacer.Wait
	}

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

		if err := toolCtx.authorise(callCtx); err != nil {
			callSpan.RecordError(err)
			callSpan.SetStatus(codes.Error, err.Error())
			callSpan.End()

			return outcome, err
		}

		if spec, ok := registry.Find(toolName); ok && toolAvailableInWindow(spec, toolCtx.Target) {
			outcome.executed = true
			nextPayload, err := spec.Execute(callCtx, toolCtx, call.Args)
			if err != nil {
				var executionError *ToolExecutionError
				if errors.As(err, &executionError) {
					callSpan.RecordError(err)
					callSpan.SetStatus(codes.Error, err.Error())
					callSpan.End()

					return outcome, err
				}

				nextPayload = ToolResultPayload{OK: false, Error: err.Error()}
			}
			payload = nextPayload
		} else if ok {
			payload.Error = fmt.Sprintf("tool %q is not available in this window", toolName)
		}

		if payload.OK {
			callSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
		} else {
			callSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
			callSpan.SetStatus(codes.Error, payload.Error)
		}

		callSpan.End()

		data, _ := json.Marshal(payload)
		outcome.results = append(outcome.results, api.ToolResult{ToolCallID: call.ID, Content: string(data)})

		if terminal := terminalToolFor(toolName); payload.OK && terminal != terminalNone {
			outcome.terminal = terminal
			return outcome, nil
		}
	}

	return outcome, nil
}

func toolAvailableInWindow(spec ToolSpec, target protocol.MsgTarget) bool {
	if spec.RequiredKind == nil {
		return true
	}

	var kind domain.ChannelKind
	switch target.(type) {
	case protocol.ChannelTarget:
		kind = domain.KindChannel
	case protocol.ClientTarget, protocol.NickTarget:
		kind = domain.KindDM
	default:
		return false
	}

	return kind == *spec.RequiredKind
}

// rejectMixedPass enforces the rule that `pass` is mutually
// exclusive with every other tool in the same turn. When violated,
// every call (including the pass itself) receives an error result
// explaining the rule. The rejection does not end the turn. The
// caller sends the results back in the same tool loop so the model can
// issue a corrected batch.
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
