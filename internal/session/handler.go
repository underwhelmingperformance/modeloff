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
	"github.com/laney/modeloff/internal/store"
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
// call stays synchronous for the caller — `Handle` returns that
// command's own `Response`.
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

func (s *Session) handleJoin(ctx context.Context, c protocol.Client, cmd protocol.Join) (protocol.Response, error) {
	return s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		actor, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}

		return commandResult(s.joinAs(ctx, actor, cmd.Channel, cmd.Key))
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

		msg, sendErr := s.sendMessageAs(ctx, actor, cmd.Target, cmd.Body)
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

		msg, sendErr := s.sendActionAs(ctx, actor, cmd.Target, cmd.Body)
		if sendErr != nil {
			return commandResult(sendErr)
		}

		return protocol.Response{Events: []protocol.Event{msg}}, nil
	})
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

		target, err := s.dispatcherResolveNick(ctx, cmd.Nick)
		if err != nil {
			return commandResult(err)
		}

		return commandResult(s.kickAs(ctx, actor, target, cmd.Channel))
	})
}

// dispatcherResolveNick resolves a wire-supplied nick and
// rewrites the store's untyped "no such nick" sentinel into the
// typed [domain.UnknownNickError] the wire protocol surfaces
// (RFC 2812 numeric 401 `ERR_NOSUCHNICK`). Internal call sites
// that don't need to round-trip the error to a client should
// keep using [Session.ResolveNick] directly.
func (s *Session) dispatcherResolveNick(ctx context.Context, nick domain.Nick) (*domain.Instance, error) {
	inst, err := s.ResolveNick(ctx, nick)
	if err == nil {
		return inst, nil
	}

	if errors.Is(err, store.ErrNoSuchNick) {
		return nil, domain.UnknownNickError{Nick: nick, At: s.now()}
	}

	return nil, err
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
func (s *Session) handleWhois(ctx context.Context, c protocol.Client, cmd protocol.Whois) (protocol.Response, error) {
	return s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		inst, err := s.dispatcherResolveNick(ctx, cmd.Nick)
		if err != nil {
			return commandResult(err)
		}

		whois := domain.Whois{
			Target:  cmd.Channel,
			Nick:    inst.Nick(),
			ModelID: inst.ModelID,
			Persona: inst.Persona(),
			At:      s.now(),
		}

		if channels := inst.Channels(); channels != nil && channels.Len() > 0 {
			whois.Channels = make([]domain.ChannelName, 0, channels.Len())
			for pair := channels.Oldest(); pair != nil; pair = pair.Next() {
				whois.Channels = append(whois.Channels, pair.Key)
			}
		}

		events := []domain.ProtocolEvent{whois}
		s.persistInstanceReplies(ctx, c, events)

		return protocol.Response{Events: events}, nil
	})
}

// handleList enumerates the channel directory and returns one
// `domain.ListReply` per visible channel followed by a closing
// `domain.ListEnd` in `Response.Events` (RFC 2812 numerics 322
// `RPL_LIST` / 323 `RPL_LISTEND`). The `+s` and `+p` filters live
// in [Session.DirectoryChannels] so the wire reply matches the
// chat-screen's directory view exactly.
func (s *Session) handleList(ctx context.Context, c protocol.Client) (protocol.Response, error) {
	return s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		channels, err := s.DirectoryChannels(ctx)
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
// A failure in step 4 unwinds steps 2 and 3: the client is detached
// and the instance deleted, so a refused ADDMODEL leaves no nick
// claimed and no dispatch goroutine behind.
func (s *Session) handleAddModel(ctx context.Context, c protocol.Client, cmd protocol.AddModel) (protocol.Response, error) {
	if !s.idHasServerOper(c.Identity()) {
		return protocol.Response{Err: domain.NotOperatorError{Command: "ADDMODEL", At: s.now()}}, nil
	}

	actor, err := s.resolveClientActor(c)
	if err != nil {
		return protocol.Response{}, err
	}

	var (
		nick    domain.Nick
		persona string
	)

	prepErr := s.inSpan(ctx, "session.prepare_model", []attribute.KeyValue{
		attribute.String(observability.AttrChannel, string(cmd.Channel)),
		attribute.String(observability.AttrModelID, string(cmd.Model)),
	}, func(ctx context.Context, _ trace.Span) error {
		var err error

		nick, persona, err = s.modelClientFactory.PrepareInstance(ctx, s, cmd.Model, cmd.Persona)

		return errWithKind(err, observability.ErrorKindDispatch)
	})
	if prepErr != nil {
		return commandResult(prepErr)
	}

	var inst *domain.Instance

	resp, err := s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		registered, registerErr := s.registerModelAs(ctx, cmd.Channel, cmd.Model, nick, persona)
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
		slog.Default().WarnContext(ctx, "attach model client",
			"component", "session",
			"instance_id", inst.ID(),
			"channel", cmd.Channel,
			"error", attachErr,
		)
	}

	admitted, admitErr := s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		return commandResult(s.admitModelAs(ctx, actor, inst, cmd.Channel))
	})

	if admitErr != nil || admitted.Err != nil {
		s.discardModel(ctx, inst)
	}

	return admitted, admitErr
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

		target, targetErr := s.dispatcherResolveNick(ctx, cmd.Nick)
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
