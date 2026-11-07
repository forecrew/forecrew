package api_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

// trackerPlan is the plan the tracker form makes: a Linear connection and a
// secure field for its token.
func trackerPlan(filled bool, status domain.ConfigPlanStatus) mcp.ConfigPlanView {
	return mcp.ConfigPlanView{ConfigPlan: domain.ConfigPlan{
		ID: fid(390), AccountID: fid(1), Status: status, Source: domain.ConfigFromForm, Title: "Connect tracker linear", Applies: "now",
		Changes: []domain.ConfigChange{{Op: domain.ConfigCreate, Kind: "tracker_connection", Name: "linear",
			New:  json.RawMessage(`{"kind":"linear","config":{"teams":["ENG"],"token_ref":"store:linear-token"},"triggers":{"label":"forecrew"}}`),
			Line: "+ Tracker connection linear (linear)"}},
		Needs:   []domain.Permission{domain.PermTrackersManage},
		Affects: "Tickets handed over on the tracker become goals; status goes back to them.",
		Steps: []domain.ConfigStep{
			{Kind: domain.ConfigStepSecret, Resource: "tracker_connection/linear", Title: "token of tracker connection linear", Secret: "linear-token", Filled: filled},
			{Kind: domain.ConfigStepConnect, Resource: "tracker_connection/linear", Title: "Test the connection", Detail: "Run forecrew tracker test linear once the plan is applied."},
		},
		LowersProtection: []string{}, Reversible: true, CreatedBy: "user:" + fid(300).String(), ExpiresAt: at(60), CreatedAt: at(0), UpdatedAt: at(0),
	}, CanConfirm: true}
}

