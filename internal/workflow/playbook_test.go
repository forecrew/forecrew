package workflow_test

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/workflow"
)

func TestLoadPlaybooks(t *testing.T) {
	books, err := workflow.LoadPlaybooks(fstest.MapFS{
		"pb/deliver.yaml": {Data: mustRead(t, "testdata/playbooks/deliver.yaml")},
		"pb/notes.txt":    {Data: []byte("ignored")},
	}, "pb")
	if err != nil {
		t.Fatal(err)
	}
	p := books["deliver"]
	if len(books) != 1 || p == nil || p.Name != "deliver" || len(p.Steps) != 4 {
		t.Fatalf("books = %v", books)
	}
	tasks := p.Steps[2]
	if tasks.Type != workflow.StepForeach || tasks.Steps[0].MaxIterations != 3 || tasks.Steps[0].OnFailure != workflow.Escalate {
		t.Fatalf("tasks = %+v", tasks)
	}
	if got := tasks.Steps[1].Timeout; got != domain.Duration(30*time.Minute) {
		t.Fatalf("timeout = %v", got)
	}

	_, err = workflow.LoadPlaybooks(fstest.MapFS{"pb/bad.yaml": {Data: []byte("steps: []")}}, "pb")
	if !errors.Is(err, workflow.ErrInvalidPlaybook) || !strings.Contains(err.Error(), `"bad"`) {
		t.Fatalf("err = %v", err)
	}
	if _, err := workflow.LoadPlaybooks(fstest.MapFS{}, "["); err == nil {
		t.Fatal("bad pattern loaded")
	}
	if _, err := workflow.LoadPlaybooks(fstest.MapFS{"pb/x.yaml": {Mode: 0o20000000000}}, "pb"); err == nil {
		t.Fatal("unreadable file loaded")
	}
}

