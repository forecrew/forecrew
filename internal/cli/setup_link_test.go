package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// emptyOrg is an organization with no member yet, which a setup link is for.
func emptyOrg(t *testing.T, e *adminEnv) domain.Account {
	t.Helper()
	a := domain.Account{Name: "Fresh"}
	if err := e.db.st.Accounts().Create(t.Context(), &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSetupLink(t *testing.T) {
	e := newAdminEnv(t)
	e.vars["USER"] = "ops"
	fresh := emptyOrg(t, e)
	out, errOut, code := e.run(t, "setup-link", "--org", fresh.Slug, "--url", "https://forecrew.example.com/", "--ttl", "30m")
	link := strings.TrimSpace(out)
	token, ok := strings.CutPrefix(link, "https://forecrew.example.com/setup-link#fcu_")
	if code != 0 || !ok || strings.Contains(link, "\n") {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
	if strings.Contains(errOut, token) {
		t.Errorf("stderr holds the token: %q", errOut)
	}
	stored, err := e.db.st.SetupLinks().GetByTokenHash(t.Context(), mcp.HashToken("fcu_"+token))
	if err != nil || stored.AccountID != fresh.ID || stored.CreatedBy != "user:ops" || !stored.UsedAt.IsZero() {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	entries, err := e.db.st.Audit().List(t.Context(), fresh.ID, store.AuditFilter{})
	if err != nil || len(entries) != 1 || entries[0].Action != "setup_link.created" {
		t.Fatalf("audit = %+v, %v", entries, err)
	}
	a := entries[0]
	if a.Actor != "user:ops" || a.SubjectID != stored.ID || strings.Contains(string(a.Data), token) {
		t.Errorf("audit = %+v", a)
	}
	var data map[string]string
	if err := json.Unmarshal(a.Data, &data); err != nil || data["ttl"] != "30m0s" {
		t.Errorf("audit data = %s", a.Data)
	}
}

func TestSetupLinkRefusals(t *testing.T) {
	e := newAdminEnv(t)
	fresh := emptyOrg(t, e)
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"an organization with a member", []string{"--org", e.account.Slug, "--url", "https://f.example"}, "already has an owner or members"},
		{"unknown organization", []string{"--org", "nope", "--url", "https://f.example"}, "organization nope"},
		{"too long", []string{"--org", fresh.Slug, "--url", "https://f.example", "--ttl", "25h"}, "between a minute and 24 hours"},
		{"too short", []string{"--org", fresh.Slug, "--url", "https://f.example", "--ttl", "5s"}, "between a minute and 24 hours"},
		{"no public URL", []string{"--org", fresh.Slug, "--config", "/missing.yaml"}, "find the public URL"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, errOut, code := e.run(t, append([]string{"setup-link"}, tt.args...)...)
			if code != 1 || !strings.Contains(errOut, tt.want) || strings.Contains(out, "fcu_") {
				t.Errorf("code %d, out %q, err %q", code, out, errOut)
			}
		})
	}
}

// A link is only for the first Owner: once one is made, a second is refused,
// and no link is stored by the refused one.
func TestSetupLinkIsRefusedOnceTheOrganizationHasAnOwner(t *testing.T) {
	e := newAdminEnv(t)
	fresh := emptyOrg(t, e)
	if _, errOut, code := e.run(t, "setup-link", "--org", fresh.Slug, "--url", "https://f.example"); code != 0 {
		t.Fatalf("first link: %d %s", code, errOut)
	}
	addOwner(t, e.db.st, fresh.ID)
	out, errOut, code := e.run(t, "setup-link", "--org", fresh.Slug, "--url", "https://f.example")
	if code != 1 || !strings.Contains(errOut, "already has an owner or members") || strings.Contains(out, "fcu_") {
		t.Errorf("second link: code %d, out %q, err %q", code, out, errOut)
	}
}
