package modelclient

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

// journalQueueDepth is how many entries the queue holds before a
// producer waits for the consumer. A turn produces one input entry,
// three per tool-loop iteration up to [maxToolLoopTurns], two per
// compaction round trip and one outcome, so this depth covers the
// whole of one turn and the start of the next.
const journalQueueDepth = 64

// journalQueue writes one model client's turn-journal entries on a
// goroutine of its own, so a dispatch turn hands over an entry and
// goes straight on to the provider call it is about to make. Each
// write opens a store transaction and takes the actor's authority
// locks, and the transaction that admits a turn also runs the actor's
// retention pass.
//
// The client's dispatch goroutine is the only producer and the run
// goroutine is the only consumer, so entries reach the store in the
// order the turn produced them. Each entry also carries the
// producer's sequence number, which is what a reader orders by.
type journalQueue struct {
	// writeContext bounds every write. It outlives a cancelled turn,
	// so the entries describing how a turn ended are still written
	// after the turn itself was abandoned.
	writeContext context.Context

	writes chan journalWrite

	dropped atomic.Int64

	// droppedTurn is the turn the last drop was reported for. The
	// client's dispatch goroutine is the only producer, so this is read
	// and written on one goroutine and needs no lock.
	droppedTurn *journalTurn

	mu      sync.Mutex
	running bool
	closed  bool
	stopped chan struct{}
}

// journalWrite is one queued entry together with the turn it belongs
// to. A write that opens a turn carries the admission call; the rest
// append to the recorder that call produced.
type journalWrite struct {
	turn  *journalTurn
	begin *journalBegin
	entry store.ModelTurnEntry
}

// journalBegin is the admission call for a turn, held until the
// consumer reaches it.
type journalBegin struct {
	journal TurnJournal
	guard   protocol.WindowGuard
	turn    store.ModelTurn
}

// journalTurn is the consumer's state for one turn. Only the consumer
// reads or writes it, so the producer can hold the pointer while the
// admission it describes is still queued.
type journalTurn struct {
	recorder store.ModelTurnRecorder
	closed   bool
}

func newJournalQueue(writeContext context.Context) *journalQueue {
	return &journalQueue{
		writeContext: writeContext,
		writes:       make(chan journalWrite, journalQueueDepth),
		stopped:      make(chan struct{}),
	}
}

// enqueue hands one write to the consumer, starting it on the first
// write the client produces. A client that never journals a turn
// therefore never runs the goroutine.
//
// A full queue drops the entry. Each write opens a store transaction
// that takes the actor's authority locks, so a consumer that has fallen
// behind can be slow for as long as its timeout allows, and waiting for
// a slot would put that wait on the dispatch goroutine. The journal is
// the operator's record of a turn and no part of the turn depends on it,
// so a hole in the record is the cheaper loss. Entries carry their
// producer's sequence number, so a reader sees the hole rather than a
// reordering.
func (q *journalQueue) enqueue(write journalWrite) {
	q.start()

	select {
	case q.writes <- write:
	default:
		q.dropped.Add(1)
		q.logDropped(write)
	}
}

// logDropped reports a dropped entry once per turn. A turn whose journal
// is behind drops many entries in a row, and the first says everything
// the rest would.
func (q *journalQueue) logDropped(write journalWrite) {
	if write.turn == nil || q.droppedTurn == write.turn {
		return
	}
	q.droppedTurn = write.turn

	slog.Default().With("component", "modelclient").WarnContext(
		q.writeContext, "model turn journal is behind, dropping entries",
		"kind", string(write.entry.Kind),
		"seq", write.entry.Seq,
		"queue_depth", journalQueueDepth,
	)
}

// Dropped reports how many entries this client's journal has lost to a
// full queue. It is what tells an operator that a turn's record has a
// hole in it.
func (q *journalQueue) Dropped() int64 {
	return q.dropped.Load()
}

func (q *journalQueue) start() {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.running {
		return
	}

	q.running = true
	go q.run()
}

func (q *journalQueue) run() {
	defer close(q.stopped)

	for write := range q.writes {
		q.perform(write)
	}
}

// stop closes the queue and waits for the entries it still holds.
// The client's dispatch goroutine is the only producer, so a caller
// must join that goroutine first: an entry handed over after this
// sends on a closed channel.
func (q *journalQueue) stop() {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.writes)
		if !q.running {
			close(q.stopped)
		}
	}
	q.mu.Unlock()

	<-q.stopped
}

func (q *journalQueue) perform(write journalWrite) {
	ctx, cancel := context.WithTimeout(q.writeContext, turnJournalWriteTimeout)
	defer cancel()

	if write.begin != nil {
		q.admit(ctx, write)

		return
	}
	if write.turn.closed || write.turn.recorder == nil {
		return
	}

	err := write.turn.recorder.AppendModelTurnEntry(ctx, write.entry)
	if errors.Is(err, store.ErrModelTurnClosed) {
		// The actor lost the window this turn runs in. Every later
		// entry would be refused the same way, so the turn's queue
		// ends here.
		write.turn.closed = true
		q.log(ctx, "model turn closed before its journal finished", write.entry, err)

		return
	}
	if err != nil {
		q.log(ctx, "append model turn entry", write.entry, err)
	}
}

func (q *journalQueue) admit(ctx context.Context, write journalWrite) {
	recorder, err := write.begin.journal.BeginModelTurn(
		ctx, write.begin.guard, write.begin.turn, write.entry,
	)
	if err != nil {
		write.turn.closed = true
		q.log(ctx, "begin model turn journal", write.entry, err)

		return
	}

	write.turn.recorder = recorder
}

func (q *journalQueue) log(
	ctx context.Context,
	message string,
	entry store.ModelTurnEntry,
	err error,
) {
	slog.Default().With("component", "modelclient").WarnContext(ctx, message,
		"kind", string(entry.Kind),
		"seq", entry.Seq,
		"error", err,
	)
}
