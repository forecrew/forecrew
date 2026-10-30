package playbooks_test

import (
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/testkit"
)

func TestRegisterProject(t *testing.T) {
	tests := []struct {
		name    string
		status  domain.ProjectStatus
		repo    string
		install bool
		wantErr string
		want    domain.ProjectStatus
	}{
		{"registered moves to onboarding", domain.ProjectRegistered, "acme/app", true, "", domain.ProjectOnboarding},
		{"onboarding is kept", domain.ProjectOnboarding, "Acme/App", true, "", domain.ProjectOnboarding},
		{"app not installed", domain.ProjectRegistered, "acme/app", false, "not installed on acme/app", domain.ProjectRegistered},
		{"other repo", domain.ProjectRegistered, "acme/other", true, "not acme/other", domain.ProjectRegistered},
		{"onboarded", domain.ProjectOnboarded, "acme/app", true, "is onboarded, not registered", domain.ProjectOnboarded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newEnv(t, tt.status)
			if !tt.install {
				v.tracker = testkit.NewTracker(v.clock.Now)
				v.register()
			}
			out, err := v.call(t, "project.register", map[string]any{"repo": tt.repo})
			if tt.wantErr != "" {
				wantErr(t, err, tt.wantErr)
			} else if err != nil {
				t.Fatal(err)
			}
			got, err := v.st.Projects().Get(t.Context(), v.account, v.project.ID)
			must(t, err)
			if got.Status != tt.want {
				t.Errorf("status = %s, want %s", got.Status, tt.want)
			}
			if tt.wantErr == "" {
				m := toMap(t, out)
				if m["project_id"] != v.project.ID.String() || m["repo"] != "acme/app" || m["default_branch"] != "main" {
					t.Errorf("output = %v", m)
				}
			}
		})
	}
}

func TestRegisterProjectErrors(t *testing.T) {
	v := newEnv(t)
	_, err := v.call(t, "project.register", map[string]any{"repo": "acme/app", "extra": 1})
	wantErr(t, err, "unknown field")
	v.project.ID = domain.NewID()
	_, err = v.call(t, "project.register", map[string]any{"repo": "acme/app"})
	wantErr(t, err, "get project")
}
