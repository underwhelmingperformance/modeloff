package components_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/components"
)

// scrollbackEvent builds a distinguishable event for the scrollback
// tests. Only the body matters, so the assertions can name an event by
// the number it was built with.
func scrollbackEvent(i int) domain.Event {
	return domain.Message{
		Target: "#general",
		From:   "alice",
		Body:   fmt.Sprintf("message %d", i),
	}
}

// scrollbackBodies reads the bodies out of a scrollback so a test can
// say which events it expects to still be there.
func scrollbackBodies(s *components.Scrollback) []string {
	var bodies []string

	for _, event := range s.Events() {
		bodies = append(bodies, event.(domain.Message).Body)
	}

	return bodies
}

// scrollbackShape is what a test asserts a scrollback's numbering
// looks like. Every operation moves some of it, and the render cache
// and the reader's mark both read all three.
//
// The two numbers are how far the scrollback has moved from where it
// started, not the numbers themselves: each scrollback is handed a
// different starting point so that no two of them ever number an
// event alike.
type scrollbackShape struct {
	Bodies   []string
	FirstSeq int64
	NextSeq  int64
}

func shapeOf(s *components.Scrollback, base int64) scrollbackShape {
	return scrollbackShape{
		Bodies:   scrollbackBodies(s),
		FirstSeq: s.FirstSeq() - base,
		NextSeq:  s.NextSeq() - base,
	}
}

func TestScrollback_numbers_events_as_they_are_added_and_dropped(t *testing.T) {
	tests := map[string]struct {
		build func(s *components.Scrollback)
		want  scrollbackShape
	}{
		"an empty scrollback has moved nowhere": {
			build: func(*components.Scrollback) {},
			want:  scrollbackShape{FirstSeq: 0, NextSeq: 0},
		},
		"appending advances the next number only": {
			build: func(s *components.Scrollback) {
				s.Append(scrollbackEvent(0))
				s.Append(scrollbackEvent(1))
			},
			want: scrollbackShape{
				Bodies:  []string{"message 0", "message 1"},
				NextSeq: 2,
			},
		},
		"clearing keeps the numbering going": {
			build: func(s *components.Scrollback) {
				s.Append(scrollbackEvent(0))
				s.Append(scrollbackEvent(1))
				s.Clear()
			},
			want: scrollbackShape{FirstSeq: 2, NextSeq: 2},
		},
		"appending after a clear starts from unused numbers": {
			build: func(s *components.Scrollback) {
				s.Append(scrollbackEvent(0))
				s.Clear()
				s.Append(scrollbackEvent(1))
			},
			want: scrollbackShape{
				Bodies:   []string{"message 1"},
				FirstSeq: 1,
				NextSeq:  2,
			},
		},
		"prepending puts the held lines in front and renumbers everything": {
			build: func(s *components.Scrollback) {
				s.Append(scrollbackEvent(0))
				s.Append(scrollbackEvent(1))
				s.Prepend([]domain.Event{scrollbackEvent(8), scrollbackEvent(9)})
			},
			want: scrollbackShape{
				Bodies:   []string{"message 8", "message 9", "message 0", "message 1"},
				FirstSeq: 2,
				NextSeq:  6,
			},
		},
		"prepending nothing leaves the scrollback alone": {
			build: func(s *components.Scrollback) {
				s.Append(scrollbackEvent(0))
				s.Prepend(nil)
			},
			want: scrollbackShape{
				Bodies:  []string{"message 0"},
				NextSeq: 1,
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := components.NewScrollback()
			base := s.FirstSeq()
			tc.build(s)

			require.Equal(t, tc.want, shapeOf(s, base))
		})
	}
}

// TestScrollback_numbers_from_a_point_no_other_scrollback_reaches
// pins what keeps a rendered line matched to the event it was
// rendered from. A window parted and rejoined builds a second
// scrollback under the same name, and a cache entry recorded against
// the first would answer for the second if both numbered from the
// same place.
func TestScrollback_numbers_from_a_point_no_other_scrollback_reaches(t *testing.T) {
	parted := components.NewScrollback()
	parted.Append(scrollbackEvent(0))

	rejoined := components.NewScrollback()
	rejoined.Append(scrollbackEvent(1))

	require.NotEqual(t, parted.FirstSeq(), rejoined.FirstSeq(),
		"two scrollbacks must not give the same number to different events")
}

// boundShape describes a scrollback too large to write out event by
// event: how many it holds, which event is at each end, and how far
// its numbering has moved from where it started.
type boundShape struct {
	Held     int
	Oldest   string
	Newest   string
	FirstSeq int64
	NextSeq  int64
}

func boundShapeOf(s *components.Scrollback, base int64) boundShape {
	bodies := scrollbackBodies(s)

	return boundShape{
		Held:     len(bodies),
		Oldest:   bodies[0],
		Newest:   bodies[len(bodies)-1],
		FirstSeq: s.FirstSeq() - base,
		NextSeq:  s.NextSeq() - base,
	}
}

// TestScrollback_bounds_a_window_at_the_limit pins the bound and what
// it does to the numbering. Without it a window the user leaves open
// all day keeps every event the session puts in it, and only `/clear`
// gives any of them back.
func TestScrollback_bounds_a_window_at_the_limit(t *testing.T) {
	const limit = components.ScrollbackLimit

	tests := map[string]struct {
		build func(s *components.Scrollback)
		want  boundShape
	}{
		"appending past the limit drops the oldest events": {
			build: func(s *components.Scrollback) {
				for i := range limit + 3 {
					s.Append(scrollbackEvent(i))
				}
			},
			want: boundShape{
				Held:     limit,
				Oldest:   "message 3",
				Newest:   fmt.Sprintf("message %d", limit+2),
				FirstSeq: 3,
				NextSeq:  limit + 3,
			},
		},
		"prepending onto a full scrollback drops the held lines first": {
			build: func(s *components.Scrollback) {
				for i := range limit {
					s.Append(scrollbackEvent(i))
				}

				s.Prepend([]domain.Event{scrollbackEvent(-2), scrollbackEvent(-1)})
			},
			want: boundShape{
				Held: limit,
				// The held lines are older than everything already
				// there, so they are what the limit takes.
				Oldest:   "message 0",
				Newest:   fmt.Sprintf("message %d", limit-1),
				FirstSeq: limit + 2,
				NextSeq:  2*limit + 2,
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := components.NewScrollback()
			base := s.FirstSeq()
			tc.build(s)

			require.Equal(t, tc.want, boundShapeOf(s, base))
		})
	}
}
