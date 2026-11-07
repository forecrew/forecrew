package advisor

import (
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/domain"
)

func TestInstalled(t *testing.T) {
	cases := map[string][]string{
		"Run `pip install semgrep==1.184.0` in CI.":                              {"semgrep"},
		"npm install --save-dev @playwright/mcp@0.0.84":                          {"@playwright/mcp"},
		"npx -y @upstash/context7-mcp@4.5.1":                                     {"@upstash/context7-mcp"},
		"go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.12":          {"github.com/rhysd/actionlint/cmd/actionlint"},
		"docker run --rm -v $PWD:/src ghcr.io/aquasecurity/trivy:0.76.2 fs /src": {"ghcr.io/aquasecurity/trivy"},
		"docker pull nomad:1.9":                                                  {"nomad"},
		"brew install kubescape\nthen apt-get install -y jq":                     {"jq", "kubescape"},
		"pip install -r requirements.txt":                                        nil,
		"uvx ruff check .":                                                       {"ruff"},
		"Add a test for the parser.":                                             nil,
		"npm install the dependencies":                                           nil,
	}
	for in, want := range cases {
		if got := installed(in); !slices.Equal(got, want) {
			t.Errorf("%q: %v, want %v", in, got, want)
		}
	}
}

func TestCatalogKnows(t *testing.T) {
	c, err := LoadCatalog(forecrew.Files)
	must(t, err)
	for _, name := range []string{"semgrep", "@playwright/mcp", "ghcr.io/aquasecurity/trivy", "github.com/rhysd/actionlint/cmd/actionlint", "Gitleaks"} {
		if !c.knows(name) {
			t.Errorf("%s is in the catalog", name)
		}
	}
	for _, name := range []string{"nomad", "kubescape", "@acme/evil-mcp"} {
		if c.knows(name) {
			t.Errorf("%s is not in the catalog", name)
		}
	}
}

// Any tool an action installs or runs is checked, whatever tools says and
// whatever the proposal answers.
func TestFileChecksToolsInTheActionText(t *testing.T) {
	sig := Signal{Detector: DetectorSecurity, Key: "security:dependency-scanning", Category: domain.CategorySecurity, Weight: 1, Summary: "s",
		Evidence: []domain.ProposalEvidence{{Kind: "security", Ref: "ci", Detail: "d"}}}
	draft := func(text string) Draft {
		d := setupDraft(sig.Key, "osv-scanner")
		d.Category, d.Measure = domain.CategorySecurity, ""
		d.Action = domain.ProposalAction{Goal: &domain.GoalAction{Title: "Scan dependencies", Body: text}}
		return d
	}
	f := newFixture(t)
	got := f.file(t, "k1", []Signal{sig, toolSignal("nomad", 2)}, draft("Add a CI step that runs `npx -y @acme/evil-mcp` on each push."))
	if len(got.Created) != 0 || !strings.Contains(got.Dropped[0], "@acme/evil-mcp, which is not in the catalog") {
		t.Fatalf("an unknown package: %+v", got)
	}
	// A missing tool of the brief counts though the draft does not cite it.
	got = f.file(t, "k2", []Signal{sig, toolSignal("nomad", 2)}, draft("Deploy the scanner with nomad."))
	if len(got.Created) != 0 || !strings.Contains(got.Dropped[0], "uses nomad") {
		t.Fatalf("a missing tool not cited: %+v", got)
	}
	got = f.file(t, "k3", []Signal{sig}, draft("Add a CI step: `docker run ghcr.io/google/osv-scanner:v2.7.0 -r .`"))
	if len(got.Created) != 1 {
		t.Fatalf("a catalog image: %+v", got)
	}
}

// A revision is held to the catalog like the draft it revises.
func TestRevisionKeepsToTheCatalog(t *testing.T) {
	ctx := t.Context()
	for name, c := range map[string]struct {
		change func(*Draft)
		want   string
	}{
		"no catalog tool": {func(d *Draft) { d.Tools = nil }, "no tool of the catalog"},
		"the missing one": {func(d *Draft) {
			d.Action = domain.ProposalAction{Setup: &domain.SetupAction{Instructions: "Install trivvy everywhere"}}
		}, "uses trivvy"},
		"an install": {func(d *Draft) {
			d.Action = domain.ProposalAction{Setup: &domain.SetupAction{Instructions: "brew install kubescape"}}
		}, "installs or runs kubescape"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			sig := toolSignal("trivvy", 2)
			p := f.proposal(t, f.file(t, "k1", []Signal{sig}, setupDraft(sig.Key, "trivy")).Created[0])
			_, err := f.Gates.Resolve(ctx, f.human, p.GateID, domain.GateChangesRequested, "change it")
			must(t, err)
			d := setupDraft(sig.Key, "trivy")
			d.Title = "Revised"
			c.change(&d)
			must(t, f.Reopen(ctx, "r1", p.AccountID, p.ID, &d))
			p = f.proposal(t, p.Ref())
			g, err := f.Gates.Get(ctx, p.AccountID, p.GateID)
			must(t, err)
			if p.Title == "Revised" || !strings.Contains(g.Summary, "refused") || !strings.Contains(g.Summary, c.want) {
				t.Fatalf("revised %+v, gate %s", p, g.Summary)
			}
		})
	}
}
