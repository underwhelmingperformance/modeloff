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

// nickGeneratingClient is an upstream that answers nick generation, so
// a preparation test reads the persona alone: without one, every case
// takes the deterministic fallback and carries the warning that
// reports it.
func nickGeneratingClient(modelID domain.ModelID) *apitest.Fake {
	return &apitest.Fake{
		ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
			return toolsCatalogue(modelID), nil
		},
		GenerateNickFn: func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error) {
			return "reviewer", nil
		},
	}
}

// TestPrepareInstance_refuses_without_a_persona covers a pool that can
// supply nothing: it is empty and there is no API client to generate
// one, which is the state a fresh install with no key is in. The
// persona becomes revision zero of the instance's lineage and nothing
// after ADDMODEL supplies one, so the command is refused and no
// instance is prepared. Both the generation failure and the empty pool
// it left behind stay recoverable from the returned error.
func TestPrepareInstance_refuses_without_a_persona(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	fx := newTestManager(t, modelmanager.Config{APIClient: nil})
	sess := newTestSession(t, fx)

	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "")

	require.ErrorIs(t, err, domain.ErrNoPersonaTemplates)
	require.ErrorIs(t, err, api.ErrClientNotConfigured)
	require.Equal(t, session.PreparedInstance{}, prepared)
}

// TestPrepareInstance_assigns_a_pool_persona is the same call with a
// persona to draw: the instance takes it, its provenance records which
// pool row it came from, and nothing is reported to the operator.
func TestPrepareInstance_assigns_a_pool_persona(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	fx := newTestManager(t, modelmanager.Config{APIClient: nickGeneratingClient(modelID)})

	require.NoError(t, fx.store.SavePersonaTemplate(t.Context(), domain.PersonaTemplate{
		ID:          "p1",
		Description: "a terse reviewer",
		Origin:      domain.PersonaGenerated,
	}))

	sess := newTestSession(t, fx)

	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "")
	require.NoError(t, err)

	require.Equal(t, session.PreparedInstance{
		Nick:    "reviewer",
		Persona: "a terse reviewer",
		PersonaTemplate: &domain.PersonaTemplateProvenance{
			ID: "p1", Origin: domain.PersonaGenerated,
			DescriptionHash: "a5d1a15a189842e2e80250db9a52d0ec8d3cbf35a10fd28ae78e7cd494582e36",
		},
	}, prepared)
}

// TestPrepareInstance_reports_a_derived_nick covers the one part of
// preparation that degrades where the persona fails outright. With no
// upstream to ask, the deterministic fallback supplies a nick and the
// warning names the nick the instance took and what stopped it being
// generated, so the fall-back reaches the operator and not only the
// log. `handleAddModel` turns the warning into a server notice.
func TestPrepareInstance_reports_a_derived_nick(t *testing.T) {
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
		Warnings: []string{
			"could not generate a nick for openai/gpt-5.4-mini: generate nick: " +
				"api client not configured. It joins as gpt-5-4, derived from its model id.",
		},
	}, prepared)
}

// TestPrepareInstance_keeps_the_requested_persona pins that a persona
// the requester supplied is never redrawn, so the pool is not consulted
// and there is nothing to report. Surrounding whitespace is dropped: it
// would otherwise reach the top of every prompt this instance ever
// renders.
func TestPrepareInstance_keeps_the_requested_persona(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	fx := newTestManager(t, modelmanager.Config{APIClient: nickGeneratingClient(modelID)})
	sess := newTestSession(t, fx)

	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "  sceptical about everything  ")
	require.NoError(t, err)

	require.Equal(t, session.PreparedInstance{
		Nick:    "reviewer",
		Persona: "sceptical about everything",
	}, prepared)
}

func TestPrepareInstance_copies_a_requested_persona_template(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	fx := newTestManager(t, modelmanager.Config{APIClient: nickGeneratingClient(modelID)})
	require.NoError(t, fx.store.SavePersonaTemplate(t.Context(), domain.PersonaTemplate{
		ID:          "careful-reader",
		Description: "checks the source before reaching a conclusion",
		Origin:      domain.PersonaUser,
	}))

	sess := newTestSession(t, fx)
	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "careful-reader")
	require.NoError(t, err)

	require.Equal(t, session.PreparedInstance{
		Nick:    "reviewer",
		Persona: "checks the source before reaching a conclusion",
		PersonaTemplate: &domain.PersonaTemplateProvenance{
			ID: "careful-reader", Origin: domain.PersonaUser,
			DescriptionHash: "da042dab335afd71b1dbd0c857aa0dd5f87028ba80412154adcc180729697204",
		},
	}, prepared)
}

// TestPrepareInstance_resolves_a_padded_persona_template_id covers a
// template id arriving with whitespace around it, which the add-model
// tool produces whenever a model puts it there. The id names a template
// and resolves to one, so the instance takes the character it asked for
// and not the id as its own description.
func TestPrepareInstance_resolves_a_padded_persona_template_id(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	fx := newTestManager(t, modelmanager.Config{APIClient: nickGeneratingClient(modelID)})
	require.NoError(t, fx.store.SavePersonaTemplate(t.Context(), domain.PersonaTemplate{
		ID:          "careful-reader",
		Description: "checks the source before reaching a conclusion",
		Origin:      domain.PersonaUser,
	}))

	sess := newTestSession(t, fx)
	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, " careful-reader ")
	require.NoError(t, err)

	require.Equal(t, session.PreparedInstance{
		Nick:    "reviewer",
		Persona: "checks the source before reaching a conclusion",
		PersonaTemplate: &domain.PersonaTemplateProvenance{
			ID: "careful-reader", Origin: domain.PersonaUser,
			DescriptionHash: "da042dab335afd71b1dbd0c857aa0dd5f87028ba80412154adcc180729697204",
		},
	}, prepared)
}

func TestPrepareInstance_copies_an_empty_persona_template(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	fx := newTestManager(t, modelmanager.Config{APIClient: nickGeneratingClient(modelID)})
	require.NoError(t, fx.store.SavePersonaTemplate(t.Context(), domain.PersonaTemplate{
		ID:     "blank-slate",
		Origin: domain.PersonaUser,
	}))

	sess := newTestSession(t, fx)
	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "blank-slate")
	require.NoError(t, err)

	require.Equal(t, session.PreparedInstance{
		Nick: "reviewer",
		PersonaTemplate: &domain.PersonaTemplateProvenance{
			ID: "blank-slate", Origin: domain.PersonaUser,
			DescriptionHash: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
	}, prepared)
}

// TestPrepareInstance_rejects_control_characters_at_the_persona_boundary
// covers a control character in the last position, which a validator
// checking only the interior would pass. Resolution drops surrounding
// whitespace, so the character here is one whitespace trimming leaves
// in place. The refusal comes before any upstream call, which the empty
// effect list is what records.
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
	_, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "careful-reader\x07")

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

	fx := newTestManager(t, modelmanager.Config{APIClient: nickGeneratingClient(modelID)})
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
