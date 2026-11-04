package councils

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/testkit"
)

// resolved returns the shipped role as roles.Catalog would resolve it.
func resolved(t *testing.T, name string) roles.Role {
	t.Helper()
	d, err := lookup(t, forecrew.Files)(name)
	if err != nil {
		t.Fatal(err)
	}
	return roles.Role{
		Name: d.Name, Runtime: "claude-code", Model: "claude-best", WritePaths: d.WritePaths,
		Instructions: d.Instructions, OutputSchema: d.OutputSchema, AllowedTools: d.AllowedTools(),
		MaxTurns: d.MaxTurns, Timeout: d.Timeout, DefinitionOfDone: d.DefinitionOfDone,
		EscalateWhen: d.EscalateWhen, Pool: d.Pool, Toolbox: d.Toolbox,
	}
}

func seat(t *testing.T, s *Set, kind domain.CouncilKind, role string) Seat {
	t.Helper()
	p, err := s.Pack(kind)
	if err != nil {
		t.Fatal(err)
	}
	if role == p.Facilitator {
		return Seat{Role: role}
	}
	for _, m := range p.Members {
		if m.Role == role {
			return m
		}
	}
	t.Fatalf("no seat %s in %s", role, kind)
	return Seat{}
}

// TestPromptGoldens renders the full job prompt (contextpack.Render) of
// one seat per step of the technical council.
func TestPromptGoldens(t *testing.T) {
	s := shipped(t)
	turns := []struct {
		step  Step
		round int
		role  string
	}{
		{StepFrame, 1, "facilitator"},
		{StepProposal, 1, "architect"},
		{StepCritique, 1, DevilsAdvocate},
		{StepRevision, 2, "security-reviewer"},
		{StepCheck, 1, "facilitator"},
		{StepSynthesis, 1, "facilitator"},
	}
	for _, tt := range turns {
		t.Run(string(tt.step), func(t *testing.T) {
			r, err := s.Job(Turn{Kind: domain.CouncilTechnical, Step: tt.step, Round: tt.round, Seat: seat(t, s, domain.CouncilTechnical, tt.role)}, resolved(t, tt.role))
			if err != nil {
				t.Fatalf("Job: %v", err)
			}
			pack, err := contextpack.Render(r, contextpack.Input{Repo: "acme/shop", BaseRef: "main"}, 0)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			for _, leak := range []string{"<no value>", "{{", "}}"} {
				if strings.Contains(pack.Prompt, leak) {
					t.Errorf("prompt contains %q", leak)
				}
			}
			testkit.CheckGolden(t, filepath.Join("..", "..", "testdata", "golden", "councils", "prompts", string(tt.step)+".md"), []byte(pack.Prompt), *update)
		})
	}
}

func TestJobRole(t *testing.T) {
	s := shipped(t)
	// A role that usually writes sits on a council read-only, without the
	// checklists of its usual job.
	arch := seat(t, s, domain.CouncilOnboarding, "test-architect")
	base := resolved(t, "test-architect")
	r, err := s.Job(Turn{Kind: domain.CouncilOnboarding, Step: StepProposal, Round: 1, Seat: arch}, base)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	proposal, _ := s.Schema(StepProposal)
	if !r.ReadOnly() || slices.Contains(r.AllowedTools, "Write") || r.DefinitionOfDone != nil || r.EscalateWhen != nil ||
		string(r.OutputSchema) != string(proposal) || r.Pool != base.Pool || r.Runtime != base.Runtime {
		t.Errorf("Job = %+v", r)
	}
	for _, want := range []string{"You are the test-architect of Forecrew", "`test-architect@claude-code`, priority **risk**", "`devils-advocate`, priority **dissent**", "`revision` to null"} {
		if !strings.Contains(r.Instructions, want) {
			t.Errorf("instructions lack %q:\n%s", want, r.Instructions)
		}
	}
	if strings.Contains(r.Instructions, base.Instructions[:40]) {
		t.Error("instructions include the role's usual prompt")
	}
	// A role written for councils keeps its prompt and checklists.
	da := resolved(t, DevilsAdvocate)
	r, err = s.Job(Turn{Kind: domain.CouncilOnboarding, Step: StepCritique, Round: 1, Seat: seat(t, s, domain.CouncilOnboarding, DevilsAdvocate)}, da)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if !strings.HasPrefix(r.Instructions, da.Instructions) || !slices.Equal(r.DefinitionOfDone, da.DefinitionOfDone) {
		t.Errorf("devil's advocate lost its prompt or checklist: %+v", r)
	}
}

