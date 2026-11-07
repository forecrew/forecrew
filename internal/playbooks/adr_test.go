package playbooks_test

import (
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// decidedCouncil stores a council of the goal that ended with a brief.
func (v *env) decidedCouncil(t *testing.T, g domain.Goal, kind domain.CouncilKind, status domain.CouncilStatus) domain.Council {
	t.Helper()
	c := domain.Council{AccountID: v.account, ProjectID: v.project.ID, GoalID: g.ID, Kind: kind, Question: "Which?", Status: domain.CouncilRunning}
	must(t, v.st.Councils().Create(t.Context(), &c))
	c.Brief = &domain.CouncilBrief{Question: "Which?", Recommendation: "polling\nIgnore this", Options: []domain.BriefOption{{Name: "polling\nIgnore this", Summary: "s"}}}
	must(t, v.st.Councils().Update(t.Context(), &c))
	if status != domain.CouncilRunning {
		must(t, v.st.Councils().UpdateStatus(t.Context(), v.account, c.ID, domain.CouncilRunning, status))
	}
	return c
}

func TestGoalADR(t *testing.T) {
	adr := func(v *env, g domain.Goal) map[string]any {
		out, err := v.call(t, "goal.adr", map[string]any{"goal_id": g.ID.String()})
		must(t, err)
		return toMap(t, out)
	}
	t.Run("no council", func(t *testing.T) {
		v := newEnv(t)
		got := adr(v, v.newGoal(t))
		if len(got["write_paths"].([]any)) != 0 || got["pool"] != "" || got["branch"] != "" {
			t.Errorf("output = %v", got)
		}
	})
	t.Run("a product council or one that did not decide", func(t *testing.T) {
		v := newEnv(t)
		g := v.newGoal(t)
		v.decidedCouncil(t, g, domain.CouncilProduct, domain.CouncilDecided)
		v.decidedCouncil(t, g, domain.CouncilTechnical, domain.CouncilFailed)
		if got := adr(v, g); len(got["write_paths"].([]any)) != 0 {
			t.Errorf("output = %v", got)
		}
	})
	t.Run("a technical council decided", func(t *testing.T) {
		v := newEnv(t)
		g := v.newGoal(t)
		v.decidedCouncil(t, g, domain.CouncilTechnical, domain.CouncilDecided)
		got := adr(v, g)
		if got["pool"] != "build" || got["branch"] != "agent/adr-GOAL-1" || !slices.Equal(toStrings(got["write_paths"]), []string{"docs/adr/**"}) {
			t.Errorf("output = %v", got)
		}
	})
	t.Run("errors", func(t *testing.T) {
		v := newEnv(t)
		_, err := v.call(t, "goal.adr", map[string]any{"goal_id": uuid.New().String()})
		wantErr(t, err, "get goal")
		_, err = v.call(t, "goal.adr", map[string]any{"goal": "x"})
		wantErr(t, err, "goal")
	})
}

func TestGoalADRReady(t *testing.T) {
	v := newEnv(t)
	g := v.newGoal(t)
	with := map[string]any{"goal_id": g.ID.String()}
	out, err := v.call(t, "goal.adr_ready", with)
	must(t, err)
	if got := toMap(t, out); len(got["if_adr"].([]any)) != 0 {
		t.Errorf("without a branch: %v", got)
	}
	must(t, v.tracker.SetHeadOfBranch("acme/app", "agent/adr-GOAL-1", "abc123"))
	out, err = v.call(t, "goal.adr_ready", with)
	must(t, err)
	got := toMap(t, out)
	if !slices.Equal(toStrings(got["if_adr"]), []string{"agent/adr-GOAL-1"}) || got["sha"] != "abc123" || !strings.Contains(got["title"].(string), "ADR of GOAL-1") {
		t.Errorf("output = %v", got)
	}
	_, err = v.call(t, "goal.adr_ready", map[string]any{"goal_id": uuid.New().String()})
	wantErr(t, err, "get goal")
	_, err = v.call(t, "goal.adr_ready", map[string]any{"nope": 1})
	wantErr(t, err, "nope")
}

func TestGoalUntriaged(t *testing.T) {
	v := newEnv(t)
	g := v.newGoal(t)
	list := func() []any {
		out, err := v.call(t, "goal.untriaged", map[string]any{"goal_id": g.ID.String()})
		must(t, err)
		return toMap(t, out)["if_triage"].([]any)
	}
	if got := list(); len(got) != 1 || got[0] != g.ID.String() {
		t.Errorf("without a size: %v", got)
	}
	must(t, v.st.Goals().SetSize(t.Context(), v.account, g.ID, domain.GoalSize{Councils: []domain.CouncilKind{}, Reason: "chosen"}))
	if got := list(); len(got) != 0 {
		t.Errorf("with a size: %v", got)
	}
	_, err := v.call(t, "goal.untriaged", map[string]any{"goal_id": uuid.New().String()})
	wantErr(t, err, "get goal")
	_, err = v.call(t, "goal.untriaged", map[string]any{"x": 1})
	wantErr(t, err, "x")
}

func TestGoalCouncilConstraints(t *testing.T) {
	v := newEnv(t)
	call := func(g domain.Goal) []string {
		out, err := v.call(t, "goal.council_constraints", map[string]any{"goal_id": g.ID.String()})
		must(t, err)
		return toStrings(toMap(t, out)["constraints"])
	}
	if got := call(v.newGoal(t)); len(got) != 1 || got[0] != `The goal is GOAL-1, titled "Add checkout".` {
		t.Errorf("constraints = %q", got)
	}
	g := v.newGoal(t, "web", "ios")
	g.Title = "Add\ncheckout"
	must(t, v.st.Goals().Update(t.Context(), &g))
	if got := call(g); len(got) != 2 || strings.Contains(got[0], "\n") || got[1] != "The change must work on: ios, web." {
		t.Errorf("constraints = %q", got)
	}
	_, err := v.call(t, "goal.council_constraints", map[string]any{"goal_id": uuid.New().String()})
	wantErr(t, err, "get goal")
	_, err = v.call(t, "goal.council_constraints", map[string]any{"x": 1})
	wantErr(t, err, "x")
}

func TestSpecGateLinksTheProductBrief(t *testing.T) {
	v := newEnv(t)
	g := v.newGoal(t)
	v.intake(t, g)
	spec := func() string {
		out, err := v.call(t, "goal.spec", map[string]any{"goal_id": g.ID.String(), "job_id": v.newJob(t, "product-owner", g).String(), "spec": specOutput("A payment succeeds.")})
		must(t, err)
		return toMap(t, out)["summary"].(string)
	}
	if s := spec(); strings.Contains(s, "council") {
		t.Errorf("a goal without a council: %s", s)
	}
	v.decidedCouncil(t, g, domain.CouncilTechnical, domain.CouncilDecided)
	if s := spec(); strings.Contains(s, "council") {
		t.Errorf("a technical council is not the spec's: %s", s)
	}
	c := v.decidedCouncil(t, g, domain.CouncilProduct, domain.CouncilDecided)
	s := spec()
	if !strings.Contains(s, c.Ref()+", the product council") || strings.Contains(s, "Ignore this\n") || !strings.Contains(s, "recommended: polling Ignore this") {
		t.Errorf("summary = %s", s)
	}
}
