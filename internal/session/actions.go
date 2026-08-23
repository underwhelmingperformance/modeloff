package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

const joinCompletionTimeout = 5 * time.Second

// joinAs joins the given actor to a channel. `key` carries the
// channel password for keyed (`+k`) channels — empty for unkeyed
// joins. `+i`, `+l`, and `+k` gate the add against an existing
// channel; a fresh channel (this call creates it) has no modes
// and so no gate applies.
//
// `kind` says on whose authority the join is happening, which is
// what decides whether `+i` admits it. See [joinKind].
//
// The first return is the channel's canonical name: the spelling
// under which the channel actually exists, which may differ in case
// from the one the client asked for. Callers that report the join
// back to the client use it, so a `/join #Dev` that reached `#dev`
// says so.
//
// Runs on the session's command loop, so the load-mutate-commit of
// the channel record below is atomic against every other command.
//
//nolint:gocognit // sequenced join steps (create-or-load, gate, op-grant, persist, broadcast, replies) read clearer inline than as further-extracted helpers.
func (s *Session) joinAs(ctx context.Context, actor *domain.Instance, kind joinKind, ch domain.ChannelName, key string) (domain.ChannelName, error) {
	ch = domain.NormaliseChannelName(ch)

	if reason := domain.ValidateChannelName(ch); reason != domain.ChannelNameAccepted {
		return "", domain.ErroneousChannelNameError{Channel: ch, Reason: reason, At: s.now()}
	}

	actorNick := actor.Nick()
	joined := false

	err := s.inSpan(ctx, "session.join", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
		attribute.String(observability.AttrInstanceID, string(actor.ID())),
	}, func(ctx context.Context, _ trace.Span) error {
		now := s.now()

		window, created, err := s.ensureChannelWindowForJoin(ctx, ch, now)
		if err != nil {
			return err
		}

		// The channel record is the authority for how the name is
		// spelled: under the casemapping a JOIN for `#Dev` reaches an
		// existing `#dev`. Every key derived from here on uses the
		// spelling the channel was created with, so the event log,
		// the actor's channel set and the wire events all agree.
		ch = window.Name()

		windowMember := window.Members.HasInstance(actor)
		actorMember := actor.InChannel(ch)
		alreadyMember := windowMember && actorMember
		invitationConsumed := false

		resetContext := !created && !windowMember
		if resetContext {
			invitationConsumed, err = s.checkJoinGates(window, actor, kind, key)
			if err != nil {
				return err
			}
		}

		// RFC 2811 §4.3: the JOIN that creates the channel auto-grants
		// the joiner `+o`. That is the only automatic `+o` grant the
		// server ever performs — the original creator parting and
		// rejoining gets nothing back; subsequent ops are granted only
		// by an existing op via wire `MODE +o`. The grant happens
		// here, before any wire event, so the Join echo and the
		// `RPL_NAMREPLY` that follow see the `+o` in the member list
		// (the `@` prefix in NAMES is how RFC 2812 §3.2.1 conveys the
		// new op's rank — there is no separate MODE message).
		if alreadyMember {
			joined = true
			return nil
		}

		joinEvent := domain.Join{
			Source:  domain.ClientSource(actor.ID(), actorNick),
			Target:  ch,
			Created: created,
			At:      now,
		}
		candidateActor := actor.Snapshot()
		candidateActor.JoinChannel(ch, now)

		window.Members.Add(actor)
		if created {
			window.Members.ApplyMode(actor, domain.ModeOperator, true)
		}
		routes := s.channelJoinRoutes(ctx, window, joinEvent)
		actorClient := s.lookupClientHandle(protocol.ClientID(actor.ID()))
		locked := lockRouteSubscriptions(routes, actorClient)
		routes = authorisedRoutesLocked(ctx, routes)
		records, indexes := projectedScrollbackRecords(routes)

		committed, err := s.store.CommitChannelJoin(ctx, store.ChannelJoin{
			Window: window, Instance: candidateActor, Event: joinEvent,
			Scrollback: records, ResetContext: resetContext, PreserveTurns: invitationConsumed,
		})
		if err != nil {
			for _, sub := range slices.Backward(locked) {
				sub.replayMu.Unlock()
			}
			s.recordPersistenceFailure(ctx, ch)

			return err
		}

		if invitationConsumed {
			s.installChannelWindowWithInvitationChange(window, actor.ID())
		} else {
			s.installChannelWindow(window)
		}
		actor.JoinChannel(ch, now)
		s.bumpClientWindow(actor.ID(), ch)
		for i := range routes {
			routes[i].eventID = committed.EventID
		}
		for _, sub := range locked {
			sub.outMu.Lock()
		}
		_, overflowed := queueRoutesLocked(
			routes, indexes, committed.ScrollbackIDs, len(records),
		)
		for _, sub := range slices.Backward(locked) {
			sub.outMu.Unlock()
			sub.replayMu.Unlock()
		}
		for _, sub := range overflowed {
			s.disconnectOverflowed(ctx, sub)
		}
		joined = true

		completionCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), joinCompletionTimeout,
		)
		defer cancel()
		ctx = completionCtx

		window, err = s.loadChannelWindow(ctx, ch)
		if err != nil {
			return fmt.Errorf("reload channel after join: %w", err)
		}

		// RFC 2812 §3.2.1 / §3.2.4: RPL_TOPIC and RPL_NAMREPLY are
		// sent only to the joiner. They are server-to-client
		// responses, not channel broadcasts, so they go directly to
		// the joiner's subscription via [Session.deliverToClient]. The
		// chat-screen consumes NamesReplyEvent to populate its
		// member-list cache when the user joins; the model-client's
		// dispatch loop files TopicInfo into history when a model
		// joins so the prompt knows who set the topic and when.
		//
		// On a `+a` channel the reply carries the mask alone (RFC
		// 2811 §4.2.1). Anyone may join an anonymous channel, so a
		// reply naming its members would be the way to ask who is on
		// one. RPL_TOPIC also masks another member recorded as the
		// setter.
		members := window.Members
		if window.Modes.Anonymous {
			members = domain.AnonymousMembers()
		}

		if window.Topic != "" {
			topicSetBy := window.TopicSetBy
			if window.Modes.Anonymous && topicSetBy != "" {
				topicSetBy = domain.AnonymousNick
			}

			topic := domain.TopicInfo{
				Target:     ch,
				Topic:      window.Topic,
				TopicSetBy: topicSetBy,
				TopicSetAt: window.TopicSetAt,
				At:         now,
			}
			s.appendInstanceReply(ctx, actor.ID(), protocol.ChannelWindowTarget(ch), topic)
			s.deliverToClient(ctx, actor.ID(), topic)
		}

		s.deliverToClient(ctx, actor.ID(), domain.NamesReplyEvent{
			Channel: ch,
			Members: members,
			At:      now,
		})

		s.deliverToClient(ctx, actor.ID(), domain.NamesEnd{
			Channel: ch,
			At:      now,
		})

		return nil
	})

	if !joined {
		return "", err
	}

	return ch, err
}

