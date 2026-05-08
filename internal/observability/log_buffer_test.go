package observability

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLogBuffer_keeps_latest_entries_within_capacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		buffer := NewLogBuffer(2)
		t.Cleanup(buffer.Close)

		now := time.Now()
		buffer.Ingest() <- PanelEntry{Message: "first", Timestamp: now}
		buffer.Ingest() <- PanelEntry{Message: "second", Timestamp: now}
		buffer.Ingest() <- PanelEntry{Message: "third", Timestamp: now}

		// The forwarder goroutine processes ingested entries. Wait
		// for it to durably block, which means it has consumed every
		// pending message and is waiting on the next one — at which
		// point the ring buffer's contents are deterministic.
		synctest.Wait()

		require.Equal(t, []PanelEntry{
			{Message: "second", Timestamp: now},
			{Message: "third", Timestamp: now},
		}, buffer.Entries())
	})
}

func TestLogBuffer_emits_update_notifications(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		buffer := NewLogBuffer(1)
		t.Cleanup(buffer.Close)

		buffer.Ingest() <- PanelEntry{Message: "entry", Timestamp: time.Now()}
		<-buffer.Updates()
	})
}
