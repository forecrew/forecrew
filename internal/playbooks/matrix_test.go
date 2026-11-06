package playbooks_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/testsys"
	"github.com/forecrew/forecrew/internal/workflow"
)

const testMatrix = `version: 1
axes: {os: [linux, windows]}
environments:
  - {match: {os: windows}, environment: win}
  - {match: {}, environment: lin}
suites:
  - {name: unit, command: [make, test], level: unit, reports: [unit.xml]}
  - {name: smoke, command: make smoke, level: e2e, reports: [smoke.xml], required: false, axes: {os: [linux]}}
required: [{os: linux}]
`

// setMatrix commits a matrix and its environments at ref.
func (v *env) setMatrix(t *testing.T, ref string) {
	t.Helper()
	must(t, v.tracker.SetFile("acme/app", ref, testsys.MatrixFile, []byte(testMatrix)))
	for _, name := range []string{"win", "lin"} {
		env := "version: 1\nname: " + name + "\nprovider: machine\nneeds: [" + name + "]\n"
		must(t, v.tracker.SetFile("acme/app", ref, testsys.EnvironmentsDir+"/"+name+".yaml", []byte(env)))
	}
}

func TestMatrixPlan(t *testing.T) {
	tests := []struct {
		name  string
		scope map[string]any
		cells []string
		jobs  []string
	}{
		{"all", map[string]any{"scope": "all"}, []string{"os=linux", "os=windows"}, []string{"os=linux/unit", "os=linux/smoke", "os=windows/unit"}},
		{"required", map[string]any{"scope": "required"}, []string{"os=linux"}, []string{"os=linux/unit"}},
		{"required and a suite", map[string]any{"scope": "required", "suites": []any{"smoke"}}, []string{"os=linux"}, []string{"os=linux/unit", "os=linux/smoke"}},
		{"suites only", map[string]any{"suites": []any{"smoke"}}, []string{"os=linux"}, []string{"os=linux/smoke"}},
		{"nothing in scope", map[string]any{"scope": "quirks"}, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newEnv(t)
			v.setMatrix(t, "abc")
			with := map[string]any{"repo": "acme/app", "ref": "abc"}
			maps.Copy(with, tt.scope)
			out, err := v.call(t, "matrix.plan", with)
			must(t, err)
			m := toMap(t, out)
			if m["errors"] != "" {
				t.Fatalf("errors = %v", m["errors"])
			}
			if got := keys(t, m["cells"]); !slices.Equal(got, tt.cells) {
				t.Errorf("cells = %v, want %v", got, tt.cells)
			}
			if got := keys(t, m["jobs"]); !slices.Equal(got, tt.jobs) {
				t.Errorf("jobs = %v, want %v", got, tt.jobs)
			}
		})
	}
}

// keys returns the key of each element of a list; the list must not be nil, as
// a foreach needs one.
func keys(t *testing.T, list any) []string {
	t.Helper()
	items, ok := list.([]any)
	if !ok {
		t.Fatalf("not a list: %#v", list)
	}
	var out []string
	for _, e := range items {
		out = append(out, e.(map[string]any)["key"].(string))
	}
	return out
}

func TestMatrixPlanCell(t *testing.T) {
	v := newEnv(t)
	v.setMatrix(t, "abc")
	out, err := v.call(t, "matrix.plan", map[string]any{"repo": "acme/app", "ref": "abc", "scope": "all"})
	must(t, err)
	cell := toMap(t, out)["cells"].([]any)[1].(map[string]any)
	needs := cell["needs"].([]any)
	if cell["key"] != "os=windows" || cell["cell"].(map[string]any)["os"] != "windows" || cell["environment"].(map[string]any)["name"] != "win" ||
		len(needs) != 2 || needs[0] != "win" || needs[1] != "windows" || cell["suites"].([]any)[0].(map[string]any)["name"] != "unit" {
		t.Fatalf("cell = %v", cell)
	}
}

