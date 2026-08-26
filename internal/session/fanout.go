package session

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

type routedDelivery struct {
	sub                   *serverClient
	delivery              protocol.Delivery
	eventID               int64
	historyOnly           bool
	terminal              bool
	connectionAuthority   *connectionAuthority
	membershipAuthorities []membershipAuthority
	dispatchAuthority     *dispatchAuthority
}

type connectionAuthority struct {
	generation uint64
}

type membershipAuthority struct {
	window domain.ChannelName
	epoch  uint64
}

type dispatchAuthority struct {
	window     *windowGuard
	invitation *invitationGuard
}

type modelDispatchRecipient struct {
	sub                  *serverClient
	connectionGeneration uint64
	source               domain.Source
	targets              []domain.ChannelName
	window               protocol.WindowTarget
}

type modelDispatch struct {
	sess       *Session
	recipients []modelDispatchRecipient
	once       sync.Once
}

func (d *modelDispatch) Done(ctx context.Context, event domain.ModelDispatchDone) {
	d.once.Do(func() {
		routes := make([]routedDelivery, 0, len(d.recipients))
		spanCtx := trace.SpanContextFromContext(ctx)
		for _, recipient := range d.recipients {
			event.Source = recipient.source
			routes = append(routes, routedDelivery{
				sub: recipient.sub,
				connectionAuthority: &connectionAuthority{
					generation: recipient.connectionGeneration,
				},
				delivery: protocol.Delivery{
					Event: event, Targets: recipient.targets, Window: recipient.window, SpanCtx: spanCtx,
				},
			})
		}

		d.sess.enqueueRoutes(ctx, routes)
	})
}

// BeginModelDispatch emits a turn start and captures the subscriptions
// that received it so the returned lifecycle can send them the matching
// completion even if membership changes during the turn.
func (s *Session) BeginModelDispatch(
	ctx context.Context,
	guard protocol.WindowGuard,
	window protocol.WindowTarget,
	event domain.ModelDispatchStarted,
) protocol.ModelDispatch {
	id, identified := event.Source.InstanceID()
	if !identified {
		return &modelDispatch{sess: s}
	}
	authority, valid := s.modelDispatchAuthority(guard, window, id)
	if !valid {
		return &modelDispatch{sess: s}
	}

	var candidates []*serverClient
	var targets []domain.ChannelName
	var anonymous []domain.ChannelName
	var scope deliveryScope
	direct := false
	switch protocol.WindowTargetKind(window) {
	case domain.KindChannel:
		channel := protocol.WindowKey(window)
		targets = []domain.ChannelName{channel}
		scope = channelScope{channel}
		anonymous = s.anonymousChannels(ctx, targets)
		candidates = s.subscriberSnapshot()
	case domain.KindDM:
		direct = true
		peer, _ := protocol.DirectWindowPeer(window)
		for _, participant := range []protocol.ClientID{protocol.ClientID(id), protocol.ClientID(peer)} {
			if sub := s.activeClientHandle(participant); sub != nil && !slices.Contains(candidates, sub) {
				candidates = append(candidates, sub)
			}
		}
	default:
		return &modelDispatch{sess: s}
	}

	spanCtx := trace.SpanContextFromContext(ctx)
	var routes []routedDelivery
	for _, sub := range candidates {
		connectionGeneration, active := sub.connection()
		if !active {
			continue
		}

		var membershipAuthorities []membershipAuthority
		if !direct {
			var admitted bool
			_, _, membershipAuthorities, admitted = admitScopedDelivery(scope, sub)
			if !admitted {
				continue
			}
		}

		projected, recipientTargets := s.projectEventForRecipient(
			ctx, event, sub, targets, anonymous, window,
		)
		if projected == nil || (!direct && !sub.canReceive(projected, recipientTargets)) {
			continue
		}
		started := projected.(domain.ModelDispatchStarted)
		recipientTargets = slices.Clone(recipientTargets)
		recipientWindow := window
		if peer, ok := protocol.DirectWindowPeer(window); ok && sub.id == protocol.ClientID(peer) {
			recipientWindow = protocol.DirectWindowTarget(id)
		}
		routes = append(routes, routedDelivery{
			sub:                   sub,
			connectionAuthority:   &connectionAuthority{generation: connectionGeneration},
			membershipAuthorities: membershipAuthorities,
			dispatchAuthority:     authority,
			delivery: protocol.Delivery{
				Event: started, Targets: recipientTargets, Window: recipientWindow, SpanCtx: spanCtx,
			},
		})
	}
	queued := s.enqueueRoutes(ctx, routes)
	recipients := make([]modelDispatchRecipient, 0, len(queued))
	for _, route := range queued {
		started := route.delivery.Event.(domain.ModelDispatchStarted)
		recipients = append(recipients, modelDispatchRecipient{
			sub: route.sub, connectionGeneration: route.connectionAuthority.generation,
			source:  started.Source,
			targets: route.delivery.Targets, window: route.delivery.Window,
		})
	}

	return &modelDispatch{sess: s, recipients: recipients}
}

