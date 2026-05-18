package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	orderedmap "github.com/wk8/go-ordered-map/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

// joinAs joins the given actor to a channel. `key` carries the
// channel password for keyed (`+k`) channels — empty for unkeyed
// joins. `+i`, `+l`, and `+k` gate the add against an existing
// channel; a fresh channel (this call creates it) has no modes
// and so no gate applies.
func (s *Session) joinAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, key string) (retErr error) {
	ch = domain.NormaliseChannelName(ch)

	actorNick := actor.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.join",
		attribute.String(observability.AttrOperation, "session.join"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actor.ID())))

	now := s.now()
	isUser := actor.ID() == ""

	window, created, err := s.ensureChannelWindowWithActor(ctx, ch, actor, now)
	if err != nil {
		return err
	}

	alreadyMember := !created && window.Members.HasInstance(actor)

	if !created && !alreadyMember {
		if err := s.checkJoinGates(window, actorNick, key); err != nil {
			return err
		}

		window.Members.Add(actor)

		if err := s.persistChannelWindow(ctx, window); err != nil {
			return fmt.Errorf("save channel: %w", err)
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
	if created {
		window.Members.SetMode(actor, domain.ModeOp)
		if isUser {
			s.setUserMode(ctx, ch, domain.ModeOp)
		}

		if err := s.persistChannelWindow(ctx, window); err != nil {
			return fmt.Errorf("save channel after mode: %w", err)
		}
	}

	if isUser {
		// Stamp the user's mark-as-read cursor at the current head so
		// the join itself does not leave the channel showing as unread.
		// `last_channel` persistence is the UI's concern and lands when
		// the chat screen receives a `ChannelActiveMsg`.
		if err := s.markRead(ctx, ch); err != nil {
			return fmt.Errorf("mark read: %w", err)
		}
	}

	if !alreadyMember {
		if err := s.recordActorMembership(ctx, actor, ch, now, isUser); err != nil {
			return err
		}
	}

	if alreadyMember {
		return nil
	}

	s.persistAndEmit(ctx, ch, domain.Join{
		Target:     ch,
		Nick:       actorNick,
		InstanceID: actor.ID(),
		Created:    created,
		At:         now,
		Instance:   actor,
	})

	window, err = s.loadChannelWindow(ctx, ch)
	if err != nil {
		return fmt.Errorf("reload channel after join: %w", err)
	}

	// RFC 2812 §3.2.1 / §3.2.4: every joiner gets the channel's
	// member list and topic. The chat-screen consumes
	// NamesReplyEvent to populate its member-list cache, and the
	// model-client's dispatch loop files TopicInfo into history so
	// the model knows who set the topic and when.
	s.emit(ctx, domain.NamesReplyEvent{
		Channel: ch,
		Members: window.Members,
		At:      now,
	})

	if window.Topic != "" {
		s.emit(ctx, domain.TopicInfo{
			Target:     ch,
			Topic:      window.Topic,
			TopicSetBy: window.TopicSetBy,
			TopicSetAt: window.TopicSetAt,
			At:         now,
		})
	}

	if isUser {
		return s.saveAutojoinList(ctx)
	}

	return nil
}

// ensureChannelWindowWithActor loads the channel-window or creates
// a fresh one that already contains the actor. Returns the
// (possibly freshly-saved) `*ChannelWindow`, whether it was newly
// created, and any persistence error encountered along the way.
// joinAs is the only caller and is gated on `#`-prefixed names by
// `NormaliseChannelName`, so a load that returns a non-channel
// row indicates a programming error in the upstream guard rather
// than a user-reachable state.
func (s *Session) ensureChannelWindowWithActor(ctx context.Context, ch domain.ChannelName, actor *domain.Instance, now time.Time) (*domain.ChannelWindow, bool, error) {
	window, err := s.loadChannelWindow(ctx, ch)
	if err == nil {
		return window, false, nil
	}

	window = domain.NewChannelWindow(ch, now)
	window.Members.Add(actor)

	if saveErr := s.persistChannelWindow(ctx, window); saveErr != nil {
		return nil, false, fmt.Errorf("save channel: %w", saveErr)
	}

	return window, true, nil
}

// recordActorMembership stamps the channel onto the actor's joined-
// channels map and — for model actors — persists the updated
// instance row.
func (s *Session) recordActorMembership(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, now time.Time, isUser bool) error {
	actor.MutateChannels(func(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
		if _, ok := m.Get(ch); !ok {
			m.Set(ch, now)
		}
	})

	if isUser {
		return nil
	}

	if err := s.store.SaveInstance(ctx, actor); err != nil {
		return fmt.Errorf("save instance: %w", err)
	}

	return nil
}

// partAs parts the given actor from a channel.
func (s *Session) partAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, message string) (retErr error) {
	actorNick := actor.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.part",
		attribute.String(observability.AttrOperation, "session.part"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	if domain.InferChannelKind(ch) != domain.KindChannel {
		return errWithKind(fmt.Errorf("cannot part %s", ch), observability.ErrorKindValidation)
	}

	window, err := s.loadChannelWindow(ctx, ch)
	if err != nil {
		return fmt.Errorf("channel not found: %w", err)
	}

	isUser := actor.ID() == ""

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actor.ID())))

	m, ok := window.Members.GetByInstance(actor)
	if !ok {
		return nil
	}

	window.Members.Remove(m)

	if err := s.persistChannelWindow(ctx, window); err != nil {
		return fmt.Errorf("save channel: %w", err)
	}

	actor.MutateChannels(func(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
		m.Delete(ch)
	})

	if isUser {
		s.forgetUserMode(ctx, ch)
		if err := s.saveAutojoinList(ctx); err != nil {
			return fmt.Errorf("save autojoin: %w", err)
		}
	} else if err := s.store.SaveInstance(ctx, actor); err != nil {
		return fmt.Errorf("save instance: %w", err)
	}

	now := s.now()
	s.persistAndEmit(ctx, ch, domain.Part{
		Target:     ch,
		Nick:       actorNick,
		InstanceID: actor.ID(),
		Message:    message,
		At:         now,
		Instance:   actor,
	})

	return nil
}

