package modelmanager_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
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

	require.NoError(t, fx.store.SavePersonaTemplate(t.Context(), domain.PersonaTemplate{
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
		PersonaTemplate: &domain.PersonaTemplateProvenance{
			ID: "p1", Origin: domain.PersonaGenerated,
			DescriptionHash: "a5d1a15a189842e2e80250db9a52d0ec8d3cbf35a10fd28ae78e7cd494582e36",
		},
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
		Persona: "  sceptical about everything  ",
	}, prepared)
}

func TestPrepareInstance_copies_a_requested_persona_template(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	fx := newTestManager(t, modelmanager.Config{APIClient: nil})
	require.NoError(t, fx.store.SavePersonaTemplate(t.Context(), domain.PersonaTemplate{
		ID:          "careful-reader",
		Description: "checks the source before reaching a conclusion",
		Origin:      domain.PersonaUser,
	}))

	sess := newTestSession(t, fx)
	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "careful-reader")
	require.NoError(t, err)

	require.Equal(t, session.PreparedInstance{
		Nick:    "gpt-5-4",
		Persona: "checks the source before reaching a conclusion",
		PersonaTemplate: &domain.PersonaTemplateProvenance{
			ID: "careful-reader", Origin: domain.PersonaUser,
			DescriptionHash: "da042dab335afd71b1dbd0c857aa0dd5f87028ba80412154adcc180729697204",
		},
	}, prepared)
}

func TestPrepareInstance_does_not_trim_a_persona_template_id(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	fx := newTestManager(t, modelmanager.Config{APIClient: nil})
	require.NoError(t, fx.store.SavePersonaTemplate(t.Context(), domain.PersonaTemplate{
		ID:          "careful-reader",
		Description: "checks the source before reaching a conclusion",
		Origin:      domain.PersonaUser,
	}))

	sess := newTestSession(t, fx)
	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, " careful-reader ")
	require.NoError(t, err)

	require.Equal(t, session.PreparedInstance{
		Nick:    "gpt-5-4",
		Persona: " careful-reader ",
	}, prepared)
}

func TestPrepareInstance_copies_an_empty_persona_template(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	fx := newTestManager(t, modelmanager.Config{APIClient: nil})
	require.NoError(t, fx.store.SavePersonaTemplate(t.Context(), domain.PersonaTemplate{
		ID:     "blank-slate",
		Origin: domain.PersonaUser,
	}))

	sess := newTestSession(t, fx)
	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "blank-slate")
	require.NoError(t, err)

	require.Equal(t, session.PreparedInstance{
		Nick: "gpt-5-4",
		PersonaTemplate: &domain.PersonaTemplateProvenance{
			ID: "blank-slate", Origin: domain.PersonaUser,
			DescriptionHash: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
	}, prepared)
}

func TestPrepareInstance_rejects_control_characters_at_the_persona_boundary(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	var effects []string
	client := &apitest.Fake{
		ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
			effects = append(effects, "list models")

			return toolsCatalogue(modelID), nil
		},
	}
	fx := newTestManager(t, modelmanager.Config{
		APIClient: client, InitialAPIKey: "test-key",
	})
	sess := newTestSession(t, fx)
	_, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "careful-reader\n")

	var personaErr domain.ErroneousPersonaError
	require.ErrorAs(t, err, &personaErr)
	require.Equal(t, struct {
		Error   domain.ErroneousPersonaError
		Effects []string
	}{
		Error: domain.ErroneousPersonaError{
			Reason: domain.PersonaControlCharacter,
			At:     fixedTime,
		},
	}, struct {
		Error   domain.ErroneousPersonaError
		Effects []string
	}{
		Error:   personaErr,
		Effects: effects,
	})
}

func TestPrepareInstance_rejects_an_invalid_persona_template(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	fx := newTestManager(t, modelmanager.Config{APIClient: nil})
	require.NoError(t, fx.store.SavePersonaTemplate(t.Context(), domain.PersonaTemplate{
		ID:          "too-much",
		Description: strings.Repeat("x", domain.PersonaMaxLen+1),
		Origin:      domain.PersonaUser,
	}))

	sess := newTestSession(t, fx)
	_, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "too-much")

	var personaErr domain.ErroneousPersonaError
	require.ErrorAs(t, err, &personaErr)
	require.Equal(t, domain.PersonaTooLong, personaErr.Reason)
}
