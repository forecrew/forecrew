package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCouncilTransitions(t *testing.T) {
	checkTable(t, CouncilStatuses(), []pair[CouncilStatus]{
		{CouncilRunning, CouncilDecided}, {CouncilRunning, CouncilFailed}, {CouncilRunning, CouncilCanceled},
	}, CouncilStatus.CanTransition, CouncilStatus.Transition)
	if !CouncilFailed.Terminal() || CouncilRunning.Terminal() {
		t.Error("Terminal")
	}
}

func brief() *CouncilBrief {
	return &CouncilBrief{
		Question: "Which queue?",
		Options: []BriefOption{
			{Name: "postgres", Summary: "SKIP LOCKED", Cost: LevelLow, Risk: LevelLow},
			{Name: "nats", Summary: "A broker", Cost: LevelMedium, Risk: LevelMedium},
		},
		Recommendation: "postgres",
		Votes:          []BriefVote{{Member: "architect", Option: "postgres", Reason: "fewer parts"}},
		Assumptions:    []StatedAssumption{stated()},
		Reversible:     true,
	}
}

func TestCouncilValidate(t *testing.T) {
	ok := Council{Kind: CouncilTechnical, Status: CouncilDecided, Question: "Which queue?", Brief: brief()}
	wantInvalid(t, "valid", ok.Validate())
	running := Council{Kind: CouncilAdhoc, Status: CouncilRunning, Question: "Q"}
	wantInvalid(t, "running without brief", running.Validate())

	bad := ok
	bad.Kind, bad.Status, bad.Question, bad.Constraints = "debate", "done", " ", []string{""}
	wantInvalid(t, "fields", bad.Validate(), "kind must be one of", "status must be one of", "question must be set", "constraints must be set")
	nobrief := ok
	nobrief.Brief = nil
	wantInvalid(t, "decided without brief", nobrief.Validate(), "needs a brief")

	b := brief()
	b.Options = append(b.Options, BriefOption{Name: "nats"})
	b.Recommendation = "kafka"
	b.Votes = append(b.Votes, BriefVote{Member: "devils-advocate", Option: "redis"})
	b.Assumptions = []StatedAssumption{{Statement: "x", Confidence: "sure", Impact: LevelLow, VerifyBy: "guess"}}
	wantInvalid(t, "brief", b.Validate(), `"nats" is listed twice`, `"kafka" is not an option`, `votes for "redis"`, "confidence must be one of", "verify_by must be one of")
	wantInvalid(t, "valid brief", brief().Validate())
}

func TestCouncilContributionValidate(t *testing.T) {
	c := CouncilContribution{Role: "architect", Runtime: "codex", Stage: StageRevision, Round: MaxRevisionRounds, Content: json.RawMessage(`{"position":"x"}`),
		EvidenceCheck: []SourceCheck{{Source: "https://example.com/bench", Kind: SourceURL, Status: SourceOK, ArtifactID: NewID()}}}
	wantInvalid(t, "valid human round", c.Validate())
	c.Round = MaxRevisionRounds + 1
	wantInvalid(t, "revision round 4", c.Validate(), "round of a revision must be from 1 to 3")
	c.Stage, c.Round = StageSynthesis, MaxSynthesisRounds
	wantInvalid(t, "second synthesis", c.Validate())
	c.Round = 3
	wantInvalid(t, "third synthesis", c.Validate(), "round of a synthesis must be from 1 to 2")
	c.Stage, c.Round = StageProposal, 2
	wantInvalid(t, "proposal round 2", c.Validate(), "round of a proposal must be from 1 to 1")
	c = CouncilContribution{Stage: "vote", Round: 0, Content: json.RawMessage(`{`), EvidenceCheck: []SourceCheck{{Kind: "rumor", Status: "maybe"}}}
	wantInvalid(t, "fields", c.Validate(), "role must be set", "stage must be one of", "content must be JSON",
		"evidence_check.source must be set", "evidence_check.kind must be one of", "evidence_check.status must be one of")
	if b, _ := json.Marshal(CouncilContribution{}); !strings.Contains(string(b), `"evidence_check":null`) {
		t.Fatalf("unchecked contribution json = %s", b)
	}
}
