package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRegisterTransitions(t *testing.T) {
	checkTable(t, AssumptionStatuses(), []pair[AssumptionStatus]{
		{AssumptionUnverified, AssumptionVerified}, {AssumptionUnverified, AssumptionInvalidated},
		{AssumptionUnverified, AssumptionAccepted}, {AssumptionAccepted, AssumptionVerified},
		{AssumptionAccepted, AssumptionInvalidated}, {AssumptionVerified, AssumptionInvalidated},
	}, AssumptionStatus.CanTransition, AssumptionStatus.Transition)
	if !AssumptionUnverified.Open() || AssumptionAccepted.Open() {
		t.Error("Open")
	}
}

// wantInvalid checks that err wraps ErrInvalidRecord and names every
// problem in want.
func wantInvalid(t *testing.T, name string, err error, want ...string) {
	t.Helper()
	if len(want) == 0 {
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		return
	}
	if !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("%s: err = %v, want ErrInvalidRecord", name, err)
		return
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("%s: %v lacks %q", name, err, w)
		}
	}
}

func stated() StatedAssumption {
	return StatedAssumption{Statement: "One instance", Confidence: LevelMedium, Impact: LevelHigh, VerifyBy: VerifyByTest}
}

func TestAssumption(t *testing.T) {
	a := Assumption{Number: 4, MadeBy: "architect", StatedAssumption: stated(), Status: AssumptionUnverified,
		Dependents: []Dependent{{Kind: DependentTask, ID: "TASK-12"}}}
	wantInvalid(t, "valid", a.Validate())
	b, err := json.Marshal(a)
	if err != nil || !strings.Contains(string(b), `"statement":"One instance"`) || !strings.Contains(string(b), `"verify_by":"test"`) {
		t.Fatalf("json = %s, %v", b, err)
	}
	bad := Assumption{Status: "maybe", Dependents: []Dependent{{Kind: "wiki"}}}
	wantInvalid(t, "fields", bad.Validate(), "statement must be set", "made_by must be set", "status must be one of",
		"dependents.kind must be one of", "dependents.id must be set", "impact must be one of")
	wantInvalid(t, "stated", StatedAssumption{}.Validate(), "statement must be set")

	by := Actor{Kind: ActorUser, Name: "ana"}
	wantInvalid(t, "verify without evidence", a.Resolve(by, AssumptionVerified, " ", now), "verified needs evidence")
	if err := a.Resolve(by, AssumptionAccepted, "", now); err != nil || a.Status != AssumptionAccepted || a.ResolvedBy != "user:ana" || !a.ResolvedAt.Equal(now) {
		t.Fatalf("accept = %+v, %v", a, err)
	}
	if err := a.Resolve(by, AssumptionInvalidated, "the test fails", now); err != nil || a.Evidence != "the test fails" {
		t.Fatalf("invalidate = %+v, %v", a, err)
	}
	if err := a.Resolve(by, AssumptionVerified, "it passes", now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("resolve after invalidated = %v", err)
	}
}

func TestDecisionValidate(t *testing.T) {
	d := Decision{Question: "Q", Choice: "postgres", DecidedBy: "user:ana", Options: []DecisionOption{{Name: "postgres"}}}
	wantInvalid(t, "valid", d.Validate())
	d.DecidedBy = DecidedByPolicy
	wantInvalid(t, "policy without council", d.Validate(), "needs a council")
	d.CouncilID = NewID()
	wantInvalid(t, "policy", d.Validate())
	wantInvalid(t, "empty", Decision{Options: []DecisionOption{{}}}.Validate(), "question must be set", "choice must be set", "decided_by must be set", "options.name must be set")
}