func (s *Session) channelJoinRoutes(
	ctx context.Context,
	window *domain.ChannelWindow,
	event domain.Join,
) []routedDelivery {
	spanCtx := trace.SpanContextFromContext(ctx)
	routes := make([]routedDelivery, 0, window.Members.Len())

	for _, sub := range s.subscriberSnapshot() {
		if !window.Members.HasInstance(sub.instance) {
			continue
		}

		generation, active := sub.connection()
		if !active {
			continue
		}

		var projected domain.ProtocolEvent = event
		if window.Modes.Anonymous {
			projected = maskChannelEvent(projected, sub)
		}
		routes = append(routes, routedDelivery{
			sub:                 sub,
			connectionAuthority: &connectionAuthority{generation: generation},
			delivery: protocol.Delivery{
				Event: projected, SpanCtx: spanCtx,
			},
		})
	}

	return routes
}

// ensureChannelWindowForJoin loads the channel window or creates an empty one.
// It also reports whether this call created the window.
//
// A channel is created only when the load says the channel does not
// exist. Any other load failure is returned as-is: creating a fresh
// window on, say, a transient store failure would overwrite the
// live channel's topic, modes and invitation list with an empty
// record. joinAs is the only caller and is gated on `#`-prefixed
// names by `NormaliseChannelName`, so a load that returns a
// non-channel row indicates a programming error in the upstream
// guard; that too is returned.
//
// A freshly created channel starts with the session's default mode
// set (see [DefaultChannelModes]); a channel that already exists
// keeps the modes it has.
func (s *Session) ensureChannelWindowForJoin(ctx context.Context, ch domain.ChannelName, now time.Time) (*domain.ChannelWindow, bool, error) {
	window, err := s.loadChannelWindow(ctx, ch)
	if err == nil {
		return window, false, nil
	}

	if !errors.Is(err, store.ErrNoSuchChannel) {
		return nil, false, fmt.Errorf("load channel: %w", err)
	}

	window = domain.NewChannelWindow(ch, now)
	window.Modes = s.newChannelModes(ctx)

	return window, true, nil
}

// partAs parts the given actor from a channel.
func (s *Session) partAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, message string) error {
	actorNick := actor.Nick()

	return s.inSpan(ctx, "session.part", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	}, func(ctx context.Context, span trace.Span) error {
		if domain.InferChannelKind(ch) != domain.KindChannel {
			return observability.ErrWithKind(fmt.Errorf("cannot part %s", ch), observability.ErrorKindValidation)
		}

		window, err := s.loadChannelWindow(ctx, ch)
		if err != nil {
			return fmt.Errorf("channel not found: %w", err)
		}

		ch = window.Name()

		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actor.ID())))

		if !window.Members.HasInstance(actor) {
			return domain.NotOnChannelError{Channel: ch, Command: "PART", At: s.now()}
		}

		now := s.now()
		return s.commitMemberDeparture(ctx, window, actor, domain.Part{
			Source:  domain.ClientSource(actor.ID(), actorNick),
			Target:  ch,
			Message: message,
			At:      now,
		})
	})
}

// quitCommittedError reports a teardown failure after the QUIT was
// already logged and broadcast. Callers must still end the connection.
type quitCommittedError struct {
	Err error
}

func (e *quitCommittedError) Error() string {
	return e.Err.Error()
}

func (e *quitCommittedError) Unwrap() error {
	return e.Err
}

type quitDurability uint8

const (
	quitRequiresDeletion quitDurability = iota
	quitForced
)

type quitOutcome struct {
	err             error
	instanceDeleted bool
}

type quitState struct {
	channels         []domain.ChannelName
	eventChannels    []domain.ChannelName
	liveChannels     []domain.ChannelName
	anonymous        []domain.ChannelName
	windows          []*domain.ChannelWindow
	unloaded         []domain.ChannelName
	teardownErrors   []error
	initialDeleteErr error
	pendingDeleteErr error
	instanceDeleted  bool
	projection       *quitProjection
	scrollbackIDs    []int64
	scrollback       []store.ChannelScrollbackRecord
}

type quitInstanceDeletion struct {
	committed         bool
	eventChannels     []domain.ChannelName
	liveChannels      []domain.ChannelName
	uncertainChannels []domain.ChannelName
	scrollbackIDs     []int64
	scrollback        []store.ChannelScrollbackRecord
}

type quitProjection struct {
	locked  []*serverClient
	routes  []routedDelivery
	records []store.ChannelScrollbackRecord
	indexes []map[domain.ChannelName]int
}

func (p *quitProjection) unlock() {
	if p == nil {
		return
	}

	for _, sub := range slices.Backward(p.locked) {
		sub.replayMu.Unlock()
	}
	p.locked = nil
}

func (p *quitProjection) queue(
	scrollbackIDs []int64,
	scrollback []store.ChannelScrollbackRecord,
	channels []domain.ChannelName,
) []*serverClient {
	if p == nil {
		return nil
	}
	p.canonicalise(scrollback, channels)

	for _, sub := range p.locked {
		sub.outMu.Lock()
	}
	_, overflowed := queueRoutesLocked(
		p.routes, p.indexes, scrollbackIDs, len(p.records),
	)
	for _, sub := range slices.Backward(p.locked) {
		sub.outMu.Unlock()
	}
	p.unlock()

	return overflowed
}

func (p *quitProjection) canonicalise(
	scrollback []store.ChannelScrollbackRecord,
	channels []domain.ChannelName,
) {
	canonicalChannel := func(channel domain.ChannelName) domain.ChannelName {
		for _, candidate := range channels {
			if domain.EqualChannel(candidate, channel) {
				return candidate
			}
		}

		return channel
	}

	for i := range p.routes {
		indexes := p.indexes[i]
		for targetIndex, target := range p.routes[i].delivery.Targets {
			if recordIndex, ok := indexes[target]; ok && recordIndex < len(scrollback) {
				p.routes[i].delivery.Targets[targetIndex] = scrollback[recordIndex].Channel
				continue
			}

			p.routes[i].delivery.Targets[targetIndex] = canonicalChannel(target)
		}

		if part, ok := p.routes[i].delivery.Event.(domain.Part); ok {
			if recordIndex, exists := indexes[part.Target]; exists && recordIndex < len(scrollback) {
				part.Target = scrollback[recordIndex].Channel
			} else {
				part.Target = canonicalChannel(part.Target)
			}
			p.routes[i].delivery.Event = part
		}

		if len(indexes) == 0 {
			continue
		}
		canonicalIndexes := make(map[domain.ChannelName]int, len(indexes))
		for _, recordIndex := range indexes {
			if recordIndex >= len(scrollback) {
				continue
			}
			canonicalIndexes[scrollback[recordIndex].Channel] = recordIndex
		}
		p.indexes[i] = canonicalIndexes
	}
}

