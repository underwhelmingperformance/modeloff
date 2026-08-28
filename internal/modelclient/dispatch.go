package modelclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
)

var errDispatchWindowClosed = errors.New("dispatch window is closed")

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
// current events it finds for one window go into a single turn in
// delivery order. Five messages that land while the model was busy
// are five lines in one prompt, which is both what a person reading
// a channel sees and four fewer round-trips than one turn each.
// [ModelClient.fileBatch] is where the split by window happens and
// where each window's history snapshot is taken.
//
// The history buffer feeds [ModelClient.dispatchTurn]'s prompt
// construction. Loaded for known channels at attach (see
// [ModelClient.loadHistory]) and for a DM window on first sight of it
// (see [history.seedDM]), the buffer is the only path the dispatch
// hot path reads conversation history from; the events log is
// consulted exclusively at load time.
//
// A turn the upstream lost to a transient failure comes back to this
// same select as a re-dispatch, one delay later (see
// [ModelClient.scheduleRedispatch]). The loop keeps draining
// deliveries while that delay runs, so a re-dispatch never holds the
// queue up. A turn raised for the same window in the meantime
// replaces the failed batch's payload but inherits its delay; the
// failed traffic is already in the new turn's history. `pending`
// retains the replacement batch until the delay ends.
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

	pending := make(redispatchSet)

	for {
		var batches []*turnBatch

		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case delivery := <-events:
			batches = mc.batchesForDeliveries(ctx, events, delivery, pending)
		case token := <-mc.redispatch:
			batch, ok := pending.claim(token)
			if !ok {
				continue
			}

			batches = []*turnBatch{batch}
		}

		for _, batch := range batches {
			// An earlier turn in this burst may have ended the
			// connection — a `quit` tool call cancels the loop's
			// context from inside the turn that made it. The
			// remaining windows belong to a client that has left, and
			// dispatching them would show peers a departed model
			// thinking.
			if ctx.Err() != nil {
				return
			}

			mc.runBatch(ctx, batch, pending)
		}
	}
}

func (mc *ModelClient) batchesForDeliveries(
	ctx context.Context,
	events <-chan protocol.Delivery,
	first protocol.Delivery,
	pending redispatchSet,
) []*turnBatch {
	deliveries := append([]protocol.Delivery{first}, drain(events)...)
	batches := mc.fileBatch(ctx, deliveries)

	closed := make(map[domain.ChannelName]bool)
	for _, delivery := range deliveries {
		if channel, closes := closedWindow(mc.instance, delivery.Event); closes {
			pending.supersede(channel)
			closed[channel] = true
		}

		if channel, opens := openedWindow(mc.instance, delivery.Event); opens {
			closed[channel] = false
		}
	}

	kept := batches[:0]
	for _, batch := range batches {
		if closed[batch.channel] && !invitationBatch(batch) {
			continue
		}

		batch = pending.merge(batch)
		if batch == nil || len(batch.triggers) == 0 {
			continue
		}

		kept = append(kept, batch)
	}

	return kept
}

// redispatchSet holds the re-dispatch waiting for each window.
//
// [ModelClient.runDispatchLoop] creates it and is the only goroutine
// that reaches it, which is why it carries no lock. A scheduler
// goroutine waiting out a delay never touches it: it hands its batch
// back over `mc.redispatch`, and the loop decides there whether the
// batch is still wanted.
type pendingRedispatch struct {
	token *turnBatch
	batch *turnBatch
}

type redispatchSet map[domain.ChannelName]*pendingRedispatch

// hold records `batch` as the re-dispatch its window is waiting for.
func (s redispatchSet) hold(batch *turnBatch) {
	s[batch.channel] = &pendingRedispatch{token: batch, batch: batch}
}

// supersede forgets whatever re-dispatch was waiting for `ch`.
func (s redispatchSet) supersede(ch domain.ChannelName) {
	delete(s, ch)
}

// merge replaces a waiting failed batch with newer traffic for the
// same window. The new history snapshot already contains the failed
// batch because fileBatch persisted it in the rolling transcript. The
// replacement keeps its own current events and span links, but waits
// for the provider-directed delay attached to the failed attempt.
func (s redispatchSet) merge(batch *turnBatch) *turnBatch {
	pending := s[batch.channel]
	if pending == nil {
		return batch
	}

	batch.retried = pending.batch.retried || batch.retried
	pending.batch = batch

	return nil
}

