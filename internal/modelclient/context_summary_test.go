package modelclient

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

// summarizedPrefixEffect is how much of the transcript and of the burst
// a committed summary already covers.
type summarizedPrefixEffect struct {
	Recent int
	Events int
}

// TestSummarizedPrefix_covers_the_burst_a_summary_reached pins that a
// retry drops the burst events a committed summary took with it.
//
// Compaction reaches into the burst once the transcript alone is not
// enough. A retry reloads the summaries and plans again from the same
// transcript and burst, so matching the transcript alone would leave
// those events in the request beside the summary that replaced them, and
// the next compaction would summarise and commit them a second time.
func TestSummarizedPrefix_covers_the_burst_a_summary_reached(t *testing.T) {
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	line := func(index int) protocol.IRCMessage {
		return protocol.IRCMessage{
			Kind: protocol.KindPrivMsg, Body: fmt.Sprintf("line %d", index),
			At: at.Add(time.Duration(index) * time.Second),
		}
	}
	recent := []protocol.IRCMessage{line(0), line(1)}
	events := []protocol.IRCMessage{line(2), line(3)}

	cases := []struct {
		name    string
		sources []protocol.IRCMessage
		want    summarizedPrefixEffect
	}{
		{
			name:    "nothing summarised",
			sources: nil,
			want:    summarizedPrefixEffect{},
		},
		{
			name:    "the transcript alone",
			sources: []protocol.IRCMessage{line(0), line(1)},
			want:    summarizedPrefixEffect{Recent: 2},
		},
		{
			name:    "the transcript and part of the burst",
			sources: []protocol.IRCMessage{line(0), line(1), line(2)},
			want:    summarizedPrefixEffect{Recent: 2, Events: 1},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var summaries []store.ContextSummary
			if testCase.sources != nil {
				summaries = []store.ContextSummary{{Sources: testCase.sources}}
			}
			got := summarizedPrefix(recent, events, summaries)

			require.Equal(t, testCase.want, summarizedPrefixEffect{
				Recent: got.recent, Events: got.events,
			})
		})
	}
}