// quitAs disconnects the given actor from every joined channel.
// For the user-client the call saves the autojoin list and clears
// the session-active marker so the next startup is classified as
// clean; the QUIT lines are persisted but not broadcast, because
// the only consumer of broadcast events (the chat-screen) is
// about to tear down. For a model-client the call broadcasts the
// `domain.Quit` event to common-channel peers and deletes the
// instance row — the dispatcher reaps the subscription separately
// via [Session.reapClient].
func (s *Session) quitAs(ctx context.Context, actor *domain.Instance, message string) (retErr error) {
	if actor.ID() == "" {
		return s.userQuit(ctx, message)
	}

	return s.modelQuit(ctx, actor, message)
}

func (s *Session) modelQuit(ctx context.Context, actor *domain.Instance, message string) (retErr error) {
	actorID := actor.ID()
	actorNick := actor.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.quit",
		attribute.String(observability.AttrOperation, "session.quit"),
		attribute.String(observability.AttrNick, string(actorNick)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actorID)))

	now := s.now()
	channels := s.instanceChannelNames(actor)

	s.propagateActorEvent(ctx, actor, actorEventConfig{
		mutate: func(window *domain.ChannelWindow) {
			if m, ok := window.Members.GetByInstance(actor); ok {
				window.Members.Remove(m)
			}
		},
		build: func() broadcastEvent {
			return domain.Quit{
				Nick:       actorNick,
				InstanceID: actorID,
				Message:    message,
				At:         now,
				Instance:   actor,
			}
		},
	})

	actor.MutateChannels(func(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
		for _, ch := range channels {
			m.Delete(ch)
		}
	})

	if err := s.store.SaveInstance(ctx, actor); err != nil {
		return fmt.Errorf("save instance: %w", err)
	}

	if err := s.store.DeleteInstanceByID(ctx, actorID); err != nil {
		return fmt.Errorf("delete instance: %w", err)
	}

	return nil
}