func TestMatrixPlanErrors(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, v *env)
		with  map[string]any
		want  string
	}{
		{"no matrix", func(*testing.T, *env) {}, map[string]any{"scope": "all"}, "matrix.yaml does not exist"},
		{"invalid matrix", func(t *testing.T, v *env) {
			must(t, v.tracker.SetFile("acme/app", "abc", testsys.MatrixFile, []byte("version: 2")))
		}, map[string]any{"scope": "all"}, testsys.MatrixFile},
		{"no environment", func(t *testing.T, v *env) {
			must(t, v.tracker.SetFile("acme/app", "abc", testsys.MatrixFile, []byte(testMatrix)))
		}, map[string]any{"scope": "all"}, "environments/win.yaml does not exist"},
		{"too many cells", func(t *testing.T, v *env) {
			huge := "version: 1\naxes: {a: [1,2,3,4,5,6,7,8,9,10], b: [1,2,3,4,5,6,7,8,9,10], c: [1,2,3,4,5,6,7,8,9,10]}\n" +
				"environments: [{match: {}, environment: lin}]\nsuites:\n  - {name: unit, command: [t], level: unit, reports: [r.xml]}\n"
			must(t, v.tracker.SetFile("acme/app", "abc", testsys.MatrixFile, []byte(huge)))
		}, map[string]any{"scope": "all"}, "more than 256 cells"},
		{"unknown suite", func(t *testing.T, v *env) { v.setMatrix(t, "abc") }, map[string]any{"scope": "all", "suites": []any{"nope"}}, `no suite "nope"`},
		{"unknown scope", func(t *testing.T, v *env) { v.setMatrix(t, "abc") }, map[string]any{"scope": "some"}, `unknown scope "some"`},
		{"no scope", func(t *testing.T, v *env) { v.setMatrix(t, "abc") }, map[string]any{}, "scope needs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newEnv(t)
			tt.setup(t, v)
			with := map[string]any{"repo": "acme/app", "ref": "abc"}
			maps.Copy(with, tt.with)
			out, err := v.call(t, "matrix.plan", with)
			must(t, err)
			m := toMap(t, out)
			if !strings.Contains(m["errors"].(string), tt.want) {
				t.Errorf("errors = %q, want %q", m["errors"], tt.want)
			}
			if got := keys(t, m["cells"]); len(got) != 0 {
				t.Errorf("cells = %v", got)
			}
			if got := keys(t, m["jobs"]); len(got) != 0 {
				t.Errorf("jobs = %v", got)
			}
		})
	}
}

func TestMatrixPlanFails(t *testing.T) {
	v := newEnv(t)
	_, err := v.call(t, "matrix.plan", map[string]any{"repo": "acme/app", "scope": "all"})
	wantErr(t, err, "needs repo and ref")
	_, err = v.call(t, "matrix.plan", map[string]any{"repo": "acme/app", "ref": "abc", "bad": 1})
	wantErr(t, err, "unknown field")

	v.wrapped = &failingFiles{Tracker: v.tracker, n: 1}
	v.register()
	_, err = v.call(t, "matrix.plan", map[string]any{"repo": "acme/app", "ref": "abc", "scope": "all"})
	wantErr(t, err, "connection reset")
}

// The jobs of a plan feed a job step through a parallel foreach, one command
// job each, with no further action.
func TestMatrixPlanDrivesCommandJobs(t *testing.T) {
	v := newEnv(t)
	v.setMatrix(t, "abc")
	book, err := workflow.ParsePlaybook("pb", []byte(`
steps:
  - {id: plan, type: action, action: matrix.plan, with: {repo: acme/app, ref: abc, scope: all}}
  - id: cells
    type: foreach
    parallel: 3
    items: '${steps.plan.output.jobs}'
    steps:
      - {id: run, type: job, with: {kind: command, repo: acme/app, base_ref: abc, from: '${item}'}}
`))
	must(t, err)
	jobs := &testkit.JobCreator{Store: v.st}
	e, err := workflow.New(workflow.Config{
		Store: v.st, Bus: v.bus, Jobs: jobs, Gates: v.gates, Actions: v.actions, Playbooks: map[string]*workflow.Playbook{"pb": book},
		Now: v.clock.Now, Workers: 2, Sweep: 5 * time.Millisecond,
	})
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = e.Run(ctx) })
	t.Cleanup(func() { cancel(); wg.Wait() })
	_, err = e.Start(t.Context(), "pb", workflow.StartRequest{AccountID: v.account, ProjectID: v.project.ID})
	must(t, err)
	eventually(t, "three jobs", func() bool { return len(jobs.Requests()) == 3 })
	reqs := jobs.Requests()
	var got []string
	for _, r := range reqs {
		got = append(got, r.Cell["os"]+"/"+r.Suite+"/"+r.Environment.Name)
		if r.Kind != domain.JobKindCommand || r.Command == nil || len(r.Command.Reports) != 1 || r.BaseRef != "abc" {
			t.Errorf("request = %+v", r)
		}
	}
	if want := []string{"linux/unit/lin", "linux/smoke/lin", "windows/unit/win"}; !slices.Equal(got, want) {
		t.Errorf("jobs = %v, want %v", got, want)
	}
	if !slices.Equal(reqs[2].Needs, []string{"win", "windows"}) {
		t.Errorf("needs = %v", reqs[2].Needs)
	}
}

