package modelmanager

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
)

// recordingOperatorNotices collects what a manager reports to its
// operators, in the order it reports it.
type recordingOperatorNotices struct {
	notices []domain.SystemNotice
}

func (r *recordingOperatorNotices) NoticeOperators(_ context.Context, notice domain.SystemNotice) {
	r.notices = append(r.notices, notice)
}

// reflectionNoticeCase is one terminal reflection outcome.
type reflectionNoticeCase struct {
	Name string
	Run  domain.ReflectionRun
}

// reflectionNoticeEffect is the line an operator reads for a case.
type reflectionNoticeEffect struct {
	Text string
}

func TestReflectionRunNotice(t *testing.T) {
	cases := []reflectionNoticeCase{
		{
			Name: "accepted",
			Run: domain.ReflectionRun{
				Outcome:        domain.ReflectionAccepted,
				BaseRevisionID: 3, ResultRevisionID: 4,
				AcceptedExperiences: 2, AcceptedAmendments: 1,
			},
		},
		{
			Name: "no change",
			Run: domain.ReflectionRun{
				Outcome:        domain.ReflectionNoChange,
				BaseRevisionID: 4, ResultRevisionID: 4,
			},
		},
		{
			Name: "shadow",
			Run: domain.ReflectionRun{
				Outcome:             domain.ReflectionShadow,
				ProposedExperiences: 3, ProposedAmendments: 2,
			},
		},
		{
			Name: "rejected",
			Run: domain.ReflectionRun{
				Outcome:         domain.ReflectionRejected,
				RejectionReason: "role_prefixed:experiences[0].summary",
			},
		},
		{
			Name: "stale",
			Run: domain.ReflectionRun{
				Outcome:         domain.ReflectionStale,
				RejectionReason: "persona_lineage_changed",
			},
		},
		{
			Name: "discarded",
			Run: domain.ReflectionRun{
				Outcome:         domain.ReflectionDiscarded,
				RejectionReason: reflectionDiscardDisabled,
			},
		},
		{
			Name: "failed",
			Run: domain.ReflectionRun{
				Outcome:         domain.ReflectionFailed,
				RejectionReason: reflectionFailureUpstream,
			},
		},
	}

	want := []reflectionNoticeEffect{
		{Text: "Reflection for botty accepted a revision: revision 3 -> 4, 2 experience(s), 1 tendency change(s)."},
		{Text: "Reflection for botty found nothing to record."},
		{Text: "Reflection for botty validated a proposal in shadow mode: 3 experience(s) and 2 tendency change(s) proposed."},
		{Text: "Reflection for botty had its proposal refused (role_prefixed:experiences[0].summary)."},
		{Text: "Reflection for botty lost its proposal to a concurrent persona change (persona_lineage_changed)."},
		{Text: "Reflection for botty discarded its proposal (reflection_disabled)."},
		{Text: "Reflection for botty failed (upstream_failed)."},
	}

	got := make([]reflectionNoticeEffect, 0, len(cases))
	for _, testCase := range cases {
		t.Run(testCase.Name, func(*testing.T) {
			got = append(got, reflectionNoticeEffect{
				Text: reflectionRunNotice("botty", testCase.Run),
			})
		})
	}

	require.Equal(t, want, got)
}

// TestManager_notices_a_reflection_recorded_without_committing covers
// the outcomes `runReflection` records for itself. `reflection_runs`
// is the only other place they appear, and nothing reads it until an
// operator runs `/persona` against the instance.
func TestManager_notices_a_reflection_recorded_without_committing(t *testing.T) {
	stored, instance := reflectionSchedulerStore(t)
	fixed := time.Date(2026, 8, 26, 20, 15, 0, 0, time.UTC)
	require.NoError(t, stored.AppendReflectionEvents(
		t.Context(), instance.ID(),
		reflectionCandidates(1, reflectionSubstantiveThreshold, fixed),
		fixed,
	))
	snapshot, err := stored.PendingReflectionSnapshot(
		t.Context(), instance.ID(), reflectionInputEventLimit,
	)
	require.NoError(t, err)
	client := &apitest.Fake{
		ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
			return []api.ModelInfo{{
				ID:                  "test/reflection",
				SupportedParameters: []string{"tools", "structured_outputs"},
			}}, nil
		},
		ProposeReflectionFn: func(
			context.Context,
			*api.Conversation,
			[]api.ToolResult,
			api.StructuredOutputSupport,
		) (api.ReflectionResult, error) {
			return api.ReflectionResult{Proposal: api.ReflectionProposal{
				Experiences: []api.ReflectionExperienceProposal{},
				Amendments:  []api.ReflectionAmendmentProposal{},
				Retract:     []string{},
			}}, nil
		},
	}
	manager := New(Config{
		Store: stored, APIClient: client, InitialAPIKey: "configured",
		BaseContext: t.Context, Now: func() time.Time { return fixed },
		ReflectionMode: ReflectionShadow, ReflectionModel: "test/reflection",
		ReflectionRunID: func() domain.ReflectionRunID { return "shadow-1" },
	})
	notices := &recordingOperatorNotices{}
	manager.notices = notices

	manager.runReflection(t.Context(), snapshot)
	require.NoError(t, manager.DetachAll(t.Context()))

	require.Equal(t, []domain.SystemNotice{{
		Target: domain.StatusChannelName,
		Text: "Reflection for botty validated a proposal in shadow mode: " +
			"0 experience(s) and 0 tendency change(s) proposed.",
		At: fixed,
	}}, notices.notices)
}

// TestManager_notices_a_committed_reflection covers the outcome the
// store writes inside the commit transaction, which is the one
// `runReflection` never records for itself.
func TestManager_notices_a_committed_reflection(t *testing.T) {
	stored, instance := reflectionSchedulerStore(t)
	fixed := time.Date(2026, 8, 26, 20, 15, 0, 0, time.UTC)
	require.NoError(t, stored.AppendReflectionEvents(
		t.Context(), instance.ID(),
		reflectionCandidates(1, reflectionSubstantiveThreshold, fixed),
		fixed,
	))
	snapshot, err := stored.PendingReflectionSnapshot(
		t.Context(), instance.ID(), reflectionInputEventLimit,
	)
	require.NoError(t, err)
	client := &apitest.Fake{
		ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
			return []api.ModelInfo{{
				ID:                  "test/reflection",
				SupportedParameters: []string{"tools", "structured_outputs"},
			}}, nil
		},
	}
	manager := New(Config{
		Store: stored, APIClient: client, InitialAPIKey: "configured",
		BaseContext: t.Context, Now: func() time.Time { return fixed },
		ReflectionMode: ReflectionActive, ReflectionModel: "test/reflection",
		ReflectionRunID: func() domain.ReflectionRunID { return "committed-1" },
	})
	notices := &recordingOperatorNotices{}
	manager.notices = notices

	manager.runReflection(t.Context(), snapshot)
	require.NoError(t, manager.DetachAll(t.Context()))

	require.Equal(t, []domain.SystemNotice{{
		Target: domain.StatusChannelName,
		Text:   "Reflection for botty found nothing to record.",
		At:     fixed,
	}}, notices.notices)
}
