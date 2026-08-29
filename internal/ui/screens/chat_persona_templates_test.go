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

func TestChatScreen_ensurePersonaTemplates_generates_when_pool_empty(t *testing.T) {
	var calls atomic.Int32

	seeded := []domain.PersonaTemplate{
		{ID: "p1", Description: "first", Origin: domain.PersonaGenerated},
	}

	fake := &uitest.FakeAPI{
		GeneratePersonaTemplatesFn: func(context.Context, domain.ModelID) ([]domain.PersonaTemplate, error) {
			calls.Add(1)
			return seeded, nil
		},
	}

	store := storetest.NewMemoryStore(t)
	sess, mgr, user := uitest.NewTestSession(t, store, fake, nil, nil, "test-key", "", t.Context)

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	cmd := screen.ensurePersonaTemplates()
	require.NotNil(t, cmd, "with an api key configured ensurePersonaTemplates must dispatch work")

	require.Nil(t, cmd(), "ensurePersonaTemplates is fire-and-forget — its Cmd returns nil")

	require.Equal(t, int32(1), calls.Load())

	got, err := mgr.ListPersonaTemplates(t.Context())
	require.NoError(t, err)
	require.Equal(t, seeded, got)
}

func TestChatScreen_ensurePersonaTemplates_noop_without_api_key(t *testing.T) {
	var calls atomic.Int32

	fake := &uitest.FakeAPI{
		GeneratePersonaTemplatesFn: func(context.Context, domain.ModelID) ([]domain.PersonaTemplate, error) {
			calls.Add(1)
			return nil, nil
		},
	}

	store := storetest.NewMemoryStore(t)
	sess, mgr, user := uitest.NewTestSession(t, store, fake, nil, nil, "", "", t.Context)

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	require.Nil(t, screen.ensurePersonaTemplates(),
		"ensurePersonaTemplates returns a nil Cmd when no api key is configured")
	require.Equal(t, int32(0), calls.Load())
}

func TestChatScreen_ensurePersonaTemplates_skips_generation_when_pool_present(t *testing.T) {
	var calls atomic.Int32

	fake := &uitest.FakeAPI{
		GeneratePersonaTemplatesFn: func(context.Context, domain.ModelID) ([]domain.PersonaTemplate, error) {
			calls.Add(1)
			return nil, nil
		},
	}

	store := storetest.NewMemoryStore(t)
	require.NoError(t, store.SavePersonaTemplate(t.Context(), domain.PersonaTemplate{
		ID: "existing", Description: "already here", Origin: domain.PersonaUser,
	}))

	sess, mgr, user := uitest.NewTestSession(t, store, fake, nil, nil, "test-key", "", t.Context)

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	require.Nil(t, screen.ensurePersonaTemplates()())
	require.Equal(t, int32(0), calls.Load(),
		"a non-empty persona pool must not be regenerated")
}
