package domain

import (
	"errors"
	"testing"
)

// pair is an allowed (from, to) move.
type pair[S ~string] struct{ from, to S }

// checkTable tries every (from, to) pair of states and compares the result
// with the allowed set written out by hand.
func checkTable[S ~string](t *testing.T, all []S, allowed []pair[S], can func(S, S) bool, apply func(S, S) (S, error)) {
	t.Helper()
	want := map[pair[S]]bool{}
	for _, p := range allowed {
		want[p] = true
	}
	for _, from := range all {
		for _, to := range all {
			p := pair[S]{from, to}
			if got := can(from, to); got != want[p] {
				t.Errorf("CanTransition(%s -> %s) = %v, want %v", from, to, got, want[p])
			}
			got, err := apply(from, to)
			switch {
			case want[p] && (err != nil || got != to):
				t.Errorf("Transition(%s -> %s) = %s, %v; want %s, nil", from, to, got, err, to)
			case !want[p] && (!errors.Is(err, ErrInvalidTransition) || got != from):
				t.Errorf("Transition(%s -> %s) = %s, %v; want %s, ErrInvalidTransition", from, to, got, err, from)
			}
		}
	}
}

func TestProjectTransitions(t *testing.T) {
	checkTable(t, ProjectStatuses(), []pair[ProjectStatus]{
		{ProjectRegistered, ProjectOnboarding},
		{ProjectOnboarding, ProjectOnboarded},
		{ProjectOnboarding, ProjectRegistered},
	}, ProjectStatus.CanTransition, ProjectStatus.Transition)
}

func TestJobTransitions(t *testing.T) {
	checkTable(t, JobStatuses(), []pair[JobStatus]{
		{JobQueued, JobRunning},
		{JobQueued, JobCanceled},
		{JobRunning, JobSucceeded},
		{JobRunning, JobFailed},
		{JobRunning, JobCanceled},
		{JobRunning, JobQueued},
	}, JobStatus.CanTransition, JobStatus.Transition)
}

func TestGateTransitions(t *testing.T) {
	checkTable(t, GateStatuses(), []pair[GateStatus]{
		{GatePending, GateApproved},
		{GatePending, GateChangesRequested},
		{GatePending, GateRejected},
	}, GateStatus.CanTransition, GateStatus.Transition)
}

func TestGoalTransitions(t *testing.T) {
	checkTable(t, GoalStatuses(), []pair[GoalStatus]{
		{GoalDraft, GoalSpecReview},
		{GoalSpecReview, GoalPlanned},
		{GoalPlanned, GoalInProgress},
		{GoalInProgress, GoalDone},
		{GoalDraft, GoalCanceled},
		{GoalSpecReview, GoalCanceled},
		{GoalPlanned, GoalCanceled},
		{GoalInProgress, GoalCanceled},
	}, GoalStatus.CanTransition, GoalStatus.Transition)
}

func TestTaskTransitions(t *testing.T) {
	checkTable(t, TaskStatuses(), []pair[TaskStatus]{
		{TaskOpen, TaskInProgress},
		{TaskInProgress, TaskInReview},
		{TaskInReview, TaskMergeReady},
		{TaskMergeReady, TaskMerged},
		{TaskInReview, TaskInProgress},
		{TaskMergeReady, TaskInProgress},
		{TaskOpen, TaskClosed},
		{TaskInProgress, TaskClosed},
		{TaskInReview, TaskClosed},
		{TaskMergeReady, TaskClosed},
	}, TaskStatus.CanTransition, TaskStatus.Transition)
}

func TestTerminal(t *testing.T) {
	checkTerminal(t, JobStatuses(), JobStatus.Terminal, JobSucceeded, JobFailed, JobCanceled)
	checkTerminal(t, GateStatuses(), GateStatus.Terminal, GateApproved, GateChangesRequested, GateRejected)
	checkTerminal(t, GoalStatuses(), GoalStatus.Terminal, GoalDone, GoalCanceled)
	checkTerminal(t, TaskStatuses(), TaskStatus.Terminal, TaskMerged, TaskClosed)
}

// checkTerminal also verifies that Terminal agrees with the table: a state is
// terminal exactly when no transition leaves it.
func checkTerminal[S ~string](t *testing.T, all []S, terminal func(S) bool, want ...S) {
	t.Helper()
	wantSet := map[S]bool{}
	for _, s := range want {
		wantSet[s] = true
	}
	for _, s := range all {
		if terminal(s) != wantSet[s] {
			t.Errorf("Terminal(%s) = %v, want %v", s, terminal(s), wantSet[s])
		}
	}
}
