package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

type salienceCase struct {
	Name       string
	Confidence domain.Confidence
	SalienceAt time.Time
}

// TestSalienceAt_buys_a_head_start_in_half_lives pins what confidence is
// worth. Salience decays by halves, so a fixed head start in time is the
// same ranking as a fixed multiplier on the score, and it is what lets a
// stored timestamp order experiences at any later moment.
func TestSalienceAt_buys_a_head_start_in_half_lives(t *testing.T) {
	citedAt := time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)
	cases := []salienceCase{
		{
			Name: "low counts from when it was cited", Confidence: domain.ConfidenceLow,
			SalienceAt: citedAt,
		},
		{
			Name: "medium is worth one half-life", Confidence: domain.ConfidenceMedium,
			SalienceAt: citedAt.Add(domain.ExperienceSalienceHalfLife),
		},
		{
			Name: "high is worth two", Confidence: domain.ConfidenceHigh,
			SalienceAt: citedAt.Add(2 * domain.ExperienceSalienceHalfLife),
		},
		{
			Name: "an unrecognised confidence buys nothing", Confidence: "unknown",
			SalienceAt: citedAt,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			require.Equal(t, testCase, salienceCase{
				Name: testCase.Name, Confidence: testCase.Confidence,
				SalienceAt: domain.SalienceAt(testCase.Confidence, citedAt),
			})
		})
	}
}

// TestSalienceAt_ranks_a_recited_experience_over_a_more_confident_one pins
// the property the ordering exists for: an experience the instance went
// back to is more live than one it has left alone, however sure of itself
// the older one was.
func TestSalienceAt_ranks_a_recited_experience_over_a_more_confident_one(t *testing.T) {
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := old.Add(3 * domain.ExperienceSalienceHalfLife)

	require.True(t,
		domain.SalienceAt(domain.ConfidenceLow, recent).
			After(domain.SalienceAt(domain.ConfidenceHigh, old)),
	)
}
