package apitest_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
)

// TestFake_GenerateNick_default covers the one default answer worth
// pinning directly: a Fake with no GenerateNickFn numbers its
// suggestion by how many the caller has already excluded, so a test
// that drives a nick collision and retries gets a distinct nick on
// each attempt without wiring its own counter.
func TestFake_GenerateNick_default(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		exclude []domain.Nick
		want    domain.Nick
	}{
		{name: "no exclusions", exclude: nil, want: "fakenick"},
		{name: "one exclusion", exclude: []domain.Nick{"fakenick"}, want: "fakenick1"},
		{name: "two exclusions", exclude: []domain.Nick{"fakenick", "fakenick1"}, want: "fakenick2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := &apitest.Fake{}

			result, err := f.GenerateNick(t.Context(), "test/model", "", tt.exclude)
			require.NoError(t, err)
			require.Equal(t, api.NicknameResult{Nick: tt.want}, result)
		})
	}
}

// TestReasonAware covers both surfaces a ReasonAware exposes: the
// plain GenerateNick path it inherits from the embedded Fake, unused
// unless a caller reaches for it, and GenerateNickWithReasons, which
// routes through GenerateNickWithReasonsFn and is what makes
// ReasonAware satisfy [api.NickReasonGenerator].
func TestReasonAware(t *testing.T) {
	t.Parallel()

	var seen []api.RejectedNick

	f := &apitest.ReasonAware{
		GenerateNickWithReasonsFn: func(_ context.Context, _ domain.ModelID, _ string, excluded []api.RejectedNick) (domain.Nick, error) {
			seen = excluded
			return "goodnick", nil
		},
	}

	var client api.NickReasonGenerator = f

	excluded := []api.RejectedNick{{Nick: "taken", Reason: "is already taken"}}

	result, err := client.GenerateNickWithReasons(t.Context(), "test/model", "", excluded)
	require.NoError(t, err)
	require.Equal(t, api.NicknameResult{Nick: "goodnick"}, result)
	require.Equal(t, excluded, seen)

	plain, err := f.GenerateNick(t.Context(), "test/model", "", nil)
	require.NoError(t, err)
	require.Equal(t, api.NicknameResult{Nick: "fakenick"}, plain)
}
