package observability

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLogBuffer_keeps_latest_entries_within_capacity(t *testing.T) {
	buffer := NewLogBuffer(2)
	t.Cleanup(buffer.Close)

	now := time.Now()
	buffer.Ingest() <- PanelEntry{Message: "first", Timestamp: now}
	buffer.Ingest() <- PanelEntry{Message: "second", Timestamp: now}
	buffer.Ingest() <- PanelEntry{Message: "third", Timestamp: now}

	require.Eventually(t, func() bool {
		entries := buffer.Entries()
		return len(entries) == 2 && entries[0].Message == "second" && entries[1].Message == "third"
	}, time.Second, 10*time.Millisecond)
}

func TestLogBuffer_emits_update_notifications(t *testing.T) {
	buffer := NewLogBuffer(1)
	t.Cleanup(buffer.Close)

	buffer.Ingest() <- PanelEntry{Message: "entry", Timestamp: time.Now()}

	select {
	case <-buffer.Updates():
	case <-time.After(time.Second):
		t.Fatal("expected log buffer update notification")
	}
}
