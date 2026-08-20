package modelmanager_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelmanager"
	"github.com/laney/modeloff/internal/session"
)

// TestPrepareInstance_reports_an_unassigned_persona covers what the
// operator learns when the persona pool cannot supply one. A model
// with no persona still works, so the add succeeds and the failure
// cannot be an error; without a warning to carry it, it reaches only
// the log. `handleAddModel` turns the warning into a server notice on
// the reply.
//
// The pool is empty and there is no API client to generate one, which
// is the state a fresh install with no key is in.
func TestPrepareInstance_reports_an_unassigned_persona(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	fx := newTestManager(t, modelmanager.Config{APIClient: nil})
	sess := newTestSession(t, fx)

	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "")
	require.NoError(t, err)

	require.Equal(t, session.PreparedInstance{
		Nick:     "gpt-5-4",
		Persona:  "",
		Warnings: []string{"no persona was assigned to openai/gpt-5.4-mini (no personas available); it joins without one"},
	}, prepared)
}

// TestPrepareInstance_assigns_a_pool_persona_without_warning is the
// same call with a persona to draw: the instance gets it and the
// operator is told nothing, so a warning only ever appears when
// something actually fell short.
func TestPrepareInstance_assigns_a_pool_persona_without_warning(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	fx := newTestManager(t, modelmanager.Config{APIClient: nil})

	require.NoError(t, fx.store.SavePersona(t.Context(), domain.Persona{
		ID:          "p1",
		Description: "a terse reviewer",
		Origin:      domain.PersonaGenerated,
	}))

	sess := newTestSession(t, fx)

	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "")
	require.NoError(t, err)

	require.Equal(t, session.PreparedInstance{
		Nick:    "gpt-5-4",
		Persona: "a terse reviewer",
	}, prepared)
}

// TestPrepareInstance_keeps_the_requested_persona_verbatim pins that
// a persona the requester supplied is never redrawn, so the pool is
// not consulted and there is nothing to warn about.
func TestPrepareInstance_keeps_the_requested_persona_verbatim(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	fx := newTestManager(t, modelmanager.Config{APIClient: nil})
	sess := newTestSession(t, fx)

	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "  sceptical about everything  ")
	require.NoError(t, err)

	require.Equal(t, session.PreparedInstance{
		Nick:    "gpt-5-4",
		Persona: "sceptical about everything",
	}, prepared)
}
