package playbooks_test

import (
	"strconv"
	"testing"

	"github.com/forecrew/forecrew/internal/authz"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/tracker"
	"github.com/forecrew/forecrew/internal/tracker/githubissues"
	"github.com/forecrew/forecrew/internal/workflow"
)

// A canceled goal leaves a person's issue to the route: a rejected spec gate
// closes it only where the route maps canceled to a state.
func TestRejectedGoalLeavesTheTicketToItsRoute(t *testing.T) {
	for name, tt := range map[string]struct {
		states map[domain.TicketState]string
		want   string
	}{
		"no mapping": {nil, "open"},
		"a mapping":  {map[domain.TicketState]string{domain.TicketCanceled: "closed"}, "closed"},
	} {
		t.Run(name, func(t *testing.T) {
			v := newEnv(t, domain.ProjectOnboarded)
			ctx := t.Context()
			issue, err := v.tracker.CreateIssue(ctx, "acme/app", github.IssueInput{Title: "Pay by card", Body: "A person's text."})
			must(t, err)
			g := v.newGoal(t)
			g.IssueNumber = issue.Number
			must(t, v.st.Goals().Update(ctx, &g))
			conn := domain.TrackerConnection{AccountID: v.account, Name: "issues", Kind: githubissues.Kind, Config: map[string]any{"repos": []any{"acme/app"}}, Triggers: domain.TrackerTriggers{Label: "forecrew"}}
			must(t, v.st.TrackerConnections().Create(ctx, &conn))
			route := domain.TrackerRoute{AccountID: v.account, ConnectionID: conn.ID, ProjectID: v.project.ID, States: tt.states}
			must(t, v.st.TrackerRoutes().Create(ctx, &route))
			must(t, v.st.TicketLinks().Create(ctx, &domain.TicketLink{
				AccountID: v.account, ConnectionID: conn.ID, Key: "acme/app#" + strconv.Itoa(issue.Number), Status: domain.LinkActive,
				ProjectID: v.project.ID, RouteID: route.ID, GoalID: g.ID, IssueNumber: issue.Number, State: domain.TicketInProgress,
			}))
			r := &goalRun{runner: startRun(t, v, "goal", workflow.StartRequest{
				AccountID: v.account, ProjectID: v.project.ID, GoalID: g.ID,
				Inputs: map[string]any{"repo": "acme/app", "goal_id": g.ID.String(), "platforms": g.Platforms},
			}), goal: g, heads: map[string]int{}}
			r.spec(t, 1, "A payment succeeds.")
			r.decide(t, domain.GateSpecApproval, mcp.ToolReject, "No.")
			r.waitRun(t, domain.RunCanceled)
			r.waitStep(t, "finally/settle", domain.StepSucceeded)
			if got := r.goalNow(t).Status; got != domain.GoalCanceled {
				t.Fatalf("goal = %s", got)
			}
			if got := r.goalIssue(t); got.State != "open" {
				t.Fatalf("settling closed the ticket: %s", got.State)
			}

			registry := tracker.NewRegistry()
			registry.Register(githubissues.Kind, githubissues.Factory(githubissues.Deps{GitHub: v.tracker, Store: v.st}))
			svc := tracker.New(tracker.Config{
				Store: v.st, Registry: registry, Gates: v.gates, Authz: authz.New(v.st, nil),
				Playbook: func(domain.Project, domain.GoalKind) string { return "goal" },
			})
			must(t, svc.Sync(ctx))
			if got := r.goalIssue(t); got.State != tt.want {
				t.Errorf("issue = %s, want %s", got.State, tt.want)
			}
		})
	}
}

// A goal whose ticket is on another tracker has an issue of its own, which
// closes with the goal as ever.
func TestSettlingClosesTheIssueOfAGoalWhoseTicketIsElsewhere(t *testing.T) {
	v := newEnv(t, domain.ProjectOnboarded)
	ctx := t.Context()
	issue, err := v.tracker.CreateIssue(ctx, "acme/app", github.IssueInput{Title: "GOAL-1"})
	must(t, err)
	g := v.newGoal(t)
	g.IssueNumber = issue.Number
	must(t, v.st.Goals().Update(ctx, &g))
	conn := domain.TrackerConnection{AccountID: v.account, Name: "jira", Kind: "jira", Triggers: domain.TrackerTriggers{Label: "forecrew"}}
	must(t, v.st.TrackerConnections().Create(ctx, &conn))
	must(t, v.st.TicketLinks().Create(ctx, &domain.TicketLink{AccountID: v.account, ConnectionID: conn.ID, Key: "ENG-7", Status: domain.LinkActive, ProjectID: v.project.ID, GoalID: g.ID}))
	_, err = v.call(t, "goal.settle", map[string]any{"goal_id": g.ID.String()})
	must(t, err)
	got, err := v.tracker.GetIssue(ctx, "acme/app", issue.Number)
	must(t, err)
	if got.State != "closed" {
		t.Errorf("issue = %s", got.State)
	}
}
