package domain

import (
	"slices"
	"testing"
)

func TestTestSystemEnums(t *testing.T) {
	if len(TestRunStatuses()) != 3 || len(TestStatuses()) != 4 || len(VerificationStatuses()) != 3 || len(EnvironmentProviders()) != 3 {
		t.Fatal("enum lists changed; update the CHECK constraints and the contracts too")
	}
}

func TestTestPlanApprovalGate(t *testing.T) {
	if !slices.Contains(GateKinds(), GateTestPlanApproval) {
		t.Fatal("GateKinds lacks test_plan_approval")
	}
	for _, d := range []GateStatus{GateApproved, GateChangesRequested, GateRejected} {
		if !GateTestPlanApproval.AllowsDecision(d) {
			t.Errorf("test_plan_approval refuses %s", d)
		}
	}
}
