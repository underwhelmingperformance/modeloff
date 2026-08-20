package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

// TestSQLiteStore_ResolveNick_is_case_insensitive covers the store
// half of the server's casemapping (RFC 2812 §2.2): a nick lookup
// matches under NOCASE and the row keeps the spelling it was saved
// with.
func TestSQLiteStore_ResolveNick_is_case_insensitive(t *testing.T) {
	tests := []struct {
		name  string
		saved domain.Nick
		asked domain.Nick
	}{
		{name: "saved lower, asked upper", saved: "botty", asked: "BOTTY"},
		{name: "saved upper, asked lower", saved: "BOTTY", asked: "botty"},
		{name: "saved mixed, asked mixed", saved: "BoTTy", asked: "bOtty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			s := newTestStore(t)

			inst := domain.NewModelInstance("inst-1", tt.saved, "test/model", "", nil)
			require.NoError(t, s.SaveInstance(ctx, inst))

			got, err := s.ResolveNick(ctx, tt.asked)
			require.NoError(t, err)
			require.Same(t, inst, got)
			require.Equal(t, tt.saved, got.Nick())
		})
	}
}

// TestSQLiteStore_GetWindow_is_case_insensitive covers the same rule
// for channel names, and pins that the returned window carries the
// spelling the channel was created with.
func TestSQLiteStore_GetWindow_is_case_insensitive(t *testing.T) {
	tests := []struct {
		name  string
		saved domain.ChannelName
		asked domain.ChannelName
	}{
		{name: "saved lower, asked upper", saved: "#dev", asked: "#DEV"},
		{name: "saved mixed, asked lower", saved: "#Dev", asked: "#dev"},
		{name: "local prefix", saved: "&Ops", asked: "&ops"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			s := newTestStore(t)

			created := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
			require.NoError(t, s.SaveWindow(ctx, domain.NewChannelWindow(tt.saved, created)))

			got, err := s.GetWindow(ctx, tt.asked)
			require.NoError(t, err)
			require.Equal(t, tt.saved, got.Name())
		})
	}
}

// TestSQLiteStore_DeleteWindow_removes_the_whole_equivalence_class
// covers what happens at the end of a channel's life on a database
// written before the casemapping existed. `GetWindow` answers such a
// pair with one row, so the other is a shadow nothing can reach.
// Deleting only the row it was handed would leave the shadow as the
// answer to the next lookup, and the next client to create a channel
// under that name would find it furnished with a topic and modes
// from a channel nobody was in, against the rule that a re-created
// channel starts fresh (RFC 2811 §2).
func TestSQLiteStore_DeleteWindow_removes_the_whole_equivalence_class(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	created := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	for _, name := range []domain.ChannelName{"#Dev", "#dev"} {
		w := domain.NewChannelWindow(name, created)
		w.Topic = "topic of " + string(name)
		require.NoError(t, s.SaveWindow(ctx, w))
	}

	answered, err := s.GetWindow(ctx, "#dev")
	require.NoError(t, err)

	require.NoError(t, s.DeleteWindow(ctx, answered.Name()))

	for _, asked := range []domain.ChannelName{"#Dev", "#dev", "#DEV"} {
		_, err := s.GetWindow(ctx, asked)
		require.ErrorIs(t, err, ErrNoSuchChannel,
			"no spelling of a destroyed channel may still answer")
	}
}

// TestSQLiteStore_GetWindow_settles_a_pre_casemapping_pair covers a
// database written before the casemapping existed, which may hold
// `#Dev` and `#dev` as separate rows. Every lookup answers with the
// same one of the pair, so the channel behaves as one channel rather
// than flipping between two.
func TestSQLiteStore_GetWindow_settles_a_pre_casemapping_pair(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	created := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	require.NoError(t, s.SaveWindow(ctx, domain.NewChannelWindow("#Dev", created)))
	require.NoError(t, s.SaveWindow(ctx, domain.NewChannelWindow("#dev", created)))

	first, err := s.GetWindow(ctx, "#DEV")
	require.NoError(t, err)

	for range 3 {
		again, err := s.GetWindow(ctx, "#dEv")
		require.NoError(t, err)
		require.Equal(t, first.Name(), again.Name())
	}
}
