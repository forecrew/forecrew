package actionsprovider

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
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
	}{
		{"branch", func(s *domain.JobSpec) { s.Branch = "agent/x" }, "agent/x", map[string]string{"ref": "", "runs_on": "windows-2025", "reports": "r/*.xml\ns.xml", "overlay_ref": ""}},
		{"base branch", func(*domain.JobSpec) {}, "main", map[string]string{"ref": ""}},
		{"base commit runs the workflow of the default branch", func(s *domain.JobSpec) { s.BaseRef = "0123456789ABCDEF0123456789abcdef01234567" }, "trunk",
			map[string]string{"ref": "0123456789abcdef0123456789abcdef01234567"}},
		{"overlay", func(s *domain.JobSpec) {
			s.Overlay = &domain.Overlay{Ref: "agent/x", Paths: []string{"a_test.go", "b/**"}}
		}, "main",
			map[string]string{"overlay_ref": "agent/x", "overlay_paths": "a_test.go\nb/**"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := spec()
			tt.edit(&s)
			p, err := plan(s, "trunk")
			if err != nil {
				t.Fatal(err)
			}
			if p.Ref != tt.ref {
				t.Errorf("ref = %q, want %q", p.Ref, tt.ref)
			}
			tt.checks["job_id"] = s.JobID.String()
			for k, v := range tt.checks {
				if p.Inputs[k] != v {
					t.Errorf("inputs[%s] = %q, want %q", k, p.Inputs[k], v)
				}
			}
		})
	}
}

func TestPlanRefusals(t *testing.T) {
	tests := []struct {
		name string
		edit func(s *domain.JobSpec)
		err  string
	}{
		{"no environment", func(s *domain.JobSpec) { s.Environment = nil }, "provider actions"},
		{"docker environment", func(s *domain.JobSpec) {
			s.Environment = &domain.Environment{Name: "d", Provider: domain.ProviderDocker, Image: "go:1"}
		}, "provider actions"},
		{"invalid spec", func(s *domain.JobSpec) { s.Command = nil }, "command"},
		{"short commit", func(s *domain.JobSpec) { s.BaseRef = "0123abc" }, "short commit SHA"},
		{"NUL byte", func(s *domain.JobSpec) { s.Command.Reports = []string{"a\x00b"} }, "NUL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := spec()
			tt.edit(&s)
			if _, err := plan(s, "trunk"); !errors.Is(err, errNotRunnable) || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("err = %v, want %q", err, tt.err)
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

func TestPlanCommit(t *testing.T) {
	s := spec()
	if p, _ := plan(s, "trunk"); p.Commit != "" {
		t.Errorf("a branch job has commit %q", p.Commit)
	}
	s.BaseRef = "0123456789ABCDEF0123456789abcdef01234567"
	if p, _ := plan(s, "trunk"); p.Commit != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("commit = %q", p.Commit)
	}
}

func TestTimeoutMinutes(t *testing.T) {
	for d, want := range map[time.Duration]int{0: 360, time.Second: 2, time.Minute: 2, 61 * time.Second: 3, 45 * time.Minute: 46, 24 * time.Hour: 360} {
		if got := timeoutMinutes(d); got != want {
			t.Errorf("timeoutMinutes(%v) = %d, want %d", d, got, want)
		}
	}
}
