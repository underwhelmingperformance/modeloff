package modelclient

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"time"
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
// `[-Jitter, +Jitter]` when `Rng` is set. A nil receiver waits zero,
// which is how a caller opts out of pacing entirely.
type Pacer struct {
	Floor  time.Duration
	CPS    float64
	Jitter time.Duration
	Rng    Randomiser
}

// Wait blocks for the typing delay implied by body, and surfaces a
// failure from the jitter source. A cancelled ctx returns
// `ctx.Err()`: the wait was interrupted because the turn is over, so
// the caller must abandon the emit it was pacing. The context it
// holds is spent by then, and the emit would have nowhere to land.
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
		return ctx.Err()
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
