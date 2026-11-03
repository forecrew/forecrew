package playbooks_test

import (
	"slices"
	"testing"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/playbooks"
)

// triage returns a triage output with the given grades and areas.
func triage(impact, reversibility string, areas ...string) map[string]any {
	return map[string]any{
		"impact": impact, "reversibility": reversibility, "areas": areas,
		"reason": "Checked by hand.", "assumptions": []any{},
	}
}

// sizeOut is goal.size's output decoded.
type sizeOut struct {
	Councils    []string `json:"councils"`
	IfProduct   []string `json:"if_product"`
	IfTechnical []string `json:"if_technical"`
}

func decodeSize(t *testing.T, out any) sizeOut {
	t.Helper()
	var s sizeOut
	m := toMap(t, out)
	list := func(k string) []string {
		var r []string
		for _, x := range m[k].([]any) {
			r = append(r, x.(string))
		}
		return r
	}
	s.Councils, s.IfProduct, s.IfTechnical = list("councils"), list("if_product"), list("if_technical")
	return s
}

func TestGoalSizeRule(t *testing.T) {
	tests := []struct {
		name      string
		sizing    *config.Sizing // nil uses the defaults
		triage    map[string]any
		councils  []string
		ifProduct bool
		ifTech    bool
	}{
		{name: "small easy change", triage: triage("low", "easy", "web"), councils: []string{}},
		{name: "medium impact", triage: triage("medium", "easy", "web"), councils: []string{"technical"}, ifTech: true},
		{name: "high impact", triage: triage("high", "easy", "web"), councils: []string{"product", "technical"}, ifProduct: true, ifTech: true},
		{name: "hard to undo", triage: triage("low", "hard", "web"), councils: []string{"product"}, ifProduct: true},
		{name: "irreversible", triage: triage("low", "irreversible", "web"), councils: []string{"product"}, ifProduct: true},
		{name: "two components", triage: triage("low", "easy", "web", "internal/api"), councils: []string{"technical"}, ifTech: true},
		{name: "repeated area is one component", triage: triage("low", "easy", "web", " web "), councils: []string{}},
		{name: "blank areas are not components", triage: triage("low", "easy", "web", " "), councils: []string{}},
		{name: "high impact and hard", triage: triage("high", "hard", "a"), councils: []string{"product", "technical"}, ifProduct: true, ifTech: true},
		{name: "medium and irreversible", triage: triage("medium", "irreversible", "a"), councils: []string{"product", "technical"}, ifProduct: true, ifTech: true},
		{name: "custom: product on medium only", sizing: &config.Sizing{
			Product:   config.SizingProduct{Impact: []domain.Level{domain.LevelMedium}, Reversibility: []domain.Reversibility{}},
			Technical: config.SizingTechnical{Impact: []domain.Level{}, Components: 9},
		}, triage: triage("medium", "irreversible", "a"), councils: []string{"product"}, ifProduct: true},
		{name: "custom: one component is enough", sizing: &config.Sizing{
			Product:   config.SizingProduct{Impact: []domain.Level{}, Reversibility: []domain.Reversibility{}},
			Technical: config.SizingTechnical{Impact: []domain.Level{}, Components: 1},
		}, triage: triage("low", "easy", "web"), councils: []string{"technical"}, ifTech: true},
		{name: "custom: no councils at all", sizing: &config.Sizing{
			Product:   config.SizingProduct{Impact: []domain.Level{}, Reversibility: []domain.Reversibility{}},
			Technical: config.SizingTechnical{Impact: []domain.Level{}, Components: 9},
		}, triage: triage("high", "irreversible", "a", "b"), councils: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newEnv(t)
			if tt.sizing != nil {
				playbooks.Register(v.actions, playbooks.Deps{Store: v.st, Tracker: v.tracker, Signals: v.bus, Sizing: *tt.sizing, Now: v.clock.Now})
			}
			g := v.newGoal(t)
			out, err := v.call(t, "goal.size", map[string]any{"goal_id": g.ID.String(), "triage": tt.triage})
			must(t, err)
			s := decodeSize(t, out)
			if !slices.Equal(s.Councils, tt.councils) {
				t.Errorf("councils = %v, want %v", s.Councils, tt.councils)
			}
			if (len(s.IfProduct) == 1) != tt.ifProduct || (len(s.IfTechnical) == 1) != tt.ifTech {
				t.Errorf("if_product = %v, if_technical = %v", s.IfProduct, s.IfTechnical)
			}
			if tt.ifProduct && s.IfProduct[0] != g.ID.String() || tt.ifTech && s.IfTechnical[0] != g.ID.String() {
				t.Errorf("if lists do not name the goal: %v %v", s.IfProduct, s.IfTechnical)
			}
			stored := v.goalNow(t, g.ID).Size
			if stored == nil || stored.Impact != domain.Level(tt.triage["impact"].(string)) || stored.Reason != "Checked by hand." ||
				!slices.Equal(stored.Councils, toCouncils(tt.councils)) {
				t.Errorf("stored size = %+v", stored)
			}
		})
	}
}