// changeNickAs changes the given actor's nickname.
func (s *Session) changeNickAs(ctx context.Context, actor *domain.Instance, newNick domain.Nick) (retErr error) {
	oldNick := actor.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.change_nick",
		attribute.String(observability.AttrOperation, "session.change_nick"),
		attribute.String(observability.AttrNick, string(oldNick)),
		attribute.String("nick.new", string(newNick)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	if newNick == oldNick {
		return nil
	}

	if existing, err := s.ResolveNick(ctx, newNick); err == nil && existing != actor {
		return errWithKind(domain.NickInUseError{Nick: newNick, At: s.now()}, observability.ErrorKindValidation)
	}

	isUser := actor.ID() == ""

	actor.SetNick(newNick)

	if !isUser {
		// The instances table is keyed by InstanceID, so a rename is
		// an in-place update of the `nick` column — no delete-then-
		// reinsert needed as it was under the old nick-keyed schema.
		if err := s.store.SaveInstance(ctx, actor); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}
	}

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actor.ID())))

	now := s.now()
	actorID := actor.ID()

	s.propagateActorEvent(ctx, actor, actorEventConfig{
		mutate: func(window *domain.ChannelWindow) {
			window.Members.RenameTo(actor, newNick)
		},
		build: func() broadcastEvent {
			return domain.NickChange{
				OldNick:    oldNick,
				NewNick:    newNick,
				InstanceID: actorID,
				At:         now,
				Instance:   actor,
			}
		},
	})

	return nil
}

// sendMessageAs records a message from the given actor and
// returns the persisted [domain.Message]. The message is emitted
// via [Session.emit]; the broadcast helper applies the
// originator-suppression rule (RFC 2812 §3.3.1) so the sender
// does not see their own line on its [protocol.Client.Events]
// channel.
func (s *Session) sendMessageAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, body string) (msg domain.Message, retErr error) {
	actorNick := actor.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.send_message",
		attribute.String(observability.AttrOperation, "session.send_message"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	instanceID := actor.ID()
	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(instanceID)))

	if err := s.checkSendGates(ctx, actor, ch); err != nil {
		return domain.Message{}, err
	}

	cm := domain.Message{
		Target:     ch,
		From:       actorNick,
		InstanceID: instanceID,
		Body:       body,
		At:         s.now(),
	}

	s.appendEvent(ctx, ch, cm)
	s.emit(ctx, cm)

	return cm, nil
}

// sendActionAs records an action message from the given actor.
// See [Session.sendMessageAs] for echo semantics.
func (s *Session) sendActionAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, body string) (msg domain.Message, retErr error) {
	actorNick := actor.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.send_action",
		attribute.String(observability.AttrOperation, "session.send_action"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	instanceID := actor.ID()
	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(instanceID)))

	if err := s.checkSendGates(ctx, actor, ch); err != nil {
		return domain.Message{}, err
	}

	cm := domain.Message{
		Target:     ch,
		From:       actorNick,
		InstanceID: instanceID,
		Body:       body,
		Action:     true,
		At:         s.now(),
	}

	s.appendEvent(ctx, ch, cm)
	s.emit(ctx, cm)

	return cm, nil
}

// setTopicAs sets the topic for a channel.
func (s *Session) setTopicAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, topic string) (retErr error) {
	actorNick := actor.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.set_topic",
		attribute.String(observability.AttrOperation, "session.set_topic"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(actor.ID())))

	if domain.InferChannelKind(ch) != domain.KindChannel {
		return errWithKind(fmt.Errorf("cannot set topic on a direct message"), observability.ErrorKindValidation)
	}

	now := s.now()

	window, err := s.loadChannelWindow(ctx, ch)
	if err != nil {
		return fmt.Errorf("get channel: %w", err)
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

	if err := s.persistChannelWindow(ctx, window); err != nil {
		return fmt.Errorf("save channel: %w", err)
	}

	s.persistAndEmit(ctx, ch, domain.TopicChange{
		Target:     ch,
		Topic:      topic,
		By:         actorNick,
		InstanceID: actor.ID(),
		At:         now,
		ByInstance: actor,
	})

	return nil
}

// kickAs removes a target from a channel on behalf of the actor.
func (s *Session) kickAs(ctx context.Context, actor, target *domain.Instance, ch domain.ChannelName) (retErr error) {
	targetNick := target.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.kick",
		attribute.String(observability.AttrOperation, "session.kick"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(targetNick)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	if domain.InferChannelKind(ch) != domain.KindChannel {
		return errWithKind(fmt.Errorf("cannot kick from a direct message"), observability.ErrorKindValidation)
	}

	window, err := s.loadChannelWindow(ctx, ch)
	if err != nil {
		return fmt.Errorf("get channel: %w", err)
	}

	span.SetAttributes(attribute.String(observability.AttrInstanceID, string(target.ID())))

	if err := s.requireChannelOp(actor, window, "KICK", ch); err != nil {
		return err
	}

	if !window.Members.HasInstance(target) {
		return nil
	}

	if m, ok := window.Members.GetByInstance(target); ok {
		window.Members.Remove(m)
	}

	if err := s.persistChannelWindow(ctx, window); err != nil {
		return fmt.Errorf("save channel: %w", err)
	}

	target.MutateChannels(func(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
		m.Delete(ch)
	})

	if target.ID() != "" {
		if err := s.store.SaveInstance(ctx, target); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}
	} else {
		s.forgetUserMode(ctx, ch)
	}

	actorNick := actor.Nick()

	now := s.now()
	s.persistAndEmit(ctx, ch, domain.ModelKicked{
		Target:       ch,
		Nick:         targetNick,
		InstanceID:   target.ID(),
		By:           actorNick,
		ByInstanceID: actor.ID(),
		At:           now,
		Instance:     target,
	})

	return nil
}

