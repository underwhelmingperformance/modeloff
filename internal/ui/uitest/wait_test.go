package uitest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fixedDeadlineTB reports a deadline of its own, standing in for the
// *testing.T an App normally holds.
type fixedDeadlineTB struct {
	testing.TB

	deadline time.Time
	timed    bool
}

func (tb fixedDeadlineTB) Deadline() (time.Time, bool) {
	return tb.deadline, tb.timed
}

// undeadlinedTB has no Deadline method at all, which is what a
// *testing.B gives an App.
type undeadlinedTB struct{ testing.TB }

type waitBudgetCase struct {
	Name   string
	Budget time.Duration
}

// TestApp_waitBudget_follows_the_test_binary_deadline pins that a wait is
// bounded by `go test -timeout` and by nothing this package chooses. The
// reserve is what leaves a failing wait time to report the view that never
// appeared.
func TestApp_waitBudget_follows_the_test_binary_deadline(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		tb   testing.TB
		want waitBudgetCase
	}{
		{
			name: "a deadline ahead leaves the reserve unspent",
			tb:   fixedDeadlineTB{deadline: now.Add(time.Minute), timed: true},
			want: waitBudgetCase{
				Name:   "a deadline ahead leaves the reserve unspent",
				Budget: time.Minute - waitReportingReserve,
			},
		},
		{
			name: "a deadline already reached waits no longer",
			tb:   fixedDeadlineTB{deadline: now, timed: true},
			want: waitBudgetCase{
				Name: "a deadline already reached waits no longer",
			},
		},
		{
			name: "an untimed run waits without a bound",
			tb:   fixedDeadlineTB{timed: false},
			want: waitBudgetCase{
				Name:   "an untimed run waits without a bound",
				Budget: waitWithoutDeadline,
			},
		},
		{
			name: "a TB with no deadline waits without a bound",
			tb:   undeadlinedTB{},
			want: waitBudgetCase{
				Name:   "a TB with no deadline waits without a bound",
				Budget: waitWithoutDeadline,
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			budget := (&App{t: testCase.tb}).waitBudget()

			// The elapsed time between building the case and reading the
			// budget comes off a live deadline, so round to the second the
			// case is expressed in.
			require.Equal(t, testCase.want, waitBudgetCase{
				Name: testCase.name, Budget: budget.Round(time.Second),
			})
		})
	}
}

// nearDeadlineEffect is what a wait asks for when the deadline is closer
// than the reserve.
type nearDeadlineEffect struct {
	WithinDeadline bool
	Negative       bool
}

// TestApp_waitBudget_never_outlives_the_deadline pins the case the
// rounded table cannot express, because the budget it produces is under
// a second and the elapsed time between building the case and reading it
// moves the last digits.
//
// Waiting the reserve here would run past the deadline, and the binary
// would be killed before the helper could report the view that never
// appeared, which is what the reserve exists to leave time for.
func TestApp_waitBudget_never_outlives_the_deadline(t *testing.T) {
	deadline := time.Now().Add(waitReportingReserve / 2)
	app := &App{t: fixedDeadlineTB{deadline: deadline, timed: true}}

	remaining := time.Until(deadline)
	budget := app.waitBudget()

	require.Equal(t, nearDeadlineEffect{WithinDeadline: true}, nearDeadlineEffect{
		WithinDeadline: budget <= remaining,
		Negative:       budget < 0,
	})
}
