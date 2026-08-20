package modelmanager

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

// TestDeterministicNickBase exercises deterministicNickBase directly,
// including the edges named in the audit: a last segment that
// collapses to nothing under sanitisation, two distinct model ids
// that share a last segment, the boundary where truncation to
// deterministicNickBaseLen lands on a character that then needs
// trimming, and a last segment that starts with a digit or an
// RFC 2812 §2.3.1 special, which [domain.ValidateNick] admits
// everywhere in a nick except first.
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
		{
			name:    "a digit-leading segment gets an m prefix",
			modelID: "vendor/007robot",
			want:    "m007robo",
		},
		{
			name:    "an all-digit segment gets an m prefix",
			modelID: "vendor/12345",
			want:    "m12345",
		},
		{
			name:    "a digit-leading segment past the length cap is prefixed after truncation",
			modelID: "vendor/0123456789abcdef",
			want:    "m0123456",
		},
		{
			name:    "a special-leading segment needs no fix",
			modelID: "vendor/_turbo",
			want:    "_turbo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deterministicNickBase(tt.modelID)
			require.Equal(t, tt.want, got)
			require.LessOrEqual(t, len(got), deterministicNickBaseLen)
			require.Equal(t, domain.NickAccepted, domain.ValidateNick(domain.Nick(got)))
		})
	}
}

// TestSanitizeNickBase exercises sanitizeNickBase directly against
// the shapes deterministicNickBase's own character substitution
// cannot produce on its own, such as the reserved
// [domain.AnonymousNick] (deterministicNickBaseLen keeps a
// substituted base too short to reach it) and a leading RFC 2812
// §2.3.1 special other than `_` (substitution replaces every one of
// those with `-`, which trimming then removes). Both are exercised
// here so the guarantee holds regardless of how those constants are
// tuned later.
func TestSanitizeNickBase(t *testing.T) {
	tests := []struct {
		name string
		base string
		want string
	}{
		{
			name: "empty string becomes the literal model",
			base: "",
			want: "model",
		},
		{
			name: "digit-leading gets an m prefix",
			base: "7bot",
			want: "m7bot",
		},
		{
			name: "a digit-leading base at the length cap is trimmed after the prefix",
			base: "01234567",
			want: "m0123456",
		},
		{
			name: "the reserved AnonymousNick is replaced outright",
			base: "anonymous",
			want: "model",
		},
		{
			name: "a non-underscore special leading character needs no fix",
			base: "[bracket",
			want: "[bracket",
		},
		{
			name: "an already-valid base passes through unchanged",
			base: "gpt-5-4",
			want: "gpt-5-4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeNickBase(tt.base)
			require.Equal(t, tt.want, got)
			require.Equal(t, domain.NickAccepted, domain.ValidateNick(domain.Nick(got)))
		})
	}
}