func TestParsePlaybookInvalid(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string // in the error, after the step name
	}{
		{"no steps", `steps: []`, "no steps"},
		{"unknown field", "steps:\n- {id: a, type: job, bogus: 1}", "bogus"},
		{"bad id", "steps:\n- {id: A, type: job}", `step "A": id must match`},
		{"duplicate id", "steps:\n- {id: a, type: action, action: x}\n- {id: a, type: action, action: x}", `step "a": duplicate id`},
		{"nested duplicate", "steps:\n- {id: a, type: action, action: x}\n- {id: l, type: loop, max_iterations: 1, until: iteration, steps: [{id: a, type: action, action: x}]}", `step "a": duplicate id`},
		{"unknown type", "steps:\n- {id: a, type: sleep}", `step "a": invalid playbook: unknown type "sleep"`},
		{"job without role", "steps:\n- {id: a, type: job, with: {repo: x}}", `step "a": invalid playbook: with: role is required`},
		{"command job without command", "steps:\n- {id: a, type: job, with: {kind: command, suite: unit}}", `step "a": invalid playbook: with: command is required`},
		{"command job unknown key", "steps:\n- {id: a, type: job, with: {kind: command, command: {shell: x}, colour: red}}", `unknown key "colour"`},
		{"job unknown key", "steps:\n- {id: a, type: job, with: {role: dev, colour: red}}", `step "a": invalid playbook: with: unknown key "colour"`},
		{"job account", "steps:\n- {id: a, type: job, with: {role: dev, account_id: x}}", `unknown key "account_id"`},
		{"prompt reference", "inputs: [x]\nsteps:\n- {id: a, type: job, with: {role: dev, prompt: '${inputs.x}'}}", `step "a": invalid playbook: with: prompt must be literal text`},
		{"prompt interpolated", "steps:\n- {id: a, type: job, with: {role: dev, prompt: 'Fix ${steps.b.output.text}'}}", "prompt must be literal text"},
		{"prompt not text", "steps:\n- {id: a, type: job, with: {role: dev, prompt: [x]}}", "prompt must be literal text"},
		{"gate kind", "steps:\n- {id: g, type: gate, with: {kind: vibe_check, title: t, subject_repo: r}}", `step "g": invalid playbook: with: unknown gate kind`},
		{"gate title", "steps:\n- {id: g, type: gate, with: {kind: spec_approval, subject_repo: r}}", `step "g": invalid playbook: with: title is required`},
		{"foreach on_failure", "inputs: [xs]\nsteps:\n- {id: f, type: foreach, on_failure: escalate, items: '${inputs.xs}', steps: [{id: a, type: action, action: x}]}", "on_failure is not allowed on a foreach step"},
		{"wait signal", "steps:\n- {id: w, type: wait, with: {signal: pr.opened}}", `step "w": invalid playbook: with: signal "pr.opened"`},
		{"wait key field", "steps:\n- {id: w, type: wait, with: {signal: checks.completed, repo: r}}", "with: sha is required"},
		{"wait extra key", "steps:\n- {id: w, type: wait, with: {signal: job.completed, job_id: x, repo: r}}", `unknown key "repo"`},
		{"wait timeout", "steps:\n- {id: w, type: wait, timeout: -1m, with: {signal: job.completed, job_id: x}}", "timeout must not be negative"},
		{"timeout on job", "steps:\n- {id: a, type: job, timeout: 1m, with: {role: dev}}", "timeout is not allowed on a job step"},
		{"action name", "steps:\n- {id: a, type: action}", `step "a": invalid playbook: action is required`},
		{"on_failure value", "steps:\n- {id: a, type: action, action: x, on_failure: retry}", `on_failure "retry"`},
		{"foreach items", "steps:\n- {id: f, type: foreach, items: list, steps: [{id: a, type: action, action: x}]}", `step "f": invalid playbook: items "list" must be one`},
		{"parallel negative", "inputs: [xs]\nsteps:\n- {id: f, type: foreach, parallel: -1, items: '${inputs.xs}', steps: [{id: a, type: action, action: x}]}", "parallel must not be negative"},
		{"parallel type", "inputs: [xs]\nsteps:\n- {id: f, type: foreach, parallel: many, items: '${inputs.xs}', steps: [{id: a, type: action, action: x}]}", "invalid playbook"},
		{"parallel on a loop", "steps:\n- {id: l, type: loop, parallel: 2, max_iterations: 1, until: steps.a.output, steps: [{id: a, type: action, action: x}]}", "parallel is not allowed on a loop step"},
		{"foreach steps", "steps:\n- {id: f, type: foreach, items: '${inputs.x}'}", "steps are required"},
		{"foreach with", "steps:\n- {id: f, type: foreach, items: '${inputs.x}', with: {a: b}, steps: [{id: a, type: action, action: x}]}", "with is not allowed"},
		{"foreach items ref", "steps:\n- {id: f, type: foreach, items: '${inputs.nope}', steps: [{id: a, type: action, action: x}]}", "undeclared input"},
		{"loop max", "steps:\n- {id: l, type: loop, until: iteration, steps: [{id: a, type: action, action: x}]}", `step "l": invalid playbook: max_iterations must be at least 1`},
		{"loop until", "steps:\n- {id: l, type: loop, max_iterations: 2, steps: [{id: a, type: action, action: x}]}", "until is required"},
		{"loop until syntax", "steps:\n- {id: l, type: loop, max_iterations: 2, until: 'a b', steps: [{id: a, type: action, action: x}]}", "until \"a b\" must be"},
		{"loop until ref", "steps:\n- {id: l, type: loop, max_iterations: 2, until: steps.zz.output == x, steps: [{id: a, type: action, action: x}]}", "until: reference \"steps.zz.output\" names an unknown step"},
		{"nested step", "steps:\n- {id: l, type: loop, max_iterations: 2, until: iteration, steps: [{id: a, type: action}]}", `step "a": invalid playbook: action is required`},
		{"until on job", "steps:\n- {id: a, type: job, until: x, with: {role: dev}}", "until is not allowed"},
		{"unterminated ref", "steps:\n- {id: a, type: action, action: x, with: {m: '${inputs.x'}}", "unterminated"},
		{"malformed ref", "steps:\n- {id: a, type: action, action: x, with: {m: '${steps..x}'}}", "malformed"},
		{"unknown step ref", "steps:\n- {id: a, type: action, action: x, with: {m: ['${steps.b.output}']}}", `names an unknown step`},
		{"short step ref", "steps:\n- {id: a, type: action, action: x, with: {m: {n: '${steps.a}'}}}", `names an unknown step`},
		{"undeclared input", "steps:\n- {id: a, type: action, action: x, with: {m: '${inputs.repo}'}}", "undeclared input"},
		{"item outside foreach", "steps:\n- {id: a, type: action, action: x, with: {m: '${item.title}'}}", `reference "item.title" is not in scope`},
		{"finally job", "steps:\n- {id: a, type: action, action: x}\nfinally:\n- {id: b, type: job, with: {role: dev}}", `finally step "b": invalid playbook: must be an action`},
		{"finally on_failure", "steps:\n- {id: a, type: action, action: x}\nfinally:\n- {id: b, type: action, action: x, on_failure: escalate}", "must be an action without on_failure"},
		{"finally step ref", "steps:\n- {id: a, type: action, action: x}\nfinally:\n- {id: b, type: action, action: x, with: {m: '${steps.a.output}'}}", "names an unknown step"},
		{"finally duplicate id", "steps:\n- {id: a, type: action, action: x}\nfinally:\n- {id: a, type: action, action: x}", `step "a": duplicate id`},
		{"iteration in foreach", "inputs: [xs]\nsteps:\n- {id: f, type: foreach, items: '${inputs.xs}', steps: [{id: a, type: action, action: x, with: {m: '${iteration}'}}]}", "not in scope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := workflow.ParsePlaybook("p", []byte(tt.yaml))
			if !errors.Is(err, workflow.ErrInvalidPlaybook) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want ErrInvalidPlaybook containing %q", err, tt.want)
			}
		})
	}
}

