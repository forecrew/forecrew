package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGoalKindAndSize(t *testing.T) {
	if GoalKind("").OrDefault() != GoalFeature || GoalBug.OrDefault() != GoalBug || len(GoalKinds()) != 2 {
		t.Fatal("GoalKind")
	}
	s := GoalSize{Impact: LevelHigh, Reversibility: ReversibleHard, Councils: []CouncilKind{CouncilProduct, CouncilTechnical}, Reason: "touches billing"}
	wantInvalid(t, "valid", s.Validate())
	s = GoalSize{Impact: "huge", Reversibility: "never", Councils: []CouncilKind{CouncilIncident}}
	wantInvalid(t, "invalid", s.Validate(), "impact must be one of", "reversibility must be one of", "councils must be one of", "reason must be set")
	b, err := json.Marshal(Goal{Title: "x"})
	if err != nil || strings.Contains(string(b), `"size"`) || strings.Contains(string(b), `"kind"`) {
		t.Fatalf("goal json = %s, %v", b, err)
	}
}

func TestContextManifestJSON(t *testing.T) {
	j := Job{ContextManifest: &ContextManifest{Budget: 100, Size: 60, Sections: []ContextSection{{
		Name: "knowledge", Size: 60, Items: []ContextItem{{Path: "docs/adr/0001.md", Size: 60}},
		Dropped: []ContextItem{{ID: "A-4", Size: 90}},
	}}}}
	b, err := json.Marshal(j)
	want := `"context_manifest":{"budget":100,"size":60,"sections":[{"name":"knowledge","size":60,"items":[{"path":"docs/adr/0001.md","size":60}],"dropped":[{"id":"A-4","size":90}]}]}`
	if err != nil || !strings.Contains(string(b), want) {
		t.Fatalf("json = %s, %v", b, err)
	}
	if b, _ := json.Marshal(Job{}); strings.Contains(string(b), "context_manifest") {
		t.Fatalf("empty manifest in %s", b)
	}
}

func TestGateOrigin(t *testing.T) {
	if GateOrigin("").OrDefault() != GateFromPlaybook || GateFromAskHuman.OrDefault() != GateFromAskHuman {
		t.Fatal("OrDefault")
	}
	ask := Gate{Kind: GateNeedsYou, Origin: GateFromAskHuman, JobID: NewID()}
	if !ask.AsksHuman() || (Gate{Kind: GateNeedsYou}).AsksHuman() {
		t.Fatal("AsksHuman")
	}
	if b, _ := json.Marshal(Gate{}); strings.Contains(string(b), "origin") {
		t.Fatalf("playbook gate json = %s", b)
	}
}