// quitAs disconnects the given actor from every joined channel. It
// loads the channels, writes their departure audit events and deletes
// the instance in one transaction before making the QUIT observable.
// A load or transaction failure therefore leaves the connection and
// its durable state intact. The session then broadcasts the event
// against the actor's existing live membership, which carries it to
// peers who share those channels and to the departing client. Live
// membership goes afterwards, so a channel the departure empties is
// destroyed like a last PART (RFC 2811 §2).
//
// This is the one teardown, whoever sent the QUIT. The subscription
// is a separate matter: the dispatcher reaps a model-client's
// through [Session.releaseClient] and [Session.reapClient]. The
// user-client's transport remains allocated for the session, but
// QUIT revokes the connection authority attached to it.
func (s *Session) quitAs(ctx context.Context, actor *domain.Instance, message string) error {
	return s.quit(ctx, actor, message, quitRequiresDeletion, nil).err
}

func (s *Session) quit(
	ctx context.Context,
	actor *domain.Instance,
	message string,
	durability quitDurability,
	preQuit domain.ProtocolEvent,
) quitOutcome {
	actorID := actor.ID()
	actorNick := actor.Nick()
	var outcome quitOutcome

	outcome.err = s.inSpan(ctx, "session.quit", []attribute.KeyValue{
		attribute.String(observability.AttrNick, string(actorNick)),
	}, func(ctx context.Context, span trace.Span) error {
		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actorID)))

		now := s.now()
		quit := domain.Quit{
			Source:  domain.ClientSource(actorID, actorNick),
			Message: message,
			At:      now,
		}
		client := s.lookupClientHandle(protocol.ClientID(actorID))
		state, err := s.prepareQuit(ctx, actor, quit, durability, client, preQuit)
		if err != nil {
			return err
		}

		if client != nil {
			client.turnGeneration++
			client.deactivateConnection()
		}
		var overflowed []*serverClient
		if state.instanceDeleted {
			overflowed = state.projection.queue(
				state.scrollbackIDs, state.scrollback, state.liveChannels,
			)
		} else {
			state.projection.unlock()
		}
		for _, sub := range overflowed {
			s.disconnectOverflowed(ctx, sub)
		}
		s.interruptModelPeerWindows(actorID)
		if !state.instanceDeleted {
			if preQuit != nil {
				s.deliverToClosingClient(ctx, client, preQuit)
			}

			quitEmission, emitErr := s.emitQuit(
				ctx, actor, quit, state.eventChannels, state.liveChannels, state.anonymous,
				state.unloaded, true,
			)
			if emitErr != nil {
				state.teardownErrors = append(state.teardownErrors,
					fmt.Errorf("persist quit event: %w", emitErr))
			}
			if quitEmission.closingDeliveryAllowed && !quitEmission.deliveredDirectly {
				s.deliverToClosingClient(ctx, client, quit)
			}
		}
		s.removeQuitMembership(ctx, actor, &state)
		s.retryForcedQuitDeletion(ctx, actor, &state)
		outcome.instanceDeleted = state.instanceDeleted
		if err := errors.Join(state.teardownErrors...); err != nil {
			return &quitCommittedError{Err: err}
		}
		if actorID == domain.InstanceID(protocol.UserClientID) {
			if err := s.store.ClearSessionActive(ctx); err != nil {
				return &quitCommittedError{Err: fmt.Errorf("clear session active: %w", err)}
			}
		}

		return nil
	})

	return outcome
}

func (s *Session) prepareQuit(
	ctx context.Context,
	actor *domain.Instance,
	quit domain.Quit,
	durability quitDurability,
	client *serverClient,
	preQuit domain.ProtocolEvent,
) (quitState, error) {
	state := quitState{channels: s.instanceChannelNames(actor)}
	state.eventChannels = slices.Clone(state.channels)
	state.liveChannels = slices.Clone(state.channels)
	state.windows = make([]*domain.ChannelWindow, 0, len(state.channels))

	for _, ch := range state.channels {
		window, err := s.loadChannelWindow(ctx, ch)
		if err == nil {
			state.windows = append(state.windows, window)
			if window.Modes.Anonymous {
				state.anonymous = append(state.anonymous, window.Name())
			}
			continue
		}
		if durability == quitRequiresDeletion {
			return quitState{}, fmt.Errorf("load channel %q: %w", ch, err)
		}

		state.unloaded = append(state.unloaded, ch)
		state.teardownErrors = append(state.teardownErrors, fmt.Errorf("load channel %q: %w", ch, err))
	}
	state.projection = s.prepareQuitProjection(
		ctx, actor, quit, &state, client, preQuit,
	)

	deletion, err := s.deleteQuitInstance(
		ctx, actor, quit, state.channels, state.windows, state.anonymous, state.unloaded,
		state.projection.records,
	)
	if !deletion.committed {
		if durability == quitRequiresDeletion {
			state.projection.unlock()
			return quitState{}, fmt.Errorf("delete instance: %w", err)
		}

		state.initialDeleteErr = fmt.Errorf("delete instance: %w", err)
		if actor.ID() != domain.InstanceID(protocol.UserClientID) {
			if markErr := s.store.MarkInstancePendingDeletion(ctx, actor.ID()); markErr != nil {
				state.pendingDeleteErr = fmt.Errorf("mark instance pending deletion: %w", markErr)
			}
		}
		return state, nil
	}

	state.instanceDeleted = true
	state.scrollbackIDs = deletion.scrollbackIDs
	state.scrollback = deletion.scrollback
	state.eventChannels = deletion.eventChannels
	state.liveChannels = deletion.liveChannels
	for _, channel := range deletion.uncertainChannels {
		if s.hasOtherActiveMember(actor.ID(), channel) {
			state.liveChannels = append(state.liveChannels, channel)
		}
	}
	if err != nil {
		state.teardownErrors = append(state.teardownErrors, err)
	}

	return state, nil
}

