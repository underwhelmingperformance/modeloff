package session

import (
	"context"
	"log/slog"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// sendQExceededReason is the QUIT message left behind by a client
// the server dropped for outbound overflow. The wording is the one
// ircds have used for this since RFC 1459 §8.10.
const sendQExceededReason = "Max SendQ exceeded"

const scrollbackPersistenceFailureReason = "Scrollback persistence failed"

// Disconnect ends `id`'s connection from the server side: the QUIT
// carrying `reason` is broadcast to the channels the client was in,
// its model-client is released, and its subscription is reaped. It
// is the same teardown a client's own QUIT runs, with the server
// supplying the message (RFC 2812 §3.1.7). The connection loses
// authority after the durable deletion commits. Its accepted delivery
// prefix drains before the subscription closes.
//
// It is undefined for a client whose lifetime is the session's, and
// refuses one. There is no connection there to close: that client
// and the process hosting the server are the same thing, so the
// teardown would end nothing. It would drop every membership and
// put a QUIT in each channel underneath a process still running on
// the far side of it. The session ends when that process does, and
// nothing before then may pretend otherwise.
//
// Runs off the command loop, which the QUIT itself takes; a caller
// already on the loop would wait for a turn it is holding. Calling
// it for an unregistered identity does nothing.
func (s *Session) Disconnect(ctx context.Context, id protocol.ClientID, reason string) {
	s.disconnectClient(ctx, id, reason, true)
}

// DisconnectDeadClient ends a model connection whose event consumer
// has already failed. Peer-visible teardown still follows the normal
// QUIT path, but the target subscription is reaped without waiting for
// terminal deliveries that no consumer remains to read.
func (s *Session) DisconnectDeadClient(ctx context.Context, id protocol.ClientID, reason string) {
	s.disconnectClient(ctx, id, reason, false)
}

func (s *Session) disconnectClient(
	ctx context.Context,
	id protocol.ClientID,
	reason string,
	drainTerminal bool,
) {
	if id == protocol.UserClientID {
		return
	}
	if !s.beginHandler() {
		return
	}
	defer s.handlers.Done()

	sc := s.lookupClientHandle(id)
	if sc == nil {
		return
	}
	sc.beginTermination()

	committed := false
	instanceDeleted := false
	resp, err := s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		outcome := s.quit(ctx, sc.instance, reason, quitForced, nil)
		committed = outcome.err == nil || quitWasCommitted(outcome.err)
		instanceDeleted = outcome.instanceDeleted

		return commandResult(outcome.err)
	})

	if err == nil {
		err = resp.Err
	}

	if err != nil {
		slog.Default().ErrorContext(ctx, "disconnect client",
			"component", "session",
			"client_id", id,
			"reason", reason,
			"error", err,
		)
	}

	if committed {
		if instanceDeleted {
			s.instanceDeleted(id)
		}
		sc.sealOutbound()
		s.emitScoped(ctx, domain.ConnectionError{
			Reason: reason,
			At:     s.now(),
		}, clientScope{client: sc.instance.ID()})
		if drainTerminal {
			s.reapModelConnection(id)
		} else {
			s.reapDeadModelConnection(id)
		}
	} else {
		sc.abortTermination()
	}
}

// disconnectOverflowed ends a subscription whose send queue passed
// its allowance. The teardown runs on its own goroutine because the
// producer that filled the queue is usually the command loop, and
// the QUIT the disconnect broadcasts needs that same loop.
// It also detaches from the producer's cancellation: the accepted
// delivery has already overflowed the connection, so returning from
// that command must not cancel the required teardown. The session's
// writer gate still refuses the work after shutdown.
//
// Only the delivery that trips the allowance starts it; the QUIT
// then lands back on this very queue, and everything still arriving
// for a client on its way out finds the flag already latched.
//
// A subscription with the session's lifetime never reaches here:
// [serverClient.queue] does not report overflow for it, because
// there is no connection to close in exchange for the bound.
func (s *Session) disconnectOverflowed(ctx context.Context, c *serverClient) {
	if c.hasSessionLifetime() {
		return
	}

	if !c.claimServerDisconnect() {
		return
	}

	go s.Disconnect(context.WithoutCancel(ctx), c.id, sendQExceededReason)
}

func (s *Session) disconnectUnreplayable(ctx context.Context, c *serverClient) {
	if c.hasSessionLifetime() {
		return
	}

	if !c.claimServerDisconnect() {
		return
	}

	go s.Disconnect(
		context.WithoutCancel(ctx), c.id, scrollbackPersistenceFailureReason,
	)
}

// releaseClient hands a departed client's model-client back to the
// factory, ending its dispatch goroutine. The user-client has none:
// it is constructed outside the session and its lifetime is the
// session's.
//
// Runs off the command loop. `Detach` reaches into a model-client
// whose dispatch goroutine may itself be queued behind the loop for
// a command of its own.
func (s *Session) releaseClient(id protocol.ClientID) {
	if id == protocol.UserClientID {
		return
	}

	s.modelClientFactory.Detach(id)
}

func (s *Session) instanceDeleted(id protocol.ClientID) {
	if id == protocol.UserClientID {
		return
	}

	s.modelClientFactory.InstanceDeleted(id)
}

func (s *Session) interruptModelTurn(id protocol.ClientID) {
	if id == protocol.UserClientID {
		return
	}

	s.modelClientFactory.InterruptTurn(id)
}

func (s *Session) interruptModelWindow(id protocol.ClientID, window domain.ChannelName) {
	if id == protocol.UserClientID {
		return
	}

	s.modelClientFactory.InterruptWindow(id, window)
}

func (s *Session) interruptModelPeerWindows(peer domain.InstanceID) {
	window := domain.ChannelName(peer)
	for id := range s.activeConnections() {
		s.interruptModelWindow(id, window)
	}
}

func (s *Session) reapModelConnection(id protocol.ClientID) {
	if id == protocol.UserClientID {
		return
	}
	s.interruptModelTurn(id)

	subscription := s.takeModelSubscription(id)
	if subscription == nil {
		return
	}

	drained := subscription.drainOutbound()
	s.reapers.Go(func() {
		<-drained
		s.releaseClient(id)
		s.reapClient(id)
	})
}

func (s *Session) reapDeadModelConnection(id protocol.ClientID) {
	if id == protocol.UserClientID {
		return
	}

	subscription := s.takeModelSubscription(id)
	if subscription == nil {
		return
	}

	s.releaseClient(id)
	s.reapClient(id)
}

func (s *Session) takeModelSubscription(id protocol.ClientID) *serverClient {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()

	subscription := s.clientHandles[id]
	if subscription != nil {
		delete(s.clientHandles, id)
		delete(s.attachments, id)
	}

	return subscription
}