func (s *Session) modelDispatchAuthority(
	guard protocol.WindowGuard,
	window protocol.WindowTarget,
	actor domain.InstanceID,
) (*dispatchAuthority, bool) {
	switch guard := guard.(type) {
	case windowGuard:
		if guard.client.sess != s || guard.client.instance.ID() != actor ||
			!protocol.EqualWindowTarget(guard.target, window) {
			return nil, false
		}

		return &dispatchAuthority{window: &guard}, true
	case invitationGuard:
		target := protocol.ChannelWindowTarget(guard.channel)
		if guard.client.sess != s || guard.client.instance.ID() != actor ||
			!protocol.EqualWindowTarget(target, window) {
			return nil, false
		}

		return &dispatchAuthority{invitation: &guard}, true
	default:
		return nil, false
	}
}

func (a *dispatchAuthority) clients() []*serverClient {
	if a == nil {
		return nil
	}
	if a.window != nil {
		return []*serverClient{a.window.client, a.window.dmPeer}
	}
	if a.invitation != nil {
		return []*serverClient{a.invitation.client}
	}

	return nil
}

func (a *dispatchAuthority) validLocked(ctx context.Context) bool {
	if a == nil {
		return true
	}
	if a.window != nil {
		return a.window.validLocked(ctx)
	}
	if a.invitation != nil {
		return a.invitation.validLocked(ctx)
	}

	return false
}

// subscriberSnapshot returns a stable copy of the subscriber set
// under the read lock so callers iterating it cannot race with a
// concurrent registration or deregistration. The returned slice's
// `*serverClient` pointers are shared with the registry.
func (s *Session) subscriberSnapshot() []*serverClient {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()

	snap := make([]*serverClient, 0, len(s.subscribers))
	for _, sub := range s.subscribers {
		snap = append(snap, sub)
	}

	// Go map iteration is randomised per process, which leaks into
	// per-fan-out delivery order: model-client dispatch goroutines
	// wake in different orders across runs, and the lifecycle
	// events they emit then interleave with the main goroutine's
	// emits in a non-deterministic order on the user-client's
	// buffered events channel. Sort by ClientID so fan-out
	// iteration is stable; the user-client (sentinel empty id)
	// always sorts first, then model-clients lexicographically by
	// instance id.
	slices.SortFunc(snap, func(a, b *serverClient) int {
		return strings.Compare(string(a.id), string(b.id))
	})

	return snap
}

// fanOutProtocol delivers a protocol event to every active
// subscription that should receive it. Each delivery is handed to
// the subscription and the call returns: a consumer that has
// stopped reading grows its own backlog and holds up nobody else.
// This is what lets the command loop fan out safely — a model that
// is mid-turn, and therefore not draining its events channel, can
// still be sent to while it waits on the loop for a command of its
// own.
//
// Chat traffic (PRIVMSG/Action) is delivered to every member of the
// target window except the sender during membership fan-out (RFC
// 2812 §3.3.1); a sender holding IRCv3 echo-message then receives a
// direct echo of its own line via [Session.echoToOriginator]. Other
// event types — JOIN, PART, MODE, TOPIC, NICK, etc. — are delivered
// to every member-subscriber including the originator, matching IRC's
// behaviour for those signals.
//
// Membership filtering keeps every client — the user-client
// included — from receiving events for windows it is not in. The
// user-client is a member of whatever it has joined, so the
// chat-screen renders exactly those windows.
type protocolRoutePlan struct {
	routes             []routedDelivery
	event              domain.ProtocolEvent
	original           domain.ProtocolEvent
	anonymous          []domain.ChannelName
	spanCtx            trace.SpanContext
	suppressOriginator bool
	sender             protocol.ClientID
}

func (s *Session) fanOutProtocol(ctx context.Context, emission protocolEmission, eventID int64) {
	s.noteChatActivity(emission.event)
	plan := s.planProtocolEmission(ctx, emission, eventID)
	s.enqueueRoutes(ctx, plan.routes)
	s.finishProtocolEmission(ctx, plan, eventID)
}

