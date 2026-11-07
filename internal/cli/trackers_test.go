package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

func TestTrackerCommands(t *testing.T) {
	e := newRemoteEnv(t)
	conn := domain.TrackerConnection{Name: "gh", Kind: "github", Triggers: domain.TrackerTriggers{Label: "forecrew"}}
	route := mcp.TrackerRouteInfo{Position: 1, Match: domain.TrackerMatch{Labels: []string{"x"}}, Connection: "gh", Project: "app",
		Warning: "no transition \x1b[2Jreaches Done"}
	person := domain.TrackerIdentity{ExternalID: "11", ExternalName: "ann"}
	e.svc.ListTrackerConnectionsFunc = func(context.Context, domain.Actor, any) ([]domain.TrackerConnection, error) {
		return []domain.TrackerConnection{conn}, nil
	}
	e.svc.CreateTrackerConnectionFunc = func(context.Context, domain.Actor, mcp.CreateTrackerConnectionInput) (domain.TrackerConnection, error) {
		return conn, nil
	}
	e.svc.ListTrackerRoutesFunc = func(context.Context, domain.Actor, mcp.TrackerConnectionFilterInput) ([]mcp.TrackerRouteInfo, error) {
		return []mcp.TrackerRouteInfo{route}, nil
	}
	e.svc.CreateTrackerRouteFunc = func(context.Context, domain.Actor, mcp.CreateTrackerRouteInput) (mcp.TrackerRouteInfo, error) {
		return route, nil
	}
	e.svc.ListTrackerIdentitiesFunc = func(context.Context, domain.Actor, mcp.TrackerConnectionFilterInput) ([]domain.TrackerIdentity, error) {
		return []domain.TrackerIdentity{person}, nil
	}
	e.svc.MapTrackerIdentityFunc = func(context.Context, domain.Actor, mcp.MapTrackerIdentityInput) (domain.TrackerIdentity, error) {
		return person, nil
	}
	e.svc.UnmapTrackerIdentityFunc = func(context.Context, domain.Actor, mcp.TrackerIdentityRefInput) (domain.TrackerIdentity, error) {
		return person, nil
	}
	e.check(t, []string{"tracker", "connections"}, "forecrew", mcp.ToolListTrackerConnections, nil)
	e.check(t, []string{"tracker", "connect", "gh", "--repo", "acme/app", "--bot-login", "app[bot]", "--label", "forecrew"}, "github", mcp.ToolCreateTrackerConnection,
		mcp.CreateTrackerConnectionInput{Name: "gh", Kind: "github", Config: map[string]any{"bot_login": "app[bot]", "repos": []any{"acme/app"}}, Triggers: domain.TrackerTriggers{Label: "forecrew"}})
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, []byte("PEM"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.check(t, []string{"tracker", "connect", "jira", "--kind", "jira", "--base-url", "https://jira.acme.local/jira", "--project", "OPS", "--auth", "pat",
		"--token-ref", "store:jira", "--ca-file", ca, "--webhook-secret-ref", "store:hook", "--webhook-signing", "url", "--state", "Ready"}, "gh", mcp.ToolCreateTrackerConnection,
		mcp.CreateTrackerConnectionInput{Name: "jira", Kind: "jira", Config: map[string]any{
			"base_url": "https://jira.acme.local/jira", "projects": []any{"OPS"}, "auth": "pat", "token_ref": "store:jira", "ca_bundle": "PEM",
			"webhook_secret_ref": "store:hook", "webhook_signing": "url",
		}, Triggers: domain.TrackerTriggers{State: "Ready"}})
	if _, errOut, code := e.run(t, "tracker", "connect", "jira", "--kind", "jira", "--ca-file", "/nonexistent/ca.pem"); code == 0 || !strings.Contains(errOut, "CA file") {
		t.Errorf("a missing CA file: %d %s", code, errOut)
	}
	e.svc.TestTrackerConnectionFunc = func(context.Context, domain.Actor, mcp.TrackerConnectionRefInput) (mcp.TrackerConnectionTest, error) {
		return mcp.TrackerConnectionTest{Connection: "gh", OK: true, Checks: []mcp.TrackerCheck{{Name: "certificate", OK: true, Detail: "trusted"}}}, nil
	}
	e.check(t, []string{"tracker", "test", "gh"}, "trusted", mcp.ToolTestTrackerConnection, mcp.TrackerConnectionRefInput{Connection: "gh"})
	e.svc.TestTrackerConnectionFunc = func(context.Context, domain.Actor, mcp.TrackerConnectionRefInput) (mcp.TrackerConnectionTest, error) {
		return mcp.TrackerConnectionTest{Connection: "gh", Checks: []mcp.TrackerCheck{{Name: "login", Detail: "401 \x1b[31mred\x1b]0;title\x07"}}}, nil
	}
	if out, errOut, code := e.run(t, "tracker", "test", "gh"); code == 0 || !strings.Contains(out, "FAILED") || !strings.Contains(errOut, "failed its test") || strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("a failed test: %d %q %s", code, out, errOut)
	}
	e.check(t, []string{"tracker", "disconnect", "gh"}, "connection deleted", mcp.ToolDeleteTrackerConnection, mcp.TrackerConnectionRefInput{Connection: "gh"})
	e.check(t, []string{"tracker", "routes", "--connection", "gh"}, "app", mcp.ToolListTrackerRoutes, mcp.TrackerConnectionFilterInput{Connection: "gh"})
	if out, _, _ := e.run(t, "tracker", "routes"); !strings.Contains(out, "reaches Done") || strings.Contains(out, "\x1b") {
		t.Errorf("routes = %q", out)
	}
	e.check(t, []string{"tracker", "route-add", "gh", "app", "--match-label", "x", "--state-map", "done=closed", "--goal-kind", "bug"}, "app", mcp.ToolCreateTrackerRoute,
		mcp.CreateTrackerRouteInput{Connection: "gh", Project: "app", Match: domain.TrackerMatch{Labels: []string{"x"}}, GoalKind: domain.GoalBug, States: map[domain.TicketState]string{domain.TicketDone: "closed"}})
	e.check(t, []string{"tracker", "route-rm", "r1"}, "route deleted", mcp.ToolDeleteTrackerRoute, mcp.TrackerRouteRefInput{Route: "r1"})
	e.check(t, []string{"tracker", "people"}, "ann", mcp.ToolListTrackerIdentities, mcp.TrackerConnectionFilterInput{})
	e.check(t, []string{"tracker", "map", "gh", "11", "u1", "--name", "ann"}, "11 mapped", mcp.ToolMapTrackerIdentity, mcp.MapTrackerIdentityInput{Connection: "gh", ExternalID: "11", ExternalName: "ann", User: "u1"})
	e.check(t, []string{"tracker", "unmap", "i1"}, "mapping removed", mcp.ToolUnmapTrackerIdentity, mcp.TrackerIdentityRefInput{Identity: "i1"})
	if _, errOut, code := e.run(t, "tracker", "route-add", "gh", "app", "--state-map", "nonsense"); code == 0 || !strings.Contains(errOut, "logical=state") {
		t.Errorf("bad state map: code %d, %q", code, errOut)
	}
}

