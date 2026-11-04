package actionsprovider_test

import (
	"encoding/json"
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
		Steps       []step            `yaml:"steps"`
	} `yaml:"jobs"`
}

type step struct {
	ID   string            `yaml:"id"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
}

// skipOrFail skips a test that needs a tool, but where CI is set (GitHub
// Actions sets it) a missing tool is a failure: the check must not vanish
// silently from the pipeline.
func skipOrFail(t *testing.T, why string) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatal(why + " (required in CI)")
	}
	t.Skip(why)
}

// step returns the step of the template's job with the given id or action.
func templateStep(t *testing.T, id string) step {
	t.Helper()
	var w workflow
	if err := yaml.Unmarshal(template(t), &w); err != nil {
		t.Fatal(err)
	}
	for _, s := range w.Jobs["run"].Steps {
		if s.ID == id || (id == "upload" && strings.HasPrefix(s.Uses, "actions/upload-artifact@")) {
			return s
		}
	}
	t.Fatalf("no step %q", id)
	return step{}
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
		bin = filepath.Join(home, "go", "bin", "actionlint")
		if _, err := os.Stat(bin); home == "" || err != nil {
			skipOrFail(t, "actionlint is not installed")
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

// runStep runs the Run step of the template in a fresh directory with the
// given payload, the way a runner would, and returns the directory and what
// the step wrote to GITHUB_OUTPUT.
func runStep(t *testing.T, payload string) (dir, output string) {
	t.Helper()
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			skipOrFail(t, tool+" is not installed")
		}
	}
	dir = t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(t.TempDir(), "output")
	cmd := exec.CommandContext(t.Context(), "bash", "-c", templateStep(t, "run").Run)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PAYLOAD="+payload, "HEAD_SHA="+strings.Repeat("ab", 20), "GITHUB_OUTPUT="+outFile,
		"ImageOS=ubuntu24", "ImageVersion=1.2.3", "RUNNER_OS=Linux", "RUNNER_ARCH=X64")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run step: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(outFile)
	return dir, string(data)
}

func readResult(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "forecrew-out", "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestTemplateRunStep executes the step that runs the command.
func TestTemplateRunStep(t *testing.T) {
	// A setup command that reads its input must not eat the lines after it.
	payload := `{"setup":["cat > /dev/null; echo one > one.txt","echo two > two.txt"],"env":{"GREETING":"it's \"quoted\" $HOME"},
		"argv":["sh","-c","echo \"$GREETING\" > greeting.txt; cat > /dev/null; exit 3"],"workdir":"sub","versions":{"tool":"cat; echo v1"}}`
	d, out := runStep(t, payload)
	for _, f := range []string{"one.txt", "two.txt"} {
		if _, err := os.Stat(filepath.Join(d, f)); err != nil {
			t.Errorf("setup command did not run: %v", err)
		}
	}
	res := readResult(t, d)
	if res["phase"] != "command" || res["exit_code"] != float64(3) || res["head_sha"] != strings.Repeat("ab", 20) ||
		res["image_os"] != "ubuntu24" || res["runner_os"] != "Linux" {
		t.Errorf("result = %v", res)
	}
	if v, _ := res["versions"].(map[string]any); v["tool"] != "v1" {
		t.Errorf("versions = %v", res["versions"])
	}
	if g, _ := os.ReadFile(filepath.Join(d, "sub", "greeting.txt")); string(g) != "it's \"quoted\" $HOME\n" {
		t.Errorf("env value = %q", g)
	}
	if !strings.Contains(out, "exit_code=3") {
		t.Errorf("output = %q", out)
	}
}

func TestTemplateRunStepSetupFailure(t *testing.T) {
	d, out := runStep(t, `{"setup":["exit 4"],"shell":"echo never"}`)
	res := readResult(t, d)
	if res["phase"] != "setup" || !strings.Contains(res["message"].(string), "exit 4") || !strings.Contains(out, "exit_code=1") {
		t.Errorf("result = %v, output %q", res, out)
	}
}

func TestTemplateRunStepShellAndWorkdir(t *testing.T) {
	d, _ := runStep(t, `{"shell":"pwd > where.txt; exit 0","workdir":"../escape"}`)
	if res := readResult(t, d); res["phase"] != "setup" || !strings.Contains(res["message"].(string), "leaves the repo") {
		t.Errorf("result = %v", res)
	}
	d, _ = runStep(t, `{"shell":"echo hi > hi.txt"}`)
	if res := readResult(t, d); res["phase"] != "command" || res["exit_code"] != float64(0) {
		t.Errorf("result = %v", res)
	}
}
