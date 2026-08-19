package session

import (
	"context"
	"errors"

	"github.com/laney/modeloff/internal/protocol"
)

// ErrSessionClosed is returned by [Session.Handle] for a command
// that arrives after the command loop has stopped. The session is
// on its way out: there is no state left to mutate and no bus left
// to emit on. Callers branch on it with `errors.Is` to tell a
// shutting-down server from a command that genuinely failed.
var ErrSessionClosed = errors.New("session: command loop has stopped")

// writerJob is one unit of work for the session's command loop. The
// submitter blocks on `done` until the loop has run `fn`, so a
// client's `Send` still returns that command's own `Response`.
type writerJob struct {
	ctx  context.Context
	fn   func(context.Context)
	done chan struct{}
}

// runWriter is the session's command loop: the single goroutine that
// processes client commands, one at a time, in the order they arrive.
// An ircd works this way — commands from every connection funnel into
// one event loop — and it is what makes a command's read-modify-write
// of channel state atomic without a lock per field.
//
// The loop exits when the lifetime ctx derived from the `baseContext`
// supplier is cancelled or when [Session.Shutdown] closes the
// shutdown gate, whichever happens first. `writerStopped` closes on
// the way out, so a submitter blocked on the handoff is released
// with an error; it does not wait on a loop that is gone.
func (s *Session) runWriter(ctx context.Context) {
	defer close(s.writerStopped)

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.shuttingDown:
			return
		case job := <-s.writerQ:
			s.runWriterJob(job)
		}
	}
}

// runWriterJob executes one job and releases its submitter. The
// release is deferred so a panicking handler unblocks its caller on
// the way out; the panic itself is left to crash the process, since
// a command loop that has lost its footing has no safe state to
// carry on from.
func (s *Session) runWriterJob(job writerJob) {
	defer close(job.done)

	job.fn(job.ctx)
}

// onWriter runs `fn` on the command loop and returns its result. The
// handoff channel is unbuffered, so a successful send means the loop
// has taken the job and will run it: there is no window in which a
// job is accepted and then dropped.
//
// Every state-mutating command handler routes through here, which is
// what makes the session a single writer. Two rules follow from that
// and are load-bearing:
//
//   - Code running under `fn` must not call `onWriter` again, whether
//     directly or through a client's `Send`. The loop is busy running
//     `fn`, so the nested submission would never be taken up.
//   - Blocking work belongs outside `fn`. An LLM round-trip
//     ([ModelClientFactory.PrepareInstance]), a subscription's
//     history load ([ModelClientFactory.Attach]), or a join on
//     another goroutine ([ModelClientFactory.Detach]) stalls every
//     other client for its duration, and `Detach` waits on a
//     dispatch goroutine that may itself be queued behind the loop.
func (s *Session) onWriter(
	ctx context.Context,
	fn func(context.Context) (protocol.Response, error),
) (protocol.Response, error) {
	var (
		resp protocol.Response
		err  error
	)

	job := writerJob{
		ctx:  ctx,
		fn:   func(ctx context.Context) { resp, err = fn(ctx) },
		done: make(chan struct{}),
	}

	select {
	case s.writerQ <- job:
	case <-s.writerStopped:
		return protocol.Response{}, ErrSessionClosed
	case <-ctx.Done():
		return protocol.Response{}, ctx.Err()
	}

	// The wait has no cancellation arm on purpose. An accepted job
	// is a job the loop is running, and it reports its result by
	// assigning through this closure; a caller that walked away
	// early would be racing those writes. The loop keeps this wait
	// short by never blocking on a consumer — deliveries go to each
	// subscription's send queue.
	<-job.done

	return resp, err
}
