package councils

import (
	"errors"
	"slices"
	"testing"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
)

func TestSeated(t *testing.T) {
	s := shipped(t)
	names := func(p Pack) []string {
		var out []string
		for _, m := range p.Members {
			out = append(out, m.Role)
		}
		return out
	}
	p, err := s.Seated(domain.CouncilProduct, nil)
	if err != nil || p.Kind != domain.CouncilProduct {
		t.Fatalf("product = %+v, %v", p, err)
	}
	p, err = s.Seated(domain.CouncilAdhoc, nil)
	if err != nil || p.Kind != domain.CouncilAdhoc || p.Question != adhocQuestion || len(p.Members) != 5 {
		t.Fatalf("adhoc = %+v, %v", p, err)
	}
	// The devil's advocate joins; the pack's seat is kept, a new one is made.
	p, err = s.Seated(domain.CouncilTechnical, []string{"architect", "ux"})
	if err != nil || !slices.Equal(names(p), []string{"architect", "ux", DevilsAdvocate}) {
		t.Fatalf("seated = %+v, %v", p, err)
	}
	if p.Members[0].Priority != "simplicity" || p.Members[1].Priority != "ux" || len(p.Members[1].Runtimes) != 2 {
		t.Fatalf("seats = %+v", p.Members)
	}
	if p, err = s.Seated(domain.CouncilTechnical, []string{"ux", DevilsAdvocate, "triage"}); err != nil || len(p.Members) != 3 {
		t.Fatalf("named advocate = %+v, %v", p, err)
	}
	for name, in := range map[string][]string{
		"unknown":     {"ghost", "ux"},
		"facilitator": {"facilitator", "ux"},
		"twice":       {"ux", "ux"},
		"too few":     {DevilsAdvocate},
	} {
		if _, err := s.Seated(domain.CouncilTechnical, in); !errors.Is(err, ErrInvalidTurn) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := s.Seated("debate", nil); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("unknown kind: %v", err)
	}
}

func TestAssign(t *testing.T) {
	both := []string{config.AdapterClaudeCode, config.AdapterCodex}
	p := Pack{Members: []Seat{
		{Role: "a", Runtimes: both},
		{Role: "b", Runtimes: []string{config.AdapterCodex, config.AdapterClaudeCode}},
		{Role: "c"},
	}}
	usual := func(string) string { return config.AdapterClaudeCode }
	only := func(names ...string) func(Seat, string) bool {
		return func(_ Seat, r string) bool { return slices.Contains(names, r) }
	}
	runtimes := func(ms []Member) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.Runtime)
		}
		return out
	}
	tests := []struct {
		name    string
		offered func(Seat, string) bool
		want    []string
	}{
		{"both offered", only(both...), []string{"claude-code", "codex", "claude-code"}},
		{"one offered", only(config.AdapterCodex), []string{"codex", "codex", "claude-code"}},
		{"none offered: first choices", only(), []string{"claude-code", "codex", "claude-code"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runtimes(p.Assign(tt.offered, usual)); !slices.Equal(got, tt.want) {
				t.Fatalf("runtimes = %v, want %v", got, tt.want)
			}
		})
	}
	// Preferences alone put everyone on claude-code; a seat that may move does.
	same := Pack{Members: []Seat{{Role: "a", Runtimes: both}, {Role: "b", Runtimes: both}, {Role: "c", Runtimes: both}}}
	got := same.Assign(only(both...), usual)
	if want := []string{"claude-code", "claude-code", "codex"}; !slices.Equal(runtimes(got), want) {
		t.Fatalf("diversity: %v, want %v", runtimes(got), want)
	}
	// No worker offers a second runtime: nobody moves.
	got = same.Assign(only(config.AdapterClaudeCode), usual)
	if want := []string{"claude-code", "claude-code", "claude-code"}; !slices.Equal(runtimes(got), want) {
		t.Fatalf("one vendor: %v, want %v", runtimes(got), want)
	}
}

func TestJobOfSeatedCouncil(t *testing.T) {
	s := shipped(t)
	r := resolved(t, "ux")
	r.Runtime = config.AdapterCodex
	got, err := s.Job(Turn{Kind: domain.CouncilAdhoc, Step: StepProposal, Round: 1, Seat: Seat{Role: "ux", Priority: "ux"}, Roles: []string{"architect", "ux"}}, r)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.AllowedTools, readTool) {
		t.Fatalf("allowed tools = %v", got.AllowedTools)
	}
	if _, err := s.Job(Turn{Kind: domain.CouncilAdhoc, Step: StepProposal, Round: 1, Seat: Seat{Role: "ux", Priority: "ux"}}, r); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("a seat the council does not have: %v", err)
	}
}
