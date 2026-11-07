package github_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/github"
)

// graphqlEnv answers /graphql with answer(query, variables); the App is
// installed on the acme organization.
func graphqlEnv(t *testing.T, answer func(query string, vars map[string]any) (int, string)) (*env, *[]string) {
	t.Helper()
	e := newEnv(t)
	var queries []string
	e.mux.HandleFunc("GET /orgs/acme/installation", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"id": 42}`) })
	e.mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ghs_") {
			http.Error(w, "need an installation token", http.StatusUnauthorized)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string
			Variables map[string]any
		}
		_ = json.Unmarshal(raw, &req)
		queries = append(queries, req.Query)
		status, body := answer(req.Query, req.Variables)
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	})
	return e, &queries
}

const boardAnswer = `{"data": {"organization": {"projectV2": {"id": "PVT_1", "field": {"id": "PVTSSF_1", "options": [
  {"id": "o1", "name": "Todo"}, {"id": "o2", "name": "Forecrew"}, {"id": "o3", "name": "Done"}]}}}}}`

func TestProjectBoard(t *testing.T) {
	e, _ := graphqlEnv(t, func(string, map[string]any) (int, string) { return 200, boardAnswer })
	b, err := e.client.ProjectBoard(t.Context(), "acme", 7, "Status")
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != "PVT_1" || b.FieldID != "PVTSSF_1" || len(b.Options) != 3 || b.Options[1].Name != "Forecrew" || b.Options[1].ID != "o2" {
		t.Errorf("board = %+v", b)
	}
}

func TestProjectBoardMissing(t *testing.T) {
	for name, body := range map[string]string{
		"no board":            `{"data": {"organization": {"projectV2": null}}}`,
		"no org":              `{"data": {"organization": null}}`,
		"no field":            `{"data": {"organization": {"projectV2": {"id": "PVT_1", "field": null}}}}`,
		"not a single select": `{"data": {"organization": {"projectV2": {"id": "PVT_1", "field": {}}}}}`,
		"graphql not found":   `{"data": null, "errors": [{"type": "NOT_FOUND", "message": "Could not resolve to an Organization"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := graphqlEnv(t, func(string, map[string]any) (int, string) { return 200, body })
			if _, err := e.client.ProjectBoard(t.Context(), "acme", 7, "Status"); !errors.Is(err, github.ErrNotFound) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestGraphQLFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"forbidden status":  {403, `{}`, github.ErrForbidden},
		"unauthorized":      {401, `{}`, github.ErrForbidden},
		"scopes":            {200, `{"errors": [{"type": "INSUFFICIENT_SCOPES", "message": "no"}]}`, github.ErrForbidden},
		"forbidden type":    {200, `{"errors": [{"type": "FORBIDDEN", "message": "no"}]}`, github.ErrForbidden},
		"rate limited type": {200, `{"errors": [{"type": "RATE_LIMITED", "message": "slow"}]}`, github.ErrRateLimited},
		"too many":          {429, `{}`, github.ErrRateLimited},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := graphqlEnv(t, func(string, map[string]any) (int, string) { return tc.status, tc.body })
			if _, err := e.client.ProjectBoard(t.Context(), "acme", 7, "Status"); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"server error":  {500, `oops`},
		"other error":   {200, `{"errors": [{"type": "OTHER", "message": "huh"}]}`},
		"bad json":      {200, `not json`},
		"bad data type": {200, `{"data": []}`},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := graphqlEnv(t, func(string, map[string]any) (int, string) { return tc.status, tc.body })
			_, err := e.client.ProjectBoard(t.Context(), "acme", 7, "Status")
			if err == nil || errors.Is(err, github.ErrNotFound) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestGraphQLNotInstalled(t *testing.T) {
	e := newEnv(t)
	e.mux.HandleFunc("GET /orgs/other/installation", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	if _, err := e.client.ProjectBoard(t.Context(), "other", 1, "Status"); !errors.Is(err, github.ErrNotInstalled) {
		t.Errorf("err = %v", err)
	}
}

func TestGraphQLTokenIsCached(t *testing.T) {
	e, _ := graphqlEnv(t, func(string, map[string]any) (int, string) { return 200, boardAnswer })
	for range 3 {
		if _, err := e.client.ProjectBoard(t.Context(), "acme", 7, "Status"); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.tokenBodies) != 1 {
		t.Errorf("tokens minted = %d", len(e.tokenBodies))
	}
	e.clock.Advance(2 * time.Hour)
	if _, err := e.client.ProjectBoard(t.Context(), "acme", 7, "Status"); err != nil {
		t.Fatal(err)
	}
	if len(e.tokenBodies) != 2 {
		t.Errorf("tokens minted after expiry = %d", len(e.tokenBodies))
	}
}

const (
	issueCard = `{"id": "PVTI_1", "type": "ISSUE", "isArchived": false, "updatedAt": "2026-03-01T10:00:00Z",
	  "status": {"name": "Forecrew", "updatedAt": "2026-03-01T11:00:00Z"},
	  "content": {"__typename": "Issue", "id": "I_1", "number": 12, "title": "Add export", "body": "Export it.", "state": "OPEN", "url": "https://github.com/acme/app/issues/12",
	    "updatedAt": "2026-03-01T09:00:00Z", "repository": {"nameWithOwner": "acme/app"},
	    "author": {"__typename": "User", "login": "ann", "databaseId": 5},
	    "labels": {"nodes": [{"name": "bug"}]}, "assignees": {"nodes": [{"__typename": "User", "login": "bob", "databaseId": 6}]}, "issueType": {"name": "Bug"}}}`
	draftCard = `{"id": "PVTI_2", "type": "DRAFT_ISSUE", "isArchived": true, "updatedAt": "2026-03-01T08:00:00Z", "status": null,
	  "content": {"__typename": "DraftIssue", "title": "Idea", "body": "Think", "updatedAt": "2026-03-01T07:00:00Z"}}`
	prCard       = `{"id": "PVTI_3", "type": "PULL_REQUEST", "isArchived": false, "updatedAt": "2026-03-01T06:00:00Z", "status": null, "content": {"__typename": "PullRequest"}}`
	redactedCard = `{"id": "PVTI_4", "type": "REDACTED", "isArchived": false, "updatedAt": "2026-03-01T05:00:00Z", "status": null, "content": null}`
)

func TestProjectItems(t *testing.T) {
	pages := 0
	e, queries := graphqlEnv(t, func(_ string, vars map[string]any) (int, string) {
		pages++
		if vars["after"] == nil {
			return 200, `{"data": {"organization": {"projectV2": {"items": {"pageInfo": {"hasNextPage": true, "endCursor": "c1"}, "nodes": [` + issueCard + `, ` + draftCard + `]}}}}}`
		}
		if vars["after"] != "c1" || vars["field"] != "Status" {
			t.Errorf("vars = %v", vars)
		}
		return 200, `{"data": {"organization": {"projectV2": {"items": {"pageInfo": {"hasNextPage": false}, "nodes": [` + prCard + `, ` + redactedCard + `]}}}}}`
	})
	items, err := e.client.ProjectItems(t.Context(), "acme", 7, "Status", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if pages != 2 || len(*queries) != 2 || len(items) != 4 {
		t.Fatalf("pages = %d, items = %d", pages, len(items))
	}
	i := items[0]
	want := github.ProjectItem{
		ID: "PVTI_1", Kind: github.ItemIssue, Status: "Forecrew", StatusAt: time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC), Title: "Add export", Body: "Export it.",
		URL: "https://github.com/acme/app/issues/12", Repo: "acme/app", Number: 12, IssueNodeID: "I_1", Type: "Bug", Labels: []string{"bug"},
		Assignees: []github.User{{ID: 6, Login: "bob"}}, Author: github.User{ID: 5, Login: "ann"}, UpdatedAt: time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC),
	}
	if fmt.Sprintf("%+v", i) != fmt.Sprintf("%+v", want) {
		t.Errorf("issue card = %+v\nwant %+v", i, want)
	}
	if d := items[1]; d.Kind != github.ItemDraft || !d.Archived || d.Repo != "" || d.Title != "Idea" || d.Status != "" {
		t.Errorf("draft card = %+v", d)
	}
	if items[2].Kind != github.ItemOther || items[3].Kind != github.ItemOther {
		t.Errorf("other cards = %+v %+v", items[2], items[3])
	}

	recent, err := e.client.ProjectItems(t.Context(), "acme", 7, "Status", time.Date(2026, 3, 1, 10, 30, 0, 0, time.UTC))
	if err != nil || len(recent) != 1 || recent[0].ID != "PVTI_1" {
		t.Errorf("recent = %+v, %v", recent, err)
	}
}

func TestProjectItemsMissingBoard(t *testing.T) {
	e, _ := graphqlEnv(t, func(string, map[string]any) (int, string) { return 200, `{"data": {"organization": null}}` })
	if _, err := e.client.ProjectItems(t.Context(), "acme", 7, "Status", time.Time{}); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	e, _ = graphqlEnv(t, func(string, map[string]any) (int, string) { return 500, `` })
	if _, err := e.client.ProjectItems(t.Context(), "acme", 7, "Status", time.Time{}); err == nil {
		t.Error("a failed page was taken for the end")
	}
}

func TestProjectItem(t *testing.T) {
	node := func(project string, number int, owner string) string {
		return fmt.Sprintf(`{"data": {"node": {"project": {"number": %d, "owner": {"login": %q}}, %s}}`, number, owner, strings.TrimPrefix(project, "{"))
	}
	for name, tc := range map[string]struct {
		body string
		ok   bool
	}{
		"its board":           {node(issueCard, 7, "Acme"), true},
		"another board":       {node(issueCard, 8, "acme"), false},
		"another owner":       {node(issueCard, 7, "evil"), false},
		"not an item":         {`{"data": {"node": {}}}`, false},
		"no node":             {`{"data": {"node": null}}`, false},
		"a deleted card":      {`{"data": null, "errors": [{"type": "NOT_FOUND", "message": "gone"}]}`, false},
		"a project with none": {`{"data": {"node": {"id": "PVTI_1"}}}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := graphqlEnv(t, func(_ string, vars map[string]any) (int, string) {
				if vars["id"] != "PVTI_1" {
					t.Errorf("vars = %v", vars)
				}
				return 200, tc.body
			})
			it, err := e.client.ProjectItem(t.Context(), "acme", 7, "Status", "PVTI_1")
			switch {
			case tc.ok && (err != nil || it.Number != 12):
				t.Errorf("item = %+v, %v", it, err)
			case !tc.ok && !errors.Is(err, github.ErrNotFound):
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestProjectStatusChanges(t *testing.T) {
	e, _ := graphqlEnv(t, func(_ string, vars map[string]any) (int, string) {
		if vars["id"] == "I_gone" {
			return 200, `{"data": {"node": null}}`
		}
		return 200, `{"data": {"node": {"timelineItems": {"nodes": [
		  {"createdAt": "2026-03-01T10:00:00Z", "status": "Todo", "previousStatus": "", "wasAutomated": true, "project": {"id": "PVT_1"}, "actor": null},
		  {"createdAt": "2026-03-01T11:00:00Z", "status": "Forecrew", "previousStatus": "Todo", "wasAutomated": false, "project": {"id": "PVT_1"},
		   "actor": {"__typename": "User", "login": "ann", "databaseId": 5}},
		  {"createdAt": "2026-03-01T12:00:00Z", "status": "Done", "previousStatus": "Forecrew", "wasAutomated": false, "project": null,
		   "actor": {"__typename": "Bot", "login": "forecrew[bot]", "databaseId": 9}}]}}}}`
	})
	got, err := e.client.ProjectStatusChanges(t.Context(), "acme", "I_1")
	if err != nil || len(got) != 3 {
		t.Fatalf("changes = %+v, %v", got, err)
	}
	if got[0].Actor != (github.User{}) || !got[0].Automated || got[0].Project != "PVT_1" {
		t.Errorf("automated change = %+v", got[0])
	}
	if got[1].Actor != (github.User{ID: 5, Login: "ann"}) || got[1].From != "Todo" || got[1].To != "Forecrew" || got[1].Automated {
		t.Errorf("move = %+v", got[1])
	}
	if !got[2].Actor.Bot || got[2].Project != "" {
		t.Errorf("bot move = %+v", got[2])
	}
	if _, err := e.client.ProjectStatusChanges(t.Context(), "acme", "I_gone"); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestSetProjectStatus(t *testing.T) {
	var got map[string]any
	e, queries := graphqlEnv(t, func(_ string, vars map[string]any) (int, string) {
		got = vars
		return 200, `{"data": {"updateProjectV2ItemFieldValue": {"projectV2Item": {"id": "PVTI_1"}}}}`
	})
	board := github.ProjectBoard{ID: "PVT_1", FieldID: "PVTSSF_1"}
	if err := e.client.SetProjectStatus(t.Context(), "acme", board, "PVTI_1", "o3"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*queries)[0], "updateProjectV2ItemFieldValue") || got["project"] != "PVT_1" || got["item"] != "PVTI_1" || got["field"] != "PVTSSF_1" || got["option"] != "o3" {
		t.Errorf("vars = %v", got)
	}
}

func TestGraphQLTokenIsNarrow(t *testing.T) {
	e, _ := graphqlEnv(t, func(string, map[string]any) (int, string) { return 200, boardAnswer })
	if _, err := e.client.ProjectBoard(t.Context(), "acme", 7, "Status"); err != nil {
		t.Fatal(err)
	}
	perms, _ := e.tokenBodies[0]["permissions"].(map[string]any)
	if len(perms) != 2 || perms["organization_projects"] != "write" || perms["issues"] != "read" {
		t.Errorf("token permissions = %v", e.tokenBodies[0])
	}
}

func TestProjectItemsAreScannedLean(t *testing.T) {
	e, queries := graphqlEnv(t, func(string, map[string]any) (int, string) {
		return 200, `{"data": {"organization": {"projectV2": {"items": {"pageInfo": {"hasNextPage": false}, "nodes": [
		  {"id": "PVTI_1", "isArchived": false, "updatedAt": "2026-03-01T10:00:00Z", "status": {"name": "Forecrew", "updatedAt": "2026-03-01T11:00:00Z"},
		   "content": {"__typename": "Issue", "updatedAt": "2026-03-01T09:00:00Z"}}]}}}}}`
	})
	items, err := e.client.ProjectItems(t.Context(), "acme", 7, "Status", time.Time{})
	if err != nil || len(items) != 1 || items[0].Kind != github.ItemIssue || items[0].Status != "Forecrew" || !items[0].UpdatedAt.Equal(time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("items = %+v, %v", items, err)
	}
	for _, field := range []string{"body", "title", "labels", "assignees", "author", "repository"} {
		if strings.Contains((*queries)[0], field) {
			t.Errorf("the scan asks for %s", field)
		}
	}
}

func TestProjectItemsPageCap(t *testing.T) {
	pages := 0
	e, _ := graphqlEnv(t, func(string, map[string]any) (int, string) {
		pages++
		return 200, `{"data": {"organization": {"projectV2": {"items": {"pageInfo": {"hasNextPage": true, "endCursor": "more"}, "nodes": []}}}}}`
	})
	_, err := e.client.ProjectItems(t.Context(), "acme", 7, "Status", time.Time{})
	if !errors.Is(err, github.ErrTooLarge) || !strings.Contains(err.Error(), "rely on webhooks") || pages != github.MaxScanPages {
		t.Errorf("err = %v after %d pages", err, pages)
	}
}

// A card whose issue the App may not read is a card it cannot use, not a
// board it cannot poll.
func TestGraphQLPartialErrors(t *testing.T) {
	forbidden := func(path string) string {
		return `"errors": [{"type": "FORBIDDEN", "message": "Resource not accessible by integration", "path": ` + path + `}]`
	}
	items := `{"data": {"organization": {"projectV2": {"items": {"pageInfo": {"hasNextPage": false}, "nodes": [
	  {"id": "PVTI_1", "isArchived": false, "updatedAt": "2026-03-01T10:00:00Z", "status": null, "content": null},
	  {"id": "PVTI_2", "isArchived": false, "updatedAt": "2026-03-01T10:00:00Z", "status": null, "content": {"__typename": "DraftIssue", "updatedAt": "2026-03-01T10:00:00Z"}}]}}}}, `
	e, _ := graphqlEnv(t, func(string, map[string]any) (int, string) {
		return 200, items + forbidden(`["organization", "projectV2", "items", "nodes", 0, "content"]`) + "}"
	})
	got, err := e.client.ProjectItems(t.Context(), "acme", 7, "Status", time.Time{})
	if err != nil || len(got) != 2 || got[0].Kind != github.ItemOther || got[1].Kind != github.ItemDraft {
		t.Fatalf("items = %+v, %v", got, err)
	}

	e, _ = graphqlEnv(t, func(string, map[string]any) (int, string) {
		return 200, `{"data": {"node": {"id": "PVTI_1", "project": {"number": 7, "owner": {"login": "acme"}}, "content": null}}, ` + forbidden(`["node", "content"]`) + "}"
	})
	it, err := e.client.ProjectItem(t.Context(), "acme", 7, "Status", "PVTI_1")
	if err != nil || it.Kind != github.ItemOther || it.ID != "PVTI_1" {
		t.Errorf("item = %+v, %v", it, err)
	}

	// An error anywhere else fails the call, whatever else came back, and so
	// does one with no data.
	for name, body := range map[string]string{
		"elsewhere":      items + forbidden(`["organization", "projectV2", "items", "pageInfo"]`) + "}",
		"mixed":          items + `"errors": [{"type": "FORBIDDEN", "message": "x", "path": ["organization", "projectV2", "items", "nodes", 0, "content"]}, {"type": "FORBIDDEN", "message": "y", "path": ["organization"]}]}`,
		"no path":        items + `"errors": [{"type": "FORBIDDEN", "message": "x"}]}`,
		"a content name": items + forbidden(`["organization", "projectV2", "content"]`) + "}",
		"no data":        `{"data": null, ` + forbidden(`["node", "content"]`) + "}",
	} {
		e, _ := graphqlEnv(t, func(string, map[string]any) (int, string) { return 200, body })
		if _, err := e.client.ProjectItems(t.Context(), "acme", 7, "Status", time.Time{}); !errors.Is(err, github.ErrForbidden) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