func (s *Session) planProtocolEmission(
	ctx context.Context,
	emission protocolEmission,
	eventID int64,
) protocolRoutePlan {
	pe := emission.event
	plan := protocolRoutePlan{
		event:    pe,
		original: pe,
		spanCtx:  trace.SpanContextFromContext(ctx),
	}
	plan.suppressOriginator, plan.sender = chatTrafficSender(pe)

	// `+a` rewrites the visible nick on chat-traffic events to
	// [domain.AnonymousNick] (RFC 2811 §4.2.1) before delivery, so
	// even the channel's own members can't see who sent what. The
	// stored event retains the real From for audit. The actor-scoped
	// events are masked further down, per recipient, by
	// [maskActorEvent].
	pe = anonymiseIfNeeded(ctx, s, pe)

	// Actor-scoped events ([domain.Quit] and [domain.NickChange])
	// carry no target on the wire; the per-recipient channel list
	// is computed at fan-out time as the intersection of the
	// actor's live membership and each recipient's. Snapshot the
	// actor's channels once so the per-sub loop does not re-walk
	// the ordered map.
	actorChannels := scopedChannels(emission.scope)
	plan.anonymous = s.anonymousChannels(ctx, actorChannels)
	if shared, ok := emission.scope.(sharedChannelsScope); ok {
		for _, channel := range shared.masked {
			if !slices.Contains(plan.anonymous, channel) {
				plan.anonymous = append(plan.anonymous, channel)
			}
		}
	}

	for _, sub := range s.subscriberSnapshot() {
		targets, direct, membershipAuthorities, eligible := admitScopedDelivery(
			emission.scope, sub,
		)
		if !eligible {
			continue
		}

		if plan.suppressOriginator && sub.Identity() == plan.sender {
			continue
		}

		event, targets := s.projectEventForRecipient(
			ctx, pe, sub, targets, plan.anonymous, emission.window,
		)
		if event == nil {
			continue
		}
		if !direct && !sub.canReceive(event, targets) {
			continue
		}

		plan.routes = append(plan.routes, routedDelivery{
			sub:                   sub,
			eventID:               eventID,
			terminal:              eventTerminatesRecipient(pe, sub),
			membershipAuthorities: membershipAuthorities,
			delivery: protocol.Delivery{
				Event:   event,
				Targets: targets,
				Window:  recipientEventWindow(emission.window, event, sub),
				SpanCtx: plan.spanCtx,
			},
		})
	}

	if plan.suppressOriginator {
		origin := s.activeClientHandle(plan.sender)
		if origin != nil && !origin.echo && origin.replayCapable {
			if _, ok := plan.original.(domain.Message); ok {
				plan.routes = append(plan.routes, routedDelivery{
					sub:         origin,
					delivery:    protocol.Delivery{Event: plan.original, SpanCtx: plan.spanCtx},
					eventID:     eventID,
					historyOnly: true,
				})
			}
		}
	}

	plan.event = pe

	return plan
}

func (s *Session) finishProtocolEmission(
	ctx context.Context,
	plan protocolRoutePlan,
	eventID int64,
) {
	if quit, ok := plan.event.(domain.Quit); ok {
		s.partAnonymousChannels(ctx, quit, plan.anonymous, plan.spanCtx)
	}

	if plan.suppressOriginator {
		s.echoToOriginator(ctx, plan.original, plan.sender, plan.spanCtx, eventID)
	}
}

func (s *Session) commitChannelMessage(
	ctx context.Context,
	channel domain.ChannelName,
	message domain.Message,
) error {
	plan := s.planProtocolEmission(ctx, protocolEmission{
		event: message,
		scope: channelScope{channel: channel},
	}, 0)
	locked := lockRouteSubscriptions(plan.routes)
	plan.routes = authorisedRoutesLocked(ctx, plan.routes)
	records, indexes := projectedScrollbackRecords(plan.routes)

	committed, err := s.store.CommitChannelEvent(ctx, store.ChannelEvent{
		Channel: channel, Event: message, Scrollback: records,
	})
	if err != nil {
		for _, sub := range slices.Backward(locked) {
			sub.replayMu.Unlock()
		}
		s.recordPersistenceFailure(ctx, channel)

		return err
	}

	for i := range plan.routes {
		plan.routes[i].eventID = committed.EventID
	}
	for _, sub := range locked {
		sub.outMu.Lock()
	}
	_, overflowed := queueRoutesLocked(
		plan.routes, indexes, committed.ScrollbackIDs, len(records),
	)
	for _, sub := range slices.Backward(locked) {
		sub.outMu.Unlock()
		sub.replayMu.Unlock()
	}

	s.noteChatActivity(message)
	for _, sub := range overflowed {
		s.disconnectOverflowed(ctx, sub)
	}
	s.finishProtocolEmission(ctx, plan, committed.EventID)

	return nil
}

func recipientEventWindow(
	window protocol.WindowTarget,
	event domain.ProtocolEvent,
	recipient *serverClient,
) protocol.WindowTarget {
	direct, ok := protocol.DirectWindowPeer(window)
	if !ok {
		return window
	}

	failure, ok := event.(domain.ModelUnavailableError)
	if !ok {
		return nil
	}
	actor, identified := failure.Source.InstanceID()
	if !identified {
		return nil
	}
	if recipient.id == protocol.ClientID(actor) {
		return window
	}
	if recipient.id == protocol.ClientID(direct) {
		return protocol.DirectWindowTarget(actor)
	}

	return nil
}

