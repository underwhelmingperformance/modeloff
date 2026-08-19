package modelmanager

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

// TestDeterministicNickBase exercises deterministicNickBase directly,
// including the edges named in the audit: a last segment that
// collapses to nothing under sanitisation, two distinct model ids
// that share a last segment, and the boundary where truncation to
// deterministicNickBaseLen lands on a character that then needs
// trimming.
func TestDeterministicNickBase(t *testing.T) {
	tests := []struct {
		name    string
		modelID domain.ModelID
		want    string
	}{
		{
			name:    "well-formed id keeps its last segment",
			modelID: "openai/gpt-5.4-mini",
			want:    "gpt-5-4",
		},
		{
			name:    "all-symbol last segment falls back to the literal model",
			modelID: "vendor/!!!",
			want:    "model",
		},
		{
			name:    "empty last segment falls back to the literal model",
			modelID: "vendor/",
			want:    "model",
		},
		{
			name:    "first shared-last-segment id",
			modelID: "meta/llama-3",
			want:    "llama-3",
		},
		{
			name:    "second shared-last-segment id derives the identical base",
			modelID: "another/llama-3",
			want:    "llama-3",
		},
		{
			name:    "truncation landing on a dash trims it away",
			modelID: "vendor/abcdefg-hij",
			want:    "abcdefg",
		},
		{
			name:    "truncation landing mid-word keeps every character up to the cap",
			modelID: "vendor/abcdefghij",
			want:    "abcdefgh",
		},
		{
			name:    "disallowed characters and the length cap both apply",
			modelID: "vendor/ABC.DEF!GHI@JKL",
			want:    "abc-def",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deterministicNickBase(tt.modelID)
			require.Equal(t, tt.want, got)
			require.LessOrEqual(t, len(got), deterministicNickBaseLen)
		})
	}
}
