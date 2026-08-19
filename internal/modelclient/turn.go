package modelclient

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
)

// EnsureStructuredOutputModel validates that the given model
// supports structured outputs. Each dispatch turn consults this
// before invoking the upstream API. Implementations carry their
// own catalogue cache; the modelclient does not retain one.
type EnsureStructuredOutputModel func(ctx context.Context, modelID domain.ModelID) error

// noEnsure is the permissive default consulted when a [ModelClient]
// is constructed without a real catalogue check. Tests that do not
// care about catalogue validation use it; in production the
// manager-supplied closure does the lookup.
func noEnsure(context.Context, domain.ModelID) error { return nil }

// dispatchToInstance runs the per-instance API turn. It assembles
// the system prompt + tool registry and calls the model via
// [runTurn]. Any chat traffic the model emits lands on the session
// bus as a side effect of its `msg` / `me` tool calls; this function
// returns only the turn's outcome.
//
// `tokenBudget` is the turn's transcript token budget (see
// [history.TokenBudget]). [composeTranscriptBudget] spends it once
// across `historyEvents`, `replyEvents` and `events` together before
// any of them is rendered — trimming the three independently would
// let each fit its own share of the budget while their sum still
// overran the model's context.
func dispatchToInstance(
	ctx context.Context,
	sess Session,
	apiClient api.Client,
	memStore memory.Store,
	tools *ToolRegistry,
	ensure EnsureStructuredOutputModel,
	pacer *Pacer,
	caller protocol.Client,
	window domain.Window,
	inst *domain.Instance,
	channelName domain.ChannelName,
	historyEvents []domain.StoredEvent,
	replyEvents []domain.StoredEvent,
	events []protocol.IRCMessage,
	tokenBudget int,
) error {
	historyEvents, replyEvents, events = composeTranscriptBudget(historyEvents, replyEvents, events, tokenBudget)

	nick := inst.Nick()

	runner := observability.SpanRunner{
		Tracer:         sess.TracerProvider().Tracer("github.com/laney/modeloff/internal/modelclient"),
		DefaultErrKind: observability.ErrorKindStore,
		ClassifyError:  classifyModelclientError,
	}

	attrs := []attribute.KeyValue{
		attribute.String(observability.AttrModelID, string(inst.ModelID)),
		attribute.String(observability.AttrNick, string(nick)),
		attribute.String(observability.AttrInstanceID, string(inst.ID())),
		attribute.String(observability.AttrChannelKind, channelKindName(window.Kind())),
	}

	return runner.Run(ctx, "modelclient.dispatch_to_instance", attrs, func(ctx context.Context, span trace.Span) error {
		type timedMessage struct {
			at  time.Time
			msg protocol.IRCMessage
		}

		var timeline []timedMessage

		// The shared channel transcript. The ring is join-scoped by
		// construction and holds only channel activity.
		for _, se := range historyEvents {
			if msg, ok := protocol.FromChannelEvent(se.Event); ok {
				timeline = append(timeline, timedMessage{at: domain.EventTime(se.Event), msg: msg})
			}
		}

		// The instance's own replies: its private experience, shown in
		// every window it dispatches in so the transcript reads as if
		// its quit never happened. They are not channel-broadcast, so
		// the model-visibility filter does not apply.
		for _, se := range replyEvents {
			if msg, ok := protocol.FromChannelEvent(se.Event); ok {
				timeline = append(timeline, timedMessage{at: domain.EventTime(se.Event), msg: msg})
			}
		}

		sort.SliceStable(timeline, func(i, j int) bool {
			return timeline[i].at.Before(timeline[j].at)
		})

		history := make([]protocol.IRCMessage, len(timeline))
		for i, tm := range timeline {
			history[i] = tm.msg
		}

		if err := ensure(ctx, inst.ModelID); err != nil {
			return errWithKind(fmt.Errorf("send events to %s: %w", nick, err), classifyEnsureModelError(err))
		}

		memories, err := memoriesForInstance(ctx, memStore, inst.ID())
		if err != nil {
			return fmt.Errorf("read memories for %s: %w", nick, err)
		}

		prompt := buildSystemPrompt(window, inst, memories)

		var mem MemoryExecutor
		if memStore != nil {
			mem = &instanceMemory{instanceID: inst.ID(), store: memStore}
		}

		registry := MergeToolRegistries(
			memoryToolRegistry(mem, memStore != nil && searchEnabled(memStore)),
			tools.Filter(modelCaps{}, window.Kind()),
		)

		outcome, err := runTurn(ctx, apiClient, sess, caller, inst, channelName, prompt, history, events, registry, pacer)
		if err != nil {
			return errWithKind(
				fmt.Errorf("send events to %s: %w", nick, err),
				observability.ErrorKindDispatch,
			)
		}

		span.SetAttributes(attribute.Int(observability.AttrToolTurnCount, outcome.toolTurnCount))
		if outcome.passReason != "" {
			span.SetAttributes(attribute.String(observability.AttrPassReason, outcome.passReason))
		}

		slog.Default().With("component", "modelclient").InfoContext(ctx, "dispatch to instance",
			"channel", channelName,
			"nick", nick,
			"model_id", inst.ModelID,
			"trigger_count", len(events),
			"trigger_summary", triggerSummary(events),
			"tool_turns", outcome.toolTurnCount,
			"pass_reason", outcome.passReason,
		)

		return nil
	})
}

// triggerSummary formats trigger events as a short description string.
// Each event is rendered as "<Kind> from <From>" and joined with "; ".
// The result is truncated to 200 characters.
func triggerSummary(events []protocol.IRCMessage) string {
	parts := make([]string, len(events))
	for i, e := range events {
		parts[i] = string(e.Kind) + " from " + e.From
	}

	s := strings.Join(parts, "; ")
	if len(s) > 200 {
		s = s[:200]
	}

	return s
}

func channelKindName(kind domain.ChannelKind) string {
	switch kind {
	case domain.KindDM:
		return "dm"
	case domain.KindStatus:
		return "status"
	default:
		return "channel"
	}
}
