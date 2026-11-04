package actionsprovider_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"go.yaml.in/yaml/v3"

	forecrew "github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/actionsprovider"
)

func template(t *testing.T) []byte {
	t.Helper()
	data, err := actionsprovider.Template(forecrew.Files)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestTemplateMissing(t *testing.T) {
	if _, err := actionsprovider.Template(fstest.MapFS{}); err == nil {
		t.Error("no error without a template")
	}
}

type workflow struct {
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]struct {
		Permissions map[string]string `yaml:"permissions"`
		Steps       []struct {
			Uses string `yaml:"uses"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// TestTemplateIsSafe: every action is pinned by commit, the token is read
// only, no secret is used and no input reaches a shell script.
func TestTemplateIsSafe(t *testing.T) {
	data := template(t)
	var w workflow
	if err := yaml.Unmarshal(data, &w); err != nil {
		t.Fatal(err)
	}
	if len(w.Permissions) != 1 || w.Permissions["contents"] != "read" {
		t.Errorf("permissions = %v, want only contents: read", w.Permissions)
	}
	pinned := regexp.MustCompile(`@[0-9a-f]{40}$`)
	for name, job := range w.Jobs {
		if len(job.Permissions) > 0 {
			t.Errorf("job %s widens the permissions: %v", name, job.Permissions)
		}
		for _, s := range job.Steps {
			if s.Uses != "" && !pinned.MatchString(s.Uses) {
				t.Errorf("%s is not pinned by commit", s.Uses)
			}
			if strings.Contains(s.Run, "${{") {
				t.Errorf("a run script interpolates an expression, which is script injection:\n%s", s.Run)
			}
		}
	}
	if strings.Contains(string(data), "secrets.") {
		t.Error("the template uses secrets")
	}
}

// TestTemplateLint runs actionlint when it is installed.
func TestTemplateLint(t *testing.T) {
	bin, err := exec.LookPath("actionlint")
	if err != nil {
		home, _ := os.UserHomeDir()
		if bin = filepath.Join(home, "go", "bin", "actionlint"); home == "" {
			t.Skip("actionlint is not installed")
		}
		if _, err := os.Stat(bin); err != nil {
			t.Skip("actionlint is not installed")
		}
	}
	file := filepath.Join(t.TempDir(), "forecrew-matrix.yml")
	if err := os.WriteFile(file, template(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(t.Context(), bin, file).CombinedOutput(); err != nil {
		t.Errorf("actionlint: %v\n%s", err, out)
	}
}
