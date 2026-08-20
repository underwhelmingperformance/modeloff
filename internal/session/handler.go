package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
)

// Handle is the single entry point through which every protocol
// [protocol.Client] sends commands to the session. Each
// [protocol.Command] case looks up the actor implied by the
// client's identity and forwards to the existing `*As` session
// method (`joinAs`, `partAs`, …).
//
// Handling is serial: each handler runs its state-touching work on
// the session's command loop via [Session.onWriter], one command at
// a time in arrival order, so a command sees the full effect of
// every command before it and none of any command after it. The
// call stays synchronous for the caller: `Handle` returns that
// command's own `Response`.
//
// Every command is first billed to the issuing connection's flood
// penalty timer (RFC 1459 §8.10). A client sending faster than the
// timer allows waits here, on its own goroutine, before its command
// reaches the loop. See [Session.throttleCommand].
//
// The `default` branch is unreachable; the [protocol.Command] sum
// is sealed.
//
// A `session.handle` span brackets every dispatch so the wire
// boundary shows up distinctly in traces. The per-command `*As`
// spans nest underneath it. Typed command refusals carried on
// `Response.Err` are tagged with `AttrErrorKind=validation`; a
// non-nil second return is tagged with `ErrorKindDispatch` since
// the underlying child span carries the finer-grained kind.
func (s *Session) Handle(ctx context.Context, c protocol.Client, cmd protocol.Command) (protocol.Response, error) {
	var resp protocol.Response

	err := (observability.SpanRunner{
		Tracer:       s.tracerProvider.Tracer("github.com/laney/modeloff/internal/session"),
		ManualResult: true,
	}).Run(ctx, "session.handle", []attribute.KeyValue{
		attribute.String("protocol.command", cmd.Name()),
	}, func(ctx context.Context, span trace.Span) error {
		if delay := s.throttleCommand(ctx, c); delay > 0 {
			span.SetAttributes(attribute.Int64("flood.delay_ms", delay.Milliseconds()))
		}

		r, dispatchErr := s.dispatchCommand(ctx, c, cmd)
		resp = r

		switch {
		case dispatchErr != nil:
			span.SetAttributes(
				attribute.String(observability.AttrResult, observability.ResultError),
				attribute.String(observability.AttrErrorKind, observability.ErrorKindDispatch),
			)
		case resp.Err != nil:
			span.SetAttributes(
				attribute.String(observability.AttrResult, observability.ResultError),
				attribute.String(observability.AttrErrorKind, observability.ErrorKindValidation),
			)
			span.SetStatus(codes.Error, resp.Err.Error())
		default:
			span.SetAttributes(attribute.String(observability.AttrResult, observability.ResultOK))
		}

		return dispatchErr
	})

	return resp, err
}

// dispatchCommand routes a [protocol.Command] to its per-command
// handler. Split out from [Session.Handle] so the span-bracketing
// runner sees the dispatch's `(resp, err)` shape on a single call.
func (s *Session) dispatchCommand(ctx context.Context, c protocol.Client, cmd protocol.Command) (protocol.Response, error) {
	switch cmd := cmd.(type) {
	case protocol.Join:
		return s.handleJoin(ctx, c, cmd)
	case protocol.Part:
		return s.handlePart(ctx, c, cmd)
	case protocol.PrivMsg:
		return s.handlePrivMsg(ctx, c, cmd)
	case protocol.Action:
		return s.handleAction(ctx, c, cmd)
	case protocol.Topic:
		return s.handleTopic(ctx, c, cmd)
	case protocol.Invite:
		return s.handleInvite(ctx, c, cmd)
	case protocol.Kick:
		return s.handleKick(ctx, c, cmd)
	case protocol.Nick:
		return s.handleNick(ctx, c, cmd)
	case protocol.Whois:
		return s.handleWhois(ctx, c, cmd)
	case protocol.List:
		return s.handleList(ctx, c)
	case protocol.AddModel:
		return s.handleAddModel(ctx, c, cmd)
	case protocol.Quit:
		return s.handleQuit(ctx, c, cmd)
	case protocol.Kill:
		return s.handleKill(ctx, c, cmd)
	case protocol.Oper:
		return s.handleOper(ctx, c, cmd)
	case protocol.ChannelMode:
		return s.handleChannelMode(ctx, c, cmd)
	default:
		return protocol.Response{}, fmt.Errorf("unknown command %T", cmd)
	}
}

