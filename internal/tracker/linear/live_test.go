//go:build smoke

package linear

import (
	"os"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/secrets"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/tracker/trackertest"
)

// TestLive runs the contract against a real Linear workspace
// (docs/runbooks/smoke-live.md, Linear). It creates issues in the team it is
// given: use a sandbox team. It never runs without FORECREW_SMOKE_LINEAR_TOKEN.
func TestLive(t *testing.T) {
	token, team := os.Getenv("FORECREW_SMOKE_LINEAR_TOKEN"), os.Getenv("FORECREW_SMOKE_LINEAR_TEAM")
	if token == "" || team == "" {
		t.Skip("set FORECREW_SMOKE_LINEAR_TOKEN and FORECREW_SMOKE_LINEAR_TEAM to run it")
	}
	vault, err := secrets.New(testkit.NewStore(nil).Secrets(), make([]byte, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	account := uuid.NewV7()
	if err := vault.Put(t.Context(), domain.SystemActor(account, domain.ForecrewdName), account, "linear", []byte(token)); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"teams": []string{team}, "token_ref": "store:linear"}
	if os.Getenv("FORECREW_SMOKE_LINEAR_OAUTH") != "" {
		cfg["auth"] = AuthOAuthToken
	}
	conn := domain.TrackerConnection{ID: uuid.NewV7(), AccountID: account, Name: "live", Kind: Kind, Config: cfg, Triggers: domain.TrackerTriggers{Label: "forecrew"}}
	ad, err := Factory(Deps{Secrets: vault, PrivateAddresses: true})(conn)
	if err != nil {
		t.Fatal(err)
	}
	a := ad.(*Adapter)
	for _, c := range a.Check(t.Context()) {
		t.Logf("check %s: ok=%v %s", c.Name, c.OK, c.Detail)
	}
	trackertest.Run(t, trackertest.Harness{
		Adapter: a,
		NewTicket: func(t *testing.T, title, body string) string {
			var teams struct {
				Teams struct{ Nodes []struct{ ID, Key string } }
			}
			if err := a.c.gql(t.Context(), `query { teams(first: 250) { nodes { id key } } }`, nil, &teams, true); err != nil {
				t.Fatal(err)
			}
			var teamID string
			for _, n := range teams.Teams.Nodes {
				if n.Key == team {
					teamID = n.ID
				}
			}
			var out struct {
				IssueCreate struct{ Issue struct{ Identifier string } }
			}
			q := `mutation($input: IssueCreateInput!) { issueCreate(input: $input) { success issue { identifier } } }`
			if err := a.c.gql(t.Context(), q, map[string]any{"input": map[string]any{"teamId": teamID, "title": title, "description": body}}, &out, false); err != nil {
				t.Fatal(err)
			}
			return out.IssueCreate.Issue.Identifier
		},
		Touch: func(t *testing.T, key string) {
			time.Sleep(time.Second) // the cursor of the first poll is now
			if err := a.c.gql(t.Context(), `mutation($id: String!, $input: IssueUpdateInput!) { issueUpdate(id: $id, input: $input) { success } }`,
				map[string]any{"id": key, "input": map[string]any{"title": "Touched " + key}}, nil, false); err != nil {
				t.Fatal(err)
			}
			time.Sleep(2 * time.Second)
		},
	})
}
