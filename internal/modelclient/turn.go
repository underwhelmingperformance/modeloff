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

// turnRequest is everything one dispatch turn is about, as against
// the client-lifetime handles the [ModelClient] receiver already
// carries. Each field is read once per turn and none of them
// outlives it: the API client because a `SetAPIKey` rebuild may have
// replaced it since the last turn, and the rest because they describe
// this turn's window and the traffic it is answering.
type turnRequest struct {
	// api is the client this turn calls upstream through, read
	// through [ModelClient.apiFn] at the top of the turn.
	api api.Client

	// window is the channel or DM the turn runs in, and target
	// addresses it. The prompt is built from the first; the model's
	// chat tools send to the second.
	window domain.Window
	target protocol.MsgTarget

	// history is the window's transcript as it stood before the
	// burst, replies is the instance's own point-to-point replies,
	// and triggers is what the model is being asked about.
	history  []domain.StoredEvent
	replies  []domain.StoredEvent
	triggers []protocol.IRCMessage

	// tokenBudget is the turn's transcript token budget (see
	// [history.TokenBudget]). [composeTranscriptBudget] spends it once
	// across the context lines, `history`, `replies` and `triggers`
	// together before any of them is rendered. Trimming them
	// independently would let each fit its own share of the budget
	// while their sum still overran the model's context.
	tokenBudget int
}

// dispatchToInstance runs the per-instance API turn. It assembles
// the system prompt + tool registry and calls the model via
// [runTurn]. Any chat traffic the model emits lands on the session
// bus as a side effect of its `msg` / `me` tool calls; this method
// returns only the turn's outcome.
func (mc *ModelClient) dispatchToInstance(ctx context.Context, turn turnRequest) error {
	inst := mc.instance
	nick := inst.Nick()

	runner := observability.SpanRunner{
		Tracer:         mc.sess.TracerProvider().Tracer("github.com/laney/modeloff/internal/modelclient"),
		DefaultErrKind: observability.ErrorKindStore,
		ClassifyError:  observability.ErrorKindOf,
	}

	attrs := []attribute.KeyValue{
		attribute.String(observability.AttrModelID, string(inst.ModelID)),
		attribute.String(observability.AttrNick, string(nick)),
		attribute.String(observability.AttrInstanceID, string(inst.ID())),
		attribute.String(observability.AttrChannelKind, channelKindName(turn.window.Kind())),
	}

	return runner.Run(ctx, "modelclient.dispatch_to_instance", attrs, func(ctx context.Context, span trace.Span) error {
		memories, err := memoriesForInstance(ctx, mc.memStore, inst.ID())
		if err != nil {
			return fmt.Errorf("read memories for %s: %w", nick, err)
		}

		contextLines := contextReplies(turn.window, memories)

		historyEvents, replyEvents, events := composeTranscriptBudget(
			contextLines, turn.history, turn.replies, turn.triggers, turn.tokenBudget,
		)

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

		history := make([]protocol.IRCMessage, 0, len(timeline)+len(contextLines))
		for _, tm := range timeline {
			history = append(history, tm.msg)
		}

		history = append(history, contextLines...)

		if err := mc.ensure(ctx, inst.ModelID); err != nil {
			return observability.ErrWithKind(fmt.Errorf("send events to %s: %w", nick, err), classifyEnsureModelError(err))
		}

		prompt := buildSystemPrompt(turn.window, inst)

		var mem MemoryExecutor
		if mc.memStore != nil {
			mem = &instanceMemory{instanceID: inst.ID(), store: mc.memStore, now: mc.sess.Now}
		}

		// The tool set is filtered by what the server says this client
		// holds, so a tool the dispatcher would refuse is not offered.
		registry := MergeToolRegistries(
			memoryToolRegistry(mem, mc.memStore != nil && searchEnabled(mc.memStore)),
			mc.tools.Filter(mc.Caps(), turn.window.Kind()),
		)

		outcome, err := runTurn(ctx, turn.api, mc.sess, mc, inst, turn.target, prompt, history, events, registry, mc.pacer)
		if err != nil {
			return observability.ErrWithKind(
				fmt.Errorf("send events to %s: %w", nick, err),
				observability.ErrorKindDispatch,
			)
		}

		span.SetAttributes(attribute.Int(observability.AttrToolTurnCount, outcome.toolTurnCount))
		if outcome.passReason != "" {
			span.SetAttributes(attribute.String(observability.AttrPassReason, outcome.passReason))
		}

		slog.Default().With("component", "modelclient").InfoContext(ctx, "dispatch to instance",
			"channel", turn.window.Name(),
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
