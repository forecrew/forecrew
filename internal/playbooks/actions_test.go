package playbooks_test

import (
	"testing"
	"testing/fstest"

	forecrew "github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/actionsprovider"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/playbooks"
	"github.com/forecrew/forecrew/internal/testkit"
)

// withActions registers the actions with a fake GitHub and the real packs.
func withActions(t *testing.T) (*env, *testkit.FakeActions) {
	t.Helper()
	v := newEnv(t)
	fake := testkit.NewFakeActions(t, v.clock.Now)
	playbooks.Register(v.actions, playbooks.Deps{
		Store: v.st, Tracker: v.tracker, Signals: v.bus, Sizing: config.DefaultSizing(), Now: v.clock.Now,
		Actions: fake.Client, Packs: forecrew.Files,
	})
	return v, fake
}

func TestInstallWorkflow(t *testing.T) {
	v, fake := withActions(t)
	with := map[string]any{"repo": "acme/app", "branch": "main"}
	out, err := v.call(t, "actions.install_workflow", with)
	must(t, err)
	if m := toMap(t, out); m["path"] != ".github/workflows/forecrew-matrix.yml" || m["changed"] != true {
		t.Errorf("output = %v", m)
	}
	want, err := actionsprovider.Template(forecrew.Files)
	must(t, err)
	if got, ok := fake.File("acme/app", actionsprovider.WorkflowPath); !ok || string(got) != string(want) {
		t.Errorf("installed workflow differs from the template (found %v)", ok)
	}
	// Again: the same file is left alone.
	out, err = v.call(t, "actions.install_workflow", with)
	must(t, err)
	if m := toMap(t, out); m["changed"] != false {
		t.Errorf("second output = %v", m)
	}
}

func TestInstallWorkflowErrors(t *testing.T) {
	v, fake := withActions(t)
	_, err := v.call(t, "actions.install_workflow", map[string]any{"repo": "acme/app"})
	wantErr(t, err, "repo and branch are required")
	_, err = v.call(t, "actions.install_workflow", map[string]any{"repo": "acme/app", "branch": "main", "extra": 1})
	wantErr(t, err, "unknown field")
	_, err = v.call(t, "actions.install_workflow", map[string]any{"repo": "bad", "branch": "main"})
	wantErr(t, err, "install forecrew-matrix.yml")

	fake.ContentsStatus = 403
	_, err = v.call(t, "actions.install_workflow", map[string]any{"repo": "acme/app", "branch": "main"})
	wantErr(t, err, "Contents and Workflows write permissions")
	fake.ContentsStatus = 0

	// A repo whose packs lack the template.
	playbooks.Register(v.actions, playbooks.Deps{Store: v.st, Tracker: v.tracker, Signals: v.bus, Actions: fake.Client, Packs: fstest.MapFS{}})
	_, err = v.call(t, "actions.install_workflow", map[string]any{"repo": "acme/app", "branch": "main"})
	wantErr(t, err, "read workflow template")
}

func TestInstallWorkflowNeedsItsDependencies(t *testing.T) {
	v := newEnv(t)
	_, err := v.call(t, "actions.install_workflow", map[string]any{"repo": "acme/app", "branch": "main"})
	wantErr(t, err, "no GitHub Actions support")
}