// claim returns the latest batch waiting under `token` and forgets it.
// A token cancelled when the window closed answers false.
func (s redispatchSet) claim(token *turnBatch) (*turnBatch, bool) {
	pending := s[token.channel]
	if pending == nil || pending.token != token {
		return nil, false
	}

	delete(s, token.channel)

	return pending.batch, true
}

// runBatch dispatches one batch and schedules the single re-dispatch a
// transient upstream failure earns it.
//
// New traffic for the same window replaces a waiting failed batch but
// remains under its provider-directed delay. The dispatch loop also
// cancels a pending batch when PART or KICK closes the window.
//
// The second attempt is the last: a provider that answered the same
// way twice is not having a moment, and a model that kept trying
// would spend the user's credits on a conversation that has already
// moved on.
func (mc *ModelClient) runBatch(ctx context.Context, batch *turnBatch, pending redispatchSet) {
	turnCtx, cancel := context.WithCancel(ctx)
	turn := &activeTurn{window: batch.channel, cancel: cancel}
	mc.mu.Lock()
	if mc.released {
		mc.mu.Unlock()
		cancel()
		return
	}
	mc.activeTurn = turn
	mc.mu.Unlock()

	err := mc.dispatchTurn(turnCtx, batch)
	cancel()

	mc.mu.Lock()
	if mc.activeTurn == turn {
		mc.activeTurn = nil
	}
	mc.mu.Unlock()

	var nonReplayable *nonReplayableTurnError
	if err == nil || batch.retried || errors.As(err, &nonReplayable) || !replayableTurnError(err) {
		return
	}

	if mc.scheduleRedispatch(ctx, batch, err) {
		pending.hold(batch)
	}
}

// replayableTurnError reports whether dispatching the batch again
// could answer differently. A transient upstream failure may have
// passed by the second attempt. A prompt the provider refused for
// length is the other case: the refusal raises the model's recorded
// token ratio, so the second attempt plans a smaller request and
// compacts where the first one did not.
func replayableTurnError(err error) bool {
	return api.Retryable(err) || errors.Is(err, api.ErrPromptTooLong)
}

// scheduleRedispatch hands `batch` back to the dispatch loop after the
// provider-directed or locally jittered delay, and reports whether it
// scheduled the attempt.
//
// The wait runs on a goroutine of its own and never on the dispatch
// goroutine, which has to stay on its select: deliveries arriving
// during the delay still have to reach the history ring, and a
// teardown still has to be answered at once. The goroutine joins the
// client's wait group, so `Wait` covers it, and so does the shutdown
// drain behind `Wait`; both of its own waits end on the loop's
// context, which [ModelClient.Release] cancels.
func (mc *ModelClient) scheduleRedispatch(
	ctx context.Context,
	batch *turnBatch,
	retryErr error,
) bool {
	batch.retried = true

	delay, err := mc.retry.durationFor(retryErr)
	if err != nil {
		slog.ErrorContext(ctx, "draw re-dispatch jitter, abandoning the retry",
			"component", "modelclient",
			"instance_id", mc.instance.ID(),
			"channel", batch.channel,
			"error", err,
		)

		return false
	}

	slog.InfoContext(ctx, "re-dispatching a turn after a transient upstream failure",
		"component", "modelclient",
		"instance_id", mc.instance.ID(),
		"channel", batch.channel,
		"delay", delay,
	)

	mc.wg.Go(func() {
		if err := mc.retry.waitFor(ctx, delay); err != nil {
			return
		}

		select {
		case mc.redispatch <- batch:
		case <-ctx.Done():
		}
	})

	return true
}

// recoverDispatchPanic ends the client's connection when its
// dispatch goroutine dies of a panic. Without it the subscription
// stays registered with nobody reading it: the instance would go on
// being a member of its channels, accumulating a backlog the server
// keeps for a consumer that no longer exists. The QUIT the
// disconnect broadcasts is what the channel sees, so the failure is
// visible where the client was.
//
// The failed goroutine cannot drain its own terminal deliveries.
// DisconnectDeadClient therefore reaps that subscription after the
// peer-visible teardown commits.
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

	mc.sess.DisconnectDeadClient(ctx, mc.Identity(), "Internal error")
}

// drain takes up to one history window of deliveries already queued
// on `events` without blocking. The caller has already taken the
// first delivery, so this function leaves one slot for it. Any
// remainder starts the next chronological turn.
func drain(events <-chan protocol.Delivery) []protocol.Delivery {
	rest := make([]protocol.Delivery, 0, modelHistorySize-1)

	for len(rest) < modelHistorySize-1 {
		select {
		case d := <-events:
			rest = append(rest, d)
		default:
			return rest
		}
	}

	return rest
}

