package modelclient

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
)

// runDispatchLoop is the long-lived dispatch goroutine for a model-
// client. It reads [protocol.Delivery] envelopes from the
// subscription's events channel and, for each delivery, decides
// whether to take an LLM turn (a message in a channel/DM the model
// is in, a JOIN/PART/MODE in a channel it shares, an INVITE
// addressed at it, or a poke) and files the event into the model's
// per-channel rolling history buffer. Replies emit on the bus so
// every subscriber sees them.
//
// When a delivery is going to trigger a turn, the loop snapshots
// history before appending the triggering event and passes the
// snapshot through to the turn. `buildMessages` lays history then
// trigger events into the LLM request; keeping the trigger out of
// the snapshot stops the same line appearing twice in the model's
// prompt.
//
// The history buffer feeds [ModelClient.dispatchTurn]'s prompt
// construction. Eager-seeded for known channels at attach (see
// [ModelClient.seedHistory]) and lazy-seeded for DM targets in
// [history.append], the buffer is the only path the dispatch hot
// path reads conversation history from; the events log is
// consulted exclusively at seed time.
//
// Each turn's span is linked to the originating handler's span via
// the [trace.SpanContext] the producer captured at emit time. The
// turn is not a child of the originator: fan-out is one-to-many
// and each turn is its own operation. OTel links express that
// "related but separate" relationship.
//
// The goroutine exits when `ctx` (the supplier-derived lifetime
// ctx passed at attach) is cancelled, or when the subscription's
// `Done` channel closes.
func (mc *ModelClient) runDispatchLoop(ctx context.Context, sub protocol.Subscription) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "dispatch goroutine panicked",
				"component", "modelclient",
				"instance_id", mc.instance.ID(),
				"panic", r,
			)
		}
	}()

	events := sub.Events()
	done := sub.Done()

	for {
		var delivery protocol.Delivery
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case delivery = <-events:
		}

		ch, irc, ok := dispatchTrigger(mc.instance.ID(), delivery.Event)

		var historyForTurn []domain.StoredEvent
		if ok {
			historyForTurn = mc.hist.snapshot(ch)
		}

		if pe, ok := delivery.Event.(domain.PersistableEvent); ok {
			stored := domain.StoredEvent{Event: pe}
			for _, target := range historyTargets(delivery) {
				mc.hist.append(ctx, mc.sess, mc.instance.ID(), stored, target)
			}
		}

		if !ok {
			continue
		}

		mc.dispatchTurn(ctx, ch, irc, delivery.SpanCtx, historyForTurn)
	}
}

// historyTargets returns the buffer slot(s) the delivery's event
// should be filed under for the receiving model-client's
// dispatch-turn history. Most events belong to a single target
// window — the channel they happened in or the DM they addressed.
// Actor-scoped events ([domain.Quit] and [domain.NickChange])
// carry no target on the wire (RFC 2812 §3.1.7 and §3.1.2); the
// per-recipient channel list is on `delivery.Targets`,
// pre-computed by the session's fan-out as the intersection of
// the actor's channel set with the recipient's.
//
// Events with no target (PokeEvent, NamesReplyEvent, …) return
// nil and are skipped: they are not LLM-prompt material.
func historyTargets(delivery protocol.Delivery) []domain.ChannelName {
	switch e := delivery.Event.(type) {
	case domain.Message:
		return []domain.ChannelName{e.Target}
	case domain.Join:
		return []domain.ChannelName{e.Target}
	case domain.Part:
		return []domain.ChannelName{e.Target}
	case domain.TopicChange:
		return []domain.ChannelName{e.Target}
	case domain.TopicInfo:
		return []domain.ChannelName{e.Target}
	case domain.ModeChange:
		return []domain.ChannelName{e.Target}
	case domain.ModelInvited:
		return []domain.ChannelName{e.Target}
	case domain.ModelKicked:
		return []domain.ChannelName{e.Target}
	case domain.Quit, domain.NickChange:
		_ = e
		return delivery.Targets
	}

	return nil
}

