package screens

import (
	"context"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/store/storetest"
	"github.com/laney/modeloff/internal/ui/uitest"
)

// drainCmd evaluates cmd and any nested tea.BatchMsg children
// recursively, so an Init that batches several Cmds can be fully
// exercised in one call. Returns every non-batch tea.Msg produced.
func drainCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}

	var out []tea.Msg

	msg := cmd()
	switch m := msg.(type) {
	case nil:
		return nil
	case tea.BatchMsg:
		for _, child := range m {
			out = append(out, drainCmd(child)...)
		}
	default:
		out = append(out, msg)
	}

	return out
}

func TestConnectionScreen_Init_ensures_personas_when_api_key_present(t *testing.T) {
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
	sess, mgr, _ := uitest.NewTestSession(t, store, fake, nil, nil, "test-key", "", t.Context)

	conn := NewConnectionScreen(ConnectionConfig{
		HasAPIKey:   true,
		Nick:        "alice",
		Session:     sess,
		Manager:     mgr,
		BaseContext: t.Context,
	}, nil)

	drainCmd(conn.Init())

	require.Equal(t, int32(1), calls.Load())

	got, err := mgr.ListPersonas(t.Context())
	require.NoError(t, err)
	require.Equal(t, seeded, got)
}

func TestConnectionScreen_Init_skips_persona_generation_without_api_key(t *testing.T) {
	var calls atomic.Int32

	fake := &uitest.FakeAPI{
		GeneratePersonasFn: func(context.Context, domain.ModelID) ([]domain.Persona, error) {
			calls.Add(1)
			return nil, nil
		},
	}

	store := storetest.NewMemoryStore(t)
	sess, mgr, _ := uitest.NewTestSession(t, store, fake, nil, nil, "", "", t.Context)

	conn := NewConnectionScreen(ConnectionConfig{
		HasAPIKey:   false,
		Nick:        "alice",
		Session:     sess,
		Manager:     mgr,
		BaseContext: t.Context,
	}, nil)

	drainCmd(conn.Init())

	require.Equal(t, int32(0), calls.Load(),
		"no api key configured means the persona pool must not be touched")
}