// turnBatch is one window's worth of a burst. History and replies are
// the pre-burst context. Events preserve every renderable delivery in
// current order. Triggers retain the subset that grants dispatch
// authority, and causes link those triggering deliveries to the turn.
type turnBatch struct {
	channel    domain.ChannelName
	history    []domain.StoredEvent
	replies    []storedReply
	events     []protocol.IRCMessage
	triggers   []protocol.IRCMessage
	causes     []trace.SpanContext
	historyErr error

	// retried records that this batch has already been handed back to
	// the dispatch loop once, which is what bounds a failing turn to
	// two attempts.
	retried bool
}

// fileBatch files `deliveries` into the per-channel history buffers
// and returns the turns they call for, one per window, in the order
// the windows first appeared in the burst.
//
// Every window that will take a turn is snapshotted before anything
// from the burst is filed. The burst's triggers and non-triggers are
// then appended to its current event block in arrival order, because
// a model catching up on five messages has to see the topic or mode
// changes that happened between them in the same positions.
func (mc *ModelClient) fileBatch(ctx context.Context, deliveries []protocol.Delivery) []*turnBatch {
	// Opening the batches is what loads a direct-message window's stored
	// thread, which files into the reflection stream through the seed
	// callback. Recording the burst first would put a message that starts
	// the turn ahead of the conversation it answers, and the prompt reads
	// them the other way round.
	batches, byWindow := mc.openBatches(ctx, deliveries)
	mc.recordReflectionDeliveries(ctx, deliveries)

	for _, delivery := range deliveries {
		ch, irc, isTrigger := dispatchTrigger(mc.instance, delivery.Event)
		isTrigger = isTrigger && !delivery.HistoryOnly

		if isTrigger {
			fileTrigger(byWindow[ch], irc, delivery.SpanCtx)
		}

		if mc.fileIssuerReply(delivery.Event, batches, byWindow) {
			continue
		}

		mc.fileChannelActivity(ctx, delivery, ch, isTrigger, byWindow)
	}

	return batches
}

func fileTrigger(batch *turnBatch, message protocol.IRCMessage, cause trace.SpanContext) {
	batch.events = append(batch.events, message)
	batch.triggers = append(batch.triggers, message)
	batch.causes = append(batch.causes, cause)
}

func (mc *ModelClient) fileIssuerReply(
	event domain.ProtocolEvent,
	batches []*turnBatch,
	byWindow map[domain.ChannelName]*turnBatch,
) bool {
	reply, ok := event.(domain.IssuerReply)
	if !ok {
		return false
	}

	target := domain.EventTarget(reply)
	var window protocol.WindowTarget
	if target != "" {
		window = protocol.WindowTargetForKey(target)
	}
	mc.hist.appendReply(window, domain.StoredEvent{Event: reply})

	message, renderable := protocol.FromChannelEvent(reply)
	if !renderable {
		return true
	}
	if target != "" {
		if batch := byWindow[target]; batch != nil {
			batch.events = append(batch.events, message)
		}

		return true
	}

	for _, batch := range batches {
		batch.events = append(batch.events, message)
	}

	return true
}

func (mc *ModelClient) fileChannelActivity(
	ctx context.Context,
	delivery protocol.Delivery,
	triggerWindow domain.ChannelName,
	isTrigger bool,
	byWindow map[domain.ChannelName]*turnBatch,
) {
	activity, ok := delivery.Event.(domain.ChannelActivity)
	if !ok {
		return
	}

	if target, opened := openedWindow(mc.instance, delivery.Event); opened {
		mc.hist.forget(target)
		resetOpenedBatch(byWindow[target])
	}
	if target, closed := closedWindow(mc.instance, delivery.Event); closed {
		mc.hist.forget(target)
		resetClosedBatch(byWindow[target])

		return
	}

	stored := domain.StoredEvent{Event: activity}
	for _, target := range historyTargets(mc.instance.ID(), delivery) {
		mc.fileWindowActivity(ctx, target, triggerWindow, isTrigger, stored, byWindow[target])
	}
}

func resetOpenedBatch(batch *turnBatch) {
	if batch == nil {
		return
	}

	batch.history = nil
	batch.replies = nil
	batch.historyErr = nil
}

func resetClosedBatch(batch *turnBatch) {
	if batch == nil {
		return
	}

	batch.history = nil
	batch.replies = nil
	batch.events = nil
	batch.triggers = nil
	batch.causes = nil
	batch.historyErr = nil
	batch.retried = false
}

