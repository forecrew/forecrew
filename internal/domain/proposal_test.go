package domain

import (
	"errors"
	"testing"
	"time"
)

func TestProposalTransitions(t *testing.T) {
	checkTable(t, ProposalStatuses(), []pair[ProposalStatus]{
		{ProposalProposed, ProposalAccepted}, {ProposalProposed, ProposalRejected}, {ProposalProposed, ProposalSnoozed},
		{ProposalProposed, ProposalSuperseded}, {ProposalSnoozed, ProposalProposed}, {ProposalSnoozed, ProposalAccepted},
		{ProposalSnoozed, ProposalRejected}, {ProposalSnoozed, ProposalSuperseded},
		{ProposalAccepted, ProposalDone}, {ProposalAccepted, ProposalSuperseded},
	}, ProposalStatus.CanTransition, ProposalStatus.Transition)
	for _, s := range ProposalStatuses() {
		final := s == ProposalRejected || s == ProposalDone || s == ProposalSuperseded
		if s.Final() != final {
			t.Errorf("%s.Final() = %v", s, s.Final())
		}
	}
}

func proposal() Proposal {
	return Proposal{Scope: ScopeProject, Category: CategoryWorkers, Title: "Add a macOS worker", Rationale: "Jobs wait",
		Evidence: []ProposalEvidence{{Kind: "workers", Ref: "JOB-41", Detail: "waited 3h for macos"}}, Benefit: "No waiting",
		Effort: EffortS, Risk: LevelLow, Action: ProposalAction{Setup: &SetupAction{Instructions: "Run forecrew worker add"}},
		Status: ProposalProposed, DedupeKey: "workers:macos", EvidenceWeight: 3, Measure: &ProposalMeasure{Detector: "workers", Value: 3}}
}

func TestProposalValidate(t *testing.T) {
	wantInvalid(t, "valid", proposal().Validate())
	p := proposal()
	p.Action = ProposalAction{ConfigPR: &ConfigPRAction{Files: []ConfigFile{{Path: ".forecrew/matrix.yaml", Content: "version: 1"}}}}
	wantInvalid(t, "config pr", p.Validate())
	p.Action.ConfigPR.Files = append(p.Action.ConfigPR.Files, ConfigFile{Path: ".github/workflows/ci.yml"}, ConfigFile{Path: ".forecrew/../x"})
	p.Scope = ScopePlatform
	wantInvalid(t, "config pr outside", p.Validate(), `".github/workflows/ci.yml" must be a file under .forecrew/`, `".forecrew/../x"`, "platform proposal is a setup action")
	p = proposal()
	p.Action = ProposalAction{Goal: &GoalAction{Title: "Trace checkout"}, Setup: &SetupAction{Instructions: "x"}}
	wantInvalid(t, "two actions", p.Validate(), "exactly one of")
	p.Action = ProposalAction{Goal: &GoalAction{}, ConfigPR: &ConfigPRAction{}}
	wantInvalid(t, "empty actions", p.Validate(), "action.goal.title must be set", "config_pr needs files", "exactly one of")
	empty := Proposal{EvidenceWeight: -1, Measure: &ProposalMeasure{}, Action: ProposalAction{Setup: &SetupAction{}}}
	wantInvalid(t, "empty", empty.Validate(), "scope must be one of", "category must be one of", "effort must be one of",
		"risk must be one of", "status must be one of", "title must be set", "rationale must be set", "benefit must be set",
		"dedupe_key must be set", "evidence_weight must not be negative", "measure.detector must be set", "action.setup.instructions must be set")
}

func TestProposalDecide(t *testing.T) {
	by := Actor{Kind: ActorMCP, Name: "laptop"}
	p := proposal()
	wantInvalid(t, "reject without reason", p.Decide(by, ProposalReject, "", time.Time{}, now), "reason must be set")
	wantInvalid(t, "snooze in the past", p.Decide(by, ProposalSnooze, "", now, now), "time in the future")
	wantInvalid(t, "unknown", p.Decide(by, "later", "", time.Time{}, now), "accept, reject or snooze")
	until := now.Add(24 * time.Hour)
	if err := p.Decide(by, ProposalSnooze, "after the release", until, now); err != nil || p.Status != ProposalSnoozed || !p.SnoozedUntil.Equal(until) {
		t.Fatalf("snooze = %+v, %v", p, err)
	}
	if err := p.Decide(by, ProposalAccept, "", time.Time{}, now); err != nil || p.Status != ProposalAccepted || !p.SnoozedUntil.IsZero() || p.DecidedBy != "mcp:laptop" {
		t.Fatalf("accept = %+v, %v", p, err)
	}
	if err := p.Decide(by, ProposalReject, "no", time.Time{}, now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("reject after accept = %v", err)
	}
	p = proposal()
	if err := p.Decide(by, ProposalReject, "we use another tool", time.Time{}, now); err != nil || p.Status != ProposalRejected || p.Reason == "" {
		t.Fatalf("reject = %+v, %v", p, err)
	}
}
