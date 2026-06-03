package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/config"
)

// stubConfigStore is a [config.Store] that returns a fixed config and
// load error, so the poke-schedule adapter can be exercised without a
// real file store.
type stubConfigStore struct {
	cfg config.Config
	err error
}

func (s stubConfigStore) Load(context.Context) (config.Config, error) { return s.cfg, s.err }
func (s stubConfigStore) Save(context.Context, config.Config) error   { return nil }
func (s stubConfigStore) OnChange(config.ChangeFunc) config.UnsubscribeFunc {
	return func() {}
}

func TestPokeScheduleFromConfig(t *testing.T) {
	cases := []struct {
		name         string
		cfg          config.Config
		loadErr      error
		wantInterval time.Duration
		wantEnabled  bool
	}{
		{
			name:         "enabled with key and positive interval",
			cfg:          config.Config{APIKey: "k", PokeInterval: 5 * time.Minute},
			wantInterval: 5 * time.Minute,
			wantEnabled:  true,
		},
		{
			name: "disabled without api key",
			cfg:  config.Config{PokeInterval: 5 * time.Minute},
		},
		{
			name: "disabled with non-positive interval",
			cfg:  config.Config{APIKey: "k"},
		},
		{
			name:    "disabled on load error",
			cfg:     config.Config{APIKey: "k", PokeInterval: 5 * time.Minute},
			loadErr: errors.New("boom"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			schedule := pokeScheduleFromConfig(stubConfigStore{cfg: tc.cfg, err: tc.loadErr})

			interval, enabled := schedule(context.Background())

			require.Equal(t, tc.wantInterval, interval)
			require.Equal(t, tc.wantEnabled, enabled)
		})
	}
}