// dispatchTrigger reports whether `ev` should make the model-client
// take a dispatch turn, and if so returns the target channel and
// the wire-shaped trigger message the LLM call uses as context.
func dispatchTrigger(selfID domain.InstanceID, ev domain.ProtocolEvent) (domain.ChannelName, protocol.IRCMessage, bool) {
	switch e := ev.(type) {
	case domain.Message:
		irc, _ := protocol.FromChannelEvent(e)
		return e.Target, irc, true

	case domain.Join:
		irc, _ := protocol.FromChannelEvent(e)
		return e.Target, irc, true

	case domain.Part:
		irc, _ := protocol.FromChannelEvent(e)
		return e.Target, irc, true

	case domain.ModelInvited:
		if e.InstanceID != protocol.ClientID(selfID) {
			return "", protocol.IRCMessage{}, false
		}

		return e.Target, protocol.IRCMessage{
			Kind:   protocol.KindInvite,
			From:   string(e.By),
			Target: string(e.Target),
			At:     e.At,
		}, true

	case domain.PokeEvent:
		return e.Channel, protocol.IRCMessage{
			Kind:   protocol.KindPoke,
			From:   "modeloff",
			Target: string(e.Channel),
			Body:   "the channel is quiet. if something comes to mind, say it — otherwise just lurk. don't force it.",
			At:     e.At,
		}, true
	}

	return "", protocol.IRCMessage{}, false
}

// dispatchTurn runs a single LLM turn for the model-client's
// instance in response to `trigger`, emitting `ModelDispatchStarted`
// / `ModelDispatchDone` around the call so consumers can scope a
// "this instance is thinking" indicator to the exact window of
// the turn. The reply Messages are persisted and emitted by
// [buildReplies].
//
// `causeCtx` is the span context the producer captured at emit
// time (see [protocol.Delivery]). When valid, the turn's span
// carries an OTel link to it so traces stay connected across the
// channel-based delivery boundary.
func (mc *ModelClient) dispatchTurn(ctx context.Context, ch domain.ChannelName, trigger protocol.IRCMessage, causeCtx trace.SpanContext, historyEvents []domain.StoredEvent) {
	inst := mc.instance
	nick := inst.Nick()

	tracer := mc.sess.TracerProvider().Tracer("github.com/laney/modeloff/internal/modelclient")

	startOpts := []trace.SpanStartOption{
		trace.WithAttributes(
			attribute.String(observability.AttrOperation, "modelclient.dispatch_turn"),
			attribute.String(observability.AttrChannel, string(ch)),
			attribute.String(observability.AttrModelID, string(inst.ModelID)),
			attribute.String(observability.AttrNick, string(nick)),
			attribute.String(observability.AttrInstanceID, string(inst.ID())),
		),
	}
	if causeCtx.IsValid() {
		startOpts = append(startOpts, trace.WithLinks(trace.Link{SpanContext: causeCtx}))
	}

	ctx, span := tracer.Start(ctx, "modelclient.dispatch_turn", startOpts...)
	defer span.End()
	defer mc.sess.Emit(ctx, domain.ModelDispatchDone{Instance: inst, At: mc.sess.Now()})

	window, err := dispatchWindowFor(ctx, mc.sess, ch, inst)
	if err != nil {
		setSpanError(span, err, observability.ErrorKindStore)
		mc.sess.Emit(ctx, domain.ModelUnavailableError{Channel: ch, Nick: nick, At: mc.sess.Now()})
		return
	}

	mc.sess.Emit(ctx, domain.ModelDispatchStarted{Instance: inst, At: mc.sess.Now()})

	apiClient := mc.apiFn()
	if apiClient == nil {
		mc.sess.Emit(ctx, domain.ModelUnavailableError{Channel: ch, Nick: nick, At: mc.sess.Now()})
		return
	}

	replies, err := dispatchToInstance(ctx, mc.sess, apiClient, mc.memStore, mc.tools, mc.ensure, mc, window, inst, ch, historyEvents, []protocol.IRCMessage{trigger})
	if err != nil {
		setSpanError(span, err, observability.ErrorKindDispatch)
		mc.sess.Emit(ctx, domain.ModelUnavailableError{Channel: ch, Nick: nick, At: mc.sess.Now()})
		return
	}

	// File the bot's own replies into its rolling history. The bus
	// suppresses self-delivery of [domain.Message] events (echo
	// gate, RFC 2812 §3.3.1) so the dispatch loop's hist.append
	// path never sees them — without this, every subsequent turn's
	// prompt would be missing the bot's own utterances.
	for _, r := range replies {
		mc.hist.append(ctx, mc.sess, inst.ID(), domain.StoredEvent{Event: r.Event}, ch)
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
}

// dispatchWindowFor produces the `Window` that the recipient
// model is "in" for the purposes of system-prompt construction
// and span tagging. For a `#`-channel target it loads the
// `*ChannelWindow` from storage. For a bare-nick target it
// synthesises a `*DMWindow` keyed by the message's addressing
// (no row is required — DMs are stateless on the server side).
func dispatchWindowFor(ctx context.Context, sess Session, target domain.ChannelName, inst *domain.Instance) (domain.Window, error) {
	if domain.InferChannelKind(target) == domain.KindDM {
		return domain.NewDMWindow(inst, sess.Now()), nil
	}

	return sess.LoadChannelWindow(ctx, target)
}