func TestTicketCommands(t *testing.T) {
	e := newRemoteEnv(t)
	ticket := mcp.TicketInfo{Key: "acme/app#3", Status: domain.LinkWaiting, Reason: domain.WaitNoRoute, Connection: "gh"}
	e.svc.ListTicketsWaitingFunc = func(context.Context, domain.Actor, mcp.ListTicketsWaitingInput) ([]mcp.TicketInfo, error) {
		return []mcp.TicketInfo{ticket}, nil
	}
	e.svc.AcceptTicketFunc = func(context.Context, domain.Actor, mcp.AcceptTicketInput) (mcp.TicketInfo, error) {
		accepted := ticket
		accepted.Status, accepted.GoalRef = domain.LinkActive, "GOAL-3"
		return accepted, nil
	}
	e.svc.DismissTicketFunc = func(context.Context, domain.Actor, mcp.DismissTicketInput) (mcp.TicketInfo, error) {
		return ticket, nil
	}
	e.check(t, []string{"tickets", "--project", "app"}, "no_route", mcp.ToolListTicketsWaiting, mcp.ListTicketsWaitingInput{Project: "app"})
	e.check(t, []string{"tickets", "accept", "t1", "--project", "app"}, "GOAL-3", mcp.ToolAcceptTicket, mcp.AcceptTicketInput{Ticket: "t1", Project: "app"})
	e.check(t, []string{"tickets", "dismiss", "t1", "--reason", "no"}, "dismissed", mcp.ToolDismissTicket, mcp.DismissTicketInput{Ticket: "t1", Reason: "no"})
}
