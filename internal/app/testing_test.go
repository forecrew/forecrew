package app

import (
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// A project listed in testing.projects of the config starts its goals on the
// playbook with the testing system; the others, on goal.
func TestTestingProjectsChoosePlaybook(t *testing.T) {
	for _, tt := range []struct {
		name     string
		projects []string
		want     string
	}{
		{"not listed", nil, "goal"},
		{"another project", []string{"other"}, "goal"},
		{"listed", []string{"app"}, "goal-v2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, func(d *Deps) { d.Config.Testing.Projects = tt.projects })
			by := domain.Actor{AccountID: f.account.ID, Kind: domain.ActorMCP, Name: "laptop"}
			must(t, f.st.MCPTokens().Create(t.Context(), &domain.MCPToken{AccountID: f.account.ID, Name: "laptop", TokenHash: mcp.HashToken("fcm_laptop")}))
			g, err := f.app.tools.CreateGoal(t.Context(), by, mcp.CreateGoalInput{Project: "app", Title: "Add checkout"})
			must(t, err)
			runs, err := f.st.WorkflowRuns().ListActive(t.Context(), f.account.ID)
			must(t, err)
			if len(runs) != 1 || runs[0].Playbook != tt.want || runs[0].GoalID != g.ID {
				t.Errorf("runs = %+v, want one of %s", runs, tt.want)
			}
		})
	}
}