func (s *Session) prepareQuitProjection(
	ctx context.Context,
	actor *domain.Instance,
	quit domain.Quit,
	state *quitState,
	client *serverClient,
	preQuit domain.ProtocolEvent,
) *quitProjection {
	channels := make([]domain.ChannelName, 0, len(state.windows)+len(state.unloaded))
	for _, window := range state.windows {
		remaining := window.Members.Len()
		if window.Members.HasID(actor.ID()) {
			remaining--
		}
		if remaining > 0 {
			channels = append(channels, window.Name())
		}
	}
	channels = append(channels, state.unloaded...)

	masked := slices.Concat(slices.Clone(state.anonymous), state.unloaded)
	routes := s.quitRoutes(ctx, quit, channels, masked, client)
	spanCtx := trace.SpanContextFromContext(ctx)
	if client != nil && preQuit != nil {
		routes = append([]routedDelivery{{
			sub: client, terminal: true,
			delivery: protocol.Delivery{Event: preQuit, SpanCtx: spanCtx},
		}}, routes...)
	}
	if client != nil {
		routes = append(routes, routedDelivery{
			sub: client, terminal: true,
			delivery: protocol.Delivery{Event: quit, SpanCtx: spanCtx},
		})
	}

	locked := lockRouteSubscriptions(routes, client)
	routes = authorisedRoutesLocked(ctx, routes)
	excluded := map[domain.InstanceID]struct{}{actor.ID(): {}}
	records, indexes := projectedScrollbackRecordsExcept(routes, excluded)

	return &quitProjection{
		locked: locked, routes: routes, records: records, indexes: indexes,
	}
}