func TestParsePlaybookValid(t *testing.T) {
	p, err := workflow.ParsePlaybook("x", []byte(`
inputs: [xs]
steps:
  - id: g
    type: gate
    with: {kind: '${inputs.xs}', title: t, subject_repo: r}
  - id: w
    type: wait
    with: {signal: pr.merged, repo: r, number: 3}
  - id: l
    type: loop
    max_iterations: 1
    until: '${steps.w.output.sha}'
    steps:
      - id: f
        type: foreach
        items: ${inputs.xs}
        steps:
          - {id: a, type: action, action: x, with: {m: '${item} ${index} ${iteration}'}}
`))
	if err != nil || p.Name != "x" || len(p.Steps) != 3 || len(p.Finally) != 0 {
		t.Fatalf("ParsePlaybook = %+v, %v", p, err)
	}
}

func TestParsePlaybookCommandJob(t *testing.T) {
	p, err := workflow.ParsePlaybook("x", []byte(`
inputs: [suites]
steps:
  - id: each
    type: foreach
    items: ${inputs.suites}
    steps:
      - id: run
        type: job
        with:
          kind: command
          suite: '${item}'
          command: {argv: [go, test, ./...], reports: [reports/unit.xml]}
          overlay: {ref: agent/x, paths: [a_test.go]}
`))
	if err != nil || len(p.Steps) != 1 {
		t.Fatalf("ParsePlaybook = %+v, %v", p, err)
	}
}

func TestParsePlaybookLiteralPrompt(t *testing.T) {
	p, err := workflow.ParsePlaybook("p", []byte("steps:\n- {id: a, type: job, with: {role: reviewer, prompt: 'Check the migrations.'}}"))
	if err != nil || p.Steps[0].With["prompt"] != "Check the migrations." {
		t.Fatalf("playbook = %+v, %v", p, err)
	}
}