func (s *Session) channelEventRoutes(
	ctx context.Context,
	event broadcastEvent,
	channel domain.ChannelName,
) []routedDelivery {
	spanCtx := trace.SpanContextFromContext(ctx)
	anonymous := s.anonymousChannels(ctx, []domain.ChannelName{channel})

	var routes []routedDelivery
	for _, sub := range s.subscriberSnapshot() {
		projected, targets := s.projectEventForRecipient(ctx, event, sub, nil, anonymous, nil)
		if projected == nil || !sub.canReceive(projected, targets) {
			continue
		}

		routes = append(routes, routedDelivery{
			sub: sub,
			delivery: protocol.Delivery{
				Event:   projected,
				Targets: targets,
				SpanCtx: spanCtx,
			},
		})
	}

	return routes
}

func (s *Session) projectEventForRecipient(
	ctx context.Context,
	event domain.ProtocolEvent,
	recipient *serverClient,
	targets, anonymous []domain.ChannelName,
	window protocol.WindowTarget,
) (domain.ProtocolEvent, []domain.ChannelName) {
	if kicked, ok := event.(domain.Kicked); ok {
		kicked.SubjectIsSelf = domain.EqualNick(kicked.Subject, recipient.instance.Nick())
		event = kicked
	}

	if failure, ok := event.(domain.ModelUnavailableError); ok {
		channel, channelWindow := protocol.ChannelWindowName(window)
		if modes, found := s.channelModes(ctx, channel); channelWindow && found && modes.Anonymous {
			if !sourceBelongsTo(failure.Source, recipient.instance.ID()) {
				failure.Source = domain.AnonymousSource()
			}

			return failure, []domain.ChannelName{channel}
		}
	}

	projected, targets := maskActorEvent(event, recipient, targets, anonymous)
	if projected == nil {
		return nil, nil
	}

	if s.eventTargetsAnonymousChannel(ctx, projected) {
		projected = maskChannelEvent(projected, recipient)
	}

	return projected, targets
}

func scopedChannels(scope deliveryScope) []domain.ChannelName {
	shared, ok := scope.(sharedChannelsScope)
	if !ok {
		return nil
	}

	return shared.channels
}

func (s *Session) eventTargetsAnonymousChannel(
	ctx context.Context,
	event domain.ProtocolEvent,
) bool {
	persistable, ok := event.(domain.PersistableEvent)
	if !ok {
		return false
	}

	target := domain.EventTarget(persistable)
	if domain.InferChannelKind(target) != domain.KindChannel {
		return false
	}

	modes, ok := s.channelModes(ctx, target)
	return ok && modes.Anonymous
}

// anonymousChannels returns the subset of `channels` carrying `+a`.
// It is read once per fan-out so the per-subscriber loop compares
// against a small slice; an empty or nil argument reads nothing.
func (s *Session) anonymousChannels(ctx context.Context, channels []domain.ChannelName) []domain.ChannelName {
	var anonymous []domain.ChannelName

	for _, ch := range channels {
		if modes, ok := s.channelModes(ctx, ch); ok && modes.Anonymous {
			anonymous = append(anonymous, ch)
		}
	}

	return anonymous
}

// maskActorEvent applies `+a` to an actor-scoped event on its way to
// one recipient, and returns what that recipient should be sent
// along with the targets to send it under. A nil event means send
// nothing.
//
// A recipient that shares only anonymous channels with the actor may
// not learn who the actor is (RFC 2811 §4.2.1), so:
//
//   - a `QUIT` is withheld and the anonymous channels receive a
//     `PART` in its place, per [Session.partAnonymousChannels];
//   - a peer's `NICK` is withheld when there is no named channel to
//     carry it under;
//   - a peer's dispatch-lifecycle event is withheld when its turn
//     window is anonymous, while the actor still receives its own
//     event so it can settle its lifecycle state.
//
// A recipient that also shares a non-anonymous channel already knows
// the actor from there, and receives the event unmasked but scoped
// to those channels alone.
func maskActorEvent(
	pe domain.ProtocolEvent,
	recipient *serverClient,
	targets, anonymous []domain.ChannelName,
) (domain.ProtocolEvent, []domain.ChannelName) {
	if len(anonymous) == 0 {
		return pe, targets
	}

	named := namedChannels(targets, anonymous)

	switch e := pe.(type) {
	case domain.Quit:
		if len(named) == 0 {
			return nil, nil
		}

		return e, named

	case domain.ModelDispatchStarted:
		if sourceBelongsTo(e.Source, recipient.instance.ID()) {
			return e, targets
		}
		if len(named) > 0 {
			return e, named
		}

		return nil, nil

	case domain.ModelDispatchDone:
		if sourceBelongsTo(e.Source, recipient.instance.ID()) {
			return e, targets
		}
		if len(named) > 0 {
			return e, named
		}

		return nil, nil

	case domain.NickChange:
		if sourceBelongsTo(e.Source, recipient.instance.ID()) {
			return e, targets
		}
		if len(named) == 0 {
			return nil, nil
		}

		return e, named
	}

	return pe, targets
}