func (mc *ModelClient) fileWindowActivity(
	ctx context.Context,
	target domain.ChannelName,
	triggerWindow domain.ChannelName,
	isTrigger bool,
	stored domain.StoredEvent,
	batch *turnBatch,
) {
	if batch != nil && batch.historyErr != nil {
		mc.hist.appendAfterSeedFailure(target, stored)
	} else if err := mc.hist.append(ctx, stored, target); err != nil && batch != nil {
		batch.historyErr = fmt.Errorf("load history for %q: %w", target, err)
	}
	if isTrigger && target == triggerWindow {
		return
	}
	if batch == nil {
		return
	}

	if message, ok := protocol.FromChannelEvent(stored.Event); ok {
		batch.events = append(batch.events, message)
	}
}

func openedWindow(self *domain.Instance, event domain.ProtocolEvent) (domain.ChannelName, bool) {
	join, ok := event.(domain.Join)
	return join.Target, ok && sourceIs(join.Source, self.ID())
}

func closedWindow(self *domain.Instance, event domain.ProtocolEvent) (domain.ChannelName, bool) {
	switch e := event.(type) {
	case domain.Part:
		return e.Target, sourceIs(e.Source, self.ID())
	case domain.Kicked:
		return e.Target, e.SubjectIsSelf
	default:
		return "", false
	}
}

// openBatches allocates one batch per window the burst will raise a
// turn for, in the order those windows first appear, and seeds each
// with its window's transcript as it stands before the burst.
//
// It is a pass of its own because the snapshots have to be taken
// while none of the burst has been filed: a window whose first
// trigger is the last delivery still needs the transcript from
// before the first.
//
// A DM window the client has not seen this connection is loaded from
// the store here, as part of taking its snapshot, so a turn is
// prompted from the conversation as it already stood. The load has to
// happen in this pass and not while the burst is filed: the snapshot
// a turn reads is taken here, so a load running after it leaves the
// first DM turn of a connection with an empty transcript.
func (mc *ModelClient) openBatches(ctx context.Context, deliveries []protocol.Delivery) ([]*turnBatch, map[domain.ChannelName]*turnBatch) {
	var batches []*turnBatch

	byWindow := make(map[domain.ChannelName]*turnBatch)

	for _, delivery := range deliveries {
		ch, _, isTrigger := dispatchTrigger(mc.instance, delivery.Event)
		if !isTrigger || delivery.HistoryOnly {
			continue
		}

		if _, ok := byWindow[ch]; ok {
			continue
		}

		history, err := mc.hist.snapshot(ctx, ch)
		batch := &turnBatch{
			channel:    ch,
			history:    history,
			replies:    mc.hist.snapshotRepliesFor(ch),
			historyErr: err,
		}
		byWindow[ch] = batch
		batches = append(batches, batch)
	}

	return batches, byWindow
}