func (s *Session) handleChannelMode(ctx context.Context, c protocol.Client, cmd protocol.ChannelMode) (protocol.Response, error) {
	return s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		actor, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}

		return commandResult(s.applyChannelModeChangesAs(ctx, actor, cmd.Channel, cmd.Changes))
	})
}

// handleOper validates the issuing client's credentials via the
// session's authenticator. On success the server issues the
// canonical MODE response: server-actor (empty `by`), target is
// the requesting client, flag is [domain.ModeOperator]. The
// emission shape matches the bootstrap path's promotion of the
// user-client.
func (s *Session) handleOper(ctx context.Context, c protocol.Client, cmd protocol.Oper) (protocol.Response, error) {
	return s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		if !s.operAuth(c, cmd.User, cmd.Password) {
			return protocol.Response{Err: domain.OperFailedError{At: s.now()}}, nil
		}

		sc := s.lookupClientHandle(c.Identity())
		if sc == nil {
			return protocol.Response{}, fmt.Errorf("oper: client %q not registered", c.Identity())
		}

		s.setUserModeAs(ctx, "", sc, domain.ModeOperator, true)

		return protocol.Response{}, nil
	})
}

// handleJoin processes every channel in cmd.Channels as one JOIN
// command (RFC 2812 §3.2.1's "JOIN #a,#b,#c"), so the connection's
// flood-control penalty is charged once regardless of how many
// channels it names. A list longer than [protocol.MaxJoinTargets]
// is refused whole, before any channel in it is joined, with
// [domain.TooManyJoinTargetsError]. Otherwise channels are joined
// one at a time, in order, on this writer-loop turn; a gate refusal
// on one channel (`+i`, `+l`, `+k`) does not stop the rest. A real
// ircd answers each target in a multi-target JOIN with its own
// numeric, and this is that per-target answer.
//
// `Response.Events` carries one entry per channel processed: a
// [domain.JoinedChannel] for a channel that joined, or the gate's
// typed refusal ([domain.ChannelKeyMismatchError],
// [domain.ChannelInviteOnlyError], [domain.ChannelFullError]), which
// already doubles as a [protocol.Event]. This is the same shape
// [Session.handleList] uses to land one [domain.ListReply] per
// directory row. `Response.Err` carries every refusal joined
// together with [errors.Join], so a caller that only checks success
// or failure still gets one answer, while a caller that wants to
// know which channels joined and which did not reads `Events`.
func (s *Session) handleJoin(ctx context.Context, c protocol.Client, cmd protocol.Join) (protocol.Response, error) {
	return s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		actor, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}

		if len(cmd.Channels) > protocol.MaxJoinTargets {
			return commandResult(domain.TooManyJoinTargetsError{
				Requested: len(cmd.Channels),
				Max:       protocol.MaxJoinTargets,
				At:        s.now(),
			})
		}

		var failures []error
		var events []protocol.Event

		for _, ch := range cmd.Channels {
			joined, joinErr := s.joinAs(ctx, actor, clientJoin, ch, cmd.Key)
			if joinErr != nil {
				failures = append(failures, joinErr)
				if ev, ok := joinErr.(protocol.Event); ok {
					events = append(events, ev)
				}
				continue
			}

			events = append(events, domain.JoinedChannel{Channel: joined})
		}

		return protocol.Response{Events: events, Err: errors.Join(failures...)}, nil
	})
}

func (s *Session) handlePart(ctx context.Context, c protocol.Client, cmd protocol.Part) (protocol.Response, error) {
	return s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		actor, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}

		return commandResult(s.partAs(ctx, actor, cmd.Channel, cmd.Reason))
	})
}

