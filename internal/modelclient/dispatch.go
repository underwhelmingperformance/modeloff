package modelclient

import (
	"context"
	"errors"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
)

// runDispatchLoop is the long-lived dispatch goroutine for a model-
// client. It reads [protocol.Delivery] envelopes from the
// subscription's events channel and decides which of them call for
// an LLM turn (a message in a channel/DM the model is in, a JOIN or
// PART in a channel it shares, an INVITE addressed at it, or a
// poke), filing each into the model's per-channel rolling history
// buffer as it goes. Replies emit on the bus so every subscriber
// sees them.
//
// A burst is taken as a batch. After a delivery arrives the loop
// drains whatever else is already queued without blocking, and the
// triggers it finds for one window go into a single turn. Five
// messages that land while the model was busy are five lines in one
// prompt, which is both what a person reading a channel sees and
// four fewer round-trips than one turn each. [ModelClient.fileBatch]
// is where the split by window happens and where each window's
// history snapshot is taken.
//
// The history buffer feeds [ModelClient.dispatchTurn]'s prompt
// construction. Loaded for known channels at attach (see
// [ModelClient.loadHistory]) and lazy-seeded for DM targets in
// [history.append], the buffer is the only path the dispatch hot
// path reads conversation history from; the events log is
// consulted exclusively at load time.
//
// Each turn's span is linked to the originating handlers' spans via
// the [trace.SpanContext] each producer captured at emit time. The
// turn is not a child of any of them: fan-out is one-to-many
// and each turn is its own operation. OTel links express that
// "related but separate" relationship.
//
// The goroutine exits when `ctx` (the supplier-derived lifetime
// ctx passed at attach) is cancelled, or when the subscription's
// `Done` channel closes.
func (mc *ModelClient) runDispatchLoop(ctx context.Context, sub protocol.Subscription) {
	events := sub.Events()
	done := sub.Done()

	defer mc.recoverDispatchPanic(ctx)

	for {
		var delivery protocol.Delivery
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case delivery = <-events:
		}

		for _, batch := range mc.fileBatch(ctx, append([]protocol.Delivery{delivery}, drain(events)...)) {
			// An earlier turn in this burst may have ended the
			// connection — a `quit` tool call cancels the loop's
			// context from inside the turn that made it. The
			// remaining windows belong to a client that has left, and
			// dispatching them would show peers a departed model
			// thinking.
			if ctx.Err() != nil {
				return
			}

			mc.dispatchTurn(ctx, batch)
		}
	}
}

// recoverDispatchPanic ends the client's connection when its
// dispatch goroutine dies of a panic. Without it the subscription
// stays registered with nobody reading it: the instance would go on
// being a member of its channels, accumulating a backlog the server
// keeps for a consumer that no longer exists. The QUIT the
// disconnect broadcasts is what the channel sees, so the failure is
// visible where the client was.
func (mc *ModelClient) recoverDispatchPanic(ctx context.Context) {
	r := recover()
	if r == nil {
		return
	}

	slog.ErrorContext(ctx, "dispatch goroutine panicked",
		"component", "modelclient",
		"instance_id", mc.instance.ID(),
		"panic", r,
	)

	mc.sess.Disconnect(ctx, mc.Identity(), "Internal error")
}

// drain takes everything already queued on `events` without
// blocking. What it returns is the rest of the burst the caller's
// first delivery started.
func drain(events <-chan protocol.Delivery) []protocol.Delivery {
	var rest []protocol.Delivery

	for {
		select {
		case d := <-events:
			rest = append(rest, d)
		default:
			return rest
		}
	}
}

// turnBatch is one window's worth of a burst: the triggers that
// arrived for it, the transcript the turn reads them against, and
// the span contexts of the deliveries that carried them.
type turnBatch struct {
	channel  domain.ChannelName
	history  []domain.StoredEvent
	triggers []protocol.IRCMessage
	causes   []trace.SpanContext
}

// fileBatch files `deliveries` into the per-channel history buffers
// and returns the turns they call for, one per window, in the order
// the windows first appeared in the burst.
//
// The split between a turn's history and its triggers is the split
// between what the model is shown as context and what it is being
// asked about. Every window that will take a turn is snapshotted
// before anything from the burst is filed, so a trigger the prompt
// lists explicitly is not also sitting in the transcript above it.
// The burst's own non-triggers — a topic change, a mode change, a
// peer's quit — are then appended to that transcript in arrival
// order, because a model catching up on five messages has to be told
// what happened between them.
func (mc *ModelClient) fileBatch(ctx context.Context, deliveries []protocol.Delivery) []*turnBatch {
	batches, byWindow := mc.openBatches(deliveries)

	for _, delivery := range deliveries {
		ch, irc, isTrigger := dispatchTrigger(mc.instance.ID(), delivery.Event)

		if isTrigger {
			batch := byWindow[ch]
			batch.triggers = append(batch.triggers, irc)
			batch.causes = append(batch.causes, delivery.SpanCtx)
		}

		ca, ok := delivery.Event.(domain.ChannelActivity)
		if !ok {
			continue
		}

		stored := domain.StoredEvent{Event: ca}

		for _, target := range historyTargets(delivery) {
			mc.hist.append(ctx, mc.sess, mc.instance.ID(), stored, target)

			if isTrigger && target == ch {
				continue
			}

			if batch, ok := byWindow[target]; ok {
				batch.history = append(batch.history, stored)
			}
		}
	}

	return batches
}