func (s *Session) deleteQuitInstance(
	ctx context.Context,
	actor *domain.Instance,
	quit domain.Quit,
	channels []domain.ChannelName,
	windows []*domain.ChannelWindow,
	anonymous []domain.ChannelName,
	unloaded []domain.ChannelName,
	scrollback []store.ChannelScrollbackRecord,
) (quitInstanceDeletion, error) {
	expectedDestroyed := make(map[domain.ChannelKey]struct{}, len(unloaded))
	for _, window := range windows {
		if window.Members.Len() == 1 && window.Members.HasID(actor.ID()) {
			expectedDestroyed[domain.KeyForChannel(window.Name())] = struct{}{}
		}
	}
	for _, channel := range unloaded {
		expectedDestroyed[domain.KeyForChannel(channel)] = struct{}{}
	}

	// Keep the durable deletion and the authority-generation change under
	// one channel-state lock so a guard cannot observe the deleted channel
	// with its previous generation.
	s.channels.mu.Lock()
	defer s.channels.mu.Unlock()

	masked := slices.Concat(slices.Clone(anonymous), unloaded)
	auditChannels := make([]domain.ChannelName, 0, len(windows)+len(unloaded))
	for _, window := range windows {
		auditChannels = append(auditChannels, window.Name())
	}
	auditChannels = append(auditChannels, unloaded...)
	auditEvents := make([]store.ChannelAuditEvent, 0, len(auditChannels))
	for _, channel := range auditChannels {
		auditEvents = append(auditEvents, store.ChannelAuditEvent{
			Channel: channel,
			Event:   actorEventForChannel(quit, channel, masked),
		})
	}
	committed, err := s.store.CommitInstanceDeletion(ctx, store.InstanceDeletion{
		InstanceID: actor.ID(),
		Events:     auditEvents,
		Scrollback: scrollback,
	})
	if err != nil {
		return quitInstanceDeletion{}, err
	}

	seen := make(map[domain.ChannelKey]struct{}, len(channels))
	eventChannels := make([]domain.ChannelName, 0, len(channels))
	liveChannels := make([]domain.ChannelName, 0, len(channels))
	var refreshErrors []error
	var uncertainChannels []domain.ChannelName
	for _, name := range channels {
		key := domain.KeyForChannel(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		delete(s.channels.windows, key)

		window, err := s.loadChannelWindowFromStore(ctx, name)
		if err == nil {
			s.channels.windows[key] = window
			eventChannels = append(eventChannels, window.Name())
			liveChannels = append(liveChannels, window.Name())
			continue
		}

		eventChannels = append(eventChannels, name)
		if errors.Is(err, store.ErrNoSuchChannel) {
			s.channels.generations[key]++
			s.channelFlood.forget(name)
			continue
		}

		uncertainChannels = append(uncertainChannels, name)
		refreshErrors = append(refreshErrors,
			fmt.Errorf("reload channel %q after instance deletion: %w", name, err))
		if _, destroyed := expectedDestroyed[key]; destroyed {
			s.channels.generations[key]++
			s.channelFlood.forget(name)
		}
	}
	s.directoryGeneration.Add(1)

	return quitInstanceDeletion{
		committed:         true,
		eventChannels:     eventChannels,
		liveChannels:      liveChannels,
		uncertainChannels: uncertainChannels,
		scrollbackIDs:     committed.ScrollbackIDs,
		scrollback:        committed.Scrollback,
	}, errors.Join(refreshErrors...)
}

func (s *Session) hasOtherActiveMember(actor domain.InstanceID, channel domain.ChannelName) bool {
	for _, connection := range s.activeConnections() {
		instance := connection.client.instance
		if instance.ID() != actor && instance.InChannel(channel) {
			return true
		}
	}

	return false
}

type quitEmission struct {
	deliveredDirectly      bool
	closingDeliveryAllowed bool
}

func (s *Session) emitQuit(
	ctx context.Context,
	actor *domain.Instance,
	quit domain.Quit,
	channels, liveChannels, anonymous []domain.ChannelName,
	maskedChannels []domain.ChannelName,
	persist bool,
) (quitEmission, error) {
	masked := slices.Clone(anonymous)
	for _, channel := range maskedChannels {
		if !slices.Contains(masked, channel) {
			masked = append(masked, channel)
		}
	}
	deliveryChannels := liveChannels
	deliveryMasked := masked
	var persistenceErrors []error
	if persist {
		var persistedChannels []domain.ChannelName
		for _, channel := range channels {
			if _, err := s.appendEventResult(
				ctx, channel, actorEventForChannel(quit, channel, masked),
			); err != nil {
				persistenceErrors = append(persistenceErrors,
					fmt.Errorf("append actor event for %q: %w", channel, err))
				continue
			}
			persistedChannels = append(persistedChannels, channel)
		}
		deliveryChannels = intersectChannelNames(liveChannels, persistedChannels)
		deliveryMasked = intersectChannelNames(masked, persistedChannels)
	}
	err := errors.Join(persistenceErrors...)
	deliverable := !persist || len(channels) == 0 || len(deliveryChannels) > 0 || len(deliveryMasked) > 0
	if !deliverable {
		return quitEmission{}, err
	}
	if len(deliveryChannels) > 0 || len(deliveryMasked) > 0 {
		s.emitScoped(ctx, quit, sharedChannelsScope{
			channels: deliveryChannels,
			masked:   deliveryMasked,
		})
	}

	// A client is told what happened to its own connection, which
	// matters for the QUIT it did not ask for: RFC 2812 §3.7.1 has
	// a killed client told it was killed. The broadcast above is
	// usually how it hears, because it is still on its channels
	// and the membership filter carries it its own QUIT.
	//
	// Two cases leave it with no channel the QUIT can arrive
	// through: a client on none at all, and one whose channels all
	// carry `+a`, where RFC 2811 §4.2.1 withholds the QUIT and
	// sends a masked PART in its place so no member learns who
	// left. `namedChannels` is the same question `maskActorEvent`
	// asks per recipient. Both are answered with a direct
	// delivery, which is the fallback `changeNickAs` makes for
	// NICK, for the same reason.
	anonymous = s.anonymousChannels(ctx, deliveryChannels)
	for _, ch := range deliveryMasked {
		if !slices.Contains(anonymous, ch) {
			anonymous = append(anonymous, ch)
		}
	}
	if len(namedChannels(deliveryChannels, anonymous)) == 0 {
		s.deliverToClient(ctx, actor.ID(), quit)

		return quitEmission{
			deliveredDirectly:      true,
			closingDeliveryAllowed: true,
		}, err
	}

	return quitEmission{closingDeliveryAllowed: true}, err
}

func (s *Session) removeQuitMembership(ctx context.Context, actor *domain.Instance, state *quitState) {
	if state.instanceDeleted {
		for _, ch := range state.channels {
			actor.LeaveChannels(ch)
		}

		return
	}

	for _, window := range state.windows {
		s.removeMemberFromWindow(window, actor)
		err := s.commitChannel(ctx, window)

		if err != nil {
			if window.Members.Len() > 0 {
				s.installChannelWindow(window)
			}
			state.teardownErrors = append(state.teardownErrors, fmt.Errorf("leave channel %q: %w", window.Name(), err))
		}
	}

	for _, ch := range state.unloaded {
		actor.LeaveChannels(ch)
	}
}

func (s *Session) retryForcedQuitDeletion(ctx context.Context, actor *domain.Instance, state *quitState) {
	if state.instanceDeleted {
		return
	}

	retryErr := s.store.DeleteInstanceByID(ctx, actor.ID())
	if retryErr == nil {
		state.instanceDeleted = true
		return
	}

	state.teardownErrors = append(state.teardownErrors, state.initialDeleteErr)
	state.teardownErrors = append(state.teardownErrors, fmt.Errorf("retry instance deletion: %w", retryErr))
	if state.pendingDeleteErr != nil {
		if markErr := s.store.MarkInstancePendingDeletion(ctx, actor.ID()); markErr != nil {
			state.teardownErrors = append(state.teardownErrors, state.pendingDeleteErr)
			state.teardownErrors = append(state.teardownErrors,
				fmt.Errorf("retry mark instance pending deletion: %w", markErr))
		}
	}
}

// changeNickAs changes the given actor's nickname. The grammar and
// collision checks are [Session.requireNickAvailable]'s, the same
// pair `ADDMODEL` runs before it claims a nick for a new instance.
//
// Runs on the session's command loop, which is what makes the
// collision check decisive: no other command can claim `newNick`
// between the check and the rename that takes it.
func (s *Session) changeNickAs(ctx context.Context, actor *domain.Instance, newNick domain.Nick) error {
	oldNick := actor.Nick()

	return s.inSpan(ctx, "session.change_nick", []attribute.KeyValue{
		attribute.String(observability.AttrNick, string(oldNick)),
		attribute.String("nick.new", string(newNick)),
	}, func(ctx context.Context, span trace.Span) error {
		if newNick == oldNick {
			return nil
		}

		if err := s.requireNickAvailable(ctx, newNick, actor); err != nil {
			return err
		}

		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actor.ID())))

		now := s.now()
		actorID := actor.ID()

		change := domain.NickChange{
			Source:  domain.ClientSource(actorID, oldNick),
			NewNick: newNick,
			At:      now,
		}

		candidateActor := actor.Snapshot()
		candidateActor.SetNick(newNick)
		channels := s.instanceChannelNames(actor)
		windows := make([]*domain.ChannelWindow, 0, len(channels))
		auditEvents := make([]store.ChannelAuditEvent, 0, len(channels))
		for _, channel := range channels {
			window, err := s.loadChannelWindow(ctx, channel)
			if err != nil {
				return fmt.Errorf("load nick change channel %q: %w", channel, err)
			}

			window.Members.RenameID(actor.ID(), newNick)
			windows = append(windows, window)
			auditEvents = append(auditEvents, store.ChannelAuditEvent{
				Channel: window.Name(),
				Event:   change,
			})
		}

		plan := s.planProtocolEmission(ctx, protocolEmission{
			event: change,
			scope: sharedChannelsScope{channels: channels},
		}, 0)
		locked := lockRouteSubscriptions(plan.routes)
		plan.routes = authorisedRoutesLocked(ctx, plan.routes)
		records, indexes := projectedScrollbackRecords(plan.routes)

		committed, err := s.store.CommitActorRename(ctx, store.ActorRename{
			Instance:   candidateActor,
			Windows:    windows,
			Events:     auditEvents,
			Scrollback: records,
		})
		if err != nil {
			for _, sub := range slices.Backward(locked) {
				sub.replayMu.Unlock()
			}
			for _, channel := range channels {
				s.recordPersistenceFailure(ctx, channel)
			}
			return fmt.Errorf("persist nick change: %w", err)
		}

		actor.SetNick(newNick)
		for _, window := range windows {
			s.installChannelWindow(window)
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
		for _, sub := range overflowed {
			s.disconnectOverflowed(ctx, sub)
		}

		// RFC 2812 §3.1.2: a client is always told its own NICK
		// succeeded. The broadcast above carries it back through the
		// membership filter for a client that is on a channel, but a
		// client on none has no channel to reach it through, and
		// without this its rename would land silently.
		if len(s.instanceChannelNames(actor)) == 0 {
			s.deliverToClient(ctx, actorID, change)
		}

		return nil
	})
}

func intersectChannelNames(
	candidates []domain.ChannelName,
	allowed []domain.ChannelName,
) []domain.ChannelName {
	allowedKeys := make(map[domain.ChannelKey]struct{}, len(allowed))
	for _, channel := range allowed {
		allowedKeys[domain.KeyForChannel(channel)] = struct{}{}
	}

	matched := make([]domain.ChannelName, 0, len(candidates))
	for _, channel := range candidates {
		if _, ok := allowedKeys[domain.KeyForChannel(channel)]; ok {
			matched = append(matched, channel)
		}
	}

	return matched
}

