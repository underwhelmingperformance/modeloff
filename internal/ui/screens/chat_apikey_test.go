package screens

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
)

// apiKeyTestConfigStore is a minimal config.Store whose Save invokes
// registered OnChange callbacks synchronously, the same contract
// config.FileStore honours (see FileStore.Save). It exists to drive
// NewChatScreen's OnChange registration directly, without a real file
// on disk.
type apiKeyTestConfigStore struct {
	cfg          config.Config
	callbacks    []config.ChangeFunc
	unsubscribed bool
}

func (s *apiKeyTestConfigStore) Load(context.Context) (config.Config, error) {
	return s.cfg, nil
}

func (s *apiKeyTestConfigStore) OnChange(fn config.ChangeFunc) config.UnsubscribeFunc {
	s.callbacks = append(s.callbacks, fn)

	return func() { s.unsubscribed = true }
}

func (s *apiKeyTestConfigStore) Save(ctx context.Context, cfg config.Config) error {
	prev := s.cfg
	s.cfg = cfg

	for _, cb := range s.callbacks {
		cb(ctx, prev, cfg)
	}

	return nil
}

func (s *apiKeyTestConfigStore) Update(ctx context.Context, fn func(config.Config) config.Config) (config.Config, error) {
	next := fn(s.cfg)
	if err := s.Save(ctx, next); err != nil {
		return config.Config{}, err
	}

	return next, nil
}

// TestChatScreen_apiKeyChanges_holds_the_latest_value_across_rapid_opposite_writes
// pins the drain-then-send fix: apiKeyChanges has capacity 1, and the
// OnChange callback can run on any goroutine that called Save or
// Update, so two writes can land before listenForAPIKeyChanges next
// drains the channel. A plain non-blocking send on a full channel
// keeps the first value and drops the second; that would leave the
// buffer holding "key configured" after a set-then-clear sequence,
// wrong until some later, unrelated write happened to correct it.
// sendLatest must instead leave the channel holding the write that
// actually happened last.
func TestChatScreen_apiKeyChanges_holds_the_latest_value_across_rapid_opposite_writes(t *testing.T) {
	cfgStore := &apiKeyTestConfigStore{}
	sess, mgr, user := newTestSession(t)

	screen, err := NewChatScreen(t.Context, sess, mgr, user, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)
	require.True(t, screen.apiKeyMissing, "no key configured at construction")

	// Two rapid, synchronous, opposite-direction writes, neither
	// drained by listenForAPIKeyChanges before the next lands.
	require.NoError(t, cfgStore.Save(t.Context(), config.Config{APIKey: "test-key"}))
	require.NoError(t, cfgStore.Save(t.Context(), config.Config{APIKey: ""}))

	cmd := screen.listenForAPIKeyChanges()
	require.NotNil(t, cmd)

	msg := cmd()
	updated, ok := msg.(apiKeyMissingMsg)
	require.True(t, ok, "want apiKeyMissingMsg, got %T", msg)
	require.True(t, updated.missing,
		"the final write cleared the key; the buffer must hold that state, not the stale value the first write left")
}

// TestChatScreen_listenForAPIKeyChanges_stops_and_unsubscribes_when_context_done
// pins the teardown: listenForAPIKeyChanges' channel has no
// subscription for a session to close, unlike listenForProtocolEvents,
// so the returned Cmd instead ends on baseContext's cancellation, and
// unsubscribes from cfgStore.OnChange in the same step. Without this,
// the goroutine the Bubble Tea runtime runs the returned Cmd on blocks
// forever once nothing sends on apiKeyChanges again, and cfgStore
// keeps calling a listener for a screen that is gone.
func TestChatScreen_listenForAPIKeyChanges_stops_and_unsubscribes_when_context_done(t *testing.T) {
	cfgStore := &apiKeyTestConfigStore{}
	sess, mgr, user := newTestSession(t)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	screen, err := NewChatScreen(func() context.Context { return ctx }, sess, mgr, user, cfgStore, nil, domain.KindStatus)
	require.NoError(t, err)

	cmd := screen.listenForAPIKeyChanges()
	require.NotNil(t, cmd)

	cancel()

	require.Nil(t, cmd())
	require.True(t, cfgStore.unsubscribed, "context cancellation must unsubscribe the OnChange listener")
}
