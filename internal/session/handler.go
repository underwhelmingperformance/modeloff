package session

import (
	"context"
	"errors"
	"fmt"

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
		return s.handleWhois(ctx, cmd)
	case protocol.List:
		return s.handleList(ctx)
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
	actor, err := s.resolveClientActor(c)
	if err != nil {
		return protocol.Response{}, err
	}

	return commandResult(s.applyChannelModeChangesAs(ctx, actor, cmd.Channel, cmd.Changes))
}

// handleOper validates the issuing client's credentials via the
// session's authenticator. On success the server issues the
// canonical MODE response: server-actor (empty `by`), target is
// the requesting client, flag is [domain.ModeOperator]. The
// emission shape matches the bootstrap path's promotion of the
// user-client.
func (s *Session) handleOper(ctx context.Context, c protocol.Client, cmd protocol.Oper) (protocol.Response, error) {
	if !s.operAuth(c, cmd.User, cmd.Password) {
		return protocol.Response{Err: domain.OperFailedError{At: s.now()}}, nil
	}

	sc := s.lookupClientHandle(c.Identity())
	if sc == nil {
		return protocol.Response{}, fmt.Errorf("oper: client %q not registered", c.Identity())
	}

	s.setUserModeAs(ctx, "", sc, domain.ModeOperator, true)
	return protocol.Response{}, nil
}

func (s *Session) handleJoin(ctx context.Context, c protocol.Client, cmd protocol.Join) (protocol.Response, error) {
	actor, err := s.resolveClientActor(c)
	if err != nil {
		return protocol.Response{}, err
	}

	return commandResult(s.joinAs(ctx, actor, cmd.Channel, cmd.Key))
}

func (s *Session) handlePart(ctx context.Context, c protocol.Client, cmd protocol.Part) (protocol.Response, error) {
	actor, err := s.resolveClientActor(c)
	if err != nil {
		return protocol.Response{}, err
	}

	return commandResult(s.partAs(ctx, actor, cmd.Channel, cmd.Reason))
}

func (s *Session) handlePrivMsg(ctx context.Context, c protocol.Client, cmd protocol.PrivMsg) (protocol.Response, error) {
	actor, err := s.resolveClientActor(c)
	if err != nil {
		return protocol.Response{}, err
	}

	msg, sendErr := s.sendMessageAs(ctx, actor, cmd.Target, cmd.Body)
	if sendErr != nil {
		return commandResult(sendErr)
	}

	return protocol.Response{Events: []protocol.Event{msg}}, nil
}

func (s *Session) handleAction(ctx context.Context, c protocol.Client, cmd protocol.Action) (protocol.Response, error) {
	actor, err := s.resolveClientActor(c)
	if err != nil {
		return protocol.Response{}, err
	}

	msg, sendErr := s.sendActionAs(ctx, actor, cmd.Target, cmd.Body)
	if sendErr != nil {
		return commandResult(sendErr)
	}

	return protocol.Response{Events: []protocol.Event{msg}}, nil
}

func (s *Session) handleTopic(ctx context.Context, c protocol.Client, cmd protocol.Topic) (protocol.Response, error) {
	actor, err := s.resolveClientActor(c)
	if err != nil {
		return protocol.Response{}, err
	}

	return commandResult(s.setTopicAs(ctx, actor, cmd.Channel, cmd.Body))
}

// handleInvite delegates to [Session.inviteAs] and lands the
// resulting envelope in `Response.Events` as the inviter's
// RPL_INVITING-equivalent. The chat-screen's `sendCommand` reads
// `Response.Events[0]` for synchronous numeric-reply payloads
// (see `internal/ui/chatcmd.sendCommand` and the `WhoisCommand`
// pattern). A typed dispatcher failure still goes through
// [commandResult].
func (s *Session) handleInvite(ctx context.Context, c protocol.Client, cmd protocol.Invite) (protocol.Response, error) {
	actor, err := s.resolveClientActor(c)
	if err != nil {
		return protocol.Response{}, err
	}

	event, err := s.inviteAs(ctx, actor, cmd.Nick, cmd.Channel)
	if err != nil {
		return commandResult(err)
	}

	return protocol.Response{Events: []domain.ProtocolEvent{event}}, nil
}