// inviteAs implements RFC 2812 §3.2.7's INVITE command. The
// invited nick is recorded against the channel's `InvitedNicks`
// set so a follow-up JOIN can clear `+i`. Delivery is scoped to
// the inviter and invitee: the returned [domain.ModelInvited]
// envelope is the inviter's `RPL_INVITING`-equivalent (the
// caller — [Session.handleInvite] — wraps it in `Response.Events`
// for the synchronous client reply), and the same envelope is
// written directly to the invitee's subscription as their wire
// `INVITE` message. The channel event log is not touched and no
// broadcast happens; other channel members are not told.
//
// An unknown target nick has no subscription to receive the
// invite. The inviter gets a [domain.SystemNotice] in its place
// so the chat-screen surfaces the missing-nick condition; the
// channel still records nothing.
func (s *Session) inviteAs(ctx context.Context, actor *domain.Instance, target domain.Nick, ch domain.ChannelName) (event domain.ProtocolEvent, retErr error) {
	actorNick := actor.Nick()

	ctx, span := s.startSpan(
		ctx,
		"session.invite",
		attribute.String(observability.AttrOperation, "session.invite"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actorNick)),
		attribute.String("nick.target", string(target)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	target = domain.Nick(strings.TrimSpace(string(target)))
	if target == "" {
		return nil, fmt.Errorf("target nick is required")
	}

	window, err := s.loadChannelWindow(ctx, ch)
	if err != nil {
		return nil, fmt.Errorf("get channel: %w", err)
	}

	// INVITE is op-gated only when the target channel carries
	// `+i` (RFC 2812 §3.2.7). On `-i` channels any member can
	// invite.
	if window.Modes.InviteOnly {
		if err := s.requireChannelOp(actor, window, "INVITE", ch); err != nil {
			return nil, err
		}
	}

	window.InvitedNicks.Add(target)
	if err := s.persistChannelWindow(ctx, window); err != nil {
		return nil, fmt.Errorf("save channel: %w", err)
	}

	now := s.now()

	inst, err := s.store.ResolveNick(ctx, target)
	switch {
	case err == nil:
		span.SetAttributes(attribute.String(observability.AttrInstanceID, string(inst.ID())))

		invited := domain.ModelInvited{
			Target:       ch,
			Nick:         inst.Nick(),
			InstanceID:   inst.ID(),
			By:           actorNick,
			ByInstanceID: actor.ID(),
			At:           now,
			Instance:     inst,
		}

		s.deliverToClient(ctx, inst.ID(), invited)

		return invited, nil

	case errors.Is(err, store.ErrNoSuchNick):
		notice := domain.SystemNotice{
			Target: ch,
			Text:   fmt.Sprintf("no such nick: %s", target),
			At:     now,
		}

		return notice, nil

	default:
		return nil, fmt.Errorf("resolve nick: %w", err)
	}
}

// deliverToClient writes a single event directly to the
// subscription registered under `id`, bypassing
// [Session.fanOutProtocol]. Used by commands whose RFC scope
// names a specific recipient (INVITE, user-mode replies) rather
// than the channel-wide audience.
func (s *Session) deliverToClient(ctx context.Context, id domain.InstanceID, evt domain.ProtocolEvent) {
	target := s.lookupClientHandle(protocol.ClientID(id))
	if target == nil {
		return
	}

	select {
	case target.events <- protocol.Delivery{
		Event:   evt,
		SpanCtx: trace.SpanContextFromContext(ctx),
	}:
	case <-target.done:
	case <-ctx.Done():
	}
}

// setUserModeAs mutates a single user-mode flag on `target` and
// announces the change via a [domain.ModeChange] with empty
// `Target` (the user-mode form). Delivered only to the affected
// client — RFC 2812 §3.1.5 scopes user-mode replies to the
// requester — so this bypasses [Session.fanOutProtocol] and
// writes directly to the target's events channel.
//
// Empty `by` signals server-originated (the canonical OPER MODE
// response shape per RFC §3.1.4): the chat-screen renderer prints
// `*** server sets mode +o nick` rather than attributing to a
// peer nick.
//
// Idempotent: a grant for an already-held mode (or a clear for an
// unheld mode) is a no-op and emits nothing.
func (s *Session) setUserModeAs(ctx context.Context, by domain.Nick, target *serverClient, mode domain.Mode, add bool) {
	if !target.setMode(mode, add) {
		return
	}

	targetInst := target.instance

	ctx, span := s.startSpan(ctx, "session.set_user_mode",
		attribute.String(observability.AttrOperation, "session.set_user_mode"),
		attribute.String(observability.AttrNick, string(targetInst.Nick())),
		attribute.String(observability.AttrInstanceID, string(targetInst.ID())),
		attribute.String("mode.flag", string(mode)),
		attribute.Bool("mode.add", add),
	)
	defer func() {
		span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
		span.End()
	}()

	evt := domain.ModeChange{
		Nick:       targetInst.Nick(),
		InstanceID: targetInst.ID(),
		Flag:       mode,
		Add:        add,
		By:         by,
		At:         s.now(),
		Instance:   targetInst,
	}

	select {
	case target.events <- protocol.Delivery{
		Event:   evt,
		SpanCtx: trace.SpanContextFromContext(ctx),
	}:
	case <-target.done:
		// Target was reaped between resolution and delivery; drop.
	case <-ctx.Done():
	}
}

// checkSendGates enforces the per-channel PRIVMSG / Action
// preconditions tied to channel modes: `+n` requires the sender
// to be a channel member; `+m` requires the sender to hold voice
// or op; `+q` silences everyone except ops. DMs (non-channel
// targets) skip the check — they have no member list and no
// channel modes.
//
// Each rejection carries a typed [domain.SendBlockReason] so
// renderers can format the right message without parsing a
// free-form error string.
func (s *Session) checkSendGates(ctx context.Context, actor *domain.Instance, ch domain.ChannelName) error {
	if domain.InferChannelKind(ch) != domain.KindChannel {
		return nil
	}

	window, err := s.loadChannelWindow(ctx, ch)
	if errors.Is(err, store.ErrNoSuchChannel) {
		// A missing channel has no gates of its own; the
		// append/emit path produces the right error downstream.
		return nil
	}
	if err != nil {
		return fmt.Errorf("load channel: %w", err)
	}

	member, isMember := window.Members.GetByInstance(actor)

	if window.Modes.NoExternal && !isMember {
		return domain.CannotSendToChannelError{Channel: ch, Reason: domain.SendBlockNoExternal, At: s.now()}
	}

	if window.Modes.Quiet && (!isMember || member.Mode != domain.ModeOp) {
		return domain.CannotSendToChannelError{Channel: ch, Reason: domain.SendBlockQuiet, At: s.now()}
	}

	if window.Modes.Moderated {
		if !isMember || (member.Mode != domain.ModeOp && member.Mode != domain.ModeVoice) {
			return domain.CannotSendToChannelError{Channel: ch, Reason: domain.SendBlockModerated, At: s.now()}
		}
	}

	return nil
}

// checkJoinGates enforces the per-channel JOIN preconditions
// every existing channel imposes via its mode set: `+i` admits
// only previously-invited nicks (and consumes the invitation on
// success); `+l` rejects when the member count reaches the
// limit; `+k` rejects on key mismatch.
//
// Returns nil for a channel with no gates active. On `+i` the
// consume happens on success — the next attempt by the same
// nick fails unless re-invited.
func (s *Session) checkJoinGates(window *domain.ChannelWindow, actorNick domain.Nick, key string) error {
	if window.Modes.Key != "" && key != window.Modes.Key {
		return domain.ChannelKeyMismatchError{Channel: window.Name(), At: s.now()}
	}

	if window.Modes.UserLimit > 0 && window.Members.Len() >= window.Modes.UserLimit {
		return domain.ChannelFullError{Channel: window.Name(), At: s.now()}
	}

	if window.Modes.InviteOnly {
		if !window.InvitedNicks.Contains(actorNick) {
			return domain.ChannelInviteOnlyError{Channel: window.Name(), At: s.now()}
		}
		window.InvitedNicks.Remove(actorNick)
	}

	return nil
}

// requireChannelOp returns [domain.ChanOpRequiredError] when the
// actor lacks `@` in `window`. Used by channel-op-gated commands
// (`MODE`, `KICK`, `+t`-conditional `TOPIC`, `+i`-conditional
// `INVITE`) to short-circuit before mutation.
//
// Server operators (`+o` user-mode) override the channel-op
// requirement — RFC 2812 §3.7 and common ircd practice: a
// network operator can act on any channel regardless of channel-
// op status. In modeloff the user-client is the only server-OPER
// today; this is what lets the user `/kick` or `/topic` on
// channels where they joined without picking up `@`.
func (s *Session) requireChannelOp(actor *domain.Instance, window *domain.ChannelWindow, cmd string, ch domain.ChannelName) error {
	if s.actorHasServerOper(actor) {
		return nil
	}

	member, ok := window.Members.GetByInstance(actor)
	if !ok || member.Mode != domain.ModeOp {
		return domain.ChanOpRequiredError{Command: cmd, Channel: ch, At: s.now()}
	}
	return nil
}

// actorHasServerOper reports whether the actor's wire client
// carries `+o` user-mode. Used by [requireChannelOp] to honour
// the server-operator override on channel-op-gated commands.
func (s *Session) actorHasServerOper(actor *domain.Instance) bool {
	sc := s.lookupClientHandle(actor.ID())
	return sc != nil && sc.HasMode(domain.ModeOperator)
}

// applyChannelModeChangesAs is the entry for [protocol.ChannelMode].
// It loads the channel window, checks the actor's channel-op status
// once for the whole batch, validates every change's shape up front,
// then applies them in order. Up-front validation rejects the whole
// batch on a malformed entry so a `MODE` with a typo never half-
// applies. A runtime failure (e.g. unknown nick on `+o`) stops the
// loop and returns the error; already-applied changes remain,
// matching typical ircd behaviour.
func (s *Session) applyChannelModeChangesAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, changes []protocol.ChannelModeChange) (retErr error) {
	ctx, span := s.startSpan(ctx, "session.apply_channel_mode_changes",
		attribute.String(observability.AttrOperation, "session.apply_channel_mode_changes"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actor.Nick())),
		attribute.Int("mode.change_count", len(changes)),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	window, err := s.loadChannelWindow(ctx, ch)
	if err != nil {
		return fmt.Errorf("get channel: %w", err)
	}

	if err := s.requireChannelOp(actor, window, "MODE", ch); err != nil {
		return err
	}

	for _, change := range changes {
		if err := validateChannelModeChange(change, s.now()); err != nil {
			return err
		}
	}

	for _, change := range changes {
		if change.Flag.MemberMode() {
			if err := s.setMemberModeAs(ctx, window, ch, actor, change); err != nil {
				return err
			}
			continue
		}

		if err := s.setChannelAttributeAs(ctx, window, ch, actor, change); err != nil {
			return err
		}
	}

	return nil
}