func trackerFixtureCases() []fixtureCase {
	jira := domain.TrackerConnection{ID: fid(920), AccountID: fid(1), Name: "jira", Kind: "jira", CreatedAt: at(-2000), UpdatedAt: at(-1000),
		Config:   map[string]any{"base_url": "https://jira.acme.local/jira", "projects": []any{"ENG", "MOBILE"}, "auth": "pat", "token_ref": "store:jira-token", "deployment": "data_center"},
		Triggers: domain.TrackerTriggers{Label: "forecrew", State: "Ready for agents"}}
	linear := domain.TrackerConnection{ID: fid(921), AccountID: fid(1), Name: "linear", Kind: "linear", CreatedAt: at(-900), UpdatedAt: at(-900),
		Config:   map[string]any{"teams": []any{"ENG"}, "token_ref": "store:linear-token"},
		Triggers: domain.TrackerTriggers{Label: "forecrew"}}
	connections := []domain.TrackerConnection{jira, linear}
	route := func(id, conn int, name string, pos int, m domain.TrackerMatch, project, warning string, states map[domain.TicketState]string) mcp.TrackerRouteInfo {
		pid := fid(10)
		if project == "mobile" {
			pid = fid(12)
		}
		return mcp.TrackerRouteInfo{
			TrackerRoute: domain.TrackerRoute{ID: fid(id), AccountID: fid(1), ConnectionID: fid(conn), Position: pos, Match: m, ProjectID: pid, States: states, Warning: warning, CreatedAt: at(-800), UpdatedAt: at(-800)},
			Connection:   name, Project: project,
		}
	}
	routes := []mcp.TrackerRouteInfo{
		route(930, 920, "jira", 1, domain.TrackerMatch{Project: "ENG", Labels: []string{"forecrew"}}, "app", "", map[domain.TicketState]string{domain.TicketInProgress: "In Progress", domain.TicketDone: "Done"}),
		route(931, 920, "jira", 2, domain.TrackerMatch{Project: "MOBILE", Types: []string{"Bug"}}, "mobile", "no transition of MOBILE-4 reaches Done; it offers In Progress, Won't Do", map[domain.TicketState]string{domain.TicketDone: "Done"}),
		route(932, 921, "linear", 1, domain.TrackerMatch{Project: "ENG"}, "app", "", nil),
	}
	identities := []domain.TrackerIdentity{{ID: fid(940), AccountID: fid(1), ConnectionID: fid(920), ExternalID: "JIRAUSER10100", ExternalName: "Ann Lee", UserID: fid(300), CreatedBy: "user:" + fid(300).String(), CreatedAt: at(-700)}}
	ticket := func(id int, key, title string, status domain.TicketLinkStatus, reason domain.TicketWaitReason, project string) mcp.TicketInfo {
		t := mcp.TicketInfo{
			TicketLink: domain.TicketLink{ID: fid(id), AccountID: fid(1), ConnectionID: fid(920), Key: key, Title: title, URL: "https://jira.acme.local/jira/browse/" + key, Status: status, Reason: reason,
				HandedBy: "Marta Rossi", CreatedAt: at(-50), UpdatedAt: at(-40)},
			Connection: "jira",
		}
		if project != "" {
			t.ProjectID, t.Project = fid(10), project
		}
		return t
	}
	unknown := ticket(950, "ENG-21", "Export the audit log <b>as CSV</b>", domain.LinkWaiting, domain.WaitUnknownPerson, "app")
	unknown.GateRef = "GATE-15"
	noRoute := ticket(951, "OPS-3", "Rotate the staging keys", domain.LinkWaiting, domain.WaitNoRoute, "")
	queued := ticket(952, "ENG-22", "Dark mode for the settings page", domain.LinkQueued, "", "app")
	queued.Limit = 3
	fixtureTest := mcp.TrackerConnectionTest{Connection: "jira", OK: true, Checks: []mcp.TrackerCheck{
		{Name: "address", OK: true, Detail: "Jira data_center 10.3.1 at https://jira.acme.local/jira"},
		{Name: "certificate", OK: true, Detail: "trusted"},
		{Name: "login", OK: true, Detail: "signed in as Forecrew (JIRAUSER10200)"},
		{Name: "projects", OK: true, Detail: "ENG, MOBILE: browse, comment and transition"},
		{Name: "transitions", OK: true, Detail: "5 statuses"},
		{Name: "routes", OK: true, Detail: "2 routes map to states the tracker has"},
	}, States: []mcp.TrackerState{{Name: "To Do"}, {Name: "In Progress"}, {Name: "Code Review"}, {Name: "Ready for agents"}, {Name: "Done", Closed: true}}}
	failedTest := mcp.TrackerConnectionTest{Connection: "jira", Checks: []mcp.TrackerCheck{
		{Name: "address", OK: true, Detail: "reached https://jira.acme.local/jira"},
		{Name: "certificate", Detail: "not trusted: x509: certificate signed by unknown authority; add the CA of the server to ca_bundle"},
		{Name: "login", Detail: "not checked: an earlier check failed"},
		{Name: "projects", Detail: "not checked: an earlier check failed"},
		{Name: "transitions", Detail: "not checked: an earlier check failed"},
		{Name: "routes", Detail: "not checked: an earlier check failed"},
	}}
	return []fixtureCase{
		{"tracker-connections", "GET", "/api/v1/tracker-connections", "", 200, "bearer", func(e *env) {
			e.svc.ListTrackerConnectionsFunc = func(context.Context, domain.Actor, any) ([]domain.TrackerConnection, error) { return connections, nil }
		}},
		{"tracker-connections-empty", "GET", "/api/v1/tracker-connections", "", 200, "bearer", func(e *env) {
			e.svc.ListTrackerConnectionsFunc = func(context.Context, domain.Actor, any) ([]domain.TrackerConnection, error) { return nil, nil }
		}},
		{"tracker-routes", "GET", "/api/v1/tracker-routes", "", 200, "bearer", func(e *env) {
			e.svc.ListTrackerRoutesFunc = func(context.Context, domain.Actor, mcp.TrackerConnectionFilterInput) ([]mcp.TrackerRouteInfo, error) {
				return routes, nil
			}
		}},
		{"tracker-identities", "GET", "/api/v1/tracker-identities", "", 200, "bearer", func(e *env) {
			e.svc.ListTrackerIdentitiesFunc = func(context.Context, domain.Actor, mcp.TrackerConnectionFilterInput) ([]domain.TrackerIdentity, error) {
				return identities, nil
			}
		}},
		{"tracker-identity-map", "POST", "/api/v1/tracker-identities", `{"connection":"jira","external_id":"JIRAUSER10300","external_name":"Marta Rossi","user":"` + fid(301).String() + `"}`, 201, "bearer", func(e *env) {
			e.svc.MapTrackerIdentityFunc = func(context.Context, domain.Actor, mcp.MapTrackerIdentityInput) (domain.TrackerIdentity, error) {
				i := identities[0]
				i.ExternalID, i.ExternalName, i.UserID = "JIRAUSER10300", "Marta Rossi", fid(301)
				return i, nil
			}
		}},
		{"tracker-unmapped", "GET", "/api/v1/tracker-people/unmapped", "", 200, "bearer", func(e *env) {
			e.svc.ListUnmappedTrackerPeopleFunc = func(context.Context, domain.Actor, mcp.TrackerConnectionFilterInput) (mcp.UnmappedTrackerPeople, error) {
				return mcp.UnmappedTrackerPeople{People: []mcp.UnmappedTrackerPerson{
					{Connection: "jira", ExternalID: "JIRAUSER10300", ExternalName: "Marta Rossi", Tickets: []string{"ENG-21", "ENG-23"}, Suggestions: []mcp.PersonSuggestion{{User: fid(301), Name: "Marta Rossi", Reason: "same name"}}},
					{Connection: "jira", ExternalID: "JIRAUSER10400", ExternalName: "R. Verdi <i>(contractor)</i>", Tickets: []string{"OPS-3"}, Suggestions: []mcp.PersonSuggestion{}},
				}, Problems: []string{}}, nil
			}
		}},
		{"tracker-unmapped-problem", "GET", "/api/v1/tracker-people/unmapped", "", 200, "bearer", func(e *env) {
			e.svc.ListUnmappedTrackerPeopleFunc = func(context.Context, domain.Actor, mcp.TrackerConnectionFilterInput) (mcp.UnmappedTrackerPeople, error) {
				return mcp.UnmappedTrackerPeople{People: []mcp.UnmappedTrackerPerson{}, Problems: []string{"connection jira: read ticket ENG-21: jira answered 503"}}, nil
			}
		}},
		{"tickets-waiting", "GET", "/api/v1/tickets/waiting?queued=true", "", 200, "bearer", func(e *env) {
			e.svc.ListTicketsWaitingFunc = func(_ context.Context, _ domain.Actor, in mcp.ListTicketsWaitingInput) ([]mcp.TicketInfo, error) {
				if !in.Queued {
					return nil, fmt.Errorf("the page asks for the queued tickets too")
				}
				return []mcp.TicketInfo{unknown, noRoute, queued}, nil
			}
		}},
		{"tickets-waiting-empty", "GET", "/api/v1/tickets/waiting?queued=true", "", 200, "bearer", func(e *env) {
			e.svc.ListTicketsWaitingFunc = func(context.Context, domain.Actor, mcp.ListTicketsWaitingInput) ([]mcp.TicketInfo, error) {
				return nil, nil
			}
		}},
		{"ticket-accept", "POST", "/api/v1/tickets/" + fid(951).String() + "/accept", `{"project":"app"}`, 200, "bearer", func(e *env) {
			e.svc.AcceptTicketFunc = func(context.Context, domain.Actor, mcp.AcceptTicketInput) (mcp.TicketInfo, error) {
				t := noRoute
				t.Status, t.Reason, t.ProjectID, t.Project, t.GoalRef = domain.LinkActive, "", fid(10), "app", "GOAL-5"
				return t, nil
			}
		}},
		{"ticket-dismiss", "POST", "/api/v1/tickets/" + fid(951).String() + "/dismiss", `{"reason":"not for us"}`, 200, "bearer", func(e *env) {
			e.svc.DismissTicketFunc = func(context.Context, domain.Actor, mcp.DismissTicketInput) (mcp.TicketInfo, error) {
				t := noRoute
				t.Status = domain.LinkDismissed
				return t, nil
			}
		}},
		{"tracker-test", "POST", "/api/v1/tracker-connections/jira/test", "", 200, "bearer", func(e *env) {
			e.svc.TestTrackerConnectionFunc = func(context.Context, domain.Actor, mcp.TrackerConnectionRefInput) (mcp.TrackerConnectionTest, error) {
				return fixtureTest, nil
			}
		}},
		{"tracker-test-failed", "POST", "/api/v1/tracker-connections/jira/test", "", 200, "bearer", func(e *env) {
			e.svc.TestTrackerConnectionFunc = func(context.Context, domain.Actor, mcp.TrackerConnectionRefInput) (mcp.TrackerConnectionTest, error) {
				return failedTest, nil
			}
		}},
		{"config-plan-tracker", "POST", "/api/v1/config/plans", `{"title":"Connect tracker linear","source":"form","changes":[{"kind":"tracker_connection","name":"linear","spec":{"kind":"linear","config":{"teams":["ENG"],"token_ref":"store:linear-token"},"triggers":{"label":"forecrew"}}}]}`, 201, "bearer", func(e *env) {
			e.svc.ConfigPlanFunc = func(context.Context, domain.Actor, mcp.ConfigPlanInput) (mcp.ConfigPlanView, error) {
				return trackerPlan(false, domain.ConfigPlanPending), nil
			}
		}},
		{"config-plan-tracker-secret", "PUT", "/api/v1/config/plans/" + fid(390).String() + "/secrets/linear-token", `{"value":"the-token"}`, 200, "bearer", func(e *env) {
			e.svc.ConfigPutSecretFunc = func(context.Context, domain.Actor, mcp.ConfigSecretInput) (mcp.ConfigPlanView, error) {
				return trackerPlan(true, domain.ConfigPlanPending), nil
			}
		}},
		{"config-plan-tracker-apply", "POST", "/api/v1/config/plans/" + fid(390).String() + "/apply", `{}`, 200, "bearer", func(e *env) {
			e.svc.ConfigApplyFunc = func(context.Context, domain.Actor, mcp.ConfigApplyInput) (mcp.ConfigApplied, error) {
				return mcp.ConfigApplied{Plan: trackerPlan(true, domain.ConfigPlanApplied), ShowOnce: []mcp.ConfigShowOnce{}}, nil
			}
		}},
	}
}