func maskChannelEvent(
	event domain.ProtocolEvent,
	recipient *serverClient,
) domain.ProtocolEvent {
	recipientID := recipient.instance.ID()
	recipientNick := recipient.instance.Nick()

	switch e := event.(type) {
	case domain.Join:
		if !sourceBelongsTo(e.Source, recipientID) {
			e.Source = domain.AnonymousSource()
		}
		return e

	case domain.Part:
		if !sourceBelongsTo(e.Source, recipientID) {
			e.Source = domain.AnonymousSource()
		}
		return e

	case domain.Kicked:
		if !e.SubjectIsSelf {
			e.Subject = domain.AnonymousNick
		}
		if !sourceBelongsTo(e.Source, recipientID) {
			e.Source = domain.AnonymousSource()
		}
		return e

	case domain.TopicChange:
		if !sourceBelongsTo(e.Source, recipientID) {
			e.Source = domain.AnonymousSource()
		}
		return e

	case domain.ChannelModeChange:
		if e.Flag.MemberMode() && !domain.EqualNick(e.Subject, recipientNick) {
			e.Subject = domain.AnonymousNick
		}
		if !e.ServerIssued() && !sourceBelongsTo(e.Source, recipientID) {
			e.Source = domain.AnonymousSource()
		}
		return e
	}

	return event
}

func sourceBelongsTo(source domain.Source, id domain.InstanceID) bool {
	sourceID, ok := source.InstanceID()
	return ok && sourceID == id
}

// namedChannels returns the members of `channels` that do not carry
// `+a`. An actor-scoped event may name its actor only on these:
// everywhere else RFC 2811 §4.2.1 has the mask stand in. An empty
// result means there is nowhere the actor can be named, which is
// what [Session.quitAs] asks before falling back to a direct
// delivery.
func namedChannels(channels, anonymous []domain.ChannelName) []domain.ChannelName {
	var named []domain.ChannelName

	for _, ch := range channels {
		if !slices.Contains(anonymous, ch) {
			named = append(named, ch)
		}
	}

	return named
}

func (s *Session) quitRoutes(
	ctx context.Context,
	quit domain.Quit,
	channels, masked []domain.ChannelName,
	closing *serverClient,
) []routedDelivery {
	plan := s.planProtocolEmission(ctx, protocolEmission{
		event: quit,
		scope: sharedChannelsScope{channels: channels, masked: masked},
	}, 0)

	routes := make([]routedDelivery, 0, len(plan.routes))
	for _, route := range plan.routes {
		if route.sub == closing {
			continue
		}

		generation, active := route.sub.connection()
		if !active {
			continue
		}
		route.connectionAuthority = &connectionAuthority{generation: generation}
		routes = append(routes, route)
	}

	return append(routes, s.anonymousPartRoutes(ctx, quit, plan.anonymous, closing)...)
}

// partAnonymousChannels delivers a `PART` to each anonymous channel
// the quitting actor was in, which is what RFC 2811 §4.2.1 puts on
// an anonymous channel in place of a `QUIT`: a member sees somebody
// leave the channel and cannot tell that they left the server. The
// departing nick is the `+a` mask, matching what every message in
// the channel was already attributed to.
//
// The actor's own subscription is among the recipients, so the
// quitting client sees the same masked departure its peers do.
func (s *Session) partAnonymousChannels(
	ctx context.Context,
	quit domain.Quit,
	anonymous []domain.ChannelName,
	spanCtx trace.SpanContext,
) {
	var closing *serverClient
	if actor, identified := quit.Source.InstanceID(); identified {
		closing = s.lookupClientHandle(protocol.ClientID(actor))
	}
	s.enqueueRoutes(ctx, s.anonymousPartRoutesWithSpan(
		quit, anonymous, closing, spanCtx,
	))
}

func (s *Session) anonymousPartRoutes(
	ctx context.Context,
	quit domain.Quit,
	anonymous []domain.ChannelName,
	closing *serverClient,
) []routedDelivery {
	return s.anonymousPartRoutesWithSpan(
		quit, anonymous, closing, trace.SpanContextFromContext(ctx),
	)
}