// historyTargets returns the buffer slot(s) the delivery's event
// should be filed under for the receiving model-client's
// dispatch-turn history. Most events belong to a single window: the
// channel they happened in, or for chat traffic the conversation
// [domain.Message.RoutingKey] places them in, which for a DM is the
// counterpart either way round. Actor-scoped events ([domain.Quit]
// and [domain.NickChange]) carry no target on the wire (RFC 2812
// §3.1.7 and §3.1.2); the per-recipient channel list is on
// `delivery.Targets`, pre-computed by the session's fan-out as the
// intersection of the actor's channel set with the recipient's.
//
// Events with no target (PokeEvent, NamesReplyEvent, …) return
// nil and are skipped: they are not LLM-prompt material.
func historyTargets(selfID domain.InstanceID, delivery protocol.Delivery) []domain.ChannelName {
	switch e := delivery.Event.(type) {
	case domain.Message:
		key, ok := e.RoutingKey(selfID)
		if !ok {
			return nil
		}

		return []domain.ChannelName{key}
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
// take a dispatch turn, and if so returns the window the turn runs in
// and the wire-shaped trigger message the LLM call uses as context.
//
// Chat traffic names its window through [domain.Message.RoutingKey],
// so a DM raises its turn under the counterpart whichever way the
// message was going. A DM between two other clients belongs to
// neither party's window and raises no turn.
func dispatchTrigger(self *domain.Instance, ev domain.ProtocolEvent) (domain.ChannelName, protocol.IRCMessage, bool) {
	switch e := ev.(type) {
	case domain.Message:
		key, ok := e.RoutingKey(self.ID())
		if !ok {
			return "", protocol.IRCMessage{}, false
		}

		msg, _ := protocol.FromChannelEvent(e)

		return key, msg, true

	case domain.Join:
		if sourceIs(e.Source, self.ID()) {
			return "", protocol.IRCMessage{}, false
		}

		msg, _ := protocol.FromChannelEvent(e)
		return e.Target, msg, true

	case domain.Part:
		if sourceIs(e.Source, self.ID()) {
			return "", protocol.IRCMessage{}, false
		}

		msg, _ := protocol.FromChannelEvent(e)
		return e.Target, msg, true

	case domain.Invited:
		msg, _ := protocol.FromChannelEvent(e)

		return e.Target, msg, true

	case domain.PokeEvent:
		return e.Channel, protocol.IRCMessage{
			Kind: protocol.KindPoke, Source: domain.ServerSource("modeloff"),
			Target: string(e.Channel),
			Body:   "the channel is quiet. if something comes to mind, say it — otherwise just lurk. don't force it.",
			At:     e.At,
		}, true
	}

	return "", protocol.IRCMessage{}, false
}

func sourceIs(source domain.Source, id domain.InstanceID) bool {
	sourceID, ok := source.InstanceID()
	return ok && sourceID == id
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
// A cancelled context is the server tearing this client down after a
// KILL, a QUIT, or shutdown, so the turn ends without the
// `ModelUnavailableError` an upstream failure would raise. Nothing
// was unavailable; the client was closed.
//
// The returned error is what [ModelClient.runBatch] classifies to
// decide whether the turn is worth a second attempt. A turn with no
// API client behind it returns nil: no key configured is a state the
// user changes, not a condition that passes.
func (mc *ModelClient) dispatchTurn(ctx context.Context, batch *turnBatch) error {
	inst := mc.instance
	nick := mc.nick()
	ch := batch.channel

	attrs := []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrModelID, string(inst.ModelID)),
		attribute.String(observability.AttrNick, string(nick)),
		attribute.String(observability.AttrInstanceID, string(inst.ID())),
	}

	return mc.inSpan(ctx, "modelclient.dispatch_turn", attrs, func(ctx context.Context, span trace.Span) error {
		for _, cause := range batch.causes {
			if cause.IsValid() {
				span.AddLink(trace.Link{SpanContext: cause})
			}
		}

		guard, err := mc.captureWindowGuard(ctx, batch)
		if errors.Is(err, errDispatchWindowClosed) {
			return nil
		}
		if err != nil {
			return mc.reportTurnFailure(ctx, ch, nick, err)
		}
		if batch.historyErr != nil {
			return &nonReplayableTurnError{
				Err: mc.reportTurnFailure(ctx, ch, nick, batch.historyErr),
			}
		}

		window, err := dispatchWindowFor(ctx, guard, ch)
		if errors.Is(err, errDispatchWindowClosed) {
			return nil
		}
		if err != nil {
			return mc.reportTurnFailure(ctx, ch, nick, err)
		}

		apiClient := mc.apiFn()
		if apiClient == nil {
			mc.sess.EmitModelFailure(ctx, window.Target(), domain.ModelUnavailableError{
				Source: domain.ClientSource(inst.ID(), nick),
				Reason: domain.ModelFailureNoAPIKey,
				At:     mc.sess.Now(),
			})
			return nil
		}

		dispatch := mc.sess.BeginModelDispatch(ctx, guard, window.Target(), domain.ModelDispatchStarted{
			Source: domain.ClientSource(inst.ID(), inst.Nick()),
			At:     mc.sess.Now(),
		})
		defer dispatch.Done(ctx, domain.ModelDispatchDone{
			Source: domain.ClientSource(inst.ID(), inst.Nick()),
			At:     mc.sess.Now(),
		})

		turn := turnRequest{
			api:    apiClient,
			window: window,
			guard:  guard,
			// The turn addresses the window it is running in, and this
			// is the only place that address is built: `batch.channel`
			// comes from [dispatchTrigger], which names a window only
			// alongside a turn to run in it, so a tool cannot be handed
			// a target derived from a window that was never there.
			target:   targetForContext(window),
			history:  batch.history,
			replies:  batch.replies,
			events:   batch.events,
			triggers: batch.triggers,
		}

		if err := mc.dispatchToInstance(ctx, turn); err != nil {
			if errors.Is(err, errDispatchWindowClosed) {
				return nil
			}
			return mc.reportTurnFailure(ctx, ch, nick, observability.ErrWithKind(err, observability.ErrorKindDispatch))
		}

		return nil
	})
}

// captureWindowGuard records the authority that must remain valid
// until the upstream request is sent.
func (mc *ModelClient) captureWindowGuard(
	ctx context.Context,
	batch *turnBatch,
) (protocol.WindowGuard, error) {
	mc.mu.Lock()
	sub := mc.sub
	mc.mu.Unlock()
	if sub == nil {
		return nil, errDispatchWindowClosed
	}

	var (
		guard protocol.WindowGuard
		err   error
	)
	if invitationBatch(batch) {
		guard, err = sub.GuardInvitation(ctx, batch.channel)
	} else {
		guard, err = sub.GuardWindow(ctx, protocol.WindowTargetForKey(batch.channel))
	}
	if err == nil {
		return guard, nil
	}
	if errors.Is(err, protocol.ErrSubscriptionClosed) {
		return nil, errDispatchWindowClosed
	}

	var notOnChannel domain.NotOnChannelError
	if errors.As(err, &notOnChannel) {
		return nil, errDispatchWindowClosed
	}

	return nil, err
}

func invitationBatch(batch *turnBatch) bool {
	if len(batch.triggers) == 0 {
		return false
	}

	for _, trigger := range batch.triggers {
		if trigger.Kind != protocol.KindInvite {
			return false
		}
	}

	return true
}

// reportTurnFailure raises the operator diagnostic for a turn that
// could not run and hands the error back for the span to record. A
// context cancellation is teardown, not a failure of the model, so
// it is recorded on the span without a diagnostic.
func (mc *ModelClient) reportTurnFailure(ctx context.Context, ch domain.ChannelName, nick domain.Nick, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}

	mc.sess.EmitModelFailure(ctx, protocol.WindowTargetForKey(ch), domain.ModelUnavailableError{
		Source: domain.ClientSource(mc.instance.ID(), nick),
		Reason: modelFailureReason(err),
		At:     mc.sess.Now(),
	})

	return err
}