func (s *Session) handlePrivMsg(ctx context.Context, c protocol.Client, cmd protocol.PrivMsg) (protocol.Response, error) {
	return s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		actor, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}

		target, err := s.resolveMsgTarget(cmd.Target)
		if err != nil {
			return commandResult(err)
		}

		msg, sendErr := s.sendMessageAs(ctx, actor, target, cmd.Body)
		if sendErr != nil {
			return commandResult(sendErr)
		}

		return protocol.Response{Events: []protocol.Event{msg}}, nil
	})
}

func (s *Session) handleAction(ctx context.Context, c protocol.Client, cmd protocol.Action) (protocol.Response, error) {
	return s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		actor, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}

		target, err := s.resolveMsgTarget(cmd.Target)
		if err != nil {
			return commandResult(err)
		}

		msg, sendErr := s.sendActionAs(ctx, actor, target, cmd.Body)
		if sendErr != nil {
			return commandResult(sendErr)
		}

		return protocol.Response{Events: []protocol.Event{msg}}, nil
	})
}

// resolveMsgTarget turns the target a client addressed into the
// conversation key the session logs and routes the message under.
// This is the server's half of RFC 2812 §3.3.1 addressing: the client
// says who it is talking to, and the server decides where that
// conversation lives.
//
// A channel target keeps the name it was given; the send gates
// canonicalise the spelling against the channel record. A client
// target, named by nick or by id, resolves to that client, and the
// key is the counterpart's [domain.InstanceID], which is what both
// sides of a DM read the conversation back under (see
// [domain.Message.RoutingKey]). A target naming no connected client
// is refused with [domain.UnknownNickError], RFC 2812 numeric 401,
// naming the target the way the client addressed it; a nick outside
// the grammar is refused with [domain.ErroneousNicknameError] (432)
// before anything is looked up.
//
// It runs on the command loop and reads only live state, so
// addressing a message costs the loop no store round-trip.
func (s *Session) resolveMsgTarget(target protocol.MsgTarget) (domain.ChannelName, error) {
	switch t := target.(type) {
	case protocol.ChannelTarget:
		return domain.ChannelName(t), nil

	case protocol.NickTarget:
		nick := domain.Nick(t)

		// The two refusals IRC distinguishes, in the order
		// [Session.requireNickAvailable] asks them: a nick outside the
		// RFC 2812 §2.3.1 grammar names nobody who could ever hold it,
		// which is 432 and not the 433-shaped answer a lookup gives.
		if reason := domain.ValidateNick(nick); reason != domain.NickAccepted {
			return "", domain.ErroneousNicknameError{Nick: nick, Reason: reason, At: s.now()}
		}

		return s.dmKeyFor(s.lookupClientByNick(nick), t)

	case protocol.ClientTarget:
		return s.dmKeyFor(s.lookupClientHandle(protocol.ClientID(t)), t)
	}

	return "", fmt.Errorf("unknown message target %T", target)
}

// dmKeyFor returns the conversation key for a resolved client, or the
// 401 refusal naming `addressed` when the target resolved to nobody.
func (s *Session) dmKeyFor(sc *serverClient, addressed protocol.MsgTarget) (domain.ChannelName, error) {
	if sc == nil {
		return "", domain.UnknownNickError{Nick: domain.Nick(addressed.String()), At: s.now()}
	}

	return domain.ChannelName(sc.instance.ID()), nil
}

func (s *Session) handleTopic(ctx context.Context, c protocol.Client, cmd protocol.Topic) (protocol.Response, error) {
	return s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		actor, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}

		return commandResult(s.setTopicAs(ctx, actor, cmd.Channel, cmd.Body))
	})
}

