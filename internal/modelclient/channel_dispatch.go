package modelclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ircfmt"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/richtext"
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

// DispatchToChannel sends new events to all model instances in a channel
// and collects their replies. The caller provides the new IRC-formatted
// events to broadcast; history is loaded from the store.
//
// Callers must not include events whose `InstanceID` matches a target
// model — the wire-layer suppression is at fan-out, not at this driver.
func (d *Dispatcher) DispatchToChannel(
	ctx context.Context,
	ch domain.ChannelName,
	newEvents []protocol.IRCMessage,
) ([]domain.ModelReplyEvent, error) {
	tracer := d.sess.TracerProvider().Tracer("github.com/laney/modeloff/internal/modelclient")
	ctx, span := tracer.Start(ctx, "modelclient.dispatch_to_channel",
		trace.WithAttributes(attribute.String(observability.AttrOperation, "modelclient.dispatch_to_channel")),
	)
	defer span.End()

	historyEvents, err := d.sess.EventsBefore(ctx, ch, nil, 500)
	if err != nil {
		setSpanError(span, err, observability.ErrorKindStore)
		return nil, fmt.Errorf("list history: %w", err)
	}

	replies, err := d.dispatchToInstances(ctx, ch, historyEvents, newEvents, d.ensure)
	if err != nil {
		setSpanError(span, err, observability.ErrorKindDispatch)
		return nil, err
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return replies, nil
}

func (d *Dispatcher) dispatchToInstances(
	ctx context.Context,
	channelName domain.ChannelName,
	historyEvents []domain.StoredEvent,
	events []protocol.IRCMessage,
	ensure EnsureStructuredOutputModel,
) ([]domain.ModelReplyEvent, error) {
	instances, err := resolveDispatchRecipients(ctx, d.sess, channelName)
	if err != nil {
		return nil, fmt.Errorf("resolve dispatch recipients: %w", err)
	}

	var errs []error
	var replies []domain.ModelReplyEvent

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
		instReplies, instErr := dispatchToInstance(ctx, d.sess, d.api, d.memStore, d.tools, ensure, caller, window, inst, channelName, historyEvents, events)
		if instErr != nil {
			errs = append(errs, instErr)
		}

		replies = append(replies, instReplies...)

		for _, r := range instReplies {
			ircMsg, _ := protocol.FromChannelEvent(r.Event)
			events = append(events, ircMsg)
		}
	}

	return replies, errors.Join(errs...)
}

// dispatchToInstance runs the per-instance API turn. It assembles
// the system prompt + tool registry, calls the model via
// [sendWithRetry], and persists any replies via [buildReplies].
func dispatchToInstance(
	ctx context.Context,
	sess Session,
	apiClient api.Client,
	memStore memory.Store,
	tools *ToolRegistry,
	ensure EnsureStructuredOutputModel,
	caller protocol.Client,
	window domain.Window,
	inst *domain.Instance,
	channelName domain.ChannelName,
	historyEvents []domain.StoredEvent,
	events []protocol.IRCMessage,
) ([]domain.ModelReplyEvent, error) {
	nick := inst.Nick()

	tracer := sess.TracerProvider().Tracer("github.com/laney/modeloff/internal/modelclient")
	ctx, instanceSpan := tracer.Start(
		ctx,
		"modelclient.dispatch_to_instance",
		trace.WithAttributes(
			attribute.String(observability.AttrOperation, "modelclient.dispatch_to_instance"),
			attribute.String(observability.AttrModelID, string(inst.ModelID)),
			attribute.String(observability.AttrNick, string(nick)),
			attribute.String(observability.AttrInstanceID, string(inst.ID())),
			attribute.String(observability.AttrChannelKind, channelKindName(window.Kind())),
		),
	)
	defer instanceSpan.End()

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
		setSpanError(instanceSpan, err, classifyEnsureModelError(err))
		return nil, fmt.Errorf("send events to %s: %w", nick, err)
	}

	memories, err := memoriesForInstance(ctx, memStore, inst.ID())
	if err != nil {
		instanceSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		instanceSpan.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("read memories for %s: %w", nick, err)
	}

	prompt := buildSystemPrompt(window, inst, memories)

	var mem MemoryExecutor
	if memStore != nil {
		mem = &instanceMemory{instanceID: inst.ID(), store: memStore}
	}

	registry := MergeToolRegistries(
		memoryToolRegistry(mem, memStore != nil && searchEnabled(memStore)),
		tools,
	)

	outcome, err := sendWithRetry(ctx, apiClient, sess, caller, inst, channelName, prompt, history, events, registry)
	if err != nil {
		instanceSpan.SetAttributes(attribute.String(observability.AttrResult, observability.ResultError))
		instanceSpan.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("send events to %s: %w", nick, err)
	}

	result := outcome.result
	result.Usage.SetSpanAttributes(instanceSpan, result.RequestID)
	instanceAttrs := []attribute.KeyValue{
		attribute.String(observability.AttrResult, api.ResponseResultKind(result.Response)),
		attribute.Int(observability.AttrRetryCount, outcome.retryCount),
		attribute.Int(observability.AttrToolTurnCount, outcome.toolTurnCount),
	}
	if outcome.passReason != "" {
		instanceAttrs = append(instanceAttrs, attribute.String(observability.AttrPassReason, outcome.passReason))
	}
	instanceSpan.SetAttributes(instanceAttrs...)

	response := result.Response

	var replyPreview string

	switch response.Kind {
	case protocol.ResponseReply:
		var parts []string
		for _, m := range response.Messages {
			parts = append(parts, m.Body)
		}

		replyPreview = strings.Join(parts, " ")

	default:
		replyPreview = response.Reason
	}

	if len(replyPreview) > 200 {
		replyPreview = replyPreview[:200]
	}

	logger := slog.Default().With("component", "modelclient")
	logger.InfoContext(ctx, "dispatch to instance",
		"channel", channelName,
		"nick", nick,
		"model_id", inst.ModelID,
		"trigger_count", len(events),
		"trigger_summary", triggerSummary(events),
		"result", api.ResponseResultKind(result.Response),
		"reply_preview", replyPreview,
	)

	switch response.Kind {
	case protocol.ResponseReply:
		if len(response.Messages) == 0 {
			return nil, nil
		}

		return buildReplies(ctx, sess, channelName, inst, response.Messages), nil

	default:
		return nil, nil
	}
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

