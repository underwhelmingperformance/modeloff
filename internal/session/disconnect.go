package session

import (
	"context"
	"log/slog"

	"github.com/laney/modeloff/internal/protocol"
)

// sendQExceededReason is the QUIT message left behind by a client
// the server dropped for outbound overflow. The wording is the one
// ircds have used for this since RFC 1459 §8.10.
const sendQExceededReason = "Max SendQ exceeded"

// Disconnect ends `id`'s connection from the server side: the QUIT
// carrying `reason` is broadcast to the channels the client was in,
// its model-client is released, and its subscription is reaped. It
// is the same teardown a client's own QUIT runs, with the server
// supplying the message (RFC 2812 §3.1.7) — so a connection the
// server closes reads in the channel exactly like one the client
// closed, and no window is left showing a member who is not there.
//
// It is undefined for a client whose lifetime is the session's, and
// refuses one. There is no connection there to close: that client
// and the process hosting the server are the same thing, so the
// teardown would not end anything, it would only run the session's
// own shutdown — dropping every membership, rewriting the autojoin
// list and clearing the crash marker — underneath a process still
// running on the far side of it. The session ends when that process
// does, and nothing before then may pretend otherwise.
//
// Runs off the command loop, which the QUIT itself takes; a caller
// already on the loop would wait for a turn it is holding. Calling
// it for an unregistered identity does nothing.
func (s *Session) Disconnect(ctx context.Context, id protocol.ClientID, reason string) {
	if id == protocol.UserClientID {
		return
	}

	sc := s.lookupClientHandle(id)
	if sc == nil {
		return
	}

	resp, err := s.onWriter(ctx, func(ctx context.Context) (protocol.Response, error) {
		return commandResult(s.quitAs(ctx, sc.instance, reason))
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

	s.releaseClient(id)
	sc.Unsubscribe()
}

// disconnectOverflowed ends a subscription whose send queue passed
// its allowance. The teardown runs on its own goroutine because the
// producer that filled the queue is usually the command loop, and
// the QUIT the disconnect broadcasts needs that same loop.
//
// Only the delivery that trips the allowance starts it; the QUIT
// then lands back on this very queue, and everything still arriving
// for a client on its way out finds the flag already latched.
//
// A subscription with the session's lifetime never reaches here:
// [serverClient.queue] does not report overflow for it, because
// there is no connection to close in exchange for the bound.
func (s *Session) disconnectOverflowed(c *serverClient) {
	if c.hasSessionLifetime() {
		return
	}

	if !c.markOverflowed() {
		return
	}

	go s.Disconnect(s.baseContext(), c.id, sendQExceededReason)
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
