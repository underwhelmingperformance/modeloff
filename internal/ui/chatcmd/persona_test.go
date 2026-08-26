package chatcmd

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
)

type personaManagerEffect struct {
	Operation   string
	Nick        domain.Nick
	Revision    domain.PersonaRevisionID
	Description string
}

type personaManager struct {
	fakeManagerAPI

	Inspection domain.PersonaInspection
	Effect     personaManagerEffect
}

func (m *personaManager) InspectPersona(
	_ context.Context,
	nick domain.Nick,
) (domain.PersonaInspection, error) {
	m.Effect = personaManagerEffect{Operation: "inspect", Nick: nick}

	return m.Inspection, nil
}

func (m *personaManager) ResetPersona(
	_ context.Context,
	nick domain.Nick,
) (domain.PersonaInspection, error) {
	m.Effect = personaManagerEffect{Operation: "reset", Nick: nick}

	return m.Inspection, nil
}

func (m *personaManager) RollbackPersona(
	_ context.Context,
	nick domain.Nick,
	revision domain.PersonaRevisionID,
) (domain.PersonaInspection, error) {
	m.Effect = personaManagerEffect{
		Operation: "rollback", Nick: nick, Revision: revision,
	}

	return m.Inspection, nil
}

func (m *personaManager) SetInstancePersona(
	_ context.Context,
	nick domain.Nick,
	description string,
) (domain.PersonaInspection, error) {
	m.Effect = personaManagerEffect{
		Operation: "describe", Nick: nick, Description: description,
	}

	return m.Inspection, nil
}

type personaCommandCase struct {
	Name    string
	Raw     string
	Command PersonaCommand
	Effect  personaManagerEffect
	Result  PersonaResult
}

// TestPersonaCommand_runs_every_operator_action covers the four things
// `/persona` does. The bare form shows the persona, a form carrying text
// writes a revision, and the two flags put a revision that already exists
// back in force. A multi-word description reaches the manager as one value,
// because the parser hands a passthrough positional over whole.
func TestPersonaCommand_runs_every_operator_action(t *testing.T) {
	rollbackTarget := domain.PersonaRevisionID(2)
	written := CommandText("cares more about being right, and says so")
	inspection := domain.PersonaInspection{
		Nick: "Botty",
		Lineage: domain.PersonaLineage{
			InstanceID: "inst-botty", Baseline: "careful and curious",
			CurrentRevisionID: 4, Checkpoint: 12,
		},
		Revision: domain.PersonaRevision{
			ID: 4, InstanceID: "inst-botty",
			ExperienceIDs: []domain.ExperienceID{2},
			AmendmentIDs:  []domain.PersonaAmendmentID{},
		},
		Experiences: []domain.Experience{}, Amendments: []domain.PersonaAmendment{},
		RecentRuns: []domain.ReflectionRun{}, Transitions: []domain.PersonaTransition{},
	}
	cases := []personaCommandCase{
		{
			Name: "inspect", Raw: "/persona Botty",
			Command: PersonaCommand{Nick: "Botty"},
			Effect:  personaManagerEffect{Operation: "inspect", Nick: "Botty"},
			Result:  PersonaResult{Action: PersonaInspected, Inspection: inspection},
		},
		{
			Name: "describe", Raw: "/persona Botty " + written.String(),
			Command: PersonaCommand{Nick: "Botty", Description: &written},
			Effect: personaManagerEffect{
				Operation: "describe", Nick: "Botty", Description: written.String(),
			},
			Result: PersonaResult{Action: PersonaDescribed, Inspection: inspection},
		},
		{
			Name: "reset", Raw: "/persona Botty --reset",
			Command: PersonaCommand{Nick: "Botty", Reset: true},
			Effect:  personaManagerEffect{Operation: "reset", Nick: "Botty"},
			Result:  PersonaResult{Action: PersonaReset, Inspection: inspection},
		},
		{
			Name: "rollback", Raw: "/persona Botty --rollback 2",
			Command: PersonaCommand{Nick: "Botty", Rollback: &rollbackTarget},
			Effect: personaManagerEffect{
				Operation: "rollback", Nick: "Botty", Revision: 2,
			},
			Result: PersonaResult{Action: PersonaRolledBack, Inspection: inspection},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			parsed, err := testParser.Parse(testCase.Raw)
			require.NoError(t, err)
			require.Equal(t, testCase.Command, parsed)
			manager := &personaManager{Inspection: inspection}
			cmd := testCase.Command.Run(t.Context(), Context{Manager: manager})

			require.Equal(t, testCase.Result, cmd())
			require.Equal(t, testCase.Effect, manager.Effect)
		})
	}
}

type personaRefusalCase struct {
	Name string
	Raw  string
}

type personaRefusalEffect struct {
	Message tea.Msg
	Effect  personaManagerEffect
}

// TestPersonaCommand_refuses_an_action_it_cannot_take covers the inputs the
// command must not read as an inspection. A revision number is positive, so a
// supplied zero or negative one names nothing, and naming two actions at once
// asks for two different revisions.
func TestPersonaCommand_refuses_an_action_it_cannot_take(t *testing.T) {
	cases := []personaRefusalCase{
		{Name: "no nick", Raw: "/persona"},
		{Name: "zero revision", Raw: "/persona Botty --rollback 0"},
		{Name: "negative revision", Raw: "/persona Botty --rollback -2"},
		{Name: "reset and zero revision", Raw: "/persona Botty --reset --rollback 0"},
		{Name: "reset and rollback", Raw: "/persona Botty --reset --rollback 2"},
		{Name: "reset and description", Raw: "/persona Botty --reset terse"},
		{Name: "rollback and description", Raw: "/persona Botty --rollback 2 terse"},
	}

	for _, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			parsed, err := testParser.Parse(testCase.Raw)
			require.NoError(t, err)
			manager := &personaManager{}
			cmd := parsed.(PersonaCommand).Run(t.Context(), Context{Manager: manager})

			require.Equal(t, personaRefusalEffect{
				Message: UsageError{Command: "persona", Usage: personaUsage},
			}, personaRefusalEffect{
				Message: cmd(), Effect: manager.Effect,
			})
		})
	}
}

// TestParse_persona_refuses_an_unrecognised_flag covers what
// `passthrough:"partial"` buys over `passthrough:"all"`. Both keep recognised
// flags active until the description takes its first value, so `--reset` and
// `--rollback` parse under either. They differ on an unknown flag before that
// point: `all` would pass `--reste` through and write it into the instance's
// personality, and `partial` refuses it.
func TestParse_persona_refuses_an_unrecognised_flag(t *testing.T) {
	_, err := testParser.Parse("/persona Botty --reste")

	var unknown *command.UnknownFlagError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, "--reste", unknown.Flag)
}

// TestParse_persona_takes_a_dashed_word_inside_a_description covers the other
// half of `partial`: once the description has begun, the rest of the line is
// text, so a persona may talk about a flag without the parser reading one.
func TestParse_persona_takes_a_dashed_word_inside_a_description(t *testing.T) {
	description := CommandText("blunt --reset is not a personality")

	parsed, err := testParser.Parse("/persona Botty " + description.String())

	require.NoError(t, err)
	require.Equal(
		t, PersonaCommand{Nick: "Botty", Description: &description}, parsed,
	)
}
