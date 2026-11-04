package actionsprovider

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

func spec() domain.JobSpec {
	return domain.JobSpec{
		JobID: uuid.New(), Kind: domain.JobKindCommand, Repo: repo, BaseRef: "main", Suite: "unit",
		Command:     &domain.CommandSpec{Shell: "make test", Workdir: "sub", Reports: []string{"r/*.xml", "s.xml"}},
		Environment: &domain.Environment{Name: "e", Provider: domain.ProviderActions, RunsOn: "windows-2025"},
	}
}

func TestPlan(t *testing.T) {
	tests := []struct {
		name   string
		edit   func(s *domain.JobSpec)
		ref    string
		checks map[string]string
		err    string
	}{
		{"branch", func(s *domain.JobSpec) { s.Branch = "agent/x" }, "agent/x", map[string]string{"ref": "agent/x", "runs_on": "windows-2025", "reports": "r/*.xml\ns.xml", "overlay_ref": ""}, ""},
		{"base branch", func(*domain.JobSpec) {}, "main", map[string]string{"ref": "main"}, ""},
		{"base commit runs the workflow of the default branch", func(s *domain.JobSpec) { s.BaseRef = "0123456789abcdef0123456789abcdef01234567" }, "trunk",
			map[string]string{"ref": "0123456789abcdef0123456789abcdef01234567"}, ""},
		{"overlay", func(s *domain.JobSpec) {
			s.Overlay = &domain.Overlay{Ref: "agent/x", Paths: []string{"a_test.go", "b/**"}}
		}, "main",
			map[string]string{"overlay_ref": "agent/x", "overlay_paths": "a_test.go\nb/**"}, ""},
		{"no environment", func(s *domain.JobSpec) { s.Environment = nil }, "", nil, "provider actions"},
		{"docker environment", func(s *domain.JobSpec) {
			s.Environment = &domain.Environment{Name: "d", Provider: domain.ProviderDocker, Image: "go:1"}
		}, "", nil, "provider actions"},
		{"invalid spec", func(s *domain.JobSpec) { s.Command = nil }, "", nil, "command"},
		{"NUL byte", func(s *domain.JobSpec) { s.Command.Reports = []string{"a\x00b"} }, "", nil, "NUL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := spec()
			tt.edit(&s)
			p, err := plan(s, "trunk")
			if tt.err != "" {
				if !errors.Is(err, errNotRunnable) || !strings.Contains(err.Error(), tt.err) {
					t.Errorf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.Ref != tt.ref {
				t.Errorf("ref = %q, want %q", p.Ref, tt.ref)
			}
			for k, v := range tt.checks {
				if p.Inputs[k] != v {
					t.Errorf("inputs[%s] = %q, want %q", k, p.Inputs[k], v)
				}
			}
			if p.Inputs["job_id"] != s.JobID.String() {
				t.Errorf("job_id = %q", p.Inputs["job_id"])
			}
		})
	}
}

// The shell form and the arguments reach the workflow as JSON fields the
// workflow tells apart by presence.
func TestPlanPayload(t *testing.T) {
	p, err := plan(spec(), "main")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(p.Inputs["payload"]), &m); err != nil {
		t.Fatal(err)
	}
	if _, has := m["argv"]; has || m["shell"] != "make test" || m["workdir"] != "sub" {
		t.Errorf("payload = %v", m)
	}
}
