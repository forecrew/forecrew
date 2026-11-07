//go:build smoke

package jira

import (
	"net/http"
	"os"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/secrets"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/tracker"
	"github.com/forecrew/forecrew/internal/tracker/trackertest"
)

// TestLive runs the contract against a real Jira of each deployment that the
// environment names (docs/runbooks/smoke-live.md, Jira). It creates issues
// in the project it is given: use a sandbox project.
func TestLive(t *testing.T) {
	for _, d := range []struct{ name, prefix, auth string }{
		{"data center", "FORECREW_SMOKE_JIRA_DC_", AuthPAT},
		{"cloud", "FORECREW_SMOKE_JIRA_CLOUD_", AuthAPIToken},
	} {
		t.Run(d.name, func(t *testing.T) {
			env := func(k string) string { return os.Getenv(d.prefix + k) }
			if env("URL") == "" {
				t.Skipf("set %sURL, %sTOKEN and %sPROJECT to run it", d.prefix, d.prefix, d.prefix)
			}
			live(t, env, d.auth)
		})
	}
}

func live(t *testing.T, env func(string) string, auth string) {
	vault, err := secrets.New(testkit.NewStore(nil).Secrets(), make([]byte, secrets.KeySize))
	must(t, err)
	account := uuid.NewV7()
	must(t, vault.Put(t.Context(), domain.SystemActor(account, domain.ForecrewdName), account, "jira", []byte(env("TOKEN"))))
	cfg := map[string]any{"base_url": env("URL"), "projects": []string{env("PROJECT")}, "auth": auth, "token_ref": "store:jira"}
	if auth != AuthPAT {
		cfg["email"] = env("EMAIL")
	}
	if f := env("CA_FILE"); f != "" {
		pem, err := os.ReadFile(f)
		must(t, err)
		cfg["ca_bundle"] = string(pem)
	}
	conn := domain.TrackerConnection{ID: uuid.NewV7(), AccountID: account, Name: "live", Kind: Kind, Config: cfg, Triggers: domain.TrackerTriggers{Label: "forecrew"}}
	deps := Deps{Secrets: vault, PrivateAddresses: true}
	ad, err := Factory(deps)(conn)
	must(t, err)
	for _, c := range ad.(*Adapter).Check(t.Context()) {
		t.Logf("check %s: ok=%v %s", c.Name, c.OK, c.Detail)
	}
	prepared, err := ad.(*Adapter).Prepare(t.Context())
	must(t, err)
	conn.Config = prepared
	ad, err = Factory(deps)(conn)
	must(t, err)
	a := ad.(*Adapter)
	issueType := env("ISSUE_TYPE")
	if issueType == "" {
		issueType = "Task"
	}
	trackertest.Run(t, trackertest.Harness{
		Adapter: a,
		NewTicket: func(t *testing.T, title, body string) string {
			fields := map[string]any{"project": map[string]string{"key": env("PROJECT")}, "summary": title, "issuetype": map[string]string{"name": issueType}}
			if body != "" {
				fields["description"] = a.body(body)
			}
			var out struct{ Key string }
			must(t, a.c.do(t.Context(), call{method: http.MethodPost, base: a.api(), path: a.rest("issue"), in: map[string]any{"fields": fields}}, &out))
			return out.Key
		},
		Touch: func(t *testing.T, key string) {
			time.Sleep(time.Second) // the cursor of the first poll is now
			in := map[string]any{"update": map[string]any{"labels": []map[string]string{{"add": "forecrew-smoke"}}}}
			must(t, a.c.do(t.Context(), call{method: http.MethodPut, base: a.api(), path: a.rest("issue", key), in: in}, nil))
			time.Sleep(5 * time.Second) // the search index follows the edit
		},
	})
	var _ tracker.Adapter = a
}