// validateChannelModeChange enforces the per-flag shape rules:
// member modes take a nick target and no param; parametric
// attribute modes (`+l`, `+k`) take a param on add only; boolean
// attribute modes take neither. Unknown flags reject.
func validateChannelModeChange(change protocol.ChannelModeChange, now time.Time) error {
	switch change.Flag {
	case domain.ModeOperator, domain.ModeChannelVoice:
		if change.Target == "" {
			return domain.MissingModeParamError{Flag: change.Flag, At: now}
		}

	case domain.ModeUserLimit:
		if change.Add {
			n, err := strconv.Atoi(change.Param)
			if err != nil || n <= 0 {
				return domain.MissingModeParamError{Flag: change.Flag, At: now}
			}
		}

	case domain.ModeKey:
		if change.Add && change.Param == "" {
			return domain.MissingModeParamError{Flag: change.Flag, At: now}
		}

	case domain.ModeAnonymous, domain.ModeInviteOnly, domain.ModeModerated,
		domain.ModeNoExternal, domain.ModePrivate, domain.ModeQuiet,
		domain.ModeSecret, domain.ModeTopicLock:
		// boolean attribute mode, no param required

	default:
		return domain.UnknownModeFlagError{Flag: change.Flag, At: now}
	}

	return nil
}

