package analyzer

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// integrationFixtures are the repos under testdata/ with integration signals,
// one per family, one with nothing to find and one polyglot repo.
var integrationFixtures = []string{
	"integrations-mcp", "integrations-secret", "integrations-links", "integrations-files",
	"integrations-ci", "integrations-none", "integrations-polyglot",
}

// TestKnowledgeSourcesFixture: the files fixture has an ADR folder, the
// contributing guide and CODEOWNERS, which are knowledge, not integrations.
func TestKnowledgeSourcesFixture(t *testing.T) {
	facts := analyzeFixture(t, "integrations-files")
	want := []KnowledgeSource{
		{Kind: "adr", Path: "docs/adr", Section: "architecture"},
		{Kind: "contributing", Path: "CONTRIBUTING.md", Section: "conventions"},
		{Kind: "codeowners", Path: "CODEOWNERS", Section: "owners"},
	}
	if !slices.Equal(facts.Knowledge, want) {
		t.Errorf("knowledge = %+v, want %+v", facts.Knowledge, want)
	}
	if got := integrationSummary(facts.Integrations); len(got) != 2 {
		t.Errorf("integrations = %+v, want backstage and techdocs only", got)
	}
	if got := analyzeFixture(t, "integrations-none").Knowledge; got == nil || len(got) != 0 {
		t.Errorf("knowledge of a repo with none = %#v, want an empty list", got)
	}
}

func TestKnowledgeSources(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []KnowledgeSource
	}{
		{"adr-dir names a folder", map[string]string{
			".adr-dir": "decisions/records\n", "decisions/records/0001.md": "x",
		}, []KnowledgeSource{{Kind: "adr", Path: "decisions/records", Section: "architecture"}}},
		{"adr-dir outside the repo is ignored", map[string]string{
			".adr-dir": "../elsewhere\n", "docs/adr/0001.md": "x",
		}, []KnowledgeSource{{Kind: "adr", Path: "docs/adr", Section: "architecture"}}},
		{"adr-dir absolute or empty is ignored", map[string]string{
			".adr-dir": "/etc\n", "docs/decisions/0001.md": "x",
		}, []KnowledgeSource{{Kind: "adr", Path: "docs/decisions", Section: "architecture"}}},
		{"adr-dir equal to a default is listed once", map[string]string{
			".adr-dir": "docs/adr\n", "docs/adr/0001.md": "x",
		}, []KnowledgeSource{{Kind: "adr", Path: "docs/adr", Section: "architecture"}}},
		{"no adr folder has files", map[string]string{"README.md": "x"}, nil},
		{"codeowners in .github, contributing", map[string]string{
			".github/CODEOWNERS": "* @acme", "CONTRIBUTING.md": "# hi", "docs/CODEOWNERS": "*",
		}, []KnowledgeSource{
			{Kind: "contributing", Path: "CONTRIBUTING.md", Section: "conventions"},
			{Kind: "codeowners", Path: ".github/CODEOWNERS", Section: "owners"},
		}},
		{"codeowners in docs", map[string]string{"docs/CODEOWNERS": "*"},
			[]KnowledgeSource{{Kind: "codeowners", Path: "docs/CODEOWNERS", Section: "owners"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFiles(t, root, tt.files)
			facts, err := Local{}.Analyze(t.Context(), root)
			if err != nil {
				t.Fatalf("Analyze: %v", err)
			}
			if !slices.Equal(facts.Knowledge, tt.want) {
				t.Errorf("knowledge = %+v, want %+v", facts.Knowledge, tt.want)
			}
		})
	}
}