// handleInvite delegates to [Session.inviteAs] and lands the
// resulting envelope in `Response.Events` as the inviter's
// RPL_INVITING-equivalent. The chat-screen's `sendCommand` reads
// `Response.Events[0]` for synchronous numeric-reply payloads
// (see `internal/ui/chatcmd.sendCommand` and the `WhoisCommand`
// pattern). A typed dispatcher failure still goes through
// [commandResult].
//
// A refused INVITE against an unknown nick yields a
// [domain.SystemNotice] in place of the `RPL_INVITING` envelope; that
// notice is an [domain.IssuerReply], so it is filed to the issuer's
// reply log and a model re-experiences the refusal on replay. The
// success-case [domain.Invited] is channel activity, not an
// issuer reply, so it is not filed here.
func (s *Session) handleInvite(ctx context.Context, c protocol.Client, cmd protocol.Invite) (protocol.Response, error) {
	return s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		actor, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}

		event, err := s.inviteAs(ctx, actor, cmd.Nick, cmd.Channel)
		if err != nil {
			return commandResult(err)
		}

		events := []domain.ProtocolEvent{event}
		s.persistInstanceReplies(ctx, c, events)

		return protocol.Response{Events: events}, nil
	})
}

func (s *Session) handleKick(ctx context.Context, c protocol.Client, cmd protocol.Kick) (protocol.Response, error) {
	return s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		actor, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}

		target, err := s.resolveConnectedNick(cmd.Nick)
		if err != nil {
			return commandResult(err)
		}

		return commandResult(s.kickAs(ctx, actor, target, cmd.Channel))
	})
}

// resolveConnectedNick resolves a wire-supplied nick against the
// registry of connected clients, the same registry
// [Session.resolveMsgTarget] reads for a [protocol.NickTarget].
// INVITE, KICK, WHOIS, KILL and a member-mode MODE change each name a
// client they mean to reach: an invitee, a member to remove, a
// snapshot to answer, a connection to end, a privilege to grant or
// revoke. "Who can the server currently reach under this nick" is the
// question every one of them is asking. The user resolves like any
// other client, through the sentinel empty [protocol.ClientID] its
// subscription is registered under.
//
// A nick outside the RFC 2812 §2.3.1 grammar is refused with
// [domain.ErroneousNicknameError] (432) before any lookup runs. A
// nick naming no connected client is refused with
// [domain.UnknownNickError] (401). This includes a nick an instances
// row still holds if the client behind it never attached: the server
// has no subscription to deliver to there, which is the case the
// store and the registry disagree about.
func (s *Session) resolveConnectedNick(nick domain.Nick) (*domain.Instance, error) {
	if reason := domain.ValidateNick(nick); reason != domain.NickAccepted {
		return nil, domain.ErroneousNicknameError{Nick: nick, Reason: reason, At: s.now()}
	}

	sc := s.lookupClientByNick(nick)
	if sc == nil {
		return nil, domain.UnknownNickError{Nick: nick, At: s.now()}
	}

	return sc.instance, nil
}

func (s *Session) handleNick(ctx context.Context, c protocol.Client, cmd protocol.Nick) (protocol.Response, error) {
	return s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		actor, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}

		return commandResult(s.changeNickAs(ctx, actor, cmd.New))
	})
}

// handleWhois resolves the requested nick and returns the
// canonical `domain.Whois` snapshot in `Response.Events` (RFC 2812
// numeric 311 `RPL_WHOISUSER`). The snapshot freezes the
// instance's mutable identity surface at the moment of issue so
// later renames or persona edits don't retro-edit historical
// renderings. Renderers consume the event directly without going
// back to the store.
//
// The channel list is what RFC 2812 §3.6.2 makes conditional:
// `RPL_WHOISCHANNELS` names the target's channels except those the
// issuer may not see. The filter is [Session.channelVisibleTo], the
// same one `LIST` answers under, so a `+s` channel cannot be hidden
// from the directory and then read straight back out of a WHOIS.
func (s *Session) handleWhois(ctx context.Context, c protocol.Client, cmd protocol.Whois) (protocol.Response, error) {
	return s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		issuer, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}

		inst, err := s.resolveConnectedNick(cmd.Nick)
		if err != nil {
			return commandResult(err)
		}

		whois := domain.Whois{
			Target:   cmd.Channel,
			Nick:     inst.Nick(),
			ModelID:  inst.ModelID,
			Persona:  inst.Persona(),
			Channels: s.whoisChannels(ctx, issuer, inst),
			At:       s.now(),
		}

		events := []domain.ProtocolEvent{whois}
		s.persistInstanceReplies(ctx, c, events)

		return protocol.Response{Events: events}, nil
	})
}

