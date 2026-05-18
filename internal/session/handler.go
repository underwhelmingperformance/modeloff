package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// errHandlerNotYetImplemented is the underlying sentinel returned
// (wrapped via [errNotYetImplemented]) by handler cases that have
// no concrete delegate yet.
var errHandlerNotYetImplemented = errors.New("not yet implemented")

// Handle is the single entry point through which every protocol
// [protocol.Client] sends commands to the session. Each
// [protocol.Command] case looks up the actor implied by the
// client's identity and forwards to the existing `*As` session
// method (`joinAs`, `partAs`, …) where one exists.
//
// `AddModel`, `Quit`, and `Kill` currently return
// [errHandlerNotYetImplemented].
//
// The `default` branch is unreachable; the [protocol.Command] sum
// is sealed.
func (s *Session) Handle(ctx context.Context, c protocol.Client, cmd protocol.Command) (protocol.Response, error) {
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
		return s.handleKill(c, cmd)
	case protocol.Oper:
		return s.handleOper(ctx, c, cmd)
	case protocol.ChannelMode:
		return s.handleChannelMode(ctx, c, cmd)
	default:
		return protocol.Response{}, fmt.Errorf("unknown command %T", cmd)
	}
}

func (s *Session) handleChannelMode(ctx context.Context, c protocol.Client, cmd protocol.ChannelMode) (protocol.Response, error) {
	actor, err := s.resolveClientActor(ctx, c)
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
	if !s.operAuth(c, cmd.Name, cmd.Password) {
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
	actor, err := s.resolveClientActor(ctx, c)
	if err != nil {
		return protocol.Response{}, err
	}

	return commandResult(s.joinAs(ctx, actor, cmd.Channel, cmd.Key))
}

func (s *Session) handlePart(ctx context.Context, c protocol.Client, cmd protocol.Part) (protocol.Response, error) {
	actor, err := s.resolveClientActor(ctx, c)
	if err != nil {
		return protocol.Response{}, err
	}

	return commandResult(s.partAs(ctx, actor, cmd.Channel, cmd.Reason))
}

func (s *Session) handlePrivMsg(ctx context.Context, c protocol.Client, cmd protocol.PrivMsg) (protocol.Response, error) {
	actor, err := s.resolveClientActor(ctx, c)
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
	actor, err := s.resolveClientActor(ctx, c)
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
	actor, err := s.resolveClientActor(ctx, c)
	if err != nil {
		return protocol.Response{}, err
	}

	return commandResult(s.setTopicAs(ctx, actor, cmd.Channel, cmd.Body))
}

func (s *Session) handleInvite(ctx context.Context, c protocol.Client, cmd protocol.Invite) (protocol.Response, error) {
	actor, err := s.resolveClientActor(ctx, c)
	if err != nil {
		return protocol.Response{}, err
	}

	return commandResult(s.inviteAs(ctx, actor, cmd.Nick, cmd.Channel))
}

func (s *Session) handleKick(ctx context.Context, c protocol.Client, cmd protocol.Kick) (protocol.Response, error) {
	actor, err := s.resolveClientActor(ctx, c)
	if err != nil {
		return protocol.Response{}, err
	}

	target, err := s.ResolveNick(ctx, cmd.Nick)
	if err != nil {
		return commandResult(err)
	}

	return commandResult(s.kickAs(ctx, actor, target, cmd.Channel))
}

func (s *Session) handleNick(ctx context.Context, c protocol.Client, cmd protocol.Nick) (protocol.Response, error) {
	actor, err := s.resolveClientActor(ctx, c)
	if err != nil {
		return protocol.Response{}, err
	}

	return commandResult(s.changeNickAs(ctx, actor, cmd.New))
}

func (s *Session) handleWhois(ctx context.Context, cmd protocol.Whois) (protocol.Response, error) {
	_, err := s.Whois(ctx, cmd.Nick)

	return commandResult(err)
}

func (s *Session) handleList(ctx context.Context) (protocol.Response, error) {
	_, err := s.DirectoryChannels(ctx)

	return commandResult(err)
}

// handleQuit dispatches a QUIT — the user-actor branch tears
// down session state in-place (autojoin save, session-active
// marker clear); the model-actor branch broadcasts the QUIT to
// peers and reaps the subscription via [Session.reapClient]. The
// user-client is never reaped: its lifetime equals the session's,
// and the process owning it shuts down after [handleQuit] returns.
func (s *Session) handleQuit(ctx context.Context, c protocol.Client, cmd protocol.Quit) (protocol.Response, error) {
	actor, err := s.resolveClientActor(ctx, c)
	if err != nil {
		return protocol.Response{}, err
	}

	if quitErr := s.quitAs(ctx, actor, cmd.Reason); quitErr != nil {
		return commandResult(quitErr)
	}

	if c.Identity() != protocol.UserClientID {
		s.reapClient(c.Identity())
	}

	return protocol.Response{}, nil
}

func (s *Session) handleAddModel(ctx context.Context, c protocol.Client, cmd protocol.AddModel) (protocol.Response, error) {
	if !c.HasMode(domain.ModeOperator) {
		return protocol.Response{Err: domain.NotOperatorError{Command: "ADDMODEL", At: s.now()}}, nil
	}

	actor, err := s.resolveClientActor(ctx, c)
	if err != nil {
		return protocol.Response{}, err
	}

	if _, addErr := s.addModelAs(ctx, actor, cmd.Channel, cmd.Model, cmd.Persona); addErr != nil {
		return commandResult(addErr)
	}

	return protocol.Response{}, nil
}

func (s *Session) handleKill(c protocol.Client, cmd protocol.Kill) (protocol.Response, error) {
	if !c.HasMode(domain.ModeOperator) {
		return protocol.Response{Err: domain.NotOperatorError{Command: "KILL", At: s.now()}}, nil
	}

	return protocol.Response{}, errNotYetImplemented(cmd)
}

// commandResult turns a delegation-call error into the canonical
// protocol shape: command failures live on [protocol.Response.Err]
// so synchronous callers can branch on them with `errors.As`. A nil
// `err` produces the empty success response.
func commandResult(err error) (protocol.Response, error) {
	return protocol.Response{Err: err}, nil
}

// resolveClientActor turns a [protocol.Client] handle into the
// `*domain.Instance` the existing `*As` methods take as their actor
// argument. The user-client (identified by [protocol.UserClientID])
// resolves to `s.user`; any other id is looked up in the store, which
// returns the canonical pointer-stable handle.
func (s *Session) resolveClientActor(ctx context.Context, c protocol.Client) (*domain.Instance, error) {
	id := c.Identity()
	if id == protocol.UserClientID {
		return s.user, nil
	}

	return s.store.GetInstanceByID(ctx, id)
}

// errNotYetImplemented wraps [errHandlerNotYetImplemented] with the
// command type so callers see which case fired.
func errNotYetImplemented(cmd protocol.Command) error {
	return fmt.Errorf("protocol command %T: %w", cmd, errHandlerNotYetImplemented)
}