func TestIsLinkSource(t *testing.T) {
	tests := map[string]bool{
		"README.md":                        true,
		"README":                           true,
		"README.rst":                       true,
		"CONTRIBUTING.md":                  true,
		"docs/guide.mdx":                   true,
		"docs/sub/notes.txt":               true,
		"docs/logo.png":                    false,
		"docs/archive.zip":                 false,
		"docs/LICENSE":                     false,
		"docs/README":                      true,
		"docs/diagram.svg":                 false,
		".github/ISSUE_TEMPLATE/bug.yml":   true,
		".github/ISSUE_TEMPLATE/bug.png":   false,
		".github/pull_request_template.md": true,
		"guide/README.md":                  false,
		"src/main.go":                      false,
	}
	for rel, want := range tests {
		if got := isLinkSource(rel); got != want {
			t.Errorf("isLinkSource(%q) = %v, want %v", rel, got, want)
		}
	}
}

// TestLinkReadsAreTextOnlyAndCapped: a binary file under docs/ is never read,
// and a text file is read up to maxLinkBytes only.
func TestLinkReadsAreTextOnlyAndCapped(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("a", maxLinkBytes) + " https://linear.app/acme/issue/ENG-1 "
	writeFiles(t, root, map[string]string{
		"docs/logo.png": "https://notion.so/acme/Logo\n",
		"docs/guide.md": "See https://notion.so/acme/Guide\n" + long,
		"docs/late.md":  strings.Repeat("a", maxLinkBytes+10) + " https://sentry.io/x",
	})
	facts, err := Local{}.Analyze(t.Context(), root)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	got := integrationSummary(facts.Integrations)
	want := []toolConfidence{{"notion", "notion.so", "low"}}
	if !slices.Equal(got, want) {
		t.Errorf("integrations = %+v, want only the notion link before the cap", got)
	}
}

// analyzeFixture analyzes the fixture repo name under testdata.
func analyzeFixture(t *testing.T, name string) Facts {
	t.Helper()
	facts, err := Local{}.Analyze(t.Context(), filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatalf("Analyze %s: %v", name, err)
	}
	return facts
}

func TestRepoIntegrationsGolden(t *testing.T) {
	for _, name := range integrationFixtures {
		t.Run(name, func(t *testing.T) {
			got, err := json.MarshalIndent(analyzeFixture(t, name), "", "  ")
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			checkGolden(t, filepath.Join("..", "..", "testdata", "golden", name+".facts.json"), append(got, '\n'))
		})
	}
}

// toolConfidence is a tool, its instance and its confidence, for the signal tests.
type toolConfidence struct{ tool, instance, confidence string }

func integrationSummary(ins []Integration) []toolConfidence {
	var out []toolConfidence
	for _, in := range ins {
		out = append(out, toolConfidence{in.Tool, in.Instance, in.Confidence})
	}
	return out
}

