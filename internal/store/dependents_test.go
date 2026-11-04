package store

import (
	"slices"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
)

func TestMergeDependents(t *testing.T) {
	goal := domain.Dependent{Kind: domain.DependentGoal, ID: "GOAL-1"}
	task := domain.Dependent{Kind: domain.DependentTask, ID: "TASK-1"}
	cur := []domain.Dependent{goal}
	got := MergeDependents(cur, []domain.Dependent{task, goal, task})
	if !slices.Equal(got, []domain.Dependent{goal, task}) {
		t.Fatalf("merged = %+v", got)
	}
	if len(cur) != 1 {
		t.Fatalf("the argument changed: %+v", cur)
	}
	if got := MergeDependents(nil, nil); len(got) != 0 {
		t.Fatalf("merged nothing = %+v", got)
	}
}
