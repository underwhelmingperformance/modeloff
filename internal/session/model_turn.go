package session

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

// sessionGuard is a window authority this session issued. Its
// operations are unexported, so no type outside this package can
// supply one, and the actor-bound store surface behind them is
// reachable only through a guard the session handed out.
//
// Each guard kind answers for itself: a member's guard admits a turn
// on the channel it captured, and an invitee's guard admits the turn
// an INVITE earns before the JOIN.
type sessionGuard interface {
	protocol.WindowGuard

	beginModelTurn(
		ctx context.Context,
		turn store.ModelTurn,
		input store.ModelTurnEntry,
	) (store.ModelTurnID, error)
	appendModelTurnEntry(
		ctx context.Context,
		turnID store.ModelTurnID,
		entry store.ModelTurnEntry,
	) error
}

// issuedGuard recovers the session-side authority behind `guard`.
// The public actor-bound methods take a [protocol.WindowGuard]
// because that is what a client holds; a value this session did not
// issue grants nothing and is refused here.
func issuedGuard(guard protocol.WindowGuard, operation string) (sessionGuard, error) {
	issued, ok := guard.(sessionGuard)
	if !ok {
		return nil, fmt.Errorf("%s: window guard %T was not issued by this session", operation, guard)
	}

	return issued, nil
}

// BeginModelTurn admits a journal only while the guard still grants
// its actor access to the same window. The guard's private session
// state makes this an actor-bound operation even though the durable
// row identity remains inside the session and store.
func (s *Session) BeginModelTurn(
	ctx context.Context,
	guard protocol.WindowGuard,
	turn store.ModelTurn,
	input store.ModelTurnEntry,
) (store.ModelTurnRecorder, error) {
	issued, err := issuedGuard(guard, "begin model turn")
	if err != nil {
		return nil, err
	}

	turnID, err := issued.beginModelTurn(ctx, turn, input)
	if err != nil {
		return nil, err
	}

	return guardedModelTurnRecorder{guard: issued, turnID: turnID}, nil
}

type guardedModelTurnRecorder struct {
	guard  sessionGuard
	turnID store.ModelTurnID
}

func (r guardedModelTurnRecorder) AppendModelTurnEntry(
	ctx context.Context,
	entry store.ModelTurnEntry,
) error {
	return r.guard.appendModelTurnEntry(ctx, r.turnID, entry)
}

func (guard windowGuard) beginModelTurn(
	ctx context.Context,
	turn store.ModelTurn,
	input store.ModelTurnEntry,
) (store.ModelTurnID, error) {
	s := guard.client.sess
	if err := validateModelTurnOwner(guard.client, guard.target, turn); err != nil {
		return 0, err
	}
	turn.Window = canonicalWindowGuardTarget(guard)

	unlock := lockTurnClients(guard.client, guard.dmPeer)
	defer unlock()

	if !guard.client.connectionValid(guard.clientGeneration) ||
		guard.client.turnGeneration != guard.turnGeneration {
		return 0, fmt.Errorf("begin model turn: %w", protocol.ErrSubscriptionClosed)
	}

	if _, direct := protocol.DirectWindowPeer(guard.target); direct {
		if guard.dmPeer == nil ||
			(guard.dmPeer != guard.client && !guard.dmPeer.connectionValid(guard.dmPeerGeneration)) ||
			guard.dmPeer.turnGeneration != guard.dmTurnGeneration {
			return 0, domain.UnknownNickError{Nick: domain.Nick(guard.window), At: s.now()}
		}

		return s.store.BeginModelTurn(ctx, turn, input)
	}
	if guard.client.windowEpoch(guard.window) != guard.epoch {
		return 0, domain.NotOnChannelError{
			Channel: guard.window,
			Command: "DISPATCH",
			At:      s.now(),
		}
	}

	window, err := s.liveChannelWindow(ctx, guard.window)
	if err != nil || !window.Members.HasInstance(guard.client.instance) ||
		!guard.client.instance.InChannel(window.Name()) ||
		guard.client.windowEpoch(window.Name()) != guard.epoch {
		return 0, domain.NotOnChannelError{
			Channel: guard.window,
			Command: "DISPATCH",
			At:      s.now(),
		}
	}

	return s.store.BeginModelTurn(ctx, turn, input)
}