func TestRepoIntegrationsSignals(t *testing.T) {
	tests := []struct {
		fixture string
		want    []toolConfidence
	}{
		{"integrations-mcp", []toolConfidence{
			{"mcp:filesystem", "npx", "high"},
			{"mcp:linear", "mcp.linear.app", "high"},
			{"mcp:sentry", "mcp.sentry.dev", "high"},
			{"mcp:team-docs", "team-docs-mcp", "high"},
		}},
		{"integrations-secret", []toolConfidence{
			{"mcp:acme-search", "mcp.acme.example", "high"},
			{"mcp:local-notes", "node", "high"},
		}},
		{"integrations-links", []toolConfidence{
			{"confluence", "acme.atlassian.net", "medium"},
			{"confluence", "wiki.selfhosted.example.com", "low"},
			{"jira", "acme.atlassian.net", "low"},
			{"jira", "tracker.selfhosted.example.com", "low"},
			{"linear", "acme", "low"},
			{"notion", "notion.so", "low"},
			{"sentry", "acme.sentry.io", "low"},
		}},
		{"integrations-files", []toolConfidence{
			{"backstage", "", "low"},
			{"techdocs", "", "low"},
		}},
		{"integrations-ci", []toolConfidence{
			{"sentry", "", "low"},
			{"slack", "", "medium"},
		}},
		{"integrations-none", nil},
		{"integrations-polyglot", []toolConfidence{
			{"backstage", "", "low"},
			{"confluence", "acme.atlassian.net", "medium"},
			{"mcp:team-docs", "team-docs-mcp", "high"},
			{"sentry", "acme.sentry.io", "low"},
			{"slack", "", "low"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			got := integrationSummary(analyzeFixture(t, tt.fixture).Integrations)
			if !slices.Equal(got, tt.want) {
				t.Errorf("integrations = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestMCPConfigOmitsSecrets: headers, env values, arguments, query strings and
// user info of an MCP config never reach the facts.
func TestMCPConfigOmitsSecrets(t *testing.T) {
	data, err := json.Marshal(analyzeFixture(t, "integrations-secret"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{"FAKE-PASSWORD", "FAKE-QUERY", "FAKE-TOKEN", "FAKE-ARG", "FAKE-ENV", "Bearer", "user:"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("facts contain %q", secret)
		}
	}
}

func TestRepoIntegrationsCICredentialsOmitted(t *testing.T) {
	data, err := json.Marshal(analyzeFixture(t, "integrations-ci"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "FAKE-") || strings.Contains(string(data), "aws") {
		t.Errorf("facts contain CI secrets or a cloud deploy provider: %s", data)
	}
}

// TestRepoIntegrationsGit: issue keys in local branches and commit subjects
// are guesses for Jira and Linear, counted per prefix; denylisted words are
// not keys. A checkout without git history yields none.
func TestRepoIntegrationsGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	writeFiles(t, root, map[string]string{"main.go": "package main\n"})
	git("init", "-q", "-b", "main")
	git("add", ".")
	for _, msg := range []string{"ENG-1 start", "ENG-2 and ENG-1 again", "HTTP-2 support, UTF-8 text"} {
		git("commit", "-q", "--allow-empty", "-m", msg)
	}
	git("branch", "feature/ENG-7-login")

	facts, err := Local{}.Analyze(t.Context(), root)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	want := []Integration{
		{Tool: "jira", Uses: []string{"tracker"}, Confidence: "low", Evidence: []IntegrationEvidence{
			{Kind: "issue_key", Location: "git:branches:ENG", Count: 1},
			{Kind: "issue_key", Location: "git:commits:ENG", Count: 3},
		}},
		{Tool: "linear", Uses: []string{"tracker"}, Confidence: "low", Evidence: []IntegrationEvidence{
			{Kind: "issue_key", Location: "git:branches:ENG", Count: 1},
			{Kind: "issue_key", Location: "git:commits:ENG", Count: 3},
		}},
	}
	if len(facts.Integrations) != len(want) {
		t.Fatalf("integrations = %+v, want %+v", facts.Integrations, want)
	}
	for i := range want {
		got, _ := json.Marshal(facts.Integrations[i])
		exp, _ := json.Marshal(want[i])
		if string(got) != string(exp) {
			t.Errorf("integration %d = %s, want %s", i, got, exp)
		}
	}
}

func TestRepoIntegrationsNoGitDirNoKeys(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"ENG-1.txt": "ENG-1 is not a commit\n"})
	facts, err := Local{}.Analyze(t.Context(), root)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(facts.Integrations) != 0 {
		t.Errorf("integrations = %+v, want none without .git", facts.Integrations)
	}
}

func TestMCPServerNames(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		".mcp.json":             `{"mcpServers": {"  my server!": {"command": "run-it"}, "!!": {"command": "x"}, "_ok.v2": {"url": "not a url %"}}}`,
		".claude/settings.json": `{"mcpServers": {"my-server": {"url": "https://mcp.example.com:8443/x"}}}`,
		".vscode/mcp.json":      "{ // comments are not JSON\n}",
	})
	facts, err := Local{}.Analyze(t.Context(), root)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	got := integrationSummary(facts.Integrations)
	want := []toolConfidence{
		{"mcp:my-server", "mcp.example.com", "high"},
		{"mcp:my-server", "run-it", "high"},
		{"mcp:ok.v2", "", "high"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("integrations = %+v, want %+v", got, want)
	}
}

func TestClassifyURL(t *testing.T) {
	tests := []struct {
		url   string
		ok    bool
		tool  string
		inst  string
		space string
		uses  []string
		str   int
	}{
		{"https://acme.atlassian.net/wiki/spaces/OPS/pages/1", true, "confluence", "acme.atlassian.net", "OPS", usesKnowledge, strengthNormal},
		{"https://acme.atlassian.net/browse/ENG-4", true, "jira", "acme.atlassian.net", "", usesTracker, strengthNormal},
		{"https://acme.atlassian.net/jira/software", false, "", "", "", nil, 0},
		{"https://www.notion.so/acme/Handbook-0123456789abcdef0123456789abcdef", true, "notion", "notion.so", "acme", usesKnowledge, strengthNormal},
		{"https://www.notion.so/0123456789abcdef0123456789abcdef", true, "notion", "notion.so", "", usesKnowledge, strengthNormal},
		{"https://notion.site/Page-1", true, "notion", "notion.site", "", usesKnowledge, strengthNormal},
		{"https://docs.google.com/document/d/1", true, "gdrive", "docs.google.com", "", usesKnowledge, strengthNormal},
		{"https://drive.google.com/drive/folders/1", true, "gdrive", "drive.google.com", "", usesKnowledge, strengthNormal},
		{"https://acme.sharepoint.com/sites/x", true, "sharepoint", "acme.sharepoint.com", "", usesKnowledge, strengthNormal},
		{"https://linear.app/acme/issue/ENG-1", true, "linear", "acme", "", usesTracker, strengthNormal},
		{"https://linear.app/", true, "linear", "linear.app", "", usesTracker, strengthNormal},
		{"https://acme.sentry.io/issues/", true, "sentry", "acme.sentry.io", "", usesMonitoring, strengthNormal},
		{"https://app.datadoghq.com/dashboard/x", true, "datadog", "app.datadoghq.com", "", usesMonitoring, strengthNormal},
		{"https://datadoghq.com/", false, "", "", "", nil, 0},
		{"https://acme.grafana.net/d/abc", true, "grafana", "acme.grafana.net", "", usesMonitoring, strengthNormal},
		{"https://acme.slack.com/archives/C1", true, "slack", "acme.slack.com", "", usesNotifications, strengthNormal},
		{"https://acme.pagerduty.com/incidents", true, "pagerduty", "acme.pagerduty.com", "", usesMonitoring, strengthNormal},
		{"https://wiki.example.com/display/ENG/Home", true, "confluence", "wiki.example.com", "ENG", usesKnowledge, strengthGuess},
		{"https://jira.example.com/browse/OPS-12", true, "jira", "jira.example.com", "", usesTracker, strengthGuess},
		{"https://jira.example.com/browse/ops-12", false, "", "", "", nil, 0},
		{"https://github.com/acme/app", false, "", "", "", nil, 0},
		{"https://[::1", false, "", "", "", nil, 0},
		{"https:///no-host", false, "", "", "", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			h, ok := classifyURL(tt.url)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if h.tool != tt.tool || h.instance != tt.inst || h.space != tt.space || !slices.Equal(h.uses, tt.uses) || h.strength != tt.str {
				t.Errorf("hit = %+v, want tool %s instance %s space %s uses %v strength %d", h, tt.tool, tt.inst, tt.space, tt.uses, tt.str)
			}
		})
	}
}

func TestFromGitHub(t *testing.T) {
	autolinks := []string{
		"https://acme.atlassian.net/browse/ENG-<num>",
		"https://tickets.example.com/PLAT-<num>",
		"https://jira.example.com/browse/OPS-<num>",
	}
	issues := []string{
		"See https://acme.sentry.io/issues/1 and [the page](https://acme.atlassian.net/wiki/spaces/OPS/pages/2).",
		"No link here, but https://github.com/acme/app/pull/3 is not a tool.",
	}
	pulls := []string{"Fixes https://acme.sentry.io/issues/2"}
	got := FromGitHub(autolinks, issues, pulls)
	want := []Integration{
		{Tool: "confluence", Instance: "acme.atlassian.net", Uses: []string{"knowledge"}, Confidence: "low",
			Evidence: []IntegrationEvidence{{Kind: "link", Location: "github:issues", Count: 1}},
			Mapping:  map[string][]string{"ops": {"space:OPS"}}},
		{Tool: "jira", Instance: "acme.atlassian.net", Uses: []string{"tracker"}, Confidence: "high",
			Evidence: []IntegrationEvidence{{Kind: "autolink", Location: "github:autolinks", Count: 1}}},
		{Tool: "jira", Instance: "jira.example.com", Uses: []string{"tracker"}, Confidence: "high",
			Evidence: []IntegrationEvidence{{Kind: "autolink", Location: "github:autolinks", Count: 1}}},
		{Tool: "sentry", Instance: "acme.sentry.io", Uses: []string{"monitoring"}, Confidence: "medium",
			Evidence: []IntegrationEvidence{
				{Kind: "link", Location: "github:issues", Count: 1},
				{Kind: "link", Location: "github:pulls", Count: 1},
			}},
	}
	if g, w := mustJSON(t, got), mustJSON(t, want); g != w {
		t.Errorf("FromGitHub =\n%s\nwant\n%s", g, w)
	}
	if got := FromGitHub(nil, nil, nil); len(got) != 0 {
		t.Errorf("no signals = %+v, want none", got)
	}
}

func TestMergeHitsConfidence(t *testing.T) {
	link := func(loc string, n int) hit {
		return hit{tool: "slack", uses: usesNotifications, kind: kindLink, location: loc, count: n, strength: strengthNormal}
	}
	tests := []struct {
		name string
		hits []hit
		want string
	}{
		{"one normal link", []hit{link("a.md", 1)}, "low"},
		{"two normal links", []hit{link("a.md", 1), link("b.md", 1)}, "medium"},
		{"one link counted twice", []hit{link("a.md", 2)}, "medium"},
		{"strong beats any count", []hit{{tool: "slack", uses: usesNotifications, kind: kindMCP, location: ".mcp.json", count: 1, strength: strengthStrong}}, "high"},
		{"guesses never rise", []hit{{tool: "slack", uses: usesNotifications, kind: kindIssueKey, count: 9, strength: strengthGuess}}, "low"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeHits(tt.hits)
			if len(got) != 1 || got[0].Confidence != tt.want {
				t.Errorf("merged = %+v, want one entry with confidence %s", got, tt.want)
			}
		})
	}
}

func TestMCPUses(t *testing.T) {
	tests := map[string]string{
		"linear mcp.linear.app": "tracker",
		"acme-slack":            "notifications",
		"sentry x":              "monitoring",
		"backstage":             "catalog",
		"team-docs":             "knowledge",
		"streams-api":           "knowledge",
		"teams":                 "notifications",
		"my_sentry-2":           "monitoring",
	}
	for text, want := range tests {
		if got := mcpUses(text); !slices.Equal(got, []string{want}) {
			t.Errorf("mcpUses(%q) = %v, want [%s]", text, got, want)
		}
	}
}

func TestActionRepo(t *testing.T) {
	tests := map[string]string{
		"slackapi/slack-github-action@v2": "slackapi/slack-github-action",
		"Getsentry/action-release/x@v3":   "getsentry/action-release",
		"./local-action":                  "",
		"docker://alpine:3":               "",
	}
	for uses, want := range tests {
		if got := actionRepo(uses); got != want {
			t.Errorf("actionRepo(%q) = %q, want %q", uses, got, want)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
