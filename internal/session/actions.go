package session

import (
	"context"
	"errors"
	"fmt"
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

// joinAs joins the given actor to a channel.
func (s *Session) joinAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName) (retErr error) {
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
	isUser := actor == s.user

	window, created, err := s.ensureChannelWindowWithActor(ctx, ch, actor, now)
	if err != nil {
		return err
	}

	alreadyMember := !created && window.Members.HasInstance(actor)

	if !created && !alreadyMember {
		window.Members.Add(actor)

		if err := s.persistChannelWindow(ctx, window); err != nil {
			return fmt.Errorf("save channel: %w", err)
		}
	}

	if isUser {
		// Stamp the user's mark-as-read cursor at the current head so
		// the join itself does not leave the channel showing as unread.
		// `last_channel` persistence is the UI's concern and lands when
		// the chat screen receives a `ChannelActiveMsg`.
		if err := s.MarkRead(ctx, ch); err != nil {
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

	if isUser {
		// Send the joiner the channel's current member list (IRC's
		// RPL_NAMREPLY). The chat-screen handler uses this to
		// populate its local member-list cache with pre-existing
		// members; without it, the cache would see only the joiner.
		s.emit(ctx, domain.NamesReplyEvent{
			Channel: ch,
			Members: window.Members,
			At:      now,
		})

		return s.emitJoinProtocol(ctx, ch, window, now)
	}

	return s.grantVoice(ctx, ch, window, actor, actorNick, now)
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

// emitJoinProtocol sets the user's mode to +o, emits topic info if
// the channel has a topic, and saves the autojoin list.
func (s *Session) emitJoinProtocol(ctx context.Context, ch domain.ChannelName, window *domain.ChannelWindow, now time.Time) error {
	s.setUserMode(ctx, ch, domain.ModeOp)
	window.Members.SetMode(s.user, domain.ModeOp)

	if err := s.persistChannelWindow(ctx, window); err != nil {
		return fmt.Errorf("save channel after mode: %w", err)
	}

	s.persistAndEmit(ctx, ch, domain.ModeChange{
		Target:     ch,
		Nick:       s.user.Nick(),
		InstanceID: s.user.ID(),
		Flag:       domain.ModeOperator,
		Add:        true,
		By:         "ChanServ",
		At:         now,
		Instance:   s.user,
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

	return s.saveAutojoinList(ctx)
}

// grantVoice gives a non-user joiner (a model instance) +v via
// ChanServ. This mirrors the +o granted to the user by
// emitJoinProtocol so the nick list distinguishes models (+nick) from
// the user (@nick) on every join, not only when the model was added
// through the invite path.
//
// The caller passes its own nick snapshot so the `Join` and
// `ModeChange` events emitted back-to-back for the same join
// record the same nick, even if the actor renames between them.
func (s *Session) grantVoice(ctx context.Context, ch domain.ChannelName, window *domain.ChannelWindow, inst *domain.Instance, nick domain.Nick, now time.Time) error {
	window.Members.SetMode(inst, domain.ModeVoice)

	if err := s.persistChannelWindow(ctx, window); err != nil {
		return fmt.Errorf("save channel after voice: %w", err)
	}

	s.persistAndEmit(ctx, ch, domain.ModeChange{
		Target:     ch,
		Nick:       nick,
		InstanceID: inst.ID(),
		Flag:       domain.ModeChannelVoice,
		Add:        true,
		By:         "ChanServ",
		At:         now,
		Instance:   inst,
	})

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

	isUser := actor == s.user

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

// QuitAs quits the given actor from every joined channel.
func (s *Session) QuitAs(ctx context.Context, actor *domain.Instance, message string) (retErr error) {
	if actor == s.user {
		return s.Quit(ctx, message)
	}

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
		build: func() domain.PersistableEvent {
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

	isUser := actor == s.user

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
		build: func() domain.PersistableEvent {
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

	if target != s.user {
		if err := s.store.SaveInstance(ctx, target); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}
	} else {
		s.forgetUserMode(ctx, ch)
	}

	actorNick := actor.Nick()

	now := s.now()
	s.persistAndEmit(ctx, ch, domain.ModelKicked{
		Target:     ch,
		Nick:       targetNick,
		InstanceID: target.ID(),
		By:         actorNick,
		At:         now,
		Instance:   target,
	})

	return nil
}

// inviteAs sends a real IRC-style invite.
func (s *Session) inviteAs(ctx context.Context, actor *domain.Instance, target domain.Nick, ch domain.ChannelName) (retErr error) {
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
		return fmt.Errorf("target nick is required")
	}

	if actor == s.user {
		inst, err := s.store.ResolveNick(ctx, target)
		if err == nil {
			span.SetAttributes(attribute.String(observability.AttrInstanceID, string(inst.ID())))
			return s.attachInstanceToChannel(ctx, ch, inst, actor)
		}

		if !errors.Is(err, store.ErrNoSuchNick) {
			return fmt.Errorf("resolve nick: %w", err)
		}
	}

	now := s.now()
	notice := fmt.Sprintf("%s invited %s to %s", actorNick, target, ch)
	s.appendEvent(ctx, ch, domain.SystemNotice{Target: ch, Text: notice, At: now})

	return nil
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
	if targetInst == nil {
		targetInst = s.user
	}

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
	case <-ctx.Done():
	}
}

// requireChannelOp returns [domain.ChanOpRequiredError] when the
// actor lacks `@` in `window`. Used by channel-op-gated commands
// (`MODE`, future `KICK`, future op-required `TOPIC`/`INVITE`) to
// short-circuit before mutation.
func (s *Session) requireChannelOp(actor *domain.Instance, window *domain.ChannelWindow, cmd string, ch domain.ChannelName) error {
	member, ok := window.Members.GetByInstance(actor)
	if !ok || member.Mode != domain.ModeOp {
		return domain.ChanOpRequiredError{Command: cmd, Channel: ch, At: s.now()}
	}
	return nil
}

// applyChannelModeChangesAs is the entry for [protocol.ChannelMode].
// It loads the channel window, checks the actor's channel-op status
// once for the whole batch, validates every change's shape up front,
// then applies them in order. Up-front validation rejects the whole
// batch on a malformed entry so a `MODE` with a typo never half-
// applies. A runtime failure (e.g. unknown nick on `+o`) stops the
// loop and returns the error; already-applied changes remain,
// matching typical ircd behaviour.
func (s *Session) applyChannelModeChangesAs(ctx context.Context, actor *domain.Instance, ch domain.ChannelName, changes []protocol.ChannelModeChange) error {
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
func (s *Session) setMemberModeAs(ctx context.Context, window *domain.ChannelWindow, ch domain.ChannelName, actor *domain.Instance, change protocol.ChannelModeChange) error {
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
func (s *Session) setChannelAttributeAs(ctx context.Context, window *domain.ChannelWindow, ch domain.ChannelName, actor *domain.Instance, change protocol.ChannelModeChange) error {
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
