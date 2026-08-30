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

// TestPrepareInstance_refuses_without_a_persona covers a fresh install
// with no API key: the requester supplied no persona and there is no
// client to write one. The persona becomes revision zero of the
// instance's lineage and nothing after ADDMODEL writes revision zero,
// so the command is refused and no instance is prepared.
func TestPrepareInstance_refuses_without_a_persona(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	fx := newTestManager(t, modelmanager.Config{APIClient: nil})
	sess := newTestSession(t, fx)

	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "")

	require.ErrorIs(t, err, api.ErrClientNotConfigured)
	require.Equal(t, session.PreparedInstance{}, prepared)
}

// TestPrepareInstance_generates_a_persona is the same call with an
// upstream configured. The manager calls `GeneratePersona`, returns the
// description it produced, and leaves `Warnings` empty.
func TestPrepareInstance_generates_a_persona(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	client := nickGeneratingClient(modelID)
	client.GeneratePersonaFn = func(context.Context, domain.ModelID, api.PersonaRequest) (string, error) {
		return "a terse reviewer", nil
	}

	fx := newTestManager(t, modelmanager.Config{APIClient: client})
	sess := newTestSession(t, fx)

	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "")
	require.NoError(t, err)

	require.Equal(t, session.PreparedInstance{
		Nick:    "reviewer",
		Persona: "a terse reviewer",
	}, prepared)
}

// TestPrepareInstance_retries_a_refused_description covers a
// description [domain.ValidatePersona] turns down. The next request
// sends it back with the reason it was refused, the way a rejected
// nick is sent back to `GenerateNick`, and the instance takes the
// description that follows.
func TestPrepareInstance_retries_a_refused_description(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	var requests []api.PersonaRequest
	client := nickGeneratingClient(modelID)
	client.GeneratePersonaFn = func(_ context.Context, _ domain.ModelID, req api.PersonaRequest) (string, error) {
		requests = append(requests, req)

		if len(requests) == 1 {
			return "helpful\nand thorough", nil
		}

		return "a terse reviewer", nil
	}

	fx := newTestManager(t, modelmanager.Config{APIClient: client})
	sess := newTestSession(t, fx)

	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "")
	require.NoError(t, err)

	require.Equal(t, struct {
		Prepared session.PreparedInstance
		Requests []api.PersonaRequest
	}{
		Prepared: session.PreparedInstance{Nick: "reviewer", Persona: "a terse reviewer"},
		Requests: []api.PersonaRequest{
			{},
			{Rejected: []api.RejectedPersona{{
				Description: "helpful\nand thorough",
				Reason: "That description was refused: " +
					domain.PersonaControlCharacter.String() + ". Write a different one.",
			}}},
		},
	}, struct {
		Prepared session.PreparedInstance
		Requests []api.PersonaRequest
	}{Prepared: prepared, Requests: requests})
}

// TestPrepareInstance_retries_an_empty_description covers a
// description [domain.ValidatePersona] accepts and preparation does
// not. Whitespace alone is empty once trimmed, and an empty persona is
// how `PrepareInstance` recognises that the requester supplied none, so
// accepting one here would admit an instance holding an empty revision
// zero and an empty reset baseline. The description and the reason it
// was refused reach the next request like any other refusal.
func TestPrepareInstance_retries_an_empty_description(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	var requests []api.PersonaRequest
	client := nickGeneratingClient(modelID)
	client.GeneratePersonaFn = func(_ context.Context, _ domain.ModelID, req api.PersonaRequest) (string, error) {
		requests = append(requests, req)

		if len(requests) == 1 {
			return "   ", nil
		}

		return "a terse reviewer", nil
	}

	fx := newTestManager(t, modelmanager.Config{APIClient: client})
	sess := newTestSession(t, fx)

	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "")
	require.NoError(t, err)

	require.Equal(t, struct {
		Prepared session.PreparedInstance
		Requests []api.PersonaRequest
	}{
		Prepared: session.PreparedInstance{Nick: "reviewer", Persona: "a terse reviewer"},
		Requests: []api.PersonaRequest{
			{},
			{Rejected: []api.RejectedPersona{{
				Reason: "That description was empty. Write one.",
			}}},
		},
	}, struct {
		Prepared session.PreparedInstance
		Requests []api.PersonaRequest
	}{Prepared: prepared, Requests: requests})
}

