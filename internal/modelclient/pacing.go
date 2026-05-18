package modelclient

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/laney/modeloff/internal/protocol"
)

// Randomiser supplies the random component of typing-delay jitter
// as a value in [0.0, 1.0). Production wires a math/rand/v2-backed
// implementation; tests pass a deterministic stub so assertions can
// pin the exact wait duration.
type Randomiser interface {
	Float64() float64
}

// NewRandRandomiser returns a [Randomiser] backed by math/rand/v2.
func NewRandRandomiser() Randomiser {
	return randRandomiser{}
}

type randRandomiser struct{}

func (randRandomiser) Float64() float64 { return rand.Float64() }

// Pacer adds a length-scaled typing delay before each model-emitted
// chat line so bots don't appear to fire at machine speed. The wait
// is `Floor + len(body)/CPS · 1s + j`, where `j` is uniform in
// `[-Jitter, +Jitter]` when `Rng` is set. A nil receiver waits zero —
// the synchronous [Dispatcher] path uses that to opt out.
type Pacer struct {
	Floor  time.Duration
	CPS    float64
	Jitter time.Duration
	Rng    Randomiser
}

// Wait blocks for the typing delay implied by body. Returns early
// when ctx is cancelled.
func (p *Pacer) Wait(ctx context.Context, body string) {
	if p == nil {
		return
	}

	d := p.duration(body)
	if d <= 0 {
		return
	}

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (p *Pacer) duration(body string) time.Duration {
	d := p.Floor

	if p.CPS > 0 {
		d += time.Duration(float64(len(body)) / p.CPS * float64(time.Second))
	}

	if p.Jitter > 0 && p.Rng != nil {
		f := p.Rng.Float64()*2 - 1
		d += time.Duration(f * float64(p.Jitter))
	}

	return d
}

// pacingBody returns the textual content of a chat tool call and
// reports whether the call should be paced at all. The two chat
// tools (`msg`, `me`) carry plain text in `body`/`action` or styled
// runs in `spans`; non-chat tools are emitted at machine speed.
func pacingBody(name string, args json.RawMessage) (string, bool) {
	switch name {
	case "msg", "me":
	default:
		return "", false
	}

	var parsed struct {
		Body   []string             `json:"body"`
		Action []string             `json:"action"`
		Spans  []protocol.ReplySpan `json:"spans"`
	}

	_ = json.Unmarshal(args, &parsed)

	parts := parsed.Body
	if len(parts) == 0 {
		parts = parsed.Action
	}

	if joined := strings.TrimSpace(strings.Join(parts, " ")); joined != "" {
		return joined, true
	}

	var buf strings.Builder
	for _, sp := range parsed.Spans {
		buf.WriteString(sp.Text)
	}

	return buf.String(), true
}