// sendMessageAs records a message from the given actor and
// returns the persisted [domain.Message]. The message is emitted
// via [Session.emit]; membership fan-out suppresses the originator
// (RFC 2812 §3.3.1), and a sender holding echo-message additionally
// receives a direct echo via [Session.echoToOriginator]. A replay-
// capable sender without echo-message receives a history-only
// delivery, which its client files in server order without exposing
// an IRC echo.
func (s *Session) sendMessageAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, body string) (domain.Message, error) {
	actorNick := actor.Nick()

	var msg domain.Message

	err := s.inSpan(ctx, "session.send_message", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	}, func(ctx context.Context, span trace.Span) error {
		instanceID := actor.ID()
		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(instanceID)))

		target, err := s.checkSendGates(ctx, actor, ch)
		if err != nil {
			return err
		}

		msg = domain.Message{
			Source: domain.ClientSource(instanceID, actorNick),
			Target: target,
			Body:   body,
			At:     s.now(),
		}

		ch = target

		return s.recordAndEmitMessage(ctx, ch, msg)
	})

	return msg, err
}

// sendActionAs records an action message from the given actor.
// See [Session.sendMessageAs] for echo semantics.
func (s *Session) sendActionAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, body string) (domain.Message, error) {
	actorNick := actor.Nick()

	var msg domain.Message

	err := s.inSpan(ctx, "session.send_action", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	}, func(ctx context.Context, span trace.Span) error {
		instanceID := actor.ID()
		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(instanceID)))

		target, err := s.checkSendGates(ctx, actor, ch)
		if err != nil {
			return err
		}

		msg = domain.Message{
			Source: domain.ClientSource(instanceID, actorNick),
			Target: target,
			Body:   body,
			Action: true,
			At:     s.now(),
		}

		ch = target

		return s.recordAndEmitMessage(ctx, ch, msg)
	})

	return msg, err
}

func (s *Session) recordAndEmitMessage(
	ctx context.Context,
	target domain.ChannelName,
	message domain.Message,
) error {
	if domain.InferChannelKind(target) == domain.KindDM {
		s.dmHistoryMu.Lock()
		defer s.dmHistoryMu.Unlock()

		eventID, err := s.appendEventResult(ctx, target, message)
		if err != nil {
			return &MessagePersistenceError{Err: err}
		}
		s.emitStored(ctx, message, eventID, clientScope{client: domain.InstanceID(target)})

		return nil
	}

	if err := s.commitChannelMessage(ctx, target, message); err != nil {
		return &MessagePersistenceError{Err: err}
	}

	return nil
}

// MessagePersistenceError reports that the session could not persist a
// message before delivery. Err is the store failure.
type MessagePersistenceError struct {
	Err error
}

func (e *MessagePersistenceError) Error() string {
	return fmt.Sprintf("persist message: %v", e.Err)
}

func (e *MessagePersistenceError) Unwrap() error { return e.Err }

// setTopicAs sets the topic for a channel. A topic longer than
// [domain.TopicMaxLen] (this server's TOPICLEN) is refused with
// [domain.ErroneousTopicError] before anything else runs: the topic
// is repeated into every dispatch turn's prompt for the channel, so
// an oversized value never enters channel state; it is refused, not
// truncated.
func (s *Session) setTopicAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, topic string) error {
	if reason := domain.ValidateTopic(topic); reason != domain.TopicAccepted {
		return domain.ErroneousTopicError{Channel: ch, Reason: reason, At: s.now()}
	}

	actorNick := actor.Nick()

	return s.inSpan(ctx, "session.set_topic", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	}, func(ctx context.Context, span trace.Span) error {
		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actor.ID())))

		if domain.InferChannelKind(ch) != domain.KindChannel {
			return observability.ErrWithKind(fmt.Errorf("cannot set topic on a direct message"), observability.ErrorKindValidation)
		}

		now := s.now()

		window, err := s.loadChannelWindow(ctx, ch)
		if err != nil {
			return fmt.Errorf("get channel: %w", err)
		}

		ch = window.Name()

		// RFC 2812 §3.2.4: setting a topic requires being on the
		// channel, whatever the channel's modes say, and a server
		// operator is no exception. The `+o` override waives channel-op
		// status, which is a privilege among the members; it does not
		// make a non-member a member, and a topic set by someone who
		// is not there has no author anyone in the channel can address.
		if !window.Members.HasInstance(actor) {
			return domain.NotOnChannelError{Channel: ch, Command: "TOPIC", At: s.now()}
		}

		// `+t` restricts TOPIC to ops (RFC 2811 §4.2.7). When the
		// channel doesn't carry `+t`, any member can change topic.
		if window.Modes.TopicLock {
			if err := s.requireChannelOp(actor, window, "TOPIC", ch); err != nil {
				return err
			}
		}

		// A TOPIC command that leaves the topic unchanged is a no-op:
		// IRC servers conventionally suppress the wire event, and
		// without this guard a chatty model can re-set the same
		// string on every turn and the channel sees a stream of
		// duplicate TopicChange events.
		if window.Topic == topic {
			return nil
		}

		window.Topic = topic
		window.TopicSetBy = actorNick
		window.TopicSetAt = now

		if err := s.commitChannelUpdate(ctx, window, domain.TopicChange{
			Source: domain.ClientSource(actor.ID(), actorNick),
			Target: ch,
			Topic:  topic,
			At:     now,
		}); err != nil {
			return fmt.Errorf("commit topic change: %w", err)
		}

		return nil
	})
}

// kickAs removes a target from a channel on behalf of the actor.
func (s *Session) kickAs(ctx context.Context, actor, target *domain.Instance, ch domain.ChannelName) error {
	targetNick := target.Nick()

	return s.inSpan(ctx, "session.kick", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(targetNick)),
	}, func(ctx context.Context, span trace.Span) error {
		if domain.InferChannelKind(ch) != domain.KindChannel {
			return observability.ErrWithKind(fmt.Errorf("cannot kick from a direct message"), observability.ErrorKindValidation)
		}

		window, err := s.loadChannelWindow(ctx, ch)
		if err != nil {
			return fmt.Errorf("get channel: %w", err)
		}

		ch = window.Name()

		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(target.ID())))

		if err := s.requireChannelOp(actor, window, "KICK", ch); err != nil {
			return err
		}

		if !window.Members.HasInstance(target) {
			return domain.UserNotInChannelError{Nick: targetNick, Channel: ch, Command: "KICK", At: s.now()}
		}

		now := s.now()
		if err := s.commitMemberDeparture(ctx, window, target, domain.Kicked{
			Source:  domain.ClientSource(actor.ID(), actor.Nick()),
			Target:  ch,
			Subject: targetNick,
			At:      now,
		}); err != nil {
			return err
		}

		s.interruptModelWindow(protocol.ClientID(target.ID()), ch)

		return nil
	})
}