// TestPrepareInstance_refuses_a_persistently_refused_description caps
// the retry loop. Every attempt is turned down, so the add fails and
// no instance is prepared.
func TestPrepareInstance_refuses_a_persistently_refused_description(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	attempts := 0
	client := nickGeneratingClient(modelID)
	client.GeneratePersonaFn = func(context.Context, domain.ModelID, api.PersonaRequest) (string, error) {
		attempts++

		return strings.Repeat("x", domain.PersonaMaxLen+1), nil
	}

	fx := newTestManager(t, modelmanager.Config{APIClient: client})
	sess := newTestSession(t, fx)

	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "")

	require.Error(t, err)
	require.Equal(t, struct {
		Prepared session.PreparedInstance
		Attempts int
	}{Attempts: 3}, struct {
		Prepared session.PreparedInstance
		Attempts int
	}{Prepared: prepared, Attempts: attempts})
}

// TestPrepareInstance_reports_a_derived_nick covers the one part of
// preparation that degrades. A failed persona ends the command; a
// failed nick does not. The upstream here returns a persona and fails
// every nick request, so the deterministic fallback supplies one and
// the warning names the nick the instance took and what stopped it
// being generated. `handleAddModel` turns the warning into a server
// notice.
func TestPrepareInstance_reports_a_derived_nick(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	client := &apitest.Fake{
		ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
			return toolsCatalogue(modelID), nil
		},
		GenerateNickFn: func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error) {
			return "", api.ErrClientNotConfigured
		},
		GeneratePersonaFn: func(context.Context, domain.ModelID, api.PersonaRequest) (string, error) {
			return "a terse reviewer", nil
		},
	}

	fx := newTestManager(t, modelmanager.Config{APIClient: client})
	sess := newTestSession(t, fx)

	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "")
	require.NoError(t, err)

	require.Equal(t, session.PreparedInstance{
		Nick:    "gpt-5-4",
		Persona: "a terse reviewer",
		Warnings: []string{
			"could not generate a nick for openai/gpt-5.4-mini: generate nick: " +
				"api client not configured. It joins as gpt-5-4, derived from its model id.",
		},
	}, prepared)
}

// TestPrepareInstance_keeps_the_requested_persona pins that a persona
// the requester supplied comes back as the text they wrote, trimmed,
// and that `GeneratePersona` is not called at all. That returned value
// is what ADDMODEL records as revision zero, and the whitespace would
// otherwise be stored with the description and rendered at the top of
// the instance's prompt.
func TestPrepareInstance_keeps_the_requested_persona(t *testing.T) {
	const modelID = domain.ModelID("openai/gpt-5.4-mini")

	generated := 0
	client := nickGeneratingClient(modelID)
	client.GeneratePersonaFn = func(context.Context, domain.ModelID, api.PersonaRequest) (string, error) {
		generated++

		return "a terse reviewer", nil
	}

	fx := newTestManager(t, modelmanager.Config{APIClient: client})
	sess := newTestSession(t, fx)

	prepared, err := fx.mgr.PrepareInstance(t.Context(), sess, modelID, "  sceptical about everything  ")
	require.NoError(t, err)

	require.Equal(t, struct {
		Prepared  session.PreparedInstance
		Generated int
	}{
		Prepared: session.PreparedInstance{
			Nick:    "reviewer",
			Persona: "sceptical about everything",
		},
	}, struct {
		Prepared  session.PreparedInstance
		Generated int
	}{Prepared: prepared, Generated: generated})
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
