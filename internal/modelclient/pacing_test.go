package modelclient

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/protocol"
)

// fixedRandomiser returns a pre-set sequence of [0,1) values,
// cycling once exhausted. The deterministic stream lets pacing
// tests pin the exact elapsed-time assertion the synctest bubble
// records.
type fixedRandomiser struct {
	values []float64
	idx    int
}

func (f *fixedRandomiser) Float64() float64 {
	v := f.values[f.idx%len(f.values)]
	f.idx++
	return v
}

func TestPacer_duration(t *testing.T) {
	cases := []struct {
		name   string
		floor  time.Duration
		cps    float64
		jitter time.Duration
		rng    []float64
		body   string
		want   time.Duration
	}{
		{
			name:  "floor only",
			floor: 250 * time.Millisecond,
			body:  "",
			want:  250 * time.Millisecond,
		},
		{
			name:  "floor plus per-char",
			floor: 250 * time.Millisecond,
			cps:   40,
			body:  "hello world", // 11 chars → 275ms
			want:  525 * time.Millisecond,
		},
		{
			name:   "zero jitter from 0.5",
			floor:  250 * time.Millisecond,
			cps:    40,
			jitter: 200 * time.Millisecond,
			rng:    []float64{0.5},
			body:   "hello", // 5 chars → 125ms
			want:   375 * time.Millisecond,
		},
		{
			name:   "negative jitter from 0.0",
			floor:  250 * time.Millisecond,
			cps:    40,
			jitter: 200 * time.Millisecond,
			rng:    []float64{0.0},
			body:   "hi", // 2 chars → 50ms
			want:   100 * time.Millisecond,
		},
		{
			name:   "positive jitter from 0.75",
			floor:  250 * time.Millisecond,
			cps:    40,
			jitter: 200 * time.Millisecond,
			rng:    []float64{0.75},
			body:   "hi", // 2 chars → 50ms
			want:   400 * time.Millisecond,
		},
		{
			name:   "jitter ignored when Rng nil",
			floor:  250 * time.Millisecond,
			jitter: 200 * time.Millisecond,
			body:   "",
			want:   250 * time.Millisecond,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Pacer{Floor: tc.floor, CPS: tc.cps, Jitter: tc.jitter}
			if tc.rng != nil {
				p.Rng = &fixedRandomiser{values: tc.rng}
			}

			require.Equal(t, tc.want, p.duration(tc.body))
		})
	}
}

func TestPacer_Wait_advancesVirtualClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &Pacer{
			Floor:  250 * time.Millisecond,
			CPS:    40,
			Jitter: 200 * time.Millisecond,
			Rng:    &fixedRandomiser{values: []float64{0.5}},
		}

		start := time.Now()
		p.Wait(t.Context(), "hello") // 250ms + 125ms + 0ms jitter
		require.Equal(t, 375*time.Millisecond, time.Since(start))
	})
}

func TestPacer_Wait_consecutiveCallsAccumulate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &Pacer{
			Floor:  100 * time.Millisecond,
			CPS:    50,
			Jitter: 0,
		}

		start := time.Now()
		p.Wait(t.Context(), "abcde")      // 100ms + 100ms = 200ms
		p.Wait(t.Context(), "abcdefghij") // 100ms + 200ms = 300ms
		require.Equal(t, 500*time.Millisecond, time.Since(start))
	})
}

func TestPacer_Wait_cancelledContextAborts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &Pacer{Floor: 1 * time.Second}

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		start := time.Now()
		p.Wait(ctx, "")
		require.Equal(t, time.Duration(0), time.Since(start))
	})
}

func TestPacer_Wait_nilReceiverIsNoOp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var p *Pacer

		start := time.Now()
		p.Wait(t.Context(), "anything")
		require.Equal(t, time.Duration(0), time.Since(start))
	})
}

func TestPacingBody(t *testing.T) {
	mustArgs := func(t *testing.T, v any) json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		return raw
	}

	cases := []struct {
		name  string
		tool  string
		args  json.RawMessage
		want  string
		paced bool
	}{
		{
			name:  "msg with body",
			tool:  "msg",
			args:  mustArgs(t, map[string]any{"target": "#room", "body": []string{"hello", "world"}}),
			want:  "hello world",
			paced: true,
		},
		{
			name:  "me with action",
			tool:  "me",
			args:  mustArgs(t, map[string]any{"action": []string{"waves", "slowly"}}),
			want:  "waves slowly",
			paced: true,
		},
		{
			name: "msg with spans",
			tool: "msg",
			args: mustArgs(t, map[string]any{
				"target": "#room",
				"spans": []protocol.ReplySpan{
					{Text: "hi "},
					{Text: "there"},
				},
			}),
			want:  "hi there",
			paced: true,
		},
		{
			name:  "msg with empty body and no spans",
			tool:  "msg",
			args:  mustArgs(t, map[string]any{"target": "#room"}),
			want:  "",
			paced: true,
		},
		{
			name:  "non-chat tool is not paced",
			tool:  "write_memory",
			args:  mustArgs(t, map[string]any{"content": "remember this"}),
			want:  "",
			paced: false,
		},
		{
			name:  "pass is not paced",
			tool:  "pass",
			args:  mustArgs(t, map[string]any{"reason": "nothing to add"}),
			want:  "",
			paced: false,
		},
		{
			name:  "malformed args still paces with empty body",
			tool:  "msg",
			args:  json.RawMessage(`{not json`),
			want:  "",
			paced: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, paced := pacingBody(tc.tool, tc.args)
			require.Equal(t, tc.paced, paced)
			require.Equal(t, tc.want, body)
		})
	}
}
