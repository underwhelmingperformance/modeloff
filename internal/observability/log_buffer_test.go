package observability

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

// TestLogBuffer_keeps_latest_entries_within_capacity pins the ring's
// contents and their order at every point around a wrap. The buffer
// overwrites the oldest entry in place and reads back from a head
// index, so a snapshot taken part way through a lap has to join the
// two halves of the ring the right way round.
func TestLogBuffer_keeps_latest_entries_within_capacity(t *testing.T) {
	tests := map[string]struct {
		ingested []string
		want     []string
	}{
		"under capacity":       {ingested: []string{"a", "b"}, want: []string{"a", "b"}},
		"exactly at capacity":  {ingested: []string{"a", "b", "c"}, want: []string{"a", "b", "c"}},
		"one entry over":       {ingested: []string{"a", "b", "c", "d"}, want: []string{"b", "c", "d"}},
		"part way round again": {ingested: []string{"a", "b", "c", "d", "e"}, want: []string{"c", "d", "e"}},
		"a whole lap on":       {ingested: []string{"a", "b", "c", "d", "e", "f"}, want: []string{"d", "e", "f"}},
		"several laps on": {
			ingested: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"},
			want:     []string{"h", "i", "j"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				buffer := NewLogBuffer(3)
				t.Cleanup(buffer.Close)

				now := time.Now()

				for _, message := range tc.ingested {
					buffer.Ingest() <- PanelEntry{Message: message, Timestamp: now}
				}

				// The forwarder goroutine processes ingested entries.
				// Wait for it to durably block: it has then consumed
				// every pending message and is waiting on the next
				// one, so the buffer's contents are settled.
				synctest.Wait()

				want := make([]PanelEntry, 0, len(tc.want))
				for _, message := range tc.want {
					want = append(want, PanelEntry{Message: message, Timestamp: now})
				}

				require.Equal(t, want, buffer.Entries())
			})
		})
	}
}

func TestLogBuffer_emits_update_notifications(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		buffer := NewLogBuffer(1)
		t.Cleanup(buffer.Close)

		buffer.Ingest() <- PanelEntry{Message: "entry", Timestamp: time.Now()}
		<-buffer.Updates()
	})
}
