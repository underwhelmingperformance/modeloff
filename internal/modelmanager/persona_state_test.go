package modelmanager_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelmanager"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

type personaRollbackEffect struct {
	Before   domain.PersonaInspection
	Rollback domain.PersonaInspection
}

type acceptedPersonaLineageFixture struct {
	Instance    *domain.Instance
	Counterpart *domain.Instance
	Subject     *domain.Instance
	Initial     domain.PersonaLineage
	Zero        domain.PersonaRevision
	Commit      store.PersonaReflectionCommit
}

type personaLookupCase struct {
	Name string
	Nick domain.Nick
}

type personaLookupEffect struct {
	UnknownNick bool
}

func TestManager_inspects_and_rolls_back_persona_lineage(t *testing.T) {
	fx := newTestManager(t, modelmanager.Config{})
	fixture := newAcceptedPersonaLineageFixture(t, fx)

	before, err := fx.mgr.InspectPersona(t.Context(), "botty")
	require.NoError(t, err)
	rollback, err := fx.mgr.RollbackPersona(
		t.Context(), "Botty", fixture.Initial.CurrentRevisionID,
	)
	require.NoError(t, err)

	require.Equal(t, personaRollbackEffect{
		Before: activePersonaInspection(fixture),
		Rollback: basePersonaInspection(
			fixture, domain.PersonaTransitionRollback,
		),
	}, personaRollbackEffect{Before: before, Rollback: rollback})
}

func TestManager_resets_persona_lineage_to_revision_zero(t *testing.T) {
	fx := newTestManager(t, modelmanager.Config{})
	fixture := newAcceptedPersonaLineageFixture(t, fx)

	reset, err := fx.mgr.ResetPersona(t.Context(), "BOTTY")
	require.NoError(t, err)

	require.Equal(t,
		basePersonaInspection(fixture, domain.PersonaTransitionReset),
		reset,
	)
}

// TestManager_persona_lineage_controls_report_an_unknown_nick covers the two
// nicks that reach no persona lineage. The user's connection record is one of
// them: only a model instance has persona lineage, so naming the user is a
// lookup that found nobody and not an instance whose reflection has yet to
// run.
func TestManager_persona_lineage_controls_report_an_unknown_nick(t *testing.T) {
	cases := []personaLookupCase{
		{Name: "no such nick", Nick: "nobody"},
		{Name: "the user's own nick", Nick: "laney"},
	}

	for _, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			fx := newTestManager(t, modelmanager.Config{})
			require.NoError(t, fx.store.SaveInstance(
				t.Context(), domain.NewUserInstance("laney"),
			))

			_, err := fx.mgr.InspectPersona(t.Context(), testCase.Nick)
			var unknown domain.UnknownNickError

			require.Equal(t, personaLookupEffect{
				UnknownNick: true,
			}, personaLookupEffect{
				UnknownNick: errors.As(err, &unknown),
			})
		})
	}
}

func newAcceptedPersonaLineageFixture(
	t *testing.T,
	fx *managerFixture,
) acceptedPersonaLineageFixture {
	t.Helper()

	instance := domain.NewModelInstance(
		"inst-botty", "Botty", "test/model", "careful and curious", nil,
	)
	require.NoError(t, fx.store.SaveInstance(t.Context(), instance))
	counterpart := domain.NewModelInstance(
		"inst-alice", "Alice", "test/model", "methodical and direct", nil,
	)
	require.NoError(t, fx.store.SaveInstance(t.Context(), counterpart))
	counterpartID := counterpart.ID()
	subject := domain.NewModelInstance(
		"inst-charlie", "Charlie", "test/model", "brisk and impatient", nil,
	)
	require.NoError(t, fx.store.SaveInstance(t.Context(), subject))
	subjectID := subject.ID()
	initial, err := fx.store.PersonaLineage(t.Context(), instance.ID())
	require.NoError(t, err)
	eventAt := fixedTime.Add(-time.Hour)
	require.NoError(t, fx.store.AppendReflectionEvents(
		t.Context(), instance.ID(), []store.ReflectionEventCandidate{{
			Source: protocol.HistoryRef{
				Kind: protocol.HistorySourceEvent, ID: 41,
				Window: protocol.ChannelWindowTarget("#dev"),
			},
			Message: protocol.IRCMessage{
				Kind:   protocol.KindPrivMsg,
				Source: domain.ClientSource("inst-alice", "alice"),
				Target: "#dev", Body: "the reproduction is ready", At: eventAt,
			},
			Substantive: true,
		}}, eventAt,
	))
	commit, err := fx.store.CommitPersonaReflection(
		t.Context(), store.PersonaReflectionAcceptance{
			RunID: "accepted-1", InstanceID: instance.ID(),
			BaseRevisionID: initial.CurrentRevisionID,
			HighWaterMark:  1, ModelID: "test/reflection",
			StartedAt: fixedTime.Add(-time.Minute), FinishedAt: fixedTime,
			Experiences: []store.PersonaExperienceDraft{
				{
					Key: "reproduction", Kind: domain.ExperienceObservation,
					Summary:    "Alice supplied a reproduction.",
					Confidence: domain.ConfidenceHigh, OccurredAt: eventAt,
					Sources: []domain.ReflectionEventRef{{Sequence: 1}},
				},
				{
					Key: "impatience", Kind: domain.ExperienceAssertion,
					Summary:    "Charlie said the explanation was too long.",
					SubjectID:  &subjectID,
					Confidence: domain.ConfidenceMedium, OccurredAt: eventAt,
					Sources: []domain.ReflectionEventRef{{Sequence: 1}},
				},
			},
			Amendments: []store.PersonaAmendmentDraft{{
				Scope:        domain.AmendmentRelationship,
				Counterpart:  &counterpartID,
				Tendency:     "Ask Alice for reproduction details.",
				Confidence:   domain.ConfidenceHigh,
				EvidenceKeys: []string{"reproduction"},
			}},
			Retract: []domain.PersonaAmendmentID{},
		},
	)
	require.NoError(t, err)
	zero, err := fx.store.PersonaRevision(t.Context(), initial.CurrentRevisionID)
	require.NoError(t, err)

	return acceptedPersonaLineageFixture{
		Instance: instance, Counterpart: counterpart, Subject: subject,
		Initial: initial, Zero: zero, Commit: commit,
	}
}