func toCouncils(names []string) []domain.CouncilKind {
	out := []domain.CouncilKind{}
	for _, n := range names {
		out = append(out, domain.CouncilKind(n))
	}
	return out
}

func TestGoalSizeOverride(t *testing.T) {
	v := newEnv(t)
	g := v.newGoal(t)
	// A human set the councils: create_goal stored the size with no triage.
	must(t, v.st.Goals().SetSize(t.Context(), v.account, g.ID, domain.GoalSize{Councils: []domain.CouncilKind{domain.CouncilTechnical}, Reason: "set by mcp:laptop"}))

	// The triage is not read, so it may even be missing.
	out, err := v.call(t, "goal.size", map[string]any{"goal_id": g.ID.String()})
	must(t, err)
	s := decodeSize(t, out)
	if !slices.Equal(s.Councils, []string{"technical"}) || len(s.IfProduct) != 0 || len(s.IfTechnical) != 1 {
		t.Errorf("override output = %+v", s)
	}
	// A triage that would give the product council does not change the override.
	out, err = v.call(t, "goal.size", map[string]any{"goal_id": g.ID.String(), "triage": triage("high", "irreversible", "a")})
	must(t, err)
	if s := decodeSize(t, out); !slices.Equal(s.Councils, []string{"technical"}) {
		t.Errorf("override with triage = %+v", s)
	}
	if got := v.goalNow(t, g.ID).Size; got.Reason != "set by mcp:laptop" || !slices.Equal(got.Councils, []domain.CouncilKind{domain.CouncilTechnical}) || got.Impact != "" {
		t.Errorf("stored override = %+v", got)
	}

	// An override for no councils gives none, and skips triage too.
	none := v.newGoal(t)
	must(t, v.st.Goals().SetSize(t.Context(), v.account, none.ID, domain.GoalSize{Councils: []domain.CouncilKind{}, Reason: "set by mcp:laptop"}))
	out, err = v.call(t, "goal.size", map[string]any{"goal_id": none.ID.String()})
	must(t, err)
	if s := decodeSize(t, out); len(s.Councils) != 0 || len(s.IfProduct)+len(s.IfTechnical) != 0 {
		t.Errorf("empty override = %+v", s)
	}
}

func TestGoalSizeIdempotent(t *testing.T) {
	v := newEnv(t)
	g := v.newGoal(t)
	with := map[string]any{"goal_id": g.ID.String(), "triage": triage("high", "easy", "web")}
	first, err := v.callKey(t, "goal.size", "k", with)
	must(t, err)
	// A repeated call, even with other grades, answers what was stored.
	again, err := v.callKey(t, "goal.size", "k", map[string]any{"goal_id": g.ID.String(), "triage": triage("low", "easy", "web")})
	must(t, err)
	if !slices.Equal(decodeSize(t, first).Councils, decodeSize(t, again).Councils) {
		t.Errorf("second answer = %v, first %v", decodeSize(t, again), decodeSize(t, first))
	}
}

func TestGoalSizeErrors(t *testing.T) {
	v := newEnv(t)
	g := v.newGoal(t)
	other := domain.Project{AccountID: v.account, Name: "other", Repo: "acme/other", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, v.st.Projects().Create(t.Context(), &other))
	foreign := domain.Goal{AccountID: v.account, ProjectID: other.ID, Title: "x", Status: domain.GoalDraft}
	must(t, v.st.Goals().Create(t.Context(), &foreign))

	bad := func(mut func(m map[string]any)) map[string]any {
		m := triage("high", "easy", "web")
		mut(m)
		return m
	}
	tests := []struct {
		name string
		with map[string]any
		want string
	}{
		{"no triage", map[string]any{"goal_id": g.ID.String()}, "triage"},
		{"triage is not an object", map[string]any{"goal_id": g.ID.String(), "triage": "high"}, "triage"},
		{"unknown impact", map[string]any{"goal_id": g.ID.String(), "triage": bad(func(m map[string]any) { m["impact"] = "huge" })}, "impact"},
		{"unknown reversibility", map[string]any{"goal_id": g.ID.String(), "triage": bad(func(m map[string]any) { m["reversibility"] = "never" })}, "reversibility"},
		{"no reason", map[string]any{"goal_id": g.ID.String(), "triage": bad(func(m map[string]any) { m["reason"] = " " })}, "reason"},
		{"no areas", map[string]any{"goal_id": g.ID.String(), "triage": triage("high", "easy")}, "no area"},
		{"blank areas", map[string]any{"goal_id": g.ID.String(), "triage": triage("high", "easy", " ")}, "no area"},
		{"goal of another project", map[string]any{"goal_id": foreign.ID.String(), "triage": triage("low", "easy", "a")}, "belongs to another project"},
		{"unknown goal", map[string]any{"goal_id": domain.NewID().String(), "triage": triage("low", "easy", "a")}, "get goal"},
		{"unknown key", map[string]any{"goal_id": g.ID.String(), "x": 1}, "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.call(t, "goal.size", tt.with)
			wantErr(t, err, tt.want)
		})
	}
	if got := v.goalNow(t, g.ID).Size; got != nil {
		t.Errorf("a refused triage stored a size: %+v", got)
	}
}
