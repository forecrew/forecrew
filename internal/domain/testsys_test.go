package domain

import (
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestValidName(t *testing.T) {
	for _, ok := range []string{"linux-go", "ubuntu-24.04", "a", "e2e_suite", strings.Repeat("a", 64)} {
		if !ValidName(ok) {
			t.Errorf("ValidName(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "Linux", "-x", ".x", "a b", "a/b", strings.Repeat("a", 65)} {
		if ValidName(bad) {
			t.Errorf("ValidName(%q) = true", bad)
		}
	}
}

func TestSecretLikeName(t *testing.T) {
	for _, n := range []string{"GITHUB_TOKEN", "aws_secret", "DB_PASSWORD", "API_KEY", "token", " KEY "} {
		if !SecretLikeName(n) {
			t.Errorf("SecretLikeName(%q) = false", n)
		}
	}
	for _, n := range []string{"GOFLAGS", "KEYBOARD", "TOKENS_DIR", "MONKEY", "PATH"} {
		if SecretLikeName(n) {
			t.Errorf("SecretLikeName(%q) = true", n)
		}
	}
}

func TestRelativePath(t *testing.T) {
	for _, ok := range []string{"a", "reports/**/*.xml", "./x"} {
		if !RelativePath(ok) {
			t.Errorf("RelativePath(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "/etc", "../x", "a/../../b", `a\b`} {
		if RelativePath(bad) {
			t.Errorf("RelativePath(%q) = true", bad)
		}
	}
}

func TestValidArtifactName(t *testing.T) {
	for _, ok := range []string{"junit.xml", "reports/unit/junit.xml", "screen_1.png"} {
		if !ValidArtifactName(ok) {
			t.Errorf("ValidArtifactName(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", ".env", "a/../b", "a//b", "/a", "a b", "a/.git/x", strings.Repeat("a", 256)} {
		if ValidArtifactName(bad) {
			t.Errorf("ValidArtifactName(%q) = true", bad)
		}
	}
}

func TestEnvironmentValidate(t *testing.T) {
	health := &Health{Command: "pg_isready", Interval: Duration(time.Second), Retries: 5}
	tests := []struct {
		name string
		env  Environment
		want string // substring of the error; empty means valid
	}{
		{"docker image", Environment{Name: "linux-go", Provider: ProviderDocker, Image: "golang:1.27",
			Env: map[string]string{"CGO_ENABLED": "0"}, Versions: map[string]string{"go": "go version"},
			Services: map[string]Service{"postgres": {Image: "postgres:18", Env: map[string]string{"POSTGRES_DB": "t"}, Ports: []int{5432}, Health: health}}}, ""},
		{"dockerfile with context", Environment{Name: "d", Provider: ProviderDocker, Dockerfile: "ci/Dockerfile", Context: "."}, ""},
		{"devcontainer", Environment{Name: "d", Provider: ProviderDocker, Devcontainer: ".devcontainer/devcontainer.json"}, ""},
		{"machine", Environment{Name: "win", Provider: ProviderMachine, Needs: []string{"windows"}, Setup: []string{"go version"}}, ""},
		{"actions", Environment{Name: "mac", Provider: ProviderActions, RunsOn: "macos-15"}, ""},
		{"bad name", Environment{Name: "Bad", Provider: ProviderMachine}, "name"},
		{"no provider", Environment{Name: "x"}, "provider must be"},
		{"docker without source", Environment{Name: "x", Provider: ProviderDocker}, "exactly one of image"},
		{"docker two sources", Environment{Name: "x", Provider: ProviderDocker, Image: "a", Dockerfile: "b"}, "exactly one of image"},
		{"context without dockerfile", Environment{Name: "x", Provider: ProviderDocker, Image: "a", Context: "."}, "context needs dockerfile"},
		{"docker runs_on", Environment{Name: "x", Provider: ProviderDocker, Image: "a", RunsOn: "ubuntu-24.04"}, "runs_on is only"},
		{"machine image", Environment{Name: "x", Provider: ProviderMachine, Image: "a"}, "only for provider docker"},
		{"machine runs_on", Environment{Name: "x", Provider: ProviderMachine, RunsOn: "a"}, "runs_on is only"},
		{"machine services", Environment{Name: "x", Provider: ProviderMachine, Services: map[string]Service{"db": {Image: "a"}}}, "services are only for provider docker"},
		{"actions services", Environment{Name: "x", Provider: ProviderActions, RunsOn: "a", Services: map[string]Service{"db": {Image: "a"}}}, "services are only for provider docker"},
		{"image flag", Environment{Name: "x", Provider: ProviderDocker, Image: "--privileged"}, "must not contain white space or start with '-'"},
		{"image space", Environment{Name: "x", Provider: ProviderDocker, Image: "a b"}, "white space"},
		{"service image flag", Environment{Name: "x", Provider: ProviderDocker, Image: "a", Services: map[string]Service{"db": {Image: "-v"}}}, "services.db: image"},
		{"services share a variable", Environment{Name: "x", Provider: ProviderDocker, Image: "a",
			Services: map[string]Service{"my-db": {Image: "a"}, "my_db": {Image: "a"}}}, "both give the job MY_DB_HOST"},
		{"actions without runs_on", Environment{Name: "x", Provider: ProviderActions}, "needs runs_on"},
		{"absolute dockerfile", Environment{Name: "x", Provider: ProviderDocker, Dockerfile: "/Dockerfile"}, "relative"},
		{"empty setup", Environment{Name: "x", Provider: ProviderMachine, Setup: []string{""}}, "empty command"},
		{"secret env", Environment{Name: "x", Provider: ProviderMachine, Env: map[string]string{"NPM_TOKEN": "abc"}}, "env.NPM_TOKEN: looks like a secret"},
		{"empty env name", Environment{Name: "x", Provider: ProviderMachine, Env: map[string]string{"": "abc"}}, "empty variable name"},
		{"empty version", Environment{Name: "x", Provider: ProviderMachine, Versions: map[string]string{"go": ""}}, "versions"},
		{"service secret", Environment{Name: "x", Provider: ProviderDocker, Image: "a",
			Services: map[string]Service{"db": {Image: "postgres", Env: map[string]string{"POSTGRES_PASSWORD": "p"}}}}, "services.db.env.POSTGRES_PASSWORD"},
		{"service without image", Environment{Name: "x", Provider: ProviderDocker, Image: "a", Services: map[string]Service{"db": {}}}, "services.db: image"},
		{"service bad name", Environment{Name: "x", Provider: ProviderDocker, Image: "a", Services: map[string]Service{"DB": {Image: "a"}}}, "services.DB: name"},
		{"service port", Environment{Name: "x", Provider: ProviderDocker, Image: "a", Services: map[string]Service{"db": {Image: "a", Ports: []int{70000}}}}, "port 70000"},
		{"service health", Environment{Name: "x", Provider: ProviderDocker, Image: "a", Services: map[string]Service{"db": {Image: "a", Health: &Health{}}}}, "health"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.env.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate = %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidSpec) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestToolboxValidate(t *testing.T) {
	good := Toolbox{Name: "security", Image: "ghcr.io/x/security:1", Needs: []string{"linux"},
		CLIs:       []ToolCLI{{Name: "semgrep", Version: "semgrep --version"}},
		MCPServers: []MCPServer{{Name: "browser", Command: "npx", Args: []string{"@playwright/mcp"}, Env: map[string]string{"HEADLESS": "1"}, CredentialEnv: []string{"BROWSER_TOKEN"}}}}
	if err := good.Validate(); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	tests := []struct {
		name string
		mut  func(*Toolbox)
		want string
	}{
		{"bad name", func(tb *Toolbox) { tb.Name = "X" }, "name"},
		{"no image", func(tb *Toolbox) { tb.Image = "" }, "exactly one of image"},
		{"both", func(tb *Toolbox) { tb.Dockerfile = "security.Dockerfile" }, "exactly one of image"},
		{"absolute dockerfile", func(tb *Toolbox) { tb.Image, tb.Dockerfile = "", "/x" }, "relative"},
		{"duplicate cli", func(tb *Toolbox) { tb.CLIs = append(tb.CLIs, tb.CLIs[0]) }, "clis: name"},
		{"cli version", func(tb *Toolbox) { tb.CLIs[0].Version = "" }, "version command"},
		{"duplicate mcp", func(tb *Toolbox) { tb.MCPServers = append(tb.MCPServers, tb.MCPServers[0]) }, "mcp_servers: name"},
		{"mcp command", func(tb *Toolbox) { tb.MCPServers[0].Command = "" }, "command must be set"},
		{"mcp secret env", func(tb *Toolbox) { tb.MCPServers[0].Env = map[string]string{"API_KEY": "x"} }, "API_KEY: looks like a secret"},
		{"mcp empty credential", func(tb *Toolbox) { tb.MCPServers[0].CredentialEnv = []string{""} }, "credential_env"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tb := good
			tb.CLIs = slices.Clone(good.CLIs)
			tb.MCPServers = slices.Clone(good.MCPServers)
			tt.mut(&tb)
			if err := tb.Validate(); !errors.Is(err, ErrInvalidSpec) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate = %v, want %q", err, tt.want)
			}
		})
	}
}

func commandSpec() JobSpec {
	return JobSpec{JobID: NewID(), Kind: JobKindCommand, Repo: "o/r", BaseRef: "main", Suite: "unit",
		Cell: map[string]string{"os": "linux"},
		Command: &CommandSpec{Argv: []string{"gotestsum", "--junitfile", "junit.xml"}, Env: map[string]string{"GOFLAGS": "-count=1"},
			Workdir: "svc", Reports: []string{"junit.xml"}, Artifacts: []string{"coverage.out"}},
		Environment: &Environment{Name: "linux-go", Provider: ProviderDocker, Image: "golang:1.27"},
		Overlay:     &Overlay{Ref: "agent/task-12", Paths: []string{"svc/new_test.go"}},
		Timeout:     Duration(20 * time.Minute)}
}

func agentSpec() JobSpec {
	return JobSpec{JobID: NewID(), Repo: "o/r", Runtime: "claude-code", Model: "m", Prompt: "p",
		WritePaths: []string{"**"}, LockedPaths: []string{"svc/*_test.go"},
		Toolbox: &Toolbox{Name: "security", Image: "img"}}
}

func TestJobSpecValidate(t *testing.T) {
	if err := commandSpec().Validate(); err != nil {
		t.Fatalf("command: %v", err)
	}
	if err := agentSpec().Validate(); err != nil {
		t.Fatalf("agent: %v", err)
	}
	shell := commandSpec()
	shell.Command.Argv, shell.Command.Shell = nil, "make test"
	shell.Environment, shell.Overlay = nil, nil
	if err := shell.Validate(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	tests := []struct {
		name string
		spec func() JobSpec
		want string
	}{
		{"unknown kind", func() JobSpec { s := agentSpec(); s.Kind = "script"; return s }, "kind must be"},
		{"agent without prompt", func() JobSpec { s := agentSpec(); s.Prompt = ""; return s }, "needs runtime, model and prompt"},
		{"agent with command", func() JobSpec { s := agentSpec(); s.Suite = "unit"; return s }, "only for command jobs"},
		{"agent locked negation", func() JobSpec { s := agentSpec(); s.LockedPaths = []string{"!x"}; return s }, "locked_paths"},
		{"agent bad toolbox", func() JobSpec { s := agentSpec(); s.Toolbox.Image = ""; return s }, "toolbox"},
		{"command with prompt", func() JobSpec { s := commandSpec(); s.Prompt = "p"; return s }, "no runtime, model, prompt"},
		{"command with write paths", func() JobSpec { s := commandSpec(); s.WritePaths = []string{"**"}; return s }, "never commits"},
		{"command missing", func() JobSpec { s := commandSpec(); s.Command = nil; return s }, "command must be set"},
		{"argv and shell", func() JobSpec { s := commandSpec(); s.Command.Shell = "x"; return s }, "exactly one of argv and shell"},
		{"neither", func() JobSpec { s := commandSpec(); s.Command.Argv = nil; return s }, "exactly one of argv and shell"},
		{"empty program", func() JobSpec { s := commandSpec(); s.Command.Argv = []string{""}; return s }, "exactly one of argv and shell"},
		{"secret env", func() JobSpec { s := commandSpec(); s.Command.Env = map[string]string{"NPM_TOKEN": "x"}; return s }, "command.env.NPM_TOKEN"},
		{"workdir escapes", func() JobSpec { s := commandSpec(); s.Command.Workdir = "../x"; return s }, "command.workdir"},
		{"report absolute", func() JobSpec { s := commandSpec(); s.Command.Reports = []string{"/tmp/j.xml"}; return s }, "command.reports"},
		{"overlay without paths", func() JobSpec { s := commandSpec(); s.Overlay.Paths = nil; return s }, "overlay needs ref and paths"},
		{"bad suite", func() JobSpec { s := commandSpec(); s.Suite = "Unit Tests"; return s }, "suite"},
		{"empty axis", func() JobSpec { s := commandSpec(); s.Cell = map[string]string{"": "x"}; return s }, "empty axis"},
		{"bad environment", func() JobSpec { s := commandSpec(); s.Environment.Image = ""; return s }, "environment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.spec().Validate(); !errors.Is(err, ErrInvalidSpec) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestJobSpecSecretsBothKinds(t *testing.T) {
	for _, s := range []JobSpec{agentSpec(), commandSpec()} {
		s.Env = map[string]string{"ANTHROPIC_API_KEY": "sk-secret-value"}
		s.RepoToken = "ghs_secret_token"
		r := s.Redacted()
		if r.Env != nil || r.RepoToken != "" {
			t.Fatalf("%s: Redacted kept secrets: %+v", s.Kind.OrDefault(), r)
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var buf strings.Builder
		slog.New(slog.NewJSONHandler(&buf, nil)).Info("job", "spec", s)
		for _, out := range []string{string(b), buf.String()} {
			if strings.Contains(out, "secret") {
				t.Fatalf("%s: secret leaked: %s", s.Kind.OrDefault(), out)
			}
		}
		if s.IsCommand() && (!strings.Contains(buf.String(), `"suite":"unit"`) || !strings.Contains(buf.String(), `"environment":"linux-go"`)) {
			t.Fatalf("command log = %s", buf.String())
		}
		if !s.IsCommand() && !strings.Contains(buf.String(), `"kind":"agent"`) {
			t.Fatalf("agent log = %s", buf.String())
		}
	}
}

func TestJobSpecKindJSON(t *testing.T) {
	b, err := json.Marshal(agentSpec())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"kind"`) || strings.Contains(string(b), `"command"`) {
		t.Fatalf("agent spec json = %s", b)
	}
	var back JobSpec
	if err := json.Unmarshal(b, &back); err != nil || back.Kind.OrDefault() != JobKindAgent || back.IsCommand() {
		t.Fatalf("decoded kind = %q, %v", back.Kind, err)
	}
	cs := commandSpec()
	b, err = json.Marshal(cs)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &back); err != nil || !back.IsCommand() || back.Environment.Name != "linux-go" || back.Overlay.Ref != cs.Overlay.Ref {
		t.Fatalf("command round trip = %+v, %v", back, err)
	}
}

func TestCommandJobRequiredLabels(t *testing.T) {
	j := Job{Kind: JobKindCommand, Needs: []string{"docker", "go"}}
	if got := j.RequiredLabels(); !slices.Equal(got, []string{"docker", "go"}) {
		t.Fatalf("RequiredLabels = %v", got)
	}
	if !slices.Equal(JobKinds(), []JobKind{JobKindAgent, JobKindCommand, JobKindModelCheck}) || JobKindCommand.OrDefault() != JobKindCommand {
		t.Fatalf("JobKinds = %v", JobKinds())
	}
}

func TestPools(t *testing.T) {
	if OrDefaultPool("") != PoolBuild || OrDefaultPool(PoolControl) != PoolControl {
		t.Fatal("OrDefaultPool")
	}
	tests := []struct {
		env  *Environment
		want string
	}{
		{nil, PoolBuild},
		{&Environment{Provider: ProviderDocker}, PoolBuild},
		{&Environment{Provider: ProviderMachine}, PoolBuild},
		{&Environment{Provider: ProviderActions}, PoolActions},
	}
	for _, tt := range tests {
		if got := CommandPool(tt.env); got != tt.want {
			t.Errorf("CommandPool(%v) = %s, want %s", tt.env, got, tt.want)
		}
	}
}

func TestServiceVar(t *testing.T) {
	for in, want := range map[string]string{"db": "DB", "main-db": "MAIN_DB", "pg.1_x": "PG_1_X"} {
		if got := ServiceVar(in); got != want {
			t.Errorf("ServiceVar(%q) = %q, want %q", in, got, want)
		}
	}
}
