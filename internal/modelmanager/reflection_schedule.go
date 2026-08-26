package modelmanager

import (
	"time"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/store"
)

const (
	reflectionSubstantiveThreshold = 12
	reflectionCooldown             = time.Hour
)

type reflectionSchedule struct {
	Checkpoint        domain.ReflectionSequence
	HighWaterMark     domain.ReflectionSequence
	SubstantiveEvents int
	DueAt             time.Time
	Ready             bool
}

// reflectionScheduleFor decides whether an instance may reflect now. An
// attempt is due once the cooldown has run out from the later of the last
// attempt the store recorded and lastAttempt, which is the worker's own
// record of the attempt it made.
//
// The two are both needed. The stored time is the only one that survives a
// restart, and the worker's own is the only one that applies when a run
// ends without recording anything, which would otherwise leave the worker
// free to call the provider again immediately.
func reflectionScheduleFor(
	status store.ReflectionInboxStatus,
	lastAttempt time.Time,
	now time.Time,
) reflectionSchedule {
	schedule := reflectionSchedule{
		Checkpoint: status.Checkpoint, HighWaterMark: status.HighWaterMark,
		SubstantiveEvents: status.SubstantiveEvents,
		DueAt:             reflectionDueAt(status.LastAttemptAt, lastAttempt),
	}
	if status.SubstantiveEvents < reflectionSubstantiveThreshold {
		return schedule
	}
	if !schedule.DueAt.IsZero() && now.Before(schedule.DueAt) {
		return schedule
	}

	schedule.Ready = true
	return schedule
}

func reflectionDueAt(recorded *time.Time, attempted time.Time) time.Time {
	latest := attempted
	if recorded != nil && recorded.After(latest) {
		latest = *recorded
	}
	if latest.IsZero() {
		return time.Time{}
	}

	return latest.Add(reflectionCooldown)
}