// whoisChannels returns the channels of `target` that `issuer` may
// be told about. A channel whose mode set cannot be read is left
// out: the session holds live modes for every channel it has
// touched, and a channel it cannot read is one it cannot say is
// public either, so the fail-closed answer is to omit it.
func (s *Session) whoisChannels(ctx context.Context, issuer, target *domain.Instance) []domain.ChannelName {
	channels := target.Channels()
	if channels == nil || channels.Len() == 0 {
		return nil
	}

	var visible []domain.ChannelName

	for pair := channels.Oldest(); pair != nil; pair = pair.Next() {
		modes, ok := s.channelModes(ctx, pair.Key)
		if !ok {
			continue
		}

		if s.channelVisibleTo(issuer, pair.Key, modes) {
			visible = append(visible, pair.Key)
		}
	}

	return visible
}

// handleList enumerates the channel directory and returns one
// `domain.ListReply` per channel the issuer may see, followed by a
// closing `domain.ListEnd` in `Response.Events` (RFC 2812 numerics
// 322 `RPL_LIST` / 323 `RPL_LISTEND`). The visibility filter is
// [Session.channelVisibleTo], applied in
// [Session.DirectoryChannels], and it is the same one `WHOIS`
// answers under.
func (s *Session) handleList(ctx context.Context, c protocol.Client) (protocol.Response, error) {
	return s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		issuer, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}

		channels, err := s.DirectoryChannels(ctx, issuer)
		if err != nil {
			return commandResult(err)
		}

		now := s.now()
		events := make([]domain.ProtocolEvent, 0, len(channels)+1)
		for _, ch := range channels {
			events = append(events, domain.ListReply{
				Channel: ch.Channel,
				Members: ch.Members,
				Topic:   ch.Topic,
				At:      now,
			})
		}

		// The directory rows are the lookup result the model remembers; the
		// closing ListEnd is a wire terminator that carries no transcript
		// line, so it stays out of the reply log.
		s.persistInstanceReplies(ctx, c, events)

		events = append(events, domain.ListEnd{At: now})

		return protocol.Response{Events: events}, nil
	})
}

// handleQuit dispatches a QUIT — the user-actor branch tears
// down session state in-place (autojoin save, session-active
// marker clear); the model-actor branch broadcasts the QUIT to
// peers and releases the model-client via [releaseClient].
//
// A model can end its own connection: the `quit` tool runs on that
// model's dispatch goroutine, so this handler is reached from
// inside the very goroutine the release addresses. The release is
// the phase that tolerates it — it cancels and unsubscribes without
// waiting — and it runs after the loop has let the QUIT go, because
// the dispatch goroutine it ends may be queued behind that loop.
func (s *Session) handleQuit(ctx context.Context, c protocol.Client, cmd protocol.Quit) (protocol.Response, error) {
	resp, err := s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		actor, resolveErr := s.resolveClientActor(c)
		if resolveErr != nil {
			return protocol.Response{}, resolveErr
		}

		return commandResult(s.quitAs(ctx, actor, cmd.Reason))
	})

	if err != nil || resp.Err != nil {
		return resp, err
	}

	s.releaseClient(c.Identity())

	return resp, nil
}

