package screens

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/store/storetest"
	"github.com/laney/modeloff/internal/ui/uitest"
)

func TestChatScreen_ensurePersonas_generates_when_pool_empty(t *testing.T) {
	var calls atomic.Int32

	seeded := []domain.Persona{
		{ID: "p1", Description: "first", Origin: domain.PersonaGenerated},
	}

	fake := &uitest.FakeAPI{
		GeneratePersonasFn: func(context.Context, domain.ModelID) ([]domain.Persona, error) {
			calls.Add(1)
			return seeded, nil
		},
	}

	store := storetest.NewMemoryStore(t)
	sess, mgr, user := uitest.NewTestSession(t, store, fake, nil, nil, "test-key", "", t.Context)

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	cmd := screen.ensurePersonas()
	require.NotNil(t, cmd, "with an api key configured ensurePersonas must dispatch work")

	require.Nil(t, cmd(), "ensurePersonas is fire-and-forget — its Cmd returns nil")

	require.Equal(t, int32(1), calls.Load())

	got, err := mgr.ListPersonas(t.Context())
	require.NoError(t, err)
	require.Equal(t, seeded, got)
}

func TestChatScreen_ensurePersonas_noop_without_api_key(t *testing.T) {
	var calls atomic.Int32

	fake := &uitest.FakeAPI{
		GeneratePersonasFn: func(context.Context, domain.ModelID) ([]domain.Persona, error) {
			calls.Add(1)
			return nil, nil
		},
	}

	store := storetest.NewMemoryStore(t)
	sess, mgr, user := uitest.NewTestSession(t, store, fake, nil, nil, "", "", t.Context)

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	require.Nil(t, screen.ensurePersonas(),
		"ensurePersonas returns a nil Cmd when no api key is configured")
	require.Equal(t, int32(0), calls.Load())
}

func TestChatScreen_ensurePersonas_skips_generation_when_pool_present(t *testing.T) {
	var calls atomic.Int32

	fake := &uitest.FakeAPI{
		GeneratePersonasFn: func(context.Context, domain.ModelID) ([]domain.Persona, error) {
			calls.Add(1)
			return nil, nil
		},
	}

	store := storetest.NewMemoryStore(t)
	require.NoError(t, store.SavePersona(t.Context(), domain.Persona{
		ID: "existing", Description: "already here", Origin: domain.PersonaUser,
	}))

	sess, mgr, user := uitest.NewTestSession(t, store, fake, nil, nil, "test-key", "", t.Context)

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	require.Nil(t, screen.ensurePersonas()())
	require.Equal(t, int32(0), calls.Load(),
		"a non-empty persona pool must not be regenerated")
}
