package modelclient

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"strings"
	"time"

	"github.com/laney/modeloff/internal/protocol"
)

// Randomiser supplies the random component of typing-delay jitter as
// a value in [0.0, 1.0); the error reports a failure of the
// underlying entropy source.
type Randomiser interface {
	Float64() (float64, error)
}

// NewRandRandomiser returns a [Randomiser] backed by crypto/rand.
func NewRandRandomiser() Randomiser {
	return randRandomiser{}
}

type randRandomiser struct{}

// Float64 draws a uniform value in [0.0, 1.0) from crypto/rand,
// taking 53 bits — a float64's mantissa width — so every
// representable value in the range is reachable.
func (randRandomiser) Float64() (float64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}

	return float64(binary.BigEndian.Uint64(b[:])>>11) / (1 << 53), nil
}

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
// when ctx is cancelled, and surfaces a failure from the jitter
// source.
func (p *Pacer) Wait(ctx context.Context, body string) error {
	if p == nil {
		return nil
	}

	d, err := p.duration(body)
	if err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
	case <-t.C:
	}

	return nil
}

func (p *Pacer) duration(body string) (time.Duration, error) {
	d := p.Floor

	if p.CPS > 0 {
		d += time.Duration(float64(len(body)) / p.CPS * float64(time.Second))
	}

	if p.Jitter > 0 && p.Rng != nil {
		f, err := p.Rng.Float64()
		if err != nil {
			return 0, err
		}
		d += time.Duration((f*2 - 1) * float64(p.Jitter))
	}

	return d, nil
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