// handleAddModel brings a new model instance into a channel. It runs
// the sequence a client goes through on a real server — register,
// connect, join — because each step needs a different place to run:
//
//  1. Off the loop: the operator gate and the actor resolution, so
//     an unauthorised or unregistered client is refused before
//     anything is spent on it, then
//     [ModelClientFactory.PrepareInstance] — a round-trip to the
//     small model for a persona and a nick.
//  2. On the loop: claim the nick and register the instance. This is
//     the only point at which the nick is actually taken, so it is
//     where the collision check has to be.
//  3. Off the loop: [ModelClientFactory.Attach], which subscribes
//     the model-client and loads its history from the store.
//  4. On the loop: the JOIN.
//
// Attaching between the two loop steps is what lets the new client
// receive its own JOIN, `RPL_NAMREPLY` and `RPL_TOPIC` on the bus:
// its subscription exists by the time the JOIN is broadcast, and
// `joinAs` records membership before it emits, so the membership
// filter admits it.
//
// The cost of that ordering is a window between step 3 and step 4
// where the new client is a bus member of a channel whose member
// list does not yet contain it: `registerModelAs` stamps the
// channel onto the instance, which is what the delivery filter
// reads, while `ChannelWindow.Members` gains it only in step 4. It
// can therefore see channel traffic a moment before it has joined.
// The skew is one-directional — anything it tries to say is refused
// by the send gates, which read the channel's member list — and it
// is the price of the subscription existing before the broadcast.
//
// A failure in step 3 or step 4 unwinds what came before it: the
// client is detached and the instance deleted, so a refused ADDMODEL
// leaves no nick claimed and no dispatch goroutine behind. A client
// that could not connect fails the command for the same reason a
// refused JOIN does: every member of a channel has a client behind
// it, so a message addressed to any nick in the member list reaches
// somebody, and a registration that stopped short of connecting
// would put a nick there that nothing can reach.
func (s *Session) handleAddModel(ctx context.Context, c protocol.Client, cmd protocol.AddModel) (protocol.Response, error) {
	if !s.idHasServerOper(c.Identity()) {
		return protocol.Response{Err: domain.NotOperatorError{Command: "ADDMODEL", At: s.now()}}, nil
	}

	if reason := domain.ValidatePersona(cmd.Persona); reason != domain.PersonaAccepted {
		return protocol.Response{Err: domain.ErroneousPersonaError{Reason: reason, At: s.now()}}, nil
	}

	actor, err := s.resolveClientActor(c)
	if err != nil {
		return protocol.Response{}, err
	}

	var prepared PreparedInstance

	prepErr := s.inSpan(ctx, "session.prepare_model", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(cmd.Channel)),
		attribute.String(observability.AttrModelID, string(cmd.Model)),
	}, func(ctx context.Context, _ trace.Span) error {
		var err error

		prepared, err = s.modelClientFactory.PrepareInstance(ctx, s, cmd.Model, cmd.Persona)

		return observability.ErrWithKind(err, observability.ErrorKindDispatch)
	})
	if prepErr != nil {
		return commandResult(prepErr)
	}

	var inst *domain.Instance

	resp, err := s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		registered, registerErr := s.registerModelAs(ctx, cmd.Channel, cmd.Model, prepared.Nick, prepared.Persona)
		if registerErr != nil {
			return commandResult(registerErr)
		}

		inst = registered

		return protocol.Response{}, nil
	})

	if err != nil || resp.Err != nil {
		return resp, err
	}

	if _, attachErr := s.modelClientFactory.Attach(ctx, s, inst); attachErr != nil {
		s.discardModel(ctx, inst)

		return commandResult(observability.ErrWithKind(
			fmt.Errorf("connect model client: %w", attachErr),
			observability.ErrorKindDispatch,
		))
	}

	admitted, admitErr := s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		resp, err := commandResult(s.admitModelAs(ctx, actor, inst, cmd.Channel))
		if err != nil || resp.Err != nil {
			return resp, err
		}

		resp.Events = s.preparationNotices(ctx, c, cmd.Channel, prepared.Warnings)

		return resp, nil
	})

	if admitErr != nil || admitted.Err != nil {
		s.discardModel(ctx, inst)
	}

	return admitted, admitErr
}