func (s *Session) anonymousPartRoutesWithSpan(
	quit domain.Quit,
	anonymous []domain.ChannelName,
	closing *serverClient,
	spanCtx trace.SpanContext,
) []routedDelivery {
	if len(anonymous) == 0 {
		return nil
	}

	subs := s.subscriberSnapshot()
	var routes []routedDelivery
	for _, ch := range anonymous {
		part := domain.Part{
			Source:  domain.AnonymousSource(),
			Target:  ch,
			Message: quit.Message,
			At:      quit.At,
		}

		for _, sub := range subs {
			generation, active := sub.connection()
			if sub != closing && (!active || !sub.canReceive(part, nil)) {
				continue
			}

			route := routedDelivery{
				sub:                   sub,
				terminal:              sub == closing,
				membershipAuthorities: []membershipAuthority{{window: ch, epoch: sub.windowEpoch(ch)}},
				delivery:              protocol.Delivery{Event: part, SpanCtx: spanCtx},
			}
			if sub != closing {
				route.connectionAuthority = &connectionAuthority{generation: generation}
			}
			routes = append(routes, route)
		}
	}

	return routes
}

// echoToOriginator delivers chat traffic back to its sender when the
// sender holds echo-message (IRCv3). The echo is a direct send to the
// originating subscription: a message is keyed for its recipient
// window — a DM's target is the counterpart's id — so the sender's
// own copy cannot ride the membership filter. A sender without the
// capability (every model) gets nothing, keeping RFC 2812 §3.3.1
// no-self-echo.
func (s *Session) echoToOriginator(
	ctx context.Context,
	pe domain.ProtocolEvent,
	sender protocol.ClientID,
	spanCtx trace.SpanContext,
	eventID int64,
) {
	origin := s.activeClientHandle(sender)
	if origin == nil || !origin.echo {
		return
	}
	s.enqueueRoutes(ctx, []routedDelivery{{
		sub:      origin,
		delivery: protocol.Delivery{Event: pe, SpanCtx: spanCtx},
		eventID:  eventID,
	}})
}

// enqueueRoutes records each replay-capable recipient's projected
// channel delivery, then queues the live copy. It holds the affected
// subscription locks across both operations, so attach cannot include
// the stored copy in a snapshot before the matching live delivery has
// entered the replay queue. Row identifiers stay inside the session.
func (s *Session) enqueueRoutes(ctx context.Context, routes []routedDelivery) []routedDelivery {
	locked := lockRouteSubscriptions(routes)
	defer func() {
		for _, sub := range slices.Backward(locked) {
			sub.replayMu.Unlock()
		}
	}()

	routes = authorisedRoutesLocked(ctx, routes)

	records, indexes := projectedScrollbackRecords(routes)

	ids, err := s.store.AppendChannelScrollback(ctx, records)
	if err != nil {
		slog.Default().ErrorContext(ctx, "append channel scrollback", "error", err)
		s.recordPersistenceFailure(ctx, "")
		ids = nil
	}
	routes, indexes, unreplayable := routesWithDurableProjection(
		routes, indexes, ids, len(records),
	)

	for _, sub := range locked {
		sub.outMu.Lock()
	}
	for _, sub := range unreplayable {
		sub.outSealed = true
	}
	queued, overflowed := queueRoutesLocked(routes, indexes, ids, len(records))
	for _, sub := range slices.Backward(locked) {
		sub.outMu.Unlock()
	}

	for _, sub := range overflowed {
		s.disconnectOverflowed(ctx, sub)
	}
	for _, sub := range unreplayable {
		s.disconnectUnreplayable(ctx, sub)
	}

	return queued
}

func authorisedRoutesLocked(
	ctx context.Context,
	routes []routedDelivery,
) []routedDelivery {
	return slices.DeleteFunc(routes, func(route routedDelivery) bool {
		if route.connectionAuthority != nil {
			generation, active := route.sub.connection()
			if !active || generation != route.connectionAuthority.generation {
				return true
			}
		}
		if !route.dispatchAuthority.validLocked(ctx) {
			return true
		}

		for _, authority := range route.membershipAuthorities {
			if route.sub.instance.InChannel(authority.window) &&
				route.sub.windowEpoch(authority.window) == authority.epoch {
				return false
			}
		}

		return len(route.membershipAuthorities) > 0
	})
}

func routesWithDurableProjection(
	routes []routedDelivery,
	indexes []map[domain.ChannelName]int,
	ids []int64,
	recordCount int,
) ([]routedDelivery, []map[domain.ChannelName]int, []*serverClient) {
	if len(ids) == recordCount {
		return routes, indexes, nil
	}

	keptRoutes := make([]routedDelivery, 0, len(routes))
	keptIndexes := make([]map[domain.ChannelName]int, 0, len(indexes))
	var unreplayable []*serverClient
	for i, route := range routes {
		if len(indexes[i]) > 0 {
			if !slices.Contains(unreplayable, route.sub) {
				unreplayable = append(unreplayable, route.sub)
			}

			continue
		}

		keptRoutes = append(keptRoutes, route)
		keptIndexes = append(keptIndexes, indexes[i])
	}

	return keptRoutes, keptIndexes, unreplayable
}

