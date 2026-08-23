package modelclient

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

// blockedTurnJournal parks the queue's consumer inside its first write.
type blockedTurnJournal struct {
	release chan struct{}
}

func (j blockedTurnJournal) BeginModelTurn(
	context.Context,
	protocol.WindowGuard,
	store.ModelTurn,
	store.ModelTurnEntry,
) (store.ModelTurnRecorder, error) {
	<-j.release

	return nil, context.Canceled
}

// journalOverflowEffect is what the producer saw after handing over more
// entries than the queue holds.
type journalOverflowEffect struct {
	Returned bool
	Dropped  int64
}

// TestJournalQueue_enqueue_drops_rather_than_waiting pins that a journal
// whose consumer has fallen behind costs the turn nothing.
//
// Each write opens a store transaction and takes the actor's authority
// locks, so a slow consumer stays slow for as long as its timeout allows.
// Waiting for a slot would put that wait on the dispatch goroutine, which
// is the turn itself. The journal is the operator's record and no part of
// the turn depends on it, so the entry is what gives way.
func TestJournalQueue_enqueue_drops_rather_than_waiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const overflow = 5

		release := make(chan struct{})
		queue := newJournalQueue(t.Context())
		turn := &journalTurn{}

		// The first write parks the consumer, the next journalQueueDepth
		// fill the buffer, and everything after that has nowhere to go.
		queue.enqueue(journalWrite{
			turn:  turn,
			begin: &journalBegin{journal: blockedTurnJournal{release: release}},
		})
		synctest.Wait()
		for range journalQueueDepth + overflow {
			queue.enqueue(journalWrite{turn: turn})
		}

		require.Equal(t, journalOverflowEffect{
			Returned: true, Dropped: overflow,
		}, journalOverflowEffect{
			Returned: true, Dropped: queue.Dropped(),
		})

		close(release)
		synctest.Wait()
		queue.stop()
	})
}