// setMemberModeAs applies a member-mode change (`+o`/`+v` add or
// remove) to `change.Target`'s entry in `window.Members`, persists
// the window, and emits a [domain.ModeChange] to channel peers.
// Called from [applyChannelModeChangesAs] after up-front
// validation, so the shape invariants are already enforced.
func (s *Session) setMemberModeAs(ctx context.Context, window *domain.ChannelWindow, ch domain.ChannelName, actor *domain.Instance, change protocol.ChannelModeChange) (retErr error) {
	ctx, span := s.startSpan(ctx, "session.set_member_mode",
		attribute.String(observability.AttrOperation, "session.set_member_mode"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(change.Target)),
		attribute.String("mode.flag", string(change.Flag)),
		attribute.Bool("mode.add", change.Add),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	target, err := s.ResolveNick(ctx, change.Target)
	if err != nil {
		return err
	}

	nickMode := domain.NickModeFor(change.Flag, change.Add)
	window.Members.SetMode(target, nickMode)

	if err := s.persistChannelWindow(ctx, window); err != nil {
		return fmt.Errorf("save channel: %w", err)
	}

	s.persistAndEmit(ctx, ch, domain.ModeChange{
		Target:     ch,
		Nick:       target.Nick(),
		InstanceID: target.ID(),
		Flag:       change.Flag,
		Add:        change.Add,
		By:         actor.Nick(),
		At:         s.now(),
		Instance:   target,
	})

	return nil
}

// setChannelAttributeAs applies an attribute-mode change to the
// channel's `Modes` field, persists the window, and emits a
// [domain.ModeChange] to peers. Called from
// [applyChannelModeChangesAs] after validation; parametric `+l` /
// `+k` carry their value in `change.Param` (already a positive
// int or non-empty key, respectively).
func (s *Session) setChannelAttributeAs(ctx context.Context, window *domain.ChannelWindow, ch domain.ChannelName, actor *domain.Instance, change protocol.ChannelModeChange) (retErr error) {
	ctx, span := s.startSpan(ctx, "session.set_channel_attribute",
		attribute.String(observability.AttrOperation, "session.set_channel_attribute"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String("mode.flag", string(change.Flag)),
		attribute.Bool("mode.add", change.Add),
	)
	defer endSpan(span, &retErr, observability.ErrorKindStore)

	applyAttribute(&window.Modes, change)

	if err := s.persistChannelWindow(ctx, window); err != nil {
		return fmt.Errorf("save channel: %w", err)
	}

	s.persistAndEmit(ctx, ch, domain.ModeChange{
		Target: ch,
		Flag:   change.Flag,
		Add:    change.Add,
		Param:  attributeEmitParam(change),
		By:     actor.Nick(),
		At:     s.now(),
	})

	return nil
}

// applyAttribute mutates `modes` according to `change`. Boolean
// flags toggle directly; parametric flags clear on `-` and set on
// `+`. The caller guarantees shape via [validateChannelModeChange].
func applyAttribute(modes *domain.ChannelModes, change protocol.ChannelModeChange) {
	switch change.Flag {
	case domain.ModeAnonymous:
		modes.Anonymous = change.Add
	case domain.ModeInviteOnly:
		modes.InviteOnly = change.Add
	case domain.ModeModerated:
		modes.Moderated = change.Add
	case domain.ModeNoExternal:
		modes.NoExternal = change.Add
	case domain.ModePrivate:
		modes.Private = change.Add
	case domain.ModeQuiet:
		modes.Quiet = change.Add
	case domain.ModeSecret:
		modes.Secret = change.Add
	case domain.ModeTopicLock:
		modes.TopicLock = change.Add
	case domain.ModeUserLimit:
		if change.Add {
			n, _ := strconv.Atoi(change.Param)
			modes.UserLimit = n
		} else {
			modes.UserLimit = 0
		}
	case domain.ModeKey:
		if change.Add {
			modes.Key = change.Param
		} else {
			modes.Key = ""
		}
	}
}

// attributeEmitParam returns the parameter to include on the
// broadcast [domain.ModeChange] for an attribute change. Boolean
// modes and remove-form parametric modes emit no parameter.
func attributeEmitParam(change protocol.ChannelModeChange) string {
	if !change.Add {
		return ""
	}

	switch change.Flag {
	case domain.ModeUserLimit, domain.ModeKey:
		return change.Param
	}

	return ""
}

// addModelAs creates a fresh model instance, generates a unique
// nick for it via the small-model API, attaches the model-client,
// and joins it to the named channel via `joinAs`. The bus carries
// a `Join` event with the same wire shape any `/join` would
// produce. The dispatcher's [protocol.AddModel] handler is the
// only caller — the operator gate lives there, so this method
// assumes the caller has already verified `actor`'s authority.
//
// Persona resolution: a non-empty `persona` is used verbatim; an
// empty value triggers a draw from the personas pool (lazily
// generated if missing). API failures during persona generation
// are logged but never block the add; the instance gets an empty
// persona instead.
func (s *Session) addModelAs(
	ctx context.Context,
	actor *domain.Instance,
	ch domain.ChannelName,
	modelID domain.ModelID,
	persona string,
) (*domain.Instance, error) {
	ctx, span := s.startSpan(ctx, "session.add_model",
		attribute.String(observability.AttrOperation, "session.add_model"),
		attribute.String(observability.AttrChannel, string(ch)),
		attribute.String(observability.AttrNick, string(actor.Nick())),
	)
	defer span.End()

	nick, assignedPersona, err := s.modelClientFactory.PrepareInstance(ctx, s, modelID, persona)
	if err != nil {
		setSpanError(span, err, observability.ErrorKindDispatch)
		return nil, err
	}

	channels := orderedmap.New[domain.ChannelName, time.Time]()
	channels.Set(ch, s.now())

	inst := domain.NewModelInstance(
		domain.GenerateInstanceID(),
		nick,
		modelID,
		assignedPersona,
		channels,
	)

	if _, err := s.loadChannelWindow(ctx, ch); err != nil {
		setSpanError(span, err, observability.ErrorKindStore)
		return nil, fmt.Errorf("get channel: %w", err)
	}

	if err := s.store.SaveInstance(ctx, inst); err != nil {
		setSpanError(span, err, observability.ErrorKindStore)
		return nil, fmt.Errorf("save instance: %w", err)
	}

	if _, err := s.modelClientFactory.Attach(ctx, s, inst); err != nil {
		slog.Default().WarnContext(ctx, "attach model client",
			"component", "session",
			"instance_id", inst.ID(),
			"channel", ch,
			"error", err,
		)
	}

	// Pre-seed `InvitedNicks` so `joinAs` clears `+i`.
	if window, err := s.loadChannelWindow(ctx, ch); err == nil {
		window.InvitedNicks.Add(nick)
		if err := s.persistChannelWindow(ctx, window); err != nil {
			setSpanError(span, err, observability.ErrorKindStore)
			return nil, fmt.Errorf("save channel: %w", err)
		}
	}

	if err := s.joinAs(ctx, inst, ch, ""); err != nil {
		setSpanError(span, err, observability.ErrorKindStore)
		return nil, err
	}

	span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))

	return inst, nil
}