// openBatches allocates one batch per window the burst will raise a
// turn for, in the order those windows first appear, and seeds each
// with its window's transcript as it stands before the burst.
//
// It is a pass of its own because the snapshots have to be taken
// while none of the burst has been filed: a window whose first
// trigger is the last delivery still needs the transcript from
// before the first.
func (mc *ModelClient) openBatches(deliveries []protocol.Delivery) ([]*turnBatch, map[domain.ChannelName]*turnBatch) {
	var batches []*turnBatch

	byWindow := make(map[domain.ChannelName]*turnBatch)

	for _, delivery := range deliveries {
		ch, _, isTrigger := dispatchTrigger(mc.instance.ID(), delivery.Event)
		if !isTrigger {
			continue
		}

		if _, ok := byWindow[ch]; ok {
			continue
		}

		batch := &turnBatch{channel: ch, history: mc.hist.snapshot(ch)}
		byWindow[ch] = batch
		batches = append(batches, batch)
	}

	return batches, byWindow
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
	case domain.ChannelModeChange:
		return []domain.ChannelName{e.Target}
	case domain.Invited:
		return []domain.ChannelName{e.Target}
	case domain.Kicked:
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
		msg, _ := protocol.FromChannelEvent(e)
		return e.Target, msg, true

	case domain.Join:
		msg, _ := protocol.FromChannelEvent(e)
		return e.Target, msg, true

	case domain.Part:
		if e.InstanceID == selfID {
			return "", protocol.IRCMessage{}, false
		}

		msg, _ := protocol.FromChannelEvent(e)
		return e.Target, msg, true

	case domain.Invited:
		if e.InstanceID != selfID {
			return "", protocol.IRCMessage{}, false
		}

		msg, _ := protocol.FromChannelEvent(e)

		return e.Target, msg, true

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
// instance in response to `batch`, emitting `ModelDispatchStarted`
// / `ModelDispatchDone` around the call so consumers can scope a
// "this instance is thinking" indicator to the exact window of
// the turn. Started is emitted first and Done deferred immediately
// after, so the pair is either both or neither: a consumer's
// thinking indicator has no way to be raised and never lowered.
// The model's chat traffic lands on the session bus as a
// side effect of its `msg` / `me` tool calls; the bus's echo gate
// (RFC 2812 §3.3.1) means we never see our own messages come back,
// so [ModelClient.Send] files them into the rolling history buffer
// at the moment they're sent.
//
// `batch.causes` holds the span contexts the producers captured at
// emit time (see [protocol.Delivery]). Each valid one becomes an
// OTel link on the turn's span, so traces stay connected across the
// channel-based delivery boundary and a coalesced turn names every
// delivery that fed it.
//
// A cancelled context is the server tearing this client down — a
// KILL, a QUIT, or shutdown — so the turn ends without the
// `ModelUnavailableError` an upstream failure would raise. Nothing
// was unavailable; the client was closed.
func (mc *ModelClient) dispatchTurn(ctx context.Context, batch *turnBatch) {
	inst := mc.instance
	nick := inst.Nick()
	ch := batch.channel

	attrs := []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrModelID, string(inst.ModelID)),
		attribute.String(observability.AttrNick, string(nick)),
		attribute.String(observability.AttrInstanceID, string(inst.ID())),
	}

	_ = mc.inSpan(ctx, "modelclient.dispatch_turn", attrs, func(ctx context.Context, span trace.Span) error {
		for _, cause := range batch.causes {
			if cause.IsValid() {
				span.AddLink(trace.Link{SpanContext: cause})
			}
		}

		mc.sess.Emit(ctx, domain.ModelDispatchStarted{Instance: inst, At: mc.sess.Now()})
		defer mc.sess.Emit(ctx, domain.ModelDispatchDone{Instance: inst, At: mc.sess.Now()})

		window, err := dispatchWindowFor(ctx, mc.sess, ch, inst)
		if err != nil {
			return mc.reportTurnFailure(ctx, ch, nick, err)
		}

		apiClient := mc.apiFn()
		if apiClient == nil {
			mc.sess.Emit(ctx, domain.ModelUnavailableError{Channel: ch, Nick: nick, At: mc.sess.Now()})
			return nil
		}

		replyEvents := mc.hist.snapshotReplies()

		if err := dispatchToInstance(ctx, mc.sess, apiClient, mc.memStore, mc.tools, mc.ensure, mc.pacer, mc, window, inst, ch, batch.history, replyEvents, batch.triggers); err != nil {
			return mc.reportTurnFailure(ctx, ch, nick, errWithKind(err, observability.ErrorKindDispatch))
		}

		return nil
	})
}

// reportTurnFailure raises the operator diagnostic for a turn that
// could not run and hands the error back for the span to record. A
// context cancellation is teardown, not a failure of the model, so
// it is recorded on the span without a diagnostic.
func (mc *ModelClient) reportTurnFailure(ctx context.Context, ch domain.ChannelName, nick domain.Nick, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}

	mc.sess.Emit(ctx, domain.ModelUnavailableError{Channel: ch, Nick: nick, At: mc.sess.Now()})

	return err
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
