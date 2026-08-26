package modelmanager

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/store"
)

type reflectionScheduleCase struct {
	Name        string
	Status      store.ReflectionInboxStatus
	LastAttempt time.Time
	Now         time.Time
	Want        reflectionSchedule
}

func TestReflectionSchedule_uses_activity_and_cooldown(t *testing.T) {
	now := time.Date(2026, 8, 26, 18, 0, 0, 0, time.UTC)
	recent := now.Add(-reflectionCooldown / 2)
	ready := now.Add(-reflectionCooldown)
	cases := []reflectionScheduleCase{
		{
			Name: "below activity threshold",
			Status: store.ReflectionInboxStatus{
				Checkpoint: 10, HighWaterMark: 30,
				PendingEvents: 20, SubstantiveEvents: reflectionSubstantiveThreshold - 1,
			},
			Now: now,
			Want: reflectionSchedule{
				Checkpoint: 10, HighWaterMark: 30,
				SubstantiveEvents: reflectionSubstantiveThreshold - 1,
			},
		},
		{
			Name: "cooldown active",
			Status: store.ReflectionInboxStatus{
				Checkpoint: 10, HighWaterMark: 31,
				PendingEvents: 21, SubstantiveEvents: reflectionSubstantiveThreshold,
				LastAttemptAt: &recent,
			},
			Now: now,
			Want: reflectionSchedule{
				Checkpoint: 10, HighWaterMark: 31,
				SubstantiveEvents: reflectionSubstantiveThreshold,
				DueAt:             recent.Add(reflectionCooldown),
			},
		},
		{
			Name: "first reflection ready",
			Status: store.ReflectionInboxStatus{
				Checkpoint: 0, HighWaterMark: 12,
				PendingEvents: 12, SubstantiveEvents: reflectionSubstantiveThreshold,
			},
			Now: now,
			Want: reflectionSchedule{
				Checkpoint: 0, HighWaterMark: 12,
				SubstantiveEvents: reflectionSubstantiveThreshold,
				Ready:             true,
			},
		},
		{
			Name: "cooldown from an unrecorded attempt",
			Status: store.ReflectionInboxStatus{
				Checkpoint: 10, HighWaterMark: 31,
				PendingEvents: 21, SubstantiveEvents: reflectionSubstantiveThreshold,
			},
			LastAttempt: recent,
			Now:         now,
			Want: reflectionSchedule{
				Checkpoint: 10, HighWaterMark: 31,
				SubstantiveEvents: reflectionSubstantiveThreshold,
				DueAt:             recent.Add(reflectionCooldown),
			},
		},
		{
			Name: "cooldown elapsed",
			Status: store.ReflectionInboxStatus{
				Checkpoint: 12, HighWaterMark: 27,
				PendingEvents: 15, SubstantiveEvents: reflectionSubstantiveThreshold + 1,
				LastAttemptAt: &ready,
			},
			Now: now,
			Want: reflectionSchedule{
				Checkpoint: 12, HighWaterMark: 27,
				SubstantiveEvents: reflectionSubstantiveThreshold + 1,
				DueAt:             ready.Add(reflectionCooldown), Ready: true,
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			require.Equal(t, testCase.Want, reflectionScheduleFor(
				testCase.Status, testCase.LastAttempt, testCase.Now,
			))
		})
	}
}