// only_runnable keeps the jobs an online worker of the pool has the labels
// for and names the others.
func TestMatrixPlanOnlyRunnable(t *testing.T) {
	v := newEnv(t)
	v.setMatrix(t, "abc")
	with := map[string]any{"repo": "acme/app", "ref": "abc", "scope": "all", "only_runnable": true}
	plan := func() map[string]any {
		out, err := v.call(t, "matrix.plan", with)
		must(t, err)
		return toMap(t, out)
	}
	addWorker := func(name, pool string, labels ...string) uuid.UUID {
		w := domain.Worker{AccountID: v.account, Name: name, Pool: pool, TokenHash: []byte(name)}
		must(t, v.st.Workers().Create(t.Context(), &w))
		must(t, v.st.Workers().Seen(t.Context(), v.account, w.ID, labels, "1"))
		return w.ID
	}

	// Nobody is online: no job runs. Only the required one is reported: the
	// smoke suite and the windows cell are optional, so nothing waits for them.
	m := plan()
	if got := keys(t, m["jobs"]); len(got) != 0 {
		t.Fatalf("jobs = %v", got)
	}
	if got := keys(t, m["unrunnable"]); !slices.Equal(got, []string{"os=linux/unit"}) {
		t.Errorf("unrunnable = %v", got)
	}
	if got := m["missing"].(string); !strings.Contains(got, "\n- no online worker in pool build has the labels lin, linux: os=linux/unit\n") || !strings.HasPrefix(got, "<untrusted-data ") {
		t.Errorf("missing = %q", got)
	}

	// A worker of another pool, or lacking a label, does not help.
	addWorker("ctl", "control", "lin", "linux")
	addWorker("half", "", "lin")
	if got := keys(t, plan()["jobs"]); len(got) != 0 {
		t.Errorf("jobs = %v", got)
	}

	addWorker("linux-box", "", "lin", "linux", "go")
	m = plan()
	if got := keys(t, m["jobs"]); !slices.Equal(got, []string{"os=linux/unit", "os=linux/smoke"}) {
		t.Errorf("jobs = %v", got)
	}
	if got := keys(t, m["unrunnable"]); len(got) != 0 || m["missing"] != "" {
		t.Errorf("unrunnable = %v, missing %q", got, m["missing"])
	}

	// A worker that stopped polling is offline.
	v.clock.Advance(domain.OnlineWindow + time.Second)
	if got := keys(t, plan()["jobs"]); len(got) != 0 {
		t.Errorf("jobs = %v", got)
	}
	// Nothing about the optional cells reaches the text.
	if text := plan()["missing"].(string); strings.Contains(text, "smoke") || strings.Contains(text, "windows") {
		t.Errorf("missing = %q", text)
	}

	// Without the option nothing is left out.
	delete(with, "only_runnable")
	m = plan()
	if len(keys(t, m["jobs"])) != 3 || len(keys(t, m["unrunnable"])) != 0 || m["missing"] != "" {
		t.Errorf("plan = %v", m)
	}
}

// Jobs of pool actions need no worker, and a docker environment needs the
// docker label.
func TestMatrixPlanRunnableByPool(t *testing.T) {
	v := newEnv(t)
	m := `version: 1
axes: {os: [linux, mac]}
environments:
  - {match: {os: mac}, environment: gh}
  - {match: {}, environment: box}
suites:
  - {name: unit, command: [make, test], level: unit, reports: [unit.xml]}
`
	must(t, v.tracker.SetFile("acme/app", "abc", testsys.MatrixFile, []byte(m)))
	must(t, v.tracker.SetFile("acme/app", "abc", testsys.EnvironmentsDir+"/gh.yaml", []byte("version: 1\nname: gh\nprovider: actions\nruns_on: macos-latest\n")))
	must(t, v.tracker.SetFile("acme/app", "abc", testsys.EnvironmentsDir+"/box.yaml", []byte("version: 1\nname: box\nprovider: docker\nimage: golang:1.27\n")))
	with := map[string]any{"repo": "acme/app", "ref": "abc", "scope": "all", "only_runnable": true}
	out, err := v.call(t, "matrix.plan", with)
	must(t, err)
	got := toMap(t, out)
	if jobs := keys(t, got["jobs"]); !slices.Equal(jobs, []string{"os=mac/unit"}) || !strings.Contains(got["missing"].(string), "the labels docker, linux: os=linux/unit") {
		t.Fatalf("jobs = %v, missing %q", jobs, got["missing"])
	}
	w := domain.Worker{AccountID: v.account, Name: "w", TokenHash: []byte("w")}
	must(t, v.st.Workers().Create(t.Context(), &w))
	must(t, v.st.Workers().Seen(t.Context(), v.account, w.ID, []string{"docker", "linux"}, "1"))
	out, err = v.call(t, "matrix.plan", with)
	must(t, err)
	if jobs := keys(t, toMap(t, out)["jobs"]); len(jobs) != 2 {
		t.Errorf("jobs = %v", jobs)
	}
}

func TestMatrixPlanWorkersFail(t *testing.T) {
	v := newEnv(t)
	v.setMatrix(t, "abc")
	v.st = failingWorkers{Store: v.st}
	v.register()
	_, err := v.call(t, "matrix.plan", map[string]any{"repo": "acme/app", "ref": "abc", "scope": "all", "only_runnable": true})
	wantErr(t, err, "list workers")
}

type failingWorkers struct{ store.Store }

func (failingWorkers) Workers() store.WorkerRepo { return brokenWorkers{} }

type brokenWorkers struct{ store.WorkerRepo }

func (brokenWorkers) List(context.Context, uuid.UUID) ([]domain.Worker, error) {
	return nil, errors.New("db down")
}
