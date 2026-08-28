package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

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
// the session's command loop via [Session.onGuardedWriter], one
// command at a time in arrival order, so a command sees the full
// effect of every command before it and none of any command after
// it. The call stays synchronous for the caller: `Handle` returns
// that command's own `Response`.
//
// A client that sent the command under a window guard ([windowGuard.Send])
// has that guard checked on the loop, immediately before the handler
// runs. `Handle` reads it once, here, and passes it to the dispatch
// as an argument. Nothing else inherits it: the teardown of a peer
// whose send queue this command's fan-out overflowed, and the
// cleanup [Session.discardModel] runs, are the session's own work
// and go through [Session.onWriter].
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
	if !s.beginHandler() {
		return protocol.Response{}, ErrSessionClosed
	}
	defer s.handlers.Done()

	var resp protocol.Response

	err := (observability.SpanRunner{
		Tracer:       s.tracerProvider.Tracer("github.com/laney/modeloff/internal/session"),
		ManualResult: true,
	}).Run(ctx, "session.handle", []attribute.KeyValue{
		attribute.String("protocol.command", cmd.Name()),
	}, func(ctx context.Context, span trace.Span) error {
		if _, err := s.resolveClientActor(c); err != nil {
			return err
		}

		if delay := s.throttleCommand(ctx, c); delay > 0 {
			span.SetAttributes(attribute.Int64("flood.delay_ms", delay.Milliseconds()))
		}

		r, dispatchErr := s.dispatchCommand(ctx, c, commandWindowGuard(ctx), cmd)
		resp = r
		if dispatchErr == nil {
			resp.Events = s.projectResponseEvents(ctx, c, resp.Events)
		}

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

func (s *Session) beginHandler() bool {
	s.handlersMu.Lock()
	defer s.handlersMu.Unlock()

	if s.handlersClosed {
		return false
	}

	s.handlers.Add(1)

	return true
}

func (s *Session) projectResponseEvents(
	ctx context.Context,
	c protocol.Client,
	events []protocol.Event,
) []protocol.Event {
	if len(events) == 0 {
		return events
	}

	recipient := s.lookupClientHandle(c.Identity())
	if recipient == nil {
		return nil
	}

	projected := make([]protocol.Event, 0, len(events))
	for _, event := range events {
		view, _ := s.projectEventForRecipient(ctx, event, recipient, nil, nil, nil)
		if view != nil {
			projected = append(projected, view)
		}
	}

	return projected
}

// dispatchCommand routes a [protocol.Command] to its per-command
// handler. Split out from [Session.Handle] so the span-bracketing
// runner sees the dispatch's `(resp, err)` shape on a single call.
func (s *Session) dispatchCommand(
	ctx context.Context,
	c protocol.Client,
	guard protocol.WindowGuard,
	cmd protocol.Command,
) (protocol.Response, error) {
	switch cmd := cmd.(type) {
	case protocol.Join:
		return s.handleJoin(ctx, c, guard, cmd)
	case protocol.Part:
		return s.handlePart(ctx, c, guard, cmd)
	case protocol.PrivMsg:
		return s.handlePrivMsg(ctx, c, guard, cmd)
	case protocol.Action:
		return s.handleAction(ctx, c, guard, cmd)
	case protocol.Topic:
		return s.handleTopic(ctx, c, guard, cmd)
	case protocol.TopicQuery:
		return s.handleTopicQuery(ctx, c, guard, cmd)
	case protocol.Invite:
		return s.handleInvite(ctx, c, guard, cmd)
	case protocol.Kick:
		return s.handleKick(ctx, c, guard, cmd)
	case protocol.Nick:
		return s.handleNick(ctx, c, guard, cmd)
	case protocol.Whois:
		return s.handleWhois(ctx, c, guard, cmd)
	case protocol.List:
		return s.handleList(ctx, c, guard, cmd)
	case protocol.AddModel:
		return s.handleAddModel(ctx, c, guard, cmd)
	case protocol.Quit:
		return s.handleQuit(ctx, c, guard, cmd)
	case protocol.Kill:
		return s.handleKill(ctx, c, guard, cmd)
	case protocol.Oper:
		return s.handleOper(ctx, c, guard, cmd)
	case protocol.ChannelMode:
		return s.handleChannelMode(ctx, c, guard, cmd)
	default:
		return protocol.Response{}, fmt.Errorf("unknown command %T", cmd)
	}
}

func (s *Session) handleChannelMode(ctx context.Context, c protocol.Client, guard protocol.WindowGuard, cmd protocol.ChannelMode) (protocol.Response, error) {
	return s.onGuardedWriter(ctx, guard, func(ctx context.Context) (protocol.Response, error) {
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
func (s *Session) handleOper(ctx context.Context, c protocol.Client, guard protocol.WindowGuard, cmd protocol.Oper) (protocol.Response, error) {
	return s.onGuardedWriter(ctx, guard, func(ctx context.Context) (protocol.Response, error) {
		if !s.operAuth(c, cmd.User, cmd.Password) {
			return protocol.Response{Err: domain.OperFailedError{At: s.now()}}, nil
		}

		sc := s.activeClientHandle(c.Identity())
		if sc == nil || sc.owner != c {
			return protocol.Response{}, fmt.Errorf("oper: client %q not connected", c.Identity())
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
// know which channels joined and which did not reads `Events`. Store
// and internal failures use the handler's second return and do not
// masquerade as per-target IRC refusals.
func (s *Session) handleJoin(ctx context.Context, c protocol.Client, guard protocol.WindowGuard, cmd protocol.Join) (protocol.Response, error) {
	return s.onGuardedWriter(ctx, guard, func(ctx context.Context) (protocol.Response, error) {
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

		var (
			refusals          []error
			executionFailures []error
			events            []protocol.Event
		)

		for _, ch := range cmd.Channels {
			joined, joinErr := s.joinAs(ctx, actor, clientJoin, ch, cmd.Key)
			if joined != "" {
				events = append(events, domain.JoinedChannel{Channel: joined})
			}
			if joinErr != nil {
				if commandRefusal(joinErr) {
					refusals = append(refusals, joinErr)
					var ev protocol.Event
					if errors.As(joinErr, &ev) {
						events = append(events, ev)
					}
				} else {
					executionFailures = append(executionFailures,
						protocol.JoinExecutionError{Channel: ch, Err: joinErr})
				}
				continue
			}

		}

		return protocol.Response{Events: events, Err: errors.Join(refusals...)}, errors.Join(executionFailures...)
	})
}

func (s *Session) handlePart(ctx context.Context, c protocol.Client, guard protocol.WindowGuard, cmd protocol.Part) (protocol.Response, error) {
	return s.onGuardedWriter(ctx, guard, func(ctx context.Context) (protocol.Response, error) {
		actor, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}

		return commandResult(s.partAs(ctx, actor, cmd.Channel, cmd.Reason))
	})
}

func (s *Session) handlePrivMsg(ctx context.Context, c protocol.Client, guard protocol.WindowGuard, cmd protocol.PrivMsg) (protocol.Response, error) {
	return s.onGuardedWriter(ctx, guard, func(ctx context.Context) (protocol.Response, error) {
		actor, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}

		if err := protocol.ValidateMessageBody(cmd.Name(), cmd.Body, s.now()); err != nil {
			return commandResult(err)
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

func (s *Session) handleAction(ctx context.Context, c protocol.Client, guard protocol.WindowGuard, cmd protocol.Action) (protocol.Response, error) {
	return s.onGuardedWriter(ctx, guard, func(ctx context.Context) (protocol.Response, error) {
		actor, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}

		if err := protocol.ValidateMessageBody(cmd.Name(), cmd.Body, s.now()); err != nil {
			return commandResult(err)
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
		return s.dmKeyFor(s.activeClientHandle(protocol.ClientID(t)), t)
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

func (s *Session) handleTopic(ctx context.Context, c protocol.Client, guard protocol.WindowGuard, cmd protocol.Topic) (protocol.Response, error) {
	return s.onGuardedWriter(ctx, guard, func(ctx context.Context) (protocol.Response, error) {
		actor, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}

		return commandResult(s.setTopicAs(ctx, actor, cmd.Channel, cmd.Body))
	})
}

func (s *Session) handleTopicQuery(
	ctx context.Context,
	c protocol.Client,
	guard protocol.WindowGuard,
	cmd protocol.TopicQuery,
) (protocol.Response, error) {
	return s.onGuardedWriter(ctx, guard, func(ctx context.Context) (protocol.Response, error) {
		actor, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}

		window, err := s.loadChannelWindow(ctx, cmd.Channel)
		if err != nil {
			return commandResult(err)
		}
		if !window.Members.HasInstance(actor) || !actor.InChannel(window.Name()) {
			return commandResult(domain.NotOnChannelError{
				Channel: window.Name(),
				Command: "TOPIC",
				At:      s.now(),
			})
		}

		setBy := window.TopicSetBy
		if window.Modes.Anonymous && setBy != "" {
			setBy = domain.AnonymousNick
		}

		topic := domain.TopicInfo{
			Target:     window.Name(),
			Topic:      window.Topic,
			TopicSetBy: setBy,
			TopicSetAt: window.TopicSetAt,
			At:         s.now(),
		}
		events := []domain.ProtocolEvent{topic}
		s.persistInstanceReplies(ctx, c, protocol.ChannelWindowTarget(window.Name()), events)

		return protocol.Response{Events: events}, nil
	})
}

// handleInvite delegates to [Session.inviteAs] and returns a separate
// [domain.Inviting] value as the issuer's RPL_INVITING reply. Callers
// structurally match the complete response for synchronous numeric
// payloads. A typed dispatcher failure still goes through
// [commandResult].
//
// A refused INVITE against an unknown nick returns its typed error and
// a [domain.SystemNotice]. The notice is an [domain.IssuerReply], so it
// is filed to the issuer's reply log and a model re-experiences the
// refusal on replay. The success-case [domain.Inviting] is filed under
// the issuing window in the same way.
func (s *Session) handleInvite(ctx context.Context, c protocol.Client, guard protocol.WindowGuard, cmd protocol.Invite) (protocol.Response, error) {
	return s.onGuardedWriter(ctx, guard, func(ctx context.Context) (protocol.Response, error) {
		actor, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}
		issuerWindow, err := s.replyWindow(ctx, actor, cmd.Window, cmd.Name())
		if err != nil {
			return commandResult(err)
		}

		event, err := s.inviteAs(ctx, actor, cmd.Nick, cmd.Channel)

		issuerEvent := event
		switch event := event.(type) {
		case domain.Invited:
			issuerEvent = domain.Inviting{
				Target: event.Target, Invitee: event.Invitee, At: event.At,
			}
			if issuerWindow == nil {
				issuerWindow = protocol.ChannelWindowTarget(event.Target)
			}
		case domain.SystemNotice:
			if issuerWindow == nil {
				issuerWindow = protocol.ChannelWindowTarget(event.Target)
			}
		}
		var events []domain.ProtocolEvent
		if issuerEvent != nil {
			events = []domain.ProtocolEvent{issuerEvent}
			s.persistInstanceReplies(ctx, c, issuerWindow, events)
		}
		if err != nil {
			response, executionErr := commandResult(err)
			response.Events = events

			return response, executionErr
		}

		return protocol.Response{Events: events}, nil
	})
}

func (s *Session) handleKick(ctx context.Context, c protocol.Client, guard protocol.WindowGuard, cmd protocol.Kick) (protocol.Response, error) {
	return s.onGuardedWriter(ctx, guard, func(ctx context.Context) (protocol.Response, error) {
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

func (s *Session) handleNick(ctx context.Context, c protocol.Client, guard protocol.WindowGuard, cmd protocol.Nick) (protocol.Response, error) {
	return s.onGuardedWriter(ctx, guard, func(ctx context.Context) (protocol.Response, error) {
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
func (s *Session) handleWhois(ctx context.Context, c protocol.Client, guard protocol.WindowGuard, cmd protocol.Whois) (protocol.Response, error) {
	return s.onGuardedWriter(ctx, guard, func(ctx context.Context) (protocol.Response, error) {
		issuer, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}
		window, err := s.replyWindow(ctx, issuer, cmd.Window, cmd.Name())
		if err != nil {
			return commandResult(err)
		}

		inst, err := s.resolveConnectedNick(cmd.Nick)
		if err != nil {
			return commandResult(err)
		}

		whois := domain.Whois{
			Nick:           inst.Nick(),
			ModelID:        inst.ModelID,
			Persona:        inst.Persona(),
			PersonaLineage: s.whoisPersonaCounts(ctx, inst),
			Channels:       s.whoisChannels(ctx, issuer, inst),
			At:             s.now(),
		}

		events := []domain.ProtocolEvent{whois}
		s.persistInstanceReplies(ctx, c, window, events)

		return protocol.Response{Events: events}, nil
	})
}

// whoisPersonaCounts returns how much accepted reflection state the
// target's active persona revision rests on. A read that fails leaves
// the reply without the counts, since a WHOIS answers about a
// client's identity and the persona lineage is one line of that.
func (s *Session) whoisPersonaCounts(ctx context.Context, target *domain.Instance) domain.PersonaCounts {
	counts, err := s.store.PersonaCounts(ctx, target.ID())
	if err != nil {
		slog.Default().ErrorContext(ctx, "read whois persona counts",
			"component", "session",
			"instance_id", target.ID(),
			"error", err,
		)

		return domain.PersonaCounts{}
	}

	return counts
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
		if modes.Anonymous && issuer != target {
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
func (s *Session) handleList(ctx context.Context, c protocol.Client, guard protocol.WindowGuard, cmd protocol.List) (protocol.Response, error) {
	return s.onGuardedWriter(ctx, guard, func(ctx context.Context) (protocol.Response, error) {
		issuer, err := s.resolveClientActor(c)
		if err != nil {
			return protocol.Response{}, err
		}
		window, err := s.replyWindow(ctx, issuer, cmd.Window, cmd.Name())
		if err != nil {
			return commandResult(err)
		}

		channels, err := s.directoryChannels(ctx, issuer)
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
		s.persistInstanceReplies(ctx, c, window, events)

		events = append(events, domain.ListEnd{At: now})

		return protocol.Response{Events: events}, nil
	})
}

func (s *Session) replyWindow(
	ctx context.Context,
	issuer *domain.Instance,
	target protocol.WindowTarget,
	command string,
) (protocol.WindowTarget, error) {
	if target == nil {
		return nil, nil
	}
	if channel, ok := protocol.ChannelWindowName(target); ok {
		window, err := s.loadChannelWindow(ctx, channel)
		if errors.Is(err, store.ErrNoSuchChannel) {
			return nil, domain.NotOnChannelError{Channel: channel, Command: command, At: s.now()}
		}
		if err != nil {
			return nil, err
		}
		if !window.Members.HasInstance(issuer) || !issuer.InChannel(window.Name()) {
			return nil, domain.NotOnChannelError{
				Channel: window.Name(), Command: command, At: s.now(),
			}
		}

		return protocol.ChannelWindowTarget(window.Name()), nil
	}
	if peer, ok := protocol.DirectWindowPeer(target); ok {
		if s.activeClientHandle(protocol.ClientID(peer)) == nil {
			return nil, fmt.Errorf(
				"%s reply window %q: %w", command, peer, protocol.ErrWindowAuthorityChanged,
			)
		}

		return target, nil
	}

	return nil, fmt.Errorf("%s reply window: %w: %T", command, protocol.ErrInvalidWindowTarget, target)
}

// handleQuit dispatches a QUIT: [Session.quitAs] broadcasts it to
// the channels the actor was on and unwinds its membership, and the
// issuing client's connection is then revoked. Its queued QUIT and
// terminal ERROR drain before the subscription and model-client close.
//
// A model can end its own connection: the `quit` tool runs on that
// model's dispatch goroutine, so this handler is reached from
// inside the very goroutine the release addresses. The release is
// the phase that tolerates it — it cancels and unsubscribes without
// waiting — and it runs after the loop has let the QUIT go, because
// the dispatch goroutine it ends may be queued behind that loop.
// Once the QUIT has been broadcast, a later persistence failure does
// not keep the connection alive. The handler still returns that
// failure after starting the terminal drain.
func (s *Session) handleQuit(ctx context.Context, c protocol.Client, guard protocol.WindowGuard, cmd protocol.Quit) (protocol.Response, error) {
	var terminal *serverClient
	if c.Identity() != protocol.UserClientID {
		terminal = s.lookupClientHandle(c.Identity())
		if terminal != nil {
			terminal.beginTermination()
		}
	}

	committed := false
	resp, err := s.onGuardedWriter(ctx, guard, func(ctx context.Context) (protocol.Response, error) {
		actor, resolveErr := s.resolveClientActor(c)
		if resolveErr != nil {
			return protocol.Response{}, resolveErr
		}

		quitErr := s.quitAs(ctx, actor, cmd.Reason)
		committed = quitErr == nil || quitWasCommitted(quitErr)

		return commandResult(quitErr)
	})

	if committed {
		s.instanceDeleted(c.Identity())
		if terminal != nil {
			terminal.sealOutbound()
		}
		s.emitScoped(ctx, domain.ConnectionError{Reason: "Connection closed", At: s.now()}, clientScope{client: domain.InstanceID(c.Identity())})
		s.reapModelConnection(c.Identity())
	} else if terminal != nil {
		terminal.abortTermination()
	}

	return resp, err
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
// The registered instance has no channel membership until step 4.
// Its subscription can therefore exist before JOIN without receiving
// channel traffic from before that membership begins.
//
// A failure in step 3 or step 4 unwinds what came before it: the
// client is detached and the instance deleted, so a refused ADDMODEL
// leaves no nick claimed and no dispatch goroutine behind. A client
// that could not connect fails the command for the same reason a
// refused JOIN does: every member of a channel has a client behind
// it, so a message addressed to any nick in the member list reaches
// somebody, and a registration that stopped short of connecting
// would put a nick there that nothing can reach.
func (s *Session) handleAddModel(ctx context.Context, c protocol.Client, guard protocol.WindowGuard, cmd protocol.AddModel) (protocol.Response, error) {
	if !s.idHasServerOper(c.Identity()) {
		return protocol.Response{Err: domain.NotOperatorError{Command: "ADDMODEL", At: s.now()}}, nil
	}

	actor, err := s.resolveClientActor(c)
	if err != nil {
		return protocol.Response{}, err
	}
	issuer := s.activeClientHandle(c.Identity())
	if issuer == nil || issuer.owner != c {
		return protocol.Response{}, fmt.Errorf("add model issuer: %w", protocol.ErrSubscriptionClosed)
	}
	issuerGeneration, active := issuer.connection()
	if !active {
		return protocol.Response{}, fmt.Errorf("add model issuer: %w", protocol.ErrSubscriptionClosed)
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

	if reason := domain.ValidatePersona(prepared.Persona); reason != domain.PersonaAccepted {
		return protocol.Response{Err: domain.ErroneousPersonaError{Reason: reason, At: s.now()}}, nil
	}

	var inst *domain.Instance

	resp, err := s.onGuardedWriter(ctx, guard, func(ctx context.Context) (protocol.Response, error) {
		if !issuer.connectionValid(issuerGeneration) {
			return protocol.Response{}, fmt.Errorf("add model issuer: %w", protocol.ErrSubscriptionClosed)
		}

		registered, registerErr := s.registerModelAs(
			ctx, cmd.Channel, cmd.Model, prepared.Nick,
			prepared.Persona, prepared.PersonaTemplate,
		)
		if registerErr != nil {
			return commandResult(registerErr)
		}

		inst = registered

		return protocol.Response{}, nil
	})

	if err != nil || resp.Err != nil {
		return resp, err
	}

	modelClient, attachErr := s.startModelClient(ctx, inst)
	if attachErr != nil {
		s.discardModel(ctx, inst)

		return commandResult(observability.ErrWithKind(
			fmt.Errorf("connect model client: %w", attachErr),
			observability.ErrorKindDispatch,
		))
	}

	admitted, admitErr := s.onGuardedWriter(ctx, guard, func(ctx context.Context) (protocol.Response, error) {
		if !issuer.connectionValid(issuerGeneration) {
			return protocol.Response{}, fmt.Errorf("add model issuer: %w", protocol.ErrSubscriptionClosed)
		}

		channel, admissionErr := s.admitModelAs(ctx, c, actor, modelClient, inst, cmd.Channel)
		resp, err := commandResult(admissionErr)
		if err != nil || resp.Err != nil {
			return resp, err
		}

		resp.Events = s.preparationNotices(ctx, c, channel, prepared.Warnings)

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

	s.persistInstanceReplies(ctx, c, protocol.ChannelWindowTarget(ch), events)

	return events
}

const discardModelTimeout = 5 * time.Second

// discardModel unwinds a registration whose JOIN did not land. It
// removes any membership installed by a partially committed join,
// deletes the instance when possible, and records a tombstone when
// cleanup must continue after restart.
func (s *Session) discardModel(ctx context.Context, inst *domain.Instance) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardModelTimeout)
	defer cancel()

	id := protocol.ClientID(inst.ID())
	if client := s.lookupClientHandle(id); client != nil {
		client.replayMu.Lock()
		client.turnGeneration++
		client.deactivateConnection()
		client.replayMu.Unlock()
	}

	markErr := s.store.MarkInstancePendingDeletion(cleanupCtx, inst.ID())
	deleted := false
	_, cleanupErr := s.onWriter(cleanupCtx, func(ctx context.Context) (protocol.Response, error) {
		var cleanupErrors []error

		for _, ch := range s.instanceChannelNames(inst) {
			window, err := s.loadChannelWindow(ctx, ch)
			if err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("load channel %q: %w", ch, err))
				continue
			}

			if err := s.removeDeletedMember(ctx, window, inst); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("leave channel %q: %w", ch, err))
			}
		}

		if err := s.store.DeleteInstanceByID(ctx, inst.ID()); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete instance: %w", err))
		} else {
			deleted = true
		}

		return protocol.Response{}, errors.Join(cleanupErrors...)
	})

	if !deleted && markErr != nil {
		if retryErr := s.store.MarkInstancePendingDeletion(cleanupCtx, inst.ID()); retryErr != nil {
			markErr = errors.Join(markErr, fmt.Errorf("retry mark instance pending deletion: %w", retryErr))
		} else {
			markErr = nil
		}
	}

	if deleted {
		s.instanceDeleted(protocol.ClientID(inst.ID()))
	}

	s.reapDeadModelConnection(protocol.ClientID(inst.ID()))

	if err := errors.Join(markErr, cleanupErr); err != nil {
		slog.Default().ErrorContext(cleanupCtx, "discard model after failed add",
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
func (s *Session) handleKill(ctx context.Context, c protocol.Client, guard protocol.WindowGuard, cmd protocol.Kill) (protocol.Response, error) {
	var killed *domain.Instance
	var killedBy domain.Nick
	var terminal *serverClient
	var instanceDeleted bool

	resp, err := s.onGuardedWriter(ctx, guard, func(ctx context.Context) (protocol.Response, error) {
		if !s.idHasServerOper(c.Identity()) {
			return protocol.Response{Err: domain.NotOperatorError{Command: "KILL", At: s.now()}}, nil
		}

		oper, resolveErr := s.resolveClientActor(c)
		if resolveErr != nil {
			return protocol.Response{}, resolveErr
		}
		killedBy = oper.Nick()

		target, targetErr := s.resolveConnectedNick(cmd.Nick)
		if targetErr != nil {
			return commandResult(targetErr)
		}
		if target.ID() != domain.InstanceID(protocol.UserClientID) {
			terminal = s.lookupClientHandle(protocol.ClientID(target.ID()))
			if terminal != nil {
				terminal.beginTermination()
			}
		}

		outcome := s.killAs(ctx, oper, target, cmd.Reason)
		killed = target
		instanceDeleted = outcome.instanceDeleted

		return commandResult(outcome.err)
	})

	if killed == nil {
		if terminal != nil {
			terminal.abortTermination()
		}
		return resp, err
	}
	if terminal != nil {
		terminal.sealOutbound()
	}
	s.emitScoped(ctx, domain.ConnectionError{
		Reason: fmt.Sprintf("Killed by %s (%s)", killedBy, cmd.Reason),
		At:     s.now(),
	}, clientScope{client: killed.ID()})

	if instanceDeleted {
		s.instanceDeleted(protocol.ClientID(killed.ID()))
	}
	s.reapModelConnection(protocol.ClientID(killed.ID()))

	return resp, err
}

// commandResult separates a client-correctable command refusal from
// an execution failure. Refusals live on [protocol.Response.Err] so
// synchronous callers can branch on their domain type. Store,
// transport and internal failures use the function's second return.
func commandResult(err error) (protocol.Response, error) {
	if err == nil {
		return protocol.Response{}, nil
	}
	if commandRefusal(err) {
		return protocol.Response{Err: err}, nil
	}

	return protocol.Response{}, err
}

func commandRefusal(err error) bool {
	if observability.ErrorKindOf(err) == observability.ErrorKindValidation {
		return true
	}
	if errors.Is(err, protocol.ErrWindowAuthorityChanged) {
		return true
	}

	var event protocol.Event

	return errors.As(err, &event)
}

func quitWasCommitted(err error) bool {
	var committed *quitCommittedError

	return errors.As(err, &committed)
}

// resolveClientActor turns a [protocol.Client] handle into the
// `*domain.Instance` the `*As` methods take as their actor
// argument. The registered subscription carries the canonical
// instance pointer; the dispatcher reads it directly with no store
// round-trip. The dispatcher accepts only the exact non-nil pointer
// that registered the subscription.
func (s *Session) resolveClientActor(c protocol.Client) (*domain.Instance, error) {
	if !validClientHandle(c) {
		return nil, fmt.Errorf("client handle: %w", ErrInvalidClientHandle)
	}

	sc := s.activeClientHandle(c.Identity())
	if sc == nil || sc.owner != c {
		return nil, fmt.Errorf("client %q: %w", c.Identity(), ErrClientNotConnected)
	}
	return sc.instance, nil
}