// killAs is the operator-issued forced disconnect of `target` per
// RFC 2812 §3.7.1. The target receives a single [domain.Killed]
// event on its own Events channel before its subscription is
// reaped — that line is the renderer's last word, the
// notification "you were KILLed by X". Peers in shared channels
// separately receive a wire `QUIT` with the conventional
// `"Killed by <oper> (<reason>)"` body, emitted by `quitAs`'s
// model-actor branch.
//
// The dispatcher's `handleKill` is the only caller and runs the
// operator gate, so this method assumes `oper` has the
// authority. The reap of the target's subscription happens in
// the dispatcher too, after this returns.
func (s *Session) killAs(ctx context.Context, oper, target *domain.Instance, reason string) error {
	if target.ID() == "" {
		return fmt.Errorf("KILL cannot target the user-client")
	}

	body := fmt.Sprintf("Killed by %s (%s)", oper.Nick(), reason)

	sc := s.lookupClientHandle(protocol.ClientID(target.ID()))
	if sc != nil {
		select {
		case sc.events <- protocol.Delivery{
			Event: domain.Killed{
				By:     oper.Nick(),
				Reason: reason,
				At:     s.now(),
			},
			SpanCtx: trace.SpanContextFromContext(ctx),
		}:
		case <-sc.done:
			// Target was concurrently reaped; the Killed line
			// belongs to a subscription that no longer exists.
		case <-ctx.Done():
		}
	}

	return s.quitAs(ctx, target, body)
}
