package screens

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/text/language"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/chatcmd"
)

type personaFormatCase struct {
	Name   string
	Result chatcmd.PersonaResult
	Text   string
}

func TestFormatPersonaResult_reports_complete_bounded_diagnostics(t *testing.T) {
	at := time.Date(2026, 8, 27, 15, 0, 0, 0, time.UTC)
	aliceID := domain.InstanceID("inst-alice")
	parentID := domain.PersonaRevisionID(3)
	inspection := domain.PersonaInspection{
		Nick: "Botty",
		Lineage: domain.PersonaLineage{
			InstanceID: "inst-botty", Baseline: "careful and curious",
			CurrentRevisionID: 4, Checkpoint: 12,
		},
		Revision: domain.PersonaRevision{
			ID: 4, InstanceID: "inst-botty", ParentID: &parentID, CreatedAt: at,
			Description:         "cares more about being right than about being easy to be around",
			DescriptionEvidence: []domain.ExperienceID{7, 8},
			ExperienceIDs:       []domain.ExperienceID{7, 8},
			AmendmentIDs:        []domain.PersonaAmendmentID{9},
		},
		Parent: &domain.PersonaRevision{
			ID: 3, InstanceID: "inst-botty",
			Description:   "careful and curious",
			ExperienceIDs: []domain.ExperienceID{7},
			AmendmentIDs:  []domain.PersonaAmendmentID{10, 11},
		},
		Experiences: []domain.Experience{
			{
				ID: 7, InstanceID: "inst-botty", Kind: domain.ExperienceObservation,
				Summary: "Alice supplied a reproduction.", Confidence: domain.ConfidenceHigh,
				Sources: []domain.ReflectionEventRef{{Sequence: 11}, {Sequence: 12}},
			},
			{
				ID: 8, InstanceID: "inst-botty", Kind: domain.ExperienceAssertion,
				Summary:   "Alice said the migration is safe to re-run.",
				SubjectID: &aliceID, Confidence: domain.ConfidenceMedium,
				Sources: []domain.ReflectionEventRef{{Sequence: 12}},
			},
		},
		Amendments: []domain.PersonaAmendment{{
			ID: 9, InstanceID: "inst-botty", Scope: domain.AmendmentRelationship,
			Counterpart: &aliceID,
			Tendency:    "Usually asks for evidence.", Confidence: domain.ConfidenceMedium,
			Evidence: []domain.ExperienceID{7},
		}},
		Departed: []domain.PersonaAmendment{{
			ID: 10, InstanceID: "inst-botty", Scope: domain.AmendmentGlobal,
			Tendency: "Gives people a figure.", Confidence: domain.ConfidenceLow,
			Evidence: []domain.ExperienceID{7},
			Departure: &domain.AmendmentDeparture{
				Kind: domain.AmendmentSuperseded, At: at,
			},
		}, {
			ID: 11, InstanceID: "inst-botty", Scope: domain.AmendmentGlobal,
			Tendency:   "Answers the question that was asked.",
			Confidence: domain.ConfidenceLow,
			Evidence:   []domain.ExperienceID{7},
		}},
		Counterparts: []domain.PersonaCounterpart{{
			InstanceID: "inst-alice", Nick: "Alice",
		}},
		RecentRuns: []domain.ReflectionRun{{
			ID: "reflection-12", InstanceID: "inst-botty",
			BaseRevisionID: 3, ResultRevisionID: 4,
			ModelID: "test/reflection", Outcome: domain.ReflectionAccepted,
			AcceptedExperiences: 1, AcceptedAmendments: 1,
			StartedAt: at.Add(-time.Second), FinishedAt: at,
		}},
		Transitions: []domain.PersonaTransition{{
			ID: 3, InstanceID: "inst-botty",
			FromRevisionID: 3, ToRevisionID: 4,
			Kind: domain.PersonaTransitionReflection, At: at,
		}},
	}
	cases := []personaFormatCase{
		{
			Name: "inspection",
			Result: chatcmd.PersonaResult{
				Action: chatcmd.PersonaInspected, Inspection: inspection,
			},
			Text: "Botty  r4  reflection  15:00\n" +
				"\n  cares more about being right than about being easy to be around\n" +
				"\nbuilt from  #7 #8" +
				"\nparent r3   careful and curious" +
				"\nbaseline    careful and curious" +
				"\n\nexperiences" +
				"\n  #7  observation  high  Alice supplied a reproduction." +
				"\n  #8  assertion by Alice  medium  Alice said the migration is safe to re-run." +
				"\n\ntendencies" +
				"\n  #9  relationship with Alice  medium  Usually asks for evidence." +
				"\n\ntendencies removed" +
				"\n  #10  Gives people a figure.  superseded r4" +
				"\n  #11  Answers the question that was asked." +
				"\n\nreflections" +
				"\n  15:00  accepted  r3 -> r4  1 experience  1 tendency change  test/reflection",
		},
		{
			Name: "reset empty state",
			Result: chatcmd.PersonaResult{
				Action: chatcmd.PersonaReset,
				Inspection: domain.PersonaInspection{
					Nick: "Botty",
					Lineage: domain.PersonaLineage{
						InstanceID: "inst-botty", Baseline: "careful and curious",
						CurrentRevisionID: 1, Checkpoint: 12,
					},
					Revision: domain.PersonaRevision{
						ID: 1, InstanceID: "inst-botty",
						Description:         "careful and curious",
						DescriptionEvidence: []domain.ExperienceID{},
						ExperienceIDs:       []domain.ExperienceID{},
						AmendmentIDs:        []domain.PersonaAmendmentID{},
					},
					Experiences:  []domain.Experience{},
					Amendments:   []domain.PersonaAmendment{},
					Counterparts: []domain.PersonaCounterpart{},
					RecentRuns:   []domain.ReflectionRun{},
					Transitions:  []domain.PersonaTransition{},
				},
			},
			Text: "Botty  r1  reset\n" +
				"\n  careful and curious\n" +
				"\nbaseline    careful and curious" +
				"\n\nexperiences\n  none" +
				"\n\ntendencies\n  none" +
				"\n\nreflections\n  none",
		},
		{
			Name: "operator description",
			Result: chatcmd.PersonaResult{
				Action: chatcmd.PersonaDescribed,
				Inspection: domain.PersonaInspection{
					Nick: "Botty",
					Lineage: domain.PersonaLineage{
						InstanceID: "inst-botty", Baseline: "careful and curious",
						CurrentRevisionID: 5, Checkpoint: 12,
					},
					Revision: domain.PersonaRevision{
						ID: 5, InstanceID: "inst-botty", ParentID: &parentID,
						Description:         "terse, and unbothered by that",
						DescriptionEvidence: []domain.ExperienceID{},
						ExperienceIDs:       []domain.ExperienceID{},
						AmendmentIDs:        []domain.PersonaAmendmentID{},
					},
					Parent: &domain.PersonaRevision{
						ID: 3, InstanceID: "inst-botty",
						Description:   "careful and curious",
						ExperienceIDs: []domain.ExperienceID{},
						AmendmentIDs:  []domain.PersonaAmendmentID{},
					},
					Experiences:  []domain.Experience{},
					Amendments:   []domain.PersonaAmendment{},
					Counterparts: []domain.PersonaCounterpart{},
					RecentRuns:   []domain.ReflectionRun{},
					Transitions: []domain.PersonaTransition{{
						ID: 4, InstanceID: "inst-botty",
						FromRevisionID: 3, ToRevisionID: 5,
						Kind: domain.PersonaTransitionOperator, At: at,
					}},
				},
			},
			Text: "Botty  r5  operator\n" +
				"\n  terse, and unbothered by that\n" +
				"\nparent r3   careful and curious" +
				"\nbaseline    careful and curious" +
				"\n\nexperiences\n  none" +
				"\n\ntendencies\n  none" +
				"\n\nreflections\n  none" +
				"\n\noperator changes" +
				"\n  15:00  operator  r3 -> r5",
		},
		{
			Name: "departed experience subject and relationship counterpart",
			Result: chatcmd.PersonaResult{
				Action: chatcmd.PersonaInspected,
				Inspection: domain.PersonaInspection{
					Nick: "Botty",
					Lineage: domain.PersonaLineage{
						InstanceID: "inst-botty", Baseline: "careful and curious",
						CurrentRevisionID: 4,
					},
					Revision: domain.PersonaRevision{
						ID: 4, InstanceID: "inst-botty", ParentID: &parentID,
						Description:         "careful and curious",
						DescriptionEvidence: []domain.ExperienceID{},
						ExperienceIDs:       []domain.ExperienceID{8},
						AmendmentIDs:        []domain.PersonaAmendmentID{9},
					},
					Parent: &domain.PersonaRevision{
						ID: 3, InstanceID: "inst-botty",
						Description:   "careful and curious",
						ExperienceIDs: []domain.ExperienceID{},
						AmendmentIDs:  []domain.PersonaAmendmentID{},
					},
					Experiences: []domain.Experience{{
						ID: 8, InstanceID: "inst-botty",
						Kind:      domain.ExperienceRelationship,
						Summary:   "Alice preferred a shorter answer.",
						SubjectID: &aliceID, Confidence: domain.ConfidenceLow,
						Sources: []domain.ReflectionEventRef{{Sequence: 12}},
					}},
					Amendments: []domain.PersonaAmendment{{
						ID: 9, InstanceID: "inst-botty",
						Scope: domain.AmendmentRelationship, Counterpart: &aliceID,
						Tendency:   "Keep earlier trust bounded.",
						Confidence: domain.ConfidenceLow,
						Evidence:   []domain.ExperienceID{7},
					}},
					Counterparts: []domain.PersonaCounterpart{},
					RecentRuns:   []domain.ReflectionRun{},
					Transitions:  []domain.PersonaTransition{},
				},
			},
			Text: "Botty  r4\n" +
				"\n  careful and curious\n" +
				"\nbaseline    careful and curious" +
				"\n\nexperiences" +
				"\n  #8  relationship with departed counterpart  low  Alice preferred a shorter answer." +
				"\n\ntendencies" +
				"\n  #9  relationship with departed counterpart  low  Keep earlier trust bounded." +
				"\n\nreflections\n  none",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			require.Equal(t, testCase.Text, formatPersonaResult(testCase.Result, nil, language.BritishEnglish))
		})
	}
}