// inviteAs implements RFC 2812 §3.2.7's INVITE command. The invitee
// is recorded against the channel's [domain.Invitations] set so a
// follow-up JOIN can pass `+i`. Delivery is scoped to the inviter
// and invitee: the returned [domain.Invited] value is written directly
// to the invitee's subscription as the wire `INVITE` message.
// [Session.handleInvite] constructs a separate [domain.Inviting]
// reply for the issuer. The channel event log is not touched and no
// broadcast happens; other channel members are not told.
//
// The gates run in RFC order, and nothing is recorded until every
// one of them has passed:
//
//   - the inviter must be on the channel, whatever its modes, or it
//     is [domain.NotOnChannelError] (numeric 442 ERR_NOTONCHANNEL).
//     An invitation is a member vouching for someone, so a client
//     that is not there has nothing to vouch with.
//   - on a `+i` channel the inviter must additionally hold `@`
//     (§3.2.7). On `-i` channels any member may invite.
//   - a target already on the channel is [domain.UserOnChannelError]
//     (numeric 443 ERR_USERONCHANNEL).
//
// The target is resolved, via [Session.resolveConnectedNick], before
// the invitation is written, so an unknown nick leaves the channel's
// invitation set untouched rather than holding an entry for a client
// that does not exist. Resolving against the registry of connected
// clients is what an invitation asks for: somebody the server can
// deliver it to, which a nick an instances row still holds is not if
// nothing ever attached under it. The inviter gets a typed
// [domain.UnknownNickError] and a private [domain.SystemNotice].
func (s *Session) inviteAs(ctx context.Context, actor *domain.Instance, target domain.Nick, ch domain.ChannelName) (domain.ProtocolEvent, error) {
	actorNick := actor.Nick()

	var event domain.ProtocolEvent

	err := s.inSpan(ctx, "session.invite", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
		attribute.String("nick.target", string(target)),
	}, func(ctx context.Context, span trace.Span) error {
		target = domain.Nick(strings.TrimSpace(string(target)))
		if target == "" {
			return fmt.Errorf("target nick is required")
		}

		window, err := s.loadChannelWindow(ctx, ch)
		if err != nil {
			return fmt.Errorf("get channel: %w", err)
		}

		ch = window.Name()

		if !window.Members.HasInstance(actor) {
			return domain.NotOnChannelError{Channel: ch, Command: "INVITE", At: s.now()}
		}

		if window.Modes.InviteOnly {
			if err := s.requireChannelOp(actor, window, "INVITE", ch); err != nil {
				return err
			}
		}

		if _, alreadyMember := window.Members.GetByNick(target); alreadyMember {
			return domain.UserOnChannelError{Nick: target, Channel: ch, At: s.now()}
		}

		now := s.now()

		inst, err := s.resolveConnectedNick(target)
		if err != nil {
			var unknown domain.UnknownNickError
			if errors.As(err, &unknown) {
				event = domain.SystemNotice{
					Target: ch,
					Text:   fmt.Sprintf("no such nick: %s", target),
					At:     now,
				}
			}

			return err
		}

		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(inst.ID())))

		if !window.Invitations.Contains(inst.ID()) {
			window.Invitations.Add(inst.ID())
			persistErr := s.persistChannelWindowWithInvitationChange(ctx, window, inst.ID())
			if persistErr != nil {
				return fmt.Errorf("save channel: %w", persistErr)
			}
		}

		invited := domain.Invited{
			Source:  domain.ClientSource(actor.ID(), actorNick),
			Target:  ch,
			Invitee: inst.Nick(),
			At:      now,
		}

		delivery := invited
		if window.Modes.Anonymous {
			delivery.Source = domain.AnonymousSource()
		}
		s.deliverToClient(ctx, inst.ID(), delivery)

		event = invited

		return nil
	})

	return event, err
}

// deliverToClient writes a single event directly to the
// subscription registered under `id`, bypassing
// [Session.fanOutProtocol]. Used by commands whose RFC scope
// names a specific recipient (INVITE, user-mode replies) rather
// than the channel-wide audience.
func (s *Session) deliverToClient(ctx context.Context, id domain.InstanceID, evt domain.ProtocolEvent) {
	s.emitScoped(ctx, evt, clientScope{client: id})
}

func (s *Session) deliverToClosingClient(
	ctx context.Context,
	client *serverClient,
	event domain.ProtocolEvent,
) {
	if client == nil {
		return
	}

	s.enqueueRoutes(ctx, []routedDelivery{{
		sub:      client,
		terminal: true,
		delivery: protocol.Delivery{
			Event:   event,
			SpanCtx: trace.SpanContextFromContext(ctx),
		},
	}})
}

// setUserModeAs mutates a single user-mode flag on `target` and
// announces the change via a [domain.UserModeChange]. Delivered only
// to the affected client — RFC 2812 §3.1.5 scopes user-mode replies
// to the requester — so this bypasses [Session.fanOutProtocol] and
// writes directly to the target's events channel.
//
// Empty `by` signals a server-originated change (the canonical OPER
// MODE response shape per RFC §3.1.4). The affected client consumes
// the event to refresh its capability-gated command palette; it
// raises no scrollback line.
//
// Idempotent: a grant for an already-held mode (or a clear for an
// unheld mode) is a no-op and emits nothing.
func (s *Session) setUserModeAs(ctx context.Context, by domain.Nick, target *serverClient, mode domain.Mode, add bool) {
	if !target.setMode(mode, add) {
		return
	}
	s.directoryGeneration.Add(1)

	targetInst := target.instance

	_ = s.inSpan(ctx, "session.set_user_mode", []attribute.KeyValue{
		attribute.String(observability.AttrNick, string(targetInst.Nick())),
		attribute.String(observability.AttrInstanceID, string(targetInst.ID())),
		attribute.String("mode.flag", string(mode)),
		attribute.Bool("mode.add", add),
	}, func(ctx context.Context, _ trace.Span) error {
		source := domain.ServerSource("")
		if by != "" {
			source = domain.LegacyClientSource(by)
		}

		s.deliverToClient(ctx, targetInst.ID(), domain.UserModeChange{
			Source:  source,
			Subject: targetInst.Nick(),
			Flag:    mode,
			Add:     add,
			At:      s.now(),
		})

		return nil
	})
}

