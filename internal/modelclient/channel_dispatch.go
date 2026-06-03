package modelclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// Dispatcher synchronously broadcasts to every model member of a
// channel, returning their replies as a slice. Each instance gets
// its own per-turn prompt and tool-registry (memory tools merged
// with the supplied user registry) built from the same
// dependencies a [ModelClient] takes.
//
// The asynchronous, per-model dispatch goroutine that
// [ModelClient.Attach] starts is the live-event path; a Dispatcher
// covers the synchronous broadcast shape — a single caller
// addressing a channel and collecting replies in-band.
type Dispatcher struct {
	sess     Session
	api      api.Client
	memStore memory.Store
	tools    *ToolRegistry
	ensure   EnsureStructuredOutputModel
}

// NewDispatcher returns a Dispatcher bound to the given session and
// dependencies.
func NewDispatcher(sess Session, apiClient api.Client, memStore memory.Store, tools *ToolRegistry, ensure EnsureStructuredOutputModel) *Dispatcher {
	if ensure == nil {
		ensure = noEnsure
	}
	return &Dispatcher{sess: sess, api: apiClient, memStore: memStore, tools: tools, ensure: ensure}
}

// noEnsure is the permissive default consulted when a [Dispatcher]
// or [ModelClient] is constructed without a real catalogue check.
// Tests that do not care about catalogue validation use it; in
// production the manager-supplied closure does the lookup.
func noEnsure(context.Context, domain.ModelID) error { return nil }

// DispatchToChannel sends new events to all model instances in a
// channel. The caller provides the new IRC-formatted events to
// broadcast; history is loaded from the store. Any messages the
// models emit land on the session's event bus as side effects of
// their tool calls — DispatchToChannel returns nothing on success.
//
// Callers must not include events whose `InstanceID` matches a target
// model — the wire-layer suppression is at fan-out, not at this driver.
func (d *Dispatcher) DispatchToChannel(
	ctx context.Context,
	ch domain.ChannelName,
	newEvents []protocol.IRCMessage,
) error {
	runner := observability.SpanRunner{
		Tracer:         d.sess.TracerProvider().Tracer("github.com/laney/modeloff/internal/modelclient"),
		DefaultErrKind: observability.ErrorKindStore,
		ClassifyError:  classifyModelclientError,
	}

	return runner.Run(ctx, "modelclient.dispatch_to_channel", nil, func(ctx context.Context, _ trace.Span) error {
		historyEvents, err := d.sess.EventsBefore(ctx, ch, nil, modelHistorySize)
		if err != nil {
			return fmt.Errorf("list history: %w", err)
		}

		if err := d.dispatchToInstances(ctx, ch, historyEvents, newEvents, d.ensure); err != nil {
			return errWithKind(err, observability.ErrorKindDispatch)
		}

		return nil
	})
}

func (d *Dispatcher) dispatchToInstances(
	ctx context.Context,
	channelName domain.ChannelName,
	historyEvents []domain.StoredEvent,
	events []protocol.IRCMessage,
	ensure EnsureStructuredOutputModel,
) error {
	instances, err := resolveDispatchRecipients(ctx, d.sess, channelName)
	if err != nil {
		return fmt.Errorf("resolve dispatch recipients: %w", err)
	}

	var errs []error

	for _, inst := range instances {
		if len(events) == 0 {
			continue
		}

		window, err := dispatchWindowFor(ctx, d.sess, channelName, inst)
		if err != nil {
			errs = append(errs, err)

			continue
		}

		caller := d.sess.LookupClient(protocol.ClientID(inst.ID()))
		if instErr := dispatchToInstance(ctx, d.sess, d.api, d.memStore, d.tools, ensure, nil, caller, window, inst, channelName, historyEvents, events); instErr != nil {
			errs = append(errs, instErr)
		}
	}

	return errors.Join(errs...)
}

// dispatchToInstance runs the per-instance API turn. It assembles
// the system prompt + tool registry and calls the model via
// [runTurn]. Any chat traffic the model emits lands on the session
// bus as a side effect of its `msg` / `me` tool calls; this function
// returns only the turn's outcome.
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
	events []protocol.IRCMessage,
) error {
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
		var joinedAt time.Time
		if channels := inst.Channels(); channels != nil {
			joinedAt, _ = channels.Get(channelName)
		}

		history := make([]protocol.IRCMessage, 0, len(historyEvents))
		for _, se := range historyEvents {
			if !se.Event.ModelVisible() {
				continue
			}

			eventTime := domain.EventTime(se.Event)
			if !joinedAt.IsZero() && eventTime.Before(joinedAt) {
				continue
			}

			if msg, ok := protocol.FromChannelEvent(se.Event); ok {
				history = append(history, msg)
			}
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

func instancesForChannelWindow(window *domain.ChannelWindow) []*domain.Instance {
	var instances []*domain.Instance

	for m := range window.Members.All() {
		// The human user has no ModelID and is never dispatched to.
		if !m.Instance.IsModel() {
			continue
		}

		instances = append(instances, m.Instance)
	}

	return instances
}

// resolveDispatchRecipients picks the model instances that should
// take a dispatch turn for the given target. It is the single
// place the dispatch path branches on the shape of the target:
//
//   - A `#`-prefixed channel name fans out to every model member
//     of that channel (the existing channel-Members iteration).
//   - A non-empty `InstanceID`-shaped target is a DM addressed
//     at that specific instance — resolve it through the store
//     and return as a single recipient if it's a model.
//   - An empty target is a DM addressed at the user. The user
//     is not a dispatch target (they read via the UI), so this
//     resolves to no recipients.
//   - The status window has no recipients; it carries server-
//     narrated notices, not dispatchable messages.
//
// The DM path deliberately does not go through `loadChannel +
// instancesForChannel`. Modeloff's DMs don't have a member-list
// concept on the server side: the recipient is encoded directly
// in the target. Dispatching by id keeps that model honest and
// works without any `*DMWindow` state on the server.
func resolveDispatchRecipients(ctx context.Context, sess Session, target domain.ChannelName) ([]*domain.Instance, error) {
	switch domain.InferChannelKind(target) {
	case domain.KindStatus:
		return nil, nil

	case domain.KindDM:
		if target == "" {
			// Empty target identifies the user as recipient. The
			// user is read by the UI, not dispatched.
			return nil, nil
		}

		inst, err := sess.ResolveInstanceByID(ctx, domain.InstanceID(target))
		if err != nil {
			return nil, err
		}

		if !inst.IsModel() {
			return nil, nil
		}

		return []*domain.Instance{inst}, nil

	case domain.KindChannel:
		window, err := sess.LoadChannelWindow(ctx, target)
		if err != nil {
			return nil, err
		}

		return instancesForChannelWindow(window), nil
	}

	return nil, nil
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