func activePersonaInspection(
	fixture acceptedPersonaLineageFixture,
) domain.PersonaInspection {
	return domain.PersonaInspection{
		Nick: "Botty", Lineage: fixture.Commit.Lineage,
		Revision:    fixture.Commit.Revision,
		Parent:      &fixture.Zero,
		Experiences: fixture.Commit.Experiences,
		Amendments:  fixture.Commit.Amendments,
		Counterparts: []domain.PersonaCounterpart{
			{InstanceID: fixture.Subject.ID(), Nick: fixture.Subject.Nick()},
			{InstanceID: fixture.Counterpart.ID(), Nick: fixture.Counterpart.Nick()},
		},
		RecentRuns: []domain.ReflectionRun{fixture.Commit.Run},
		Transitions: []domain.PersonaTransition{{
			ID: 1, InstanceID: fixture.Instance.ID(),
			FromRevisionID: fixture.Initial.CurrentRevisionID,
			ToRevisionID:   fixture.Commit.Revision.ID,
			Kind:           domain.PersonaTransitionReflection, At: fixedTime,
		}},
	}
}

func basePersonaInspection(
	fixture acceptedPersonaLineageFixture,
	kind domain.PersonaTransitionKind,
) domain.PersonaInspection {
	return domain.PersonaInspection{
		Nick: "Botty",
		Lineage: domain.PersonaLineage{
			InstanceID: fixture.Instance.ID(), Baseline: "careful and curious",
			CurrentRevisionID: fixture.Initial.CurrentRevisionID,
			Checkpoint:        1, ReflectedAt: new(fixedTime),
		},
		Revision:     fixture.Zero,
		Experiences:  []domain.Experience{},
		Amendments:   []domain.PersonaAmendment{},
		Counterparts: []domain.PersonaCounterpart{},
		RecentRuns:   []domain.ReflectionRun{fixture.Commit.Run},
		Transitions: []domain.PersonaTransition{
			{
				ID: 1, InstanceID: fixture.Instance.ID(),
				FromRevisionID: fixture.Initial.CurrentRevisionID,
				ToRevisionID:   fixture.Commit.Revision.ID,
				Kind:           domain.PersonaTransitionReflection, At: fixedTime,
			},
			{
				ID: 2, InstanceID: fixture.Instance.ID(),
				FromRevisionID: fixture.Commit.Revision.ID,
				ToRevisionID:   fixture.Initial.CurrentRevisionID,
				Kind:           kind, At: fixedTime,
			},
		},
	}
}

type personaDescriptionEffect struct {
	Persona     string
	Baseline    string
	Parent      *domain.PersonaRevision
	Transitions []domain.PersonaTransition
	Refused     bool
}

// TestManager_writes_an_operator_description_as_a_revision covers `/config
// persona`'s missing counterpart: an operator changing the persona of an
// instance that is already running. It lands in the same lineage reflection
// writes into, leaving revision zero as the reset target, and the transition
// says an operator made it.
func TestManager_writes_an_operator_description_as_a_revision(t *testing.T) {
	fx := newTestManager(t, modelmanager.Config{})
	fixture := newAcceptedPersonaLineageFixture(t, fx)
	written := "cares more about being right than about being easy to be around"

	described, err := fx.mgr.SetInstancePersona(t.Context(), "BOTTY", written)
	require.NoError(t, err)
	_, refusal := fx.mgr.SetInstancePersona(
		t.Context(), "botty", strings.Repeat("x", domain.PersonaMaxLen+1),
	)
	var erroneous domain.ErroneousPersonaError

	require.Equal(t, personaDescriptionEffect{
		Persona:  written,
		Baseline: "careful and curious",
		Parent:   &fixture.Commit.Revision,
		Transitions: []domain.PersonaTransition{
			{
				ID: 1, InstanceID: fixture.Instance.ID(),
				FromRevisionID: fixture.Initial.CurrentRevisionID,
				ToRevisionID:   fixture.Commit.Revision.ID,
				Kind:           domain.PersonaTransitionReflection, At: fixedTime,
			},
			{
				// The revision the write produced, read back rather than
				// predicted: the store promises a child revision, not the
				// next integer after its parent.
				ID: 2, InstanceID: fixture.Instance.ID(),
				FromRevisionID: fixture.Commit.Revision.ID,
				ToRevisionID:   described.Revision.ID,
				Kind:           domain.PersonaTransitionOperator, At: fixedTime,
			},
		},
		Refused: true,
	}, personaDescriptionEffect{
		Persona:     described.Revision.Description,
		Baseline:    described.Lineage.Baseline,
		Parent:      described.Parent,
		Transitions: described.Transitions,
		Refused:     errors.As(refusal, &erroneous),
	})
}