// registerModelAs claims `nick` for a fresh model instance and
// records it. This is the registration half of `ADDMODEL`: the
// instance exists and holds its nick, but has no subscription and is
// in no channel yet. The dispatcher's [protocol.AddModel] handler is
// the only caller and has already run the operator gate.
//
// Runs on the session's command loop. `nick` was chosen against a
// snapshot of the nick space taken before the loop was reached, so
// the claim is checked here: this is the point at which the nick is
// actually taken, and the only point where the check cannot be
// overtaken by a concurrent rename.
func (s *Session) registerModelAs(
	ctx context.Context,
	ch domain.ChannelName,
	modelID domain.ModelID,
	nick domain.Nick,
	persona string,
) (*domain.Instance, error) {
	var inst *domain.Instance

	err := s.inSpan(ctx, "session.register_model", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(nick)),
		attribute.String(observability.AttrModelID, string(modelID)),
	}, func(ctx context.Context, _ trace.Span) error {
		if err := s.requireNickAvailable(ctx, nick, nil); err != nil {
			return err
		}

		if _, err := s.loadChannelWindow(ctx, ch); err != nil {
			return fmt.Errorf("get channel: %w", err)
		}

		inst = domain.NewModelInstance(
			domain.GenerateInstanceID(),
			nick,
			modelID,
			persona,
			nil,
		)

		if err := s.store.SaveInstance(ctx, inst); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return inst, nil
}

// requireNickAvailable is the one place a nick is checked before it
// is claimed, whether the claim comes from `NICK` or from the
// registration half of `ADDMODEL`. It answers the two refusals IRC
// distinguishes, in the order a server does: a nick outside the RFC
// 2812 §2.3.1 grammar is [domain.ErroneousNicknameError] (numeric
// 432) and no client will ever hold it; a well-formed nick another
// client holds is [domain.NickInUseError] (numeric 433) and the
// answer changes when that client renames or quits.
//
// `holder` is the instance allowed to already hold the nick, which
// is the renaming client under `NICK`, so re-taking one's own nick
// in a different case is not a collision. Pass nil when no client
// may hold it.
//
// Only a clean "no such nick" from the store counts as free: any
// other resolve failure is returned, so a store that is briefly
// unavailable refuses the claim and no duplicate gets through.
func (s *Session) requireNickAvailable(ctx context.Context, nick domain.Nick, holder *domain.Instance) error {
	if reason := domain.ValidateNick(nick); reason != domain.NickAccepted {
		return observability.ErrWithKind(
			domain.ErroneousNicknameError{Nick: nick, Reason: reason, At: s.now()},
			observability.ErrorKindValidation,
		)
	}

	existing, err := s.store.ResolveNick(ctx, nick)

	switch {
	case err == nil:
		if existing == holder {
			return nil
		}

		return observability.ErrWithKind(domain.NickInUseError{Nick: nick, At: s.now()}, observability.ErrorKindValidation)
	case errors.Is(err, store.ErrNoSuchNick):
		return nil
	default:
		return fmt.Errorf("resolve nick: %w", err)
	}
}

// admitModelAs joins a registered model instance to `ch`, the
// closing half of `ADDMODEL`. The bus carries a `Join` event with
// the same wire shape any `/join` would produce. `actor` is the
// operator who issued the command; it is the join's authority, so a
// `+i` channel admits the new instance on the operator's privileges
// (see [Session.checkJoinGates]).
//
// Runs on the session's command loop, after the instance's
// model-client has attached, so the JOIN reaches its subscription.
func (s *Session) admitModelAs(
	ctx context.Context,
	issuer protocol.Client,
	actor *domain.Instance,
	modelClient protocol.Client,
	inst *domain.Instance,
	ch domain.ChannelName,
) (domain.ChannelName, error) {
	var admittedChannel domain.ChannelName
	err := s.inSpan(ctx, "session.add_model", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actor.Nick())),
		attribute.String(observability.AttrInstanceID, string(inst.ID())),
	}, func(ctx context.Context, _ trace.Span) error {
		window, err := s.loadChannelWindow(ctx, ch)
		if err != nil {
			if errors.Is(err, store.ErrNoSuchChannel) {
				return domain.NotOnChannelError{Channel: ch, Command: "ADDMODEL", At: s.now()}
			}

			return fmt.Errorf("get channel before model admission: %w", err)
		}

		ch = window.Name()
		admittedChannel = ch
		if s.clientOwner(protocol.ClientID(actor.ID())) != issuer ||
			!s.idHasServerOper(protocol.ClientID(actor.ID())) {
			return domain.NotOperatorError{Command: "ADDMODEL", At: s.now()}
		}
		if !actor.InChannel(ch) || !window.Members.HasInstance(actor) {
			return domain.NotOnChannelError{Channel: ch, Command: "ADDMODEL", At: s.now()}
		}

		if modelClient == nil || s.clientOwner(protocol.ClientID(inst.ID())) != modelClient {
			return fmt.Errorf(
				"model client %q disconnected before admission: %w",
				inst.ID(), protocol.ErrSubscriptionClosed,
			)
		}

		canonical, err := s.store.GetInstanceByID(ctx, inst.ID())
		if err != nil {
			return fmt.Errorf("resolve model before admission: %w", err)
		}
		if canonical != inst {
			return fmt.Errorf("model instance %q changed before admission", inst.ID())
		}

		_, err = s.joinAs(ctx, inst, operatorJoin, ch, "")

		return err
	})

	return admittedChannel, err
}

// killAs is the operator-issued forced disconnect of `target` per
// RFC 2812 §3.7.1. The target receives KILL first. `quitAs` then
// broadcasts QUIT to the target and peers in shared channels with the
// conventional `"Killed by <oper> (<reason>)"` body. The dispatcher
// sends the terminal ERROR after teardown completes.
//
// The dispatcher's `handleKill` is the only caller and runs the
// operator gate, so this method assumes `oper` has the
// authority. The reap of the target's subscription happens in
// the dispatcher too, after this returns.
//
// The command names a nick, and no nick is exempt, the operator's
// own included. The teardown the channels see is the same one for
// every target. What the server can do about the connection
// underneath differs: `Session.releaseClient` and
// `Session.reapClient` refuse for the client whose lifetime is the
// session's.
func (s *Session) killAs(ctx context.Context, oper, target *domain.Instance, reason string) quitOutcome {
	body := fmt.Sprintf("Killed by %s (%s)", oper.Nick(), reason)
	kill := domain.KillNotice{
		Source:  domain.ClientSource(oper.ID(), oper.Nick()),
		Subject: target.Nick(),
		Reason:  reason,
		At:      s.now(),
	}

	return s.quit(ctx, target, body, quitForced, kill)
}