// modelFailureReason classifies what stopped a turn, so the line the
// operator reads names something to do about it. A fault the dispatch
// path raised for itself has no provider status behind it and keeps the
// unclassified reason.
//
// 401 and 403 are different answers. A provider returns 401 when it will
// not accept the key at all, and 403 when it accepts the key and refuses
// the request anyway, which is what an account without access to a model
// receives. Telling somebody to check a key the provider just accepted
// sends them to the wrong place.
func modelFailureReason(err error) domain.ModelFailureReason {
	var contextWindowExceeded *ContextWindowExceededError
	if errors.As(err, &contextWindowExceeded) {
		return domain.ModelFailureContextWindow
	}

	status, fromProvider := api.FailureStatus(err)
	if !fromProvider {
		return domain.ModelFailureUnavailable
	}

	switch status {
	case http.StatusUnauthorized:
		return domain.ModelFailureAuth
	case http.StatusForbidden:
		return domain.ModelFailureForbidden
	case http.StatusPaymentRequired:
		return domain.ModelFailureNoCredit
	case http.StatusNotFound:
		return domain.ModelFailureUnknownModel
	case http.StatusTooManyRequests:
		return domain.ModelFailureRateLimited
	case http.StatusBadRequest:
		return domain.ModelFailureBadRequest
	}

	return domain.ModelFailureUpstream
}

// dispatchWindowFor asks the actor-bound guard for the current window
// view. Invitation turns have no membership interval yet and are
// represented by the caller as a synthetic channel window.
func dispatchWindowFor(
	ctx context.Context,
	guard protocol.WindowGuard,
	target domain.ChannelName,
) (protocol.WindowContext, error) {
	if guard == nil {
		return nil, fmt.Errorf("%w: %s", errDispatchWindowClosed, target)
	}

	window, err := guard.Context(ctx)
	if err != nil {
		return nil, err
	}
	if protocol.WindowKey(window.Target()) != target {
		return nil, fmt.Errorf("window guard returned %q for %q", protocol.WindowKey(window.Target()), target)
	}

	return window, nil
}

func targetForContext(window protocol.WindowContext) protocol.MsgTarget {
	if peer, ok := protocol.DirectWindowPeer(window.Target()); ok {
		return protocol.ClientTarget(peer)
	}
	if channel, ok := protocol.ChannelWindowName(window.Target()); ok {
		return protocol.ChannelTarget(channel)
	}

	return nil
}
