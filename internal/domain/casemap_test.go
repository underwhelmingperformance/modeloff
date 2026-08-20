package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

func TestEqualNick(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		a, b  domain.Nick
		equal bool
	}{
		{name: "identical", a: "botty", b: "botty", equal: true},
		{name: "ascii case differs", a: "Botty", b: "bOTTY", equal: true},
		{name: "different nicks", a: "botty", b: "zed", equal: false},
		{name: "brackets are not folded onto braces", a: "foo[1]", b: "foo{1}", equal: false},
		{name: "backslash is not folded onto pipe", a: `a\b`, b: "a|b", equal: false},
		{name: "non-ascii is left alone", a: "Éclair", b: "éclair", equal: false},
		{name: "empty nicks", a: "", b: "", equal: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.equal, domain.EqualNick(tt.a, tt.b))
			require.Equal(t, tt.equal, domain.EqualNick(tt.b, tt.a))
		})
	}
}

func TestKeyForChannel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   domain.ChannelName
		want domain.ChannelKey
	}{
		{name: "already lower case", in: "#dev", want: "#dev"},
		{name: "mixed case folds", in: "#Dev", want: "#dev"},
		{name: "all upper case folds", in: "#DEV", want: "#dev"},
		{name: "local prefix keeps its prefix", in: "&Local", want: "&local"},
		{name: "non-ascii is left alone", in: "#Café", want: "#café"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, domain.KeyForChannel(tt.in))
		})
	}
}

// TestInstance_channel_set_is_casemapped covers the one map whose
// correctness depends on the casemapping. An instance records the
// spelling a channel goes by and answers about it under any
// spelling, so a caller reaching [domain.Instance.InChannel] with a
// client-supplied name cannot get a silent false that looks like a
// membership bug.
func TestInstance_channel_set_is_casemapped(t *testing.T) {
	t.Parallel()

	joinedAt := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)

	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	inst.JoinChannel("#Mixed", joinedAt)

	for _, asked := range []domain.ChannelName{"#Mixed", "#mixed", "#MIXED"} {
		require.True(t, inst.InChannel(asked), "asked as %q", asked)
	}

	// The spelling the channel goes by survives, and a repeat join
	// under another spelling does not overwrite it.
	inst.JoinChannel("#mixed", joinedAt.Add(time.Hour))

	channels := inst.Channels()
	require.Equal(t, 1, channels.Len())

	recorded, ok := channels.Get("#Mixed")
	require.True(t, ok)
	require.Equal(t, joinedAt, recorded)

	inst.LeaveChannels("#MIXED")
	require.False(t, inst.InChannel("#Mixed"))
	require.Equal(t, 0, inst.Channels().Len())
}

func TestEqualChannel(t *testing.T) {
	t.Parallel()

	require.True(t, domain.EqualChannel("#Dev", "#dev"))
	require.False(t, domain.EqualChannel("#dev", "&dev"))
	require.False(t, domain.EqualChannel("#dev", "#devs"))
}