// shadowRunLineEffect is the line `/persona` renders for one run.
type shadowRunLineEffect struct {
	Line string
}

// TestFormatPersonaResult_reports_what_a_shadow_run_proposed pins that
// shadow mode shows an operator something.
//
// A shadow run accepts nothing by design, so reporting its accepted
// counts renders zeroes for a run that read the events and proposed
// several changes. Shadow is the mode an operator picks to watch
// reflection before letting it commit.
func TestFormatPersonaResult_reports_what_a_shadow_run_proposed(t *testing.T) {
	at := time.Date(2026, 8, 27, 15, 0, 0, 0, time.UTC)
	inspection := domain.PersonaInspection{
		Nick: "Botty",
		Lineage: domain.PersonaLineage{
			InstanceID: "inst-botty", Baseline: "quiet", CurrentRevisionID: 1,
		},
		Revision: domain.PersonaRevision{ID: 1, Description: "quiet"},
		RecentRuns: []domain.ReflectionRun{{
			ID: "reflection-shadow", InstanceID: "inst-botty",
			BaseRevisionID: 1, ResultRevisionID: 1,
			ModelID: "test/reflection", Outcome: domain.ReflectionShadow,
			ProposedExperiences: 3, ProposedAmendments: 2,
			FinishedAt: at,
		}},
	}

	text := formatPersonaResult(chatcmd.PersonaResult{Inspection: inspection}, nil, language.BritishEnglish)
	line := ""
	for candidate := range strings.SplitSeq(text, "\n") {
		if strings.Contains(candidate, "  shadow  ") {
			line = candidate
		}
	}

	require.Equal(t, shadowRunLineEffect{
		Line: "  15:00  shadow  r1 -> r1  3 experiences proposed  " +
			"2 tendency changes proposed  test/reflection",
	}, shadowRunLineEffect{Line: line})
}
