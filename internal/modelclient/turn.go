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
	"github.com/laney/modeloff/internal/store"
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
	window protocol.WindowContext
	target protocol.MsgTarget
	guard  protocol.WindowGuard

	// history is the window's transcript as it stood before the
	// burst, replies is the instance's own point-to-point replies,
	// and events is the current chronological burst. triggers is the
	// subset that authorised the turn, while latestTrigger identifies
	// its newest member within events.
	history       []domain.StoredEvent
	replies       []storedReply
	events        []protocol.IRCMessage
	triggers      []protocol.IRCMessage
	latestTrigger int

	// tokenBudget is the turn's transcript token budget (see
	// [history.TokenBudget]). [composeTranscriptBudget] spends it once
	// across the context lines, `history`, `replies` and `triggers`
	// together before any of them is rendered. Trimming them
	// independently would let each fit its own share of the budget
	// while their sum still overran the model's context.
	tokenBudget int
}

type timedMessage struct {
	at  time.Time
	msg protocol.IRCMessage
}

func renderTurnHistory(
	historyEvents []domain.StoredEvent,
	replyEvents []storedReply,
	contextLines []protocol.IRCMessage,
	projection providerTargetProjection,
) []protocol.IRCMessage {
	timeline := make([]timedMessage, 0, len(historyEvents)+len(replyEvents))
	for _, event := range historyEvents {
		if message, ok := protocol.FromChannelEvent(event.Event); ok {
			timeline = append(timeline, timedMessage{
				at: domain.EventTime(event.Event), msg: projection.message(message),
			})
		}
	}
	for _, reply := range replyEvents {
		if message, ok := protocol.FromChannelEvent(reply.event.Event); ok {
			timeline = append(timeline, timedMessage{
				at:  domain.EventTime(reply.event.Event),
				msg: projection.reply(reply.window, message),
			})
		}
	}

	sort.SliceStable(timeline, func(i, j int) bool {
		return timeline[i].at.Before(timeline[j].at)
	})

	history := make([]protocol.IRCMessage, 0, len(timeline)+len(contextLines))
	for _, entry := range timeline {
		history = append(history, entry.msg)
	}

	for _, message := range contextLines {
		history = append(history, projection.message(message))
	}

	return history
}

// dispatchToInstance runs the per-instance API turn. It assembles
// the system prompt + tool registry and calls the model via
// [runTurn]. Any chat traffic the model emits lands on the session
// bus as a side effect of its `msg` / `me` tool calls; this method
// returns only the turn's outcome.
func (mc *ModelClient) dispatchToInstance(ctx context.Context, turn turnRequest) error {
	inst := mc.instance
	nick := mc.nick()

	runner := observability.SpanRunner{
		Tracer:         mc.sess.TracerProvider().Tracer("github.com/laney/modeloff/internal/modelclient"),
		DefaultErrKind: observability.ErrorKindStore,
		ClassifyError:  observability.ErrorKindOf,
	}

	attrs := []attribute.KeyValue{
		attribute.String(observability.AttrModelID, string(inst.ModelID)),
		attribute.String(observability.AttrNick, string(nick)),
		attribute.String(observability.AttrInstanceID, string(inst.ID())),
		attribute.String(observability.AttrChannelKind, channelKindName(protocol.WindowTargetKind(turn.window.Target()))),
	}

	return runner.Run(ctx, "modelclient.dispatch_to_instance", attrs, func(ctx context.Context, span trace.Span) error {
		memories, err := memoriesForInstance(ctx, mc.memStore, inst.ID())
		if err != nil {
			return fmt.Errorf("read memories for %s: %w", nick, err)
		}

		contextLines := contextReplies(turn.window, memories)

		if err := mc.ensure(ctx, inst.ModelID); err != nil {
			return observability.ErrWithKind(fmt.Errorf("send events to %s: %w", nick, err), classifyEnsureModelError(err))
		}
		mc.hist.SetContextLen(mc.contextLenFn(inst.ModelID))
		turn.tokenBudget = mc.hist.TokenBudget()

		projection, err := newProviderTargetProjection(ctx, mc.sess, turn.window, inst.ID(), nick)
		if err != nil {
			if turn.guard != nil && !turn.guard.Valid(ctx) {
				return errDispatchWindowClosed
			}

			return observability.ErrWithKind(err, observability.ErrorKindClientState)
		}
		historyEvents, replyEvents, events := composeProjectedTranscriptBudget(
			projection, contextLines, turn.history, turn.replies, turn.events,
			turn.latestTrigger, turn.tokenBudget,
		)
		history := renderTurnHistory(historyEvents, replyEvents, contextLines, projection)
		events = projection.messages(events)

		prompt := buildSystemPrompt(turn.window, nick, inst.Persona())

		var mem MemoryExecutor
		if mc.memStore != nil {
			mem = &instanceMemory{instanceID: inst.ID(), store: mc.memStore, now: mc.sess.Now}
		}

		// The tool set is filtered by what the server says this client
		// holds. Window-specific tools stay present so channel and DM
		// turns have the same cacheable prefix; execution still refuses
		// them outside the required window.
		registry := MergeToolRegistries(
			memoryToolRegistry(mem, mc.memStore != nil && searchEnabled(mc.memStore)),
			mc.tools.Filter(mc.Caps()),
		)

		definitions := registry.Definitions()
		renderedRequest, err := turn.api.RenderEventRequest(
			inst.ModelID,
			inst.ID(),
			prompt,
			history,
			events,
			definitions...,
		)
		if err != nil {
			return observability.ErrWithKind(err, observability.ErrorKindClientState)
		}
		var journal *turnJournalWriter
		startJournal := func() *turnJournalWriter {
			journal = beginTurnJournal(
				mc.journalQueue, mc.journal, turn.guard, store.ModelTurn{
					InstanceID: inst.ID(),
					Window:     turn.window.Target(),
					ModelID:    inst.ModelID,
					StartedAt:  mc.sess.Now(),
				}, journalInput(renderedRequest), mc.sess.Now,
			)

			return journal
		}

		outcome, turnErr := runTurn(ctx, runTurnRequest{
			apiClient:    turn.api,
			session:      mc.sess,
			caller:       mc,
			instance:     inst,
			target:       turn.target,
			projection:   projection,
			prompt:       prompt,
			history:      history,
			events:       events,
			registry:     registry,
			pacer:        mc.pacer,
			guard:        turn.guard,
			startJournal: startJournal,
			retry:        mc.retry,
		})
		journal.outcome(outcome, turnErr)
		if turnErr != nil {
			return observability.ErrWithKind(
				fmt.Errorf("send events to %s: %w", nick, turnErr),
				observability.ErrorKindDispatch,
			)
		}

		span.SetAttributes(attribute.Int(observability.AttrToolTurnCount, outcome.toolTurnCount))
		if outcome.passReason != "" {
			span.SetAttributes(attribute.String(observability.AttrPassReason, outcome.passReason))
		}

		slog.Default().With("component", "modelclient").InfoContext(ctx, "dispatch to instance",
			"channel", protocol.WindowKey(turn.window.Target()),
			"nick", nick,
			"model_id", inst.ModelID,
			"trigger_count", len(turn.triggers),
			"trigger_summary", triggerSummary(turn.triggers),
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
		parts[i] = string(e.Kind) + " from " + string(e.Source.Nick())
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
