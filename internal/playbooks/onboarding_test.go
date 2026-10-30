package playbooks_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
)

var fullProfile = map[string]any{
	"version": 1, "name": "shop", "summary": "A shop.", "platforms": []any{"linux", "macos"},
	"components": []any{
		map[string]any{"path": ".", "stack": "go", "build": "go build ./...", "test": "go test ./...", "platforms": []any{"linux"}, "needs": []any{"go", "docker"}},
		map[string]any{"path": "web", "stack": "node"},
	},
	"open_questions": []any{"Which Postgres version do the tests need?"},
}

func TestOnboardingProposal(t *testing.T) {
	v := newEnv(t)
	out, err := v.call(t, "onboarding.proposal", map[string]any{
		"profile": fullProfile, "summary": "Found a Go service and a web app.",
		"assumptions": []any{map[string]any{"text": "CI runs on Linux", "confidence": "high", "impact": "low"}},
	})
	must(t, err)
	m := toMap(t, out)
	if m["title"] != "Add the Forecrew project profile" {
		t.Errorf("title = %v", m["title"])
	}
	body := m["body"].(string)
	for _, want := range []string{
		"**shop**", "Merge this PR to approve", "## Summary\n\nFound a Go service and a web app.",
		"- `.` (go); build: `go build ./...`; test: `go test ./...`; platforms: linux; needs: go, docker",
		"- `web` (node)\n", "## Platforms\n\nlinux, macos",
		"- CI runs on Linux (confidence high, impact low)", "## Open questions\n\n- Which Postgres version do the tests need?",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
}

func TestOnboardingProposalMinimalProfile(t *testing.T) {
	v := newEnv(t)
	out, err := v.call(t, "onboarding.proposal", map[string]any{
		"profile": map[string]any{"version": 1, "name": "shop", "summary": "A shop."}, "summary": " ",
	})
	must(t, err)
	body := toMap(t, out)["body"].(string)
	for _, section := range []string{"## Summary", "## Components", "## Platforms", "## Assumptions", "## Open questions"} {
		if strings.Contains(body, section) {
			t.Errorf("body has the empty section %q:\n%s", section, body)
		}
	}
	_, err = v.call(t, "onboarding.proposal", map[string]any{"profile": map[string]any{}, "other": 1})
	wantErr(t, err, "unknown field")
	_, err = v.call(t, "onboarding.proposal", map[string]any{"profile": "components: [", "summary": "x"})
	wantErr(t, err, "profile")
}

func (v *env) getProject(t *testing.T) domain.Project {
	t.Helper()
	p, err := v.st.Projects().Get(t.Context(), v.account, v.project.ID)
	must(t, err)
	return p
}

func TestSettleOnboarding(t *testing.T) {
	tests := []struct {
		status domain.ProjectStatus
		want   domain.ProjectStatus
	}{
		{domain.ProjectOnboarding, domain.ProjectRegistered},
		{domain.ProjectRegistered, domain.ProjectRegistered}, // called twice
		{domain.ProjectOnboarded, domain.ProjectOnboarded},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			v := newEnv(t, tt.status)
			out, err := v.call(t, "onboarding.settle", nil)
			must(t, err)
			if got := v.getProject(t).Status; got != tt.want || toMap(t, out)["status"] != string(tt.want) {
				t.Errorf("status = %s, output %v, want %s", got, out, tt.want)
			}
		})
	}
	v := newEnv(t)
	v.project.ID = domain.NewID()
	_, err := v.call(t, "onboarding.settle", nil)
	wantErr(t, err, "get project")
}

const profileYAML = `# written by the analyst, edited by a human
version: 1
name: shop
summary: A shop.
components:
  - {path: ., stack: go, build: go build ./..., test: go test ./...}
`

func TestCompleteOnboarding(t *testing.T) {
	tests := []struct {
		name    string
		status  domain.ProjectStatus
		file    string // empty means no file
		wantErr string
	}{
		{"onboarding", domain.ProjectOnboarding, profileYAML, ""},
		{"again", domain.ProjectOnboarded, profileYAML, ""},
		{"registered", domain.ProjectRegistered, profileYAML, "is registered, not onboarding"},
		{"no file", domain.ProjectOnboarding, "", "read the merged profile"},
		{"invalid file", domain.ProjectOnboarding, "version: 1\n", "invalid project profile"},
		{"broken file", domain.ProjectOnboarding, "name: [", "invalid project profile"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newEnv(t, tt.status)
			if tt.file != "" {
				must(t, v.tracker.SetFile("acme/app", "main", ".forecrew/profile.yaml", []byte(tt.file)))
			}
			_, err := v.call(t, "onboarding.complete", map[string]any{"repo": "acme/app", "ref": "main"})
			p := v.getProject(t)
			if tt.wantErr != "" {
				wantErr(t, err, tt.wantErr)
				if len(p.Profile) != 0 || p.Status != tt.status {
					t.Errorf("project changed: %s %s", p.Status, p.Profile)
				}
				return
			}
			must(t, err)
			var got map[string]any
			must(t, json.Unmarshal(p.Profile, &got))
			comps, _ := got["components"].([]any)
			if p.Status != domain.ProjectOnboarded || got["name"] != "shop" || len(comps) != 1 {
				t.Errorf("project = %s %s", p.Status, p.Profile)
			}
		})
	}
	v := newEnv(t)
	_, err := v.call(t, "onboarding.complete", map[string]any{"other": 1})
	wantErr(t, err, "unknown field")
	v.project.ID = domain.NewID()
	_, err = v.call(t, "onboarding.complete", map[string]any{"repo": "acme/app", "ref": "main"})
	wantErr(t, err, "get project")
}

func TestCompleteOnboardingReadsTheMergeCommit(t *testing.T) {
	v := newEnv(t, domain.ProjectOnboarding)
	must(t, v.tracker.SetFile("acme/app", "main", ".forecrew/profile.yaml", []byte(`{"version": 1, "name": "main", "summary": "x"}`)))
	must(t, v.tracker.SetFile("acme/app", "merge-1", ".forecrew/profile.yaml", []byte(`{"version": 1, "name": "merged", "summary": "x"}`)))
	_, err := v.call(t, "onboarding.complete", map[string]any{"repo": "acme/app", "sha": "merge-1", "ref": "main"})
	must(t, err)
	if p := v.getProject(t); !strings.Contains(string(p.Profile), `"name":"merged"`) {
		t.Errorf("profile = %s", p.Profile)
	}
}