// preparationNotices turns each warning
// [ModelClientFactory.PrepareInstance] reported into a
// [domain.SystemNotice] on the `ADDMODEL` reply, addressed to the
// channel the command was issued from. Without them a preparation
// that quietly fell short, such as a persona pool the small model
// could not supply, reaches only the log, and the operator sees a
// model join with none of the character they asked for and nothing
// to say why.
//
// The notices are point-to-point replies to the issuer, so they are
// filed to its reply log exactly as a refused INVITE's notice is,
// and no other member of the channel is told.
func (s *Session) preparationNotices(
	ctx context.Context,
	c protocol.Client,
	ch domain.ChannelName,
	warnings []string,
) []domain.ProtocolEvent {
	if len(warnings) == 0 {
		return nil
	}

	now := s.now()

	events := make([]domain.ProtocolEvent, 0, len(warnings))
	for _, warning := range warnings {
		events = append(events, domain.SystemNotice{Target: ch, Text: warning, At: now})
	}

	s.persistInstanceReplies(ctx, c, events)

	return events
}

// discardModel unwinds a registration whose JOIN did not land. The
// release runs off the command loop for the same reason QUIT's does
// — it ends a dispatch goroutine that may be queued behind the loop
// — and the instance is deleted so its nick returns to the pool.
// Best-effort: the ADDMODEL has already failed, and the caller's
// refusal is the answer the client gets either way.
func (s *Session) discardModel(ctx context.Context, inst *domain.Instance) {
	s.releaseClient(protocol.ClientID(inst.ID()))

	if err := s.store.DeleteInstanceByID(ctx, inst.ID()); err != nil {
		slog.Default().ErrorContext(ctx, "discard model after failed add",
			"component", "session",
			"instance_id", inst.ID(),
			"error", err,
		)
	}
}

// handleKill is the operator-issued forced disconnect. As with
// QUIT, the target's release ends a goroutine that may be waiting
// on the command loop, so it runs once the loop has let the KILL
// go. The operator gate is checked on the loop alongside the act
// it authorises.
func (s *Session) handleKill(ctx context.Context, c protocol.Client, cmd protocol.Kill) (protocol.Response, error) {
	var killed *domain.Instance

	resp, err := s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		if !s.idHasServerOper(c.Identity()) {
			return protocol.Response{Err: domain.NotOperatorError{Command: "KILL", At: s.now()}}, nil
		}

		oper, resolveErr := s.resolveClientActor(c)
		if resolveErr != nil {
			return protocol.Response{}, resolveErr
		}

		target, targetErr := s.resolveConnectedNick(cmd.Nick)
		if targetErr != nil {
			return commandResult(targetErr)
		}

		if killErr := s.killAs(ctx, oper, target, cmd.Reason); killErr != nil {
			return commandResult(killErr)
		}

		killed = target

		return protocol.Response{}, nil
	})

	if killed == nil {
		return resp, err
	}

	s.releaseClient(protocol.ClientID(killed.ID()))

	return resp, err
}

// commandResult turns a delegation-call error into the canonical
// protocol shape: command failures live on [protocol.Response.Err]
// so synchronous callers can branch on them with `errors.As`. A nil
// `err` produces the empty success response.
func commandResult(err error) (protocol.Response, error) {
	return protocol.Response{Err: err}, nil
}

// resolveClientActor turns a [protocol.Client] handle into the
// `*domain.Instance` the `*As` methods take as their actor
// argument. The registered subscription carries the canonical
// instance pointer; the dispatcher reads it directly with no store
// round-trip. An unregistered client is a structural bug — the
// dispatcher only sees handles the session issued.
func (s *Session) resolveClientActor(c protocol.Client) (*domain.Instance, error) {
	sc := s.lookupClientHandle(c.Identity())
	if sc == nil {
		return nil, fmt.Errorf("client %q not registered with this session", c.Identity())
	}
	return sc.instance, nil
}