func (s *Session) handleKick(ctx context.Context, c protocol.Client, cmd protocol.Kick) (protocol.Response, error) {
	actor, err := s.resolveClientActor(c)
	if err != nil {
		return protocol.Response{}, err
	}

	target, err := s.dispatcherResolveNick(ctx, cmd.Nick)
	if err != nil {
		return commandResult(err)
	}

	return commandResult(s.kickAs(ctx, actor, target, cmd.Channel))
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
	actor, err := s.resolveClientActor(c)
	if err != nil {
		return protocol.Response{}, err
	}

	return commandResult(s.changeNickAs(ctx, actor, cmd.New))
}

// handleWhois resolves the requested nick and returns the
// canonical `domain.Whois` snapshot in `Response.Events` (RFC 2812
// numeric 311 `RPL_WHOISUSER`). The snapshot freezes the
// instance's mutable identity surface at the moment of issue so
// later renames or persona edits don't retro-edit historical
// renderings. Renderers consume the event directly without going
// back to the store.
func (s *Session) handleWhois(ctx context.Context, cmd protocol.Whois) (protocol.Response, error) {
	inst, err := s.dispatcherResolveNick(ctx, cmd.Nick)
	if err != nil {
		return commandResult(err)
	}

	whois := domain.Whois{
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

	return protocol.Response{Events: []domain.ProtocolEvent{whois}}, nil
}

// handleList enumerates the channel directory and returns one
// `domain.ListReply` per visible channel followed by a closing
// `domain.ListEnd` in `Response.Events` (RFC 2812 numerics 322
// `RPL_LIST` / 323 `RPL_LISTEND`). The `+s` and `+p` filters live
// in [Session.DirectoryChannels] so the wire reply matches the
// chat-screen's directory view exactly.
func (s *Session) handleList(ctx context.Context) (protocol.Response, error) {
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
	events = append(events, domain.ListEnd{At: now})

	return protocol.Response{Events: events}, nil
}

// handleQuit dispatches a QUIT — the user-actor branch tears
// down session state in-place (autojoin save, session-active
// marker clear); the model-actor branch broadcasts the QUIT to
// peers and releases the subscription via
// [ModelClientFactory.Detach] so the model-client's dispatch
// goroutine joins deterministically. The user-client is never
// detached: its lifetime equals the session's, and the process
// owning it shuts down after [handleQuit] returns.
func (s *Session) handleQuit(ctx context.Context, c protocol.Client, cmd protocol.Quit) (protocol.Response, error) {
	actor, err := s.resolveClientActor(c)
	if err != nil {
		return protocol.Response{}, err
	}

	if quitErr := s.quitAs(ctx, actor, cmd.Reason); quitErr != nil {
		return commandResult(quitErr)
	}

	if c.Identity() != protocol.UserClientID {
		s.modelClientFactory.Detach(c.Identity())
	}

	return protocol.Response{}, nil
}

func (s *Session) handleAddModel(ctx context.Context, c protocol.Client, cmd protocol.AddModel) (protocol.Response, error) {
	if !s.idHasServerOper(c.Identity()) {
		return protocol.Response{Err: domain.NotOperatorError{Command: "ADDMODEL", At: s.now()}}, nil
	}

	actor, err := s.resolveClientActor(c)
	if err != nil {
		return protocol.Response{}, err
	}

	if _, addErr := s.addModelAs(ctx, actor, cmd.Channel, cmd.Model, cmd.Persona); addErr != nil {
		return commandResult(addErr)
	}

	return protocol.Response{}, nil
}

func (s *Session) handleKill(ctx context.Context, c protocol.Client, cmd protocol.Kill) (protocol.Response, error) {
	if !s.idHasServerOper(c.Identity()) {
		return protocol.Response{Err: domain.NotOperatorError{Command: "KILL", At: s.now()}}, nil
	}

	oper, err := s.resolveClientActor(c)
	if err != nil {
		return protocol.Response{}, err
	}

	target, err := s.dispatcherResolveNick(ctx, cmd.Nick)
	if err != nil {
		return commandResult(err)
	}

	if killErr := s.killAs(ctx, oper, target, cmd.Reason); killErr != nil {
		return commandResult(killErr)
	}

	s.modelClientFactory.Detach(protocol.ClientID(target.ID()))

	return protocol.Response{}, nil
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