func admitScopedDelivery(
	scope deliveryScope,
	sub *serverClient,
) (
	targets []domain.ChannelName,
	direct bool,
	authorities []membershipAuthority,
	eligible bool,
) {
	return scopedDeliveryCandidateFor(scope, sub).admit(sub)
}

type scopedDeliveryCandidate struct {
	targets     []domain.ChannelName
	direct      bool
	authorities []membershipAuthority
	eligible    bool
}

func scopedDeliveryCandidateFor(
	scope deliveryScope,
	sub *serverClient,
) scopedDeliveryCandidate {
	var candidate scopedDeliveryCandidate

	switch scope := scope.(type) {
	case channelScope:
		candidate.authorities = []membershipAuthority{{
			window: scope.channel,
			epoch:  sub.windowEpoch(scope.channel),
		}}
		if !sub.instance.InChannel(scope.channel) {
			return scopedDeliveryCandidate{}
		}
	case sharedChannelsScope:
		epochs := make(map[domain.ChannelName]uint64, len(scope.channels))
		for _, window := range scope.channels {
			epochs[window] = sub.windowEpoch(window)
		}
		candidate.targets = intersectActorTargets(sub, scope.channels)
		if len(candidate.targets) == 0 {
			return scopedDeliveryCandidate{}
		}
		candidate.authorities = make([]membershipAuthority, 0, len(candidate.targets))
		for _, window := range candidate.targets {
			candidate.authorities = append(candidate.authorities, membershipAuthority{
				window: window,
				epoch:  epochs[window],
			})
		}
	case clientScope:
		candidate.direct = true
		candidate.eligible = sub.instance.ID() == scope.client

		return candidate
	case operatorsScope:
		_, active := sub.connection()
		candidate.eligible = active && sub.HasMode(domain.ModeOperator)

		return candidate
	default:
		return scopedDeliveryCandidate{}
	}
	candidate.eligible = true

	return candidate
}

func (candidate scopedDeliveryCandidate) admit(
	sub *serverClient,
) (
	targets []domain.ChannelName,
	direct bool,
	authorities []membershipAuthority,
	eligible bool,
) {
	if !candidate.eligible {
		return nil, false, nil, false
	}
	if len(candidate.authorities) == 0 {
		return candidate.targets, candidate.direct, nil, true
	}

	sub.replayMu.Lock()
	defer sub.replayMu.Unlock()
	for _, authority := range candidate.authorities {
		if !sub.instance.InChannel(authority.window) ||
			sub.windowEpoch(authority.window) != authority.epoch {
			return nil, false, nil, false
		}
	}

	return candidate.targets, false, candidate.authorities, true
}

func lockRouteSubscriptions(routes []routedDelivery, additional ...*serverClient) []*serverClient {
	locked := make([]*serverClient, 0, len(routes)+len(additional))
	seen := make(map[*serverClient]struct{}, len(routes)+len(additional))
	add := func(client *serverClient) {
		if client == nil {
			return
		}
		if _, ok := seen[client]; ok {
			return
		}

		seen[client] = struct{}{}
		locked = append(locked, client)
	}
	for _, route := range routes {
		add(route.sub)
		for _, client := range route.dispatchAuthority.clients() {
			add(client)
		}
	}
	for _, sub := range additional {
		add(sub)
	}
	slices.SortFunc(locked, func(a, b *serverClient) int {
		return strings.Compare(string(a.id), string(b.id))
	})
	for _, sub := range locked {
		sub.replayMu.Lock()
	}

	return locked
}

func projectedScrollbackRecords(
	routes []routedDelivery,
) ([]store.ChannelScrollbackRecord, []map[domain.ChannelName]int) {
	return projectedScrollbackRecordsExcept(routes, nil)
}

func projectedScrollbackRecordsExcept(
	routes []routedDelivery,
	excluded map[domain.InstanceID]struct{},
) ([]store.ChannelScrollbackRecord, []map[domain.ChannelName]int) {
	records := make([]store.ChannelScrollbackRecord, 0, len(routes))
	indexes := make([]map[domain.ChannelName]int, len(routes))

	for i, route := range routes {
		if !route.sub.replayCapable {
			continue
		}
		if _, skip := excluded[route.sub.instance.ID()]; skip {
			continue
		}

		event, ok := route.delivery.Event.(domain.ChannelActivity)
		if !ok {
			continue
		}

		windows := route.delivery.Targets
		if len(windows) == 0 {
			target := domain.EventTarget(event)
			if domain.InferChannelKind(target) == domain.KindChannel {
				windows = []domain.ChannelName{target}
			}
		}

		for _, window := range windows {
			if domain.InferChannelKind(window) != domain.KindChannel {
				continue
			}

			if indexes[i] == nil {
				indexes[i] = make(map[domain.ChannelName]int)
			}
			indexes[i][window] = len(records)
			records = append(records, store.ChannelScrollbackRecord{
				InstanceID: route.sub.instance.ID(),
				Channel:    window,
				Event:      event,
			})
		}
	}

	return records, indexes
}

