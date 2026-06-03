package command

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeHolder is a test-side [CapabilityHolder] backed by a set
// literal. The tests use synthetic capabilities (`"alpha"`, `"beta"`)
// rather than coupling to any concrete consumer's constants — the
// framework knows nothing about specific capabilities.
type fakeHolder map[Capability]struct{}

func (f fakeHolder) Has(c Capability) bool {
	_, ok := f[c]
	return ok
}

func held(caps ...Capability) fakeHolder {
	set := make(fakeHolder, len(caps))
	for _, c := range caps {
		set[c] = struct{}{}
	}
	return set
}

func TestNoCapabilities_HoldsNothing(t *testing.T) {
	h := NoCapabilities()
	require.False(t, h.Has(Capability("alpha")))
	require.False(t, h.Has(Capability("")))
}

func TestHolds(t *testing.T) {
	tests := []struct {
		name   string
		holder CapabilityHolder
		caps   []Capability
		want   bool
	}{
		{name: "no requirements with nil holder", holder: nil, caps: nil, want: true},
		{name: "no requirements with empty holder", holder: held(), caps: []Capability{}, want: true},
		{name: "no requirements with populated holder", holder: held("alpha"), caps: nil, want: true},
		{name: "single requirement held", holder: held("alpha"), caps: []Capability{"alpha"}, want: true},
		{name: "single requirement missing", holder: held("beta"), caps: []Capability{"alpha"}, want: false},
		{name: "multiple requirements all held", holder: held("alpha", "beta"), caps: []Capability{"alpha", "beta"}, want: true},
		{name: "multiple requirements one missing", holder: held("alpha"), caps: []Capability{"alpha", "beta"}, want: false},
		{name: "nil holder with requirements", holder: nil, caps: []Capability{"alpha"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Holds(tt.holder, tt.caps))
		})
	}
}

func TestParseRequiredCapabilities(t *testing.T) {
	tests := []struct {
		name string
		tag  string
		want []Capability
	}{
		{name: "empty tag", tag: "", want: nil},
		{name: "single capability", tag: "alpha", want: []Capability{"alpha"}},
		{name: "multiple capabilities", tag: "alpha,beta", want: []Capability{"alpha", "beta"}},
		{name: "whitespace tolerance", tag: " alpha , beta ", want: []Capability{"alpha", "beta"}},
		{name: "trailing comma", tag: "alpha,", want: []Capability{"alpha"}},
		{name: "leading comma", tag: ",alpha", want: []Capability{"alpha"}},
		{name: "only commas", tag: ",,,", want: nil},
		{name: "only whitespace", tag: "  ", want: nil},
		{name: "three caps", tag: "alpha,beta,gamma", want: []Capability{"alpha", "beta", "gamma"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, parseRequiredCapabilities(tt.tag))
		})
	}
}