func TestJobSynthesisVariants(t *testing.T) {
	s := shipped(t)
	fac := resolved(t, "facilitator")
	turn := Turn{Kind: domain.CouncilProduct, Step: StepSynthesis, Round: 2, Seat: Seat{Role: "facilitator"}, BudgetExhausted: true}
	r, err := s.Job(turn, fac)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	for _, want := range []string{"set `budget_exhausted` to true", "A human asked for changes", "synthesis, round 2", "`market-researcher`, priority **market-fit**"} {
		if !strings.Contains(r.Instructions, want) {
			t.Errorf("instructions lack %q", want)
		}
	}
	brief, _ := s.Schema(StepSynthesis)
	if string(r.OutputSchema) != string(brief) {
		t.Error("synthesis does not use the brief schema")
	}
	if !slices.Equal(r.DefinitionOfDone, fac.DefinitionOfDone) {
		t.Errorf("synthesis definition of done = %v, want the facilitator's", r.DefinitionOfDone)
	}
	// The facilitator's definition of done is about the synthesis.
	for _, step := range []Step{StepFrame, StepCheck} {
		r, err := s.Job(Turn{Kind: domain.CouncilProduct, Step: step, Round: 1, Seat: Seat{Role: "facilitator"}}, fac)
		if err != nil {
			t.Fatalf("Job(%s): %v", step, err)
		}
		if r.DefinitionOfDone != nil {
			t.Errorf("%s definition of done = %v, want none", step, r.DefinitionOfDone)
		}
	}
}

func TestMemberName(t *testing.T) {
	if got := MemberName("architect", "claude-code"); got != "architect@claude-code" || !memberName.MatchString(got) {
		t.Errorf("MemberName = %q", got)
	}
}

func TestJobInvalid(t *testing.T) {
	s := shipped(t)
	arch := seat(t, s, domain.CouncilTechnical, "architect")
	tests := []struct {
		name    string
		turn    Turn
		role    string
		runtime string // overrides the resolved role's
		want    string
	}{
		{"unknown kind", Turn{Kind: domain.CouncilKind("debate"), Step: StepProposal, Round: 1, Seat: arch}, "architect", "", `no council pack "debate"`},
		{"unknown step", Turn{Kind: domain.CouncilTechnical, Step: "vote", Round: 1, Seat: arch}, "architect", "", `unknown step "vote"`},
		{"round 0", Turn{Kind: domain.CouncilTechnical, Step: StepProposal, Seat: arch}, "architect", "", "must be from 1 to 1, got 0"},
		{"fourth revision", Turn{Kind: domain.CouncilTechnical, Step: StepRevision, Round: 4, Seat: arch}, "architect", "", "from 1 to 3, got 4"},
		{"check beyond max_rounds", Turn{Kind: domain.CouncilOnboarding, Step: StepCheck, Round: 2, Seat: Seat{Role: "facilitator"}}, "facilitator", "", "from 1 to 1, got 2"},
		{"role off seat", Turn{Kind: domain.CouncilTechnical, Step: StepProposal, Round: 1, Seat: arch}, "developer", "", `role "developer" does not hold the seat of "architect"`},
		{"member frames", Turn{Kind: domain.CouncilTechnical, Step: StepFrame, Round: 1, Seat: arch}, "architect", "", `run by the facilitator "facilitator"`},
		{"no such seat", Turn{Kind: domain.CouncilProduct, Step: StepProposal, Round: 1, Seat: arch}, "architect", "", "no seat architect"},
		{"no runtime", Turn{Kind: domain.CouncilTechnical, Step: StepProposal, Round: 1, Seat: arch}, "architect", "-", `cannot run on runtime ""`},
		{"runtime off seat", Turn{Kind: domain.CouncilTechnical, Step: StepProposal, Round: 1, Seat: Seat{Role: "architect", Priority: "simplicity", Runtimes: []string{"codex"}}}, "architect", "gemini", `cannot run on runtime "gemini"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := resolved(t, tt.role)
			switch tt.runtime {
			case "":
			case "-":
				r.Runtime = ""
			default:
				r.Runtime = tt.runtime
			}
			_, err := s.Job(tt.turn, r)
			if !errors.Is(err, ErrInvalidTurn) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want ErrInvalidTurn containing %q", err, tt.want)
			}
		})
	}
}

// TestOutputFixtures checks testdata/councils/<step> samples with
// CheckOutput.
func TestOutputFixtures(t *testing.T) {
	s := shipped(t)
	for _, step := range []Step{StepFrame, StepProposal, StepCritique, StepRevision, StepCheck, StepSynthesis} {
		t.Run(string(step), func(t *testing.T) {
			testkit.Fixtures(t, filepath.Join("..", "..", "testdata", "councils", string(step)),
				filepath.Join("..", "..", "testdata", "golden", "councils", string(step)), ErrInvalid, *update,
				func(_ string, data []byte) (any, error) {
					if err := s.CheckOutput(step, data); err != nil {
						return nil, err
					}
					return json.RawMessage(data), nil
				})
		})
	}
	if err := s.CheckOutput("vote", []byte(`{}`)); !errors.Is(err, ErrInvalidTurn) {
		t.Errorf("CheckOutput(vote) error = %v", err)
	}
}