func queueRoutesLocked(
	routes []routedDelivery,
	indexes []map[domain.ChannelName]int,
	ids []int64,
	recordCount int,
) ([]routedDelivery, []*serverClient) {
	queuedRoutes := make([]routedDelivery, 0, len(routes))
	var overflowed []*serverClient
	for i, route := range routes {
		var byWindow map[domain.ChannelName]int64
		if len(ids) == recordCount && len(indexes[i]) > 0 {
			byWindow = make(map[domain.ChannelName]int64, len(indexes[i]))
			for window, index := range indexes[i] {
				byWindow[window] = ids[index]
			}
		}

		queued := queuedDelivery{
			delivery:      route.delivery,
			scrollbackIDs: byWindow,
			eventID:       route.eventID,
			terminal:      route.terminal,
		}
		queued.delivery.History = historyRefs(route, byWindow)
		if route.historyOnly {
			queued.delivery.HistoryOnly = true
		}

		if !route.sub.queueLocked(queued) {
			overflowed = append(overflowed, route.sub)
			continue
		}

		queuedRoutes = append(queuedRoutes, route)
	}

	return queuedRoutes, overflowed
}

func historyRefs(
	route routedDelivery,
	byWindow map[domain.ChannelName]int64,
) []protocol.HistoryRef {
	var refs []protocol.HistoryRef
	for _, window := range slices.Sorted(maps.Keys(byWindow)) {
		refs = append(refs, protocol.ChannelHistoryRef(byWindow[window], window))
	}

	message, ok := route.delivery.Event.(domain.Message)
	if !ok || route.eventID == 0 {
		return refs
	}
	window, involved := message.RoutingKey(route.sub.instance.ID())
	if !involved || domain.InferChannelKind(window) != domain.KindDM {
		return refs
	}

	return append(refs, protocol.DirectHistoryRef(route.eventID, domain.InstanceID(window)))
}

func eventTerminatesRecipient(event domain.ProtocolEvent, recipient *serverClient) bool {
	if _, ok := event.(domain.ConnectionError); ok {
		return true
	}

	quit, ok := event.(domain.Quit)
	if !ok {
		return false
	}

	actor, identified := quit.Source.InstanceID()
	return identified && protocol.ClientID(actor) == recipient.id
}

// anonymiseIfNeeded rewrites a chat-traffic event's `From` field
// to `"anonymous"` when the target channel carries `+a`. Returns
// the event unchanged when the channel is not anonymous or when
// the event is not chat traffic. The mode set comes from the
// session's live channel state, which is what makes this
// affordable on every message the server fans out.
func anonymiseIfNeeded(ctx context.Context, s *Session, pe domain.ProtocolEvent) domain.ProtocolEvent {
	msg, ok := pe.(domain.Message)
	if !ok {
		return pe
	}

	if domain.InferChannelKind(msg.Target) != domain.KindChannel {
		return pe
	}

	modes, ok := s.channelModes(ctx, msg.Target)
	if !ok || !modes.Anonymous {
		return pe
	}

	msg.Source = domain.AnonymousSource()
	return msg
}

// intersectActorTargets returns the recipient-visible channel
// list for an actor-scoped event: those channels in
// `actorChannels` that `sub` is also a member of. The user-client
// uses the same intersection as a model-client — it sees the actor
// move only in the windows the two share. Window-scoped events pass
// `actorChannels == nil` and receive a nil result.
func intersectActorTargets(sub *serverClient, actorChannels []domain.ChannelName) []domain.ChannelName {
	if len(actorChannels) == 0 {
		return nil
	}

	var out []domain.ChannelName
	for _, ch := range actorChannels {
		if sub.instance.InChannel(ch) {
			out = append(out, ch)
		}
	}

	return out
}

// chatTrafficSender reports whether `ev` carries the
// originator-suppression rule (PRIVMSG/Action), and returns the
// sender's [protocol.ClientID] when it does. The empty client id
// returned alongside `false` is unused and never compared.
//
// Today only [domain.Message] (covering both PRIVMSG and `/me`
// via [domain.Message.Action]) qualifies. Future event types
// needing the same rule add a switch arm here.
func chatTrafficSender(ev domain.ProtocolEvent) (suppress bool, sender protocol.ClientID) {
	if msg, ok := ev.(domain.Message); ok {
		id, identified := msg.Source.InstanceID()
		return identified, protocol.ClientID(id)
	}

	return false, ""
}