func (guard invitationGuard) beginModelTurn(
	ctx context.Context,
	turn store.ModelTurn,
	input store.ModelTurnEntry,
) (store.ModelTurnID, error) {
	s := guard.client.sess
	target := protocol.ChannelWindowTarget(guard.channel)
	if err := validateModelTurnOwner(guard.client, target, turn); err != nil {
		return 0, err
	}
	turn.Window = target

	unlock := lockTurnClients(guard.client)
	defer unlock()

	if !guard.client.connectionValid(guard.clientGeneration) ||
		guard.client.turnGeneration != guard.turnGeneration {
		return 0, fmt.Errorf("begin model turn: %w", protocol.ErrSubscriptionClosed)
	}
	windowEpoch := guard.client.windowEpoch(guard.channel)

	s.channels.mu.Lock()
	defer s.channels.mu.Unlock()

	_, authority, err := s.invitationGuardStateLocked(ctx, guard, windowEpoch)
	if err != nil || authority != invitationAuthorityPending {
		return 0, domain.NotOnChannelError{
			Channel: guard.channel,
			Command: "INVITE",
			At:      s.now(),
		}
	}

	return s.store.BeginModelTurn(ctx, turn, input)
}

func (guard windowGuard) appendModelTurnEntry(
	ctx context.Context,
	turnID store.ModelTurnID,
	entry store.ModelTurnEntry,
) error {
	s := guard.client.sess
	unlock := lockTurnClients(guard.client, guard.dmPeer)
	defer unlock()

	if !guard.client.connectionValid(guard.clientGeneration) ||
		guard.client.turnGeneration != guard.turnGeneration {
		return store.ErrModelTurnClosed
	}

	if _, direct := protocol.DirectWindowPeer(guard.target); direct {
		if guard.dmPeer == nil ||
			(guard.dmPeer != guard.client && !guard.dmPeer.connectionValid(guard.dmPeerGeneration)) ||
			guard.dmPeer.turnGeneration != guard.dmTurnGeneration {
			return store.ErrModelTurnClosed
		}

		return s.store.AppendModelTurnEntry(ctx, turnID, entry)
	}
	if guard.client.windowEpoch(guard.window) != guard.epoch {
		return store.ErrModelTurnClosed
	}

	window, err := s.liveChannelWindow(ctx, guard.window)
	if err != nil {
		return err
	}
	if !window.Members.HasInstance(guard.client.instance) ||
		!guard.client.instance.InChannel(window.Name()) ||
		guard.client.windowEpoch(window.Name()) != guard.epoch {
		return store.ErrModelTurnClosed
	}

	return s.store.AppendModelTurnEntry(ctx, turnID, entry)
}

func (guard invitationGuard) appendModelTurnEntry(
	ctx context.Context,
	turnID store.ModelTurnID,
	entry store.ModelTurnEntry,
) error {
	s := guard.client.sess
	unlock := lockTurnClients(guard.client)
	defer unlock()

	if !guard.client.connectionValid(guard.clientGeneration) ||
		guard.client.turnGeneration != guard.turnGeneration {
		return store.ErrModelTurnClosed
	}
	windowEpoch := guard.client.windowEpoch(guard.channel)

	_, authority, err := s.invitationGuardState(ctx, guard, windowEpoch)
	if err != nil {
		return err
	}
	if authority == invitationAuthorityNone {
		return store.ErrModelTurnClosed
	}

	return s.store.AppendModelTurnEntry(ctx, turnID, entry)
}

func canonicalWindowGuardTarget(guard windowGuard) protocol.WindowTarget {
	if _, direct := protocol.DirectWindowPeer(guard.target); direct {
		return protocol.DirectWindowTarget(domain.InstanceID(guard.window))
	}

	return protocol.ChannelWindowTarget(guard.window)
}

func validateModelTurnOwner(
	client *serverClient,
	target protocol.WindowTarget,
	turn store.ModelTurn,
) error {
	if client == nil || turn.InstanceID != client.instance.ID() ||
		!protocol.EqualWindowTarget(turn.Window, target) {
		return fmt.Errorf("begin model turn: guard does not match actor and window")
	}

	return nil
}

func lockTurnClients(clients ...*serverClient) func() {
	unique := make([]*serverClient, 0, len(clients))
	seen := make(map[*serverClient]struct{}, len(clients))
	for _, client := range clients {
		if client == nil {
			continue
		}
		if _, ok := seen[client]; ok {
			continue
		}

		seen[client] = struct{}{}
		unique = append(unique, client)
	}
	slices.SortFunc(unique, func(a, b *serverClient) int {
		return strings.Compare(string(a.id), string(b.id))
	})
	for _, client := range unique {
		client.replayMu.Lock()
	}

	return func() {
		for _, client := range slices.Backward(unique) {
			client.replayMu.Unlock()
		}
	}
}

func (c *serverClient) windowEpoch(window domain.ChannelName) uint64 {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	return c.windowEpochs[window]
}