// buildReplies converts model reply parts into domain events and
// persists each message. Returns the per-reply envelopes; the
// goroutine driver in [ModelClient.dispatchTurn] emits each
// reply's `Event` on the wire, while the synchronous
// [Dispatcher.dispatchToInstances] driver returns replies to its
// caller without emitting.
func buildReplies(
	ctx context.Context,
	sess Session,
	channelName domain.ChannelName,
	inst *domain.Instance,
	parts []protocol.ReplyPart,
) []domain.ModelReplyEvent {
	var replies []domain.ModelReplyEvent

	nick := inst.Nick()
	instanceID := inst.ID()

	for _, part := range parts {
		body := strings.TrimSpace(renderReplyBody(part))
		if body == "" {
			continue
		}

		now := sess.Now()
		cm := domain.Message{
			Target:     channelName,
			From:       nick,
			InstanceID: instanceID,
			Body:       body,
			Action:     part.Kind == protocol.ReplyAction,
			At:         now,
		}

		sess.AppendEvent(ctx, channelName, cm)

		replies = append(replies, domain.ModelReplyEvent{
			Channel:  channelName,
			Event:    cm,
			Instance: inst,
			At:       now,
		})
	}

	return replies
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

func renderReplyBody(part protocol.ReplyPart) string {
	if len(part.Spans) == 0 {
		return part.Body
	}

	if err := protocol.ValidateReplyPart(part); err != nil {
		return part.Body
	}

	spans := make([]richtext.Span, 0, len(part.Spans))

	for _, span := range part.Spans {
		attrs := richtext.Attrs{}
		if span.Style != nil {
			attrs = replyStyleToAttrs(*span.Style)
		}

		spans = append(spans, richtext.Span{
			Text:  span.Text,
			Attrs: attrs,
		})
	}

	return ircfmt.Encode(richtext.NewDocumentFromLines([]richtext.Line{{Spans: spans}}))
}

func replyStyleToAttrs(style protocol.ReplyStyle) richtext.Attrs {
	return richtext.Attrs{
		Bold:      style.Bold,
		Italic:    style.Italic,
		Underline: style.Underline,
		Reverse:   style.Reverse,
		Strike:    style.Strike,
		FG:        cloneReplyColour(style.FG),
		BG:        cloneReplyColour(style.BG),
	}
}

func cloneReplyColour(colour *uint8) *uint8 {
	if colour == nil {
		return nil
	}

	value := *colour

	return &value
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
