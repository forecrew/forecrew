package analyzer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"
)

// mcpConfigFiles are the files that declare MCP servers.
var mcpConfigFiles = []string{".mcp.json", ".claude/settings.json", ".cursor/mcp.json", ".vscode/mcp.json"}

// mcpConfig is the part of an MCP config the analyzer reads. Only the URL and
// the command of a server are decoded: headers, env values and arguments are
// never read, so they cannot reach the facts.
type mcpConfig struct {
	MCPServers map[string]mcpServer `json:"mcpServers"`
	Servers    map[string]mcpServer `json:"servers"` // VS Code
}

type mcpServer struct {
	URL     string `json:"url"`
	Command string `json:"command"`
}

// mcpUseWords map words in an MCP server's name or instance to the use they
// suggest. A server with none of them is assumed to be a knowledge source.
var mcpUseWords = []struct {
	words []string
	uses  []string
}{
	{[]string{"jira", "linear"}, usesTracker},
	{[]string{"slack", "teams"}, usesNotifications},
	{[]string{"sentry", "datadog", "grafana", "pagerduty"}, usesMonitoring},
	{[]string{"backstage", "catalog"}, usesCatalog},
}

// mcpNameChars matches what a tool name may not contain.
var mcpNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// ciActions maps the owner/repo of a GitHub Action to the tool it notifies or
// releases to. Cloud deploy actions have no tool in the vocabulary and are
// not recorded.
var ciActions = map[string]hit{
	"slackapi/slack-github-action":      {tool: "slack", uses: usesNotifications},
	"8398a7/action-slack":               {tool: "slack", uses: usesNotifications},
	"rtcamp/action-slack-notify":        {tool: "slack", uses: usesNotifications},
	"aliencube/microsoft-teams-actions": {tool: "teams", uses: usesNotifications},
	"skitionek/notify-microsoft-teams":  {tool: "teams", uses: usesNotifications},
	"getsentry/action-release":          {tool: "sentry", uses: usesMonitoring},
}

// notKeys are uppercase words that look like issue keys but are not.
var notKeys = map[string]bool{
	"HTTP": true, "UTF": true, "ISO": true, "RFC": true, "TLS": true, "SSL": true, "SHA": true,
	"CVE": true, "TCP": true, "UDP": true, "AES": true, "IPV": true, "X86": true, "ARM": true,
	"GPT": true, "PEP": true, "GH": true, "PR": true, "MD": true, "ES": true,
}

// issueKey matches an issue key such as ENG-123 and captures its prefix.
var issueKey = regexp.MustCompile(`\b([A-Z][A-Z0-9]+)-\d+\b`)

// recentCommits is how many commit subjects the issue keys are read from.
const recentCommits = 200

// repoIntegrations returns the integrations the checkout shows: the MCP
// servers of its configs, the links in its docs and templates, the Backstage
// and TechDocs files, the notify and release steps of its workflows, and the
// issue keys of its branches and recent commits. It never contacts a host.
func repoIntegrations(ctx context.Context, root string, files []string) ([]Integration, error) {
	var hits []hit
	for _, rel := range files {
		h, err := fileIntegrations(root, rel)
		if err != nil {
			return nil, err
		}
		hits = append(hits, h...)
	}
	return mergeHits(append(hits, gitIssueKeys(ctx, root)...)), nil
}

// fileIntegrations returns the hits of one file of the checkout, or none.
func fileIntegrations(root, rel string) ([]hit, error) {
	switch {
	case slices.Contains(mcpConfigFiles, rel):
		return mcpHits(root, rel)
	case isLinkSource(rel):
		data, err := readRepoHead(root, rel, maxLinkBytes)
		if err != nil {
			return nil, err
		}
		return linkHits(data, rel), nil
	case isWorkflow(rel):
		data, err := readRepoFile(root, rel)
		if err != nil {
			return nil, err
		}
		return ciHits(data, rel), nil
	}
	return fileHits(root, rel)
}

// readRepoFile reads a file of the checkout.
func readRepoFile(root, rel string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) // #nosec G304 -- rel comes from a walk under root
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	return data, nil
}

// readRepoHead reads at most n bytes of a file of the checkout.
func readRepoHead(root, rel string, n int64) ([]byte, error) {
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel))) // #nosec G304 -- rel comes from a walk under root
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	defer func() { _ = f.Close() }() // read-only: a close error loses nothing
	data, err := io.ReadAll(io.LimitReader(f, n))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	return data, nil
}

// textExts are the extensions of the text files whose links are read. Other
// files, such as images or archives, are never read.
var textExts = map[string]bool{
	".md": true, ".mdx": true, ".rst": true, ".txt": true, ".adoc": true,
	".html": true, ".htm": true, ".yaml": true, ".yml": true,
}

// maxLinkBytes caps the bytes read from a file for its links.
const maxLinkBytes = 1 << 20

// isLinkSource reports whether rel is a text file whose links name the team's
// tools: the root README and CONTRIBUTING.md, text files under docs/, the
// issue templates and the pull request template.
func isLinkSource(rel string) bool {
	if !isTextFile(rel) {
		return false
	}
	if !strings.Contains(rel, "/") && (matchGlob("README*", rel) || rel == "CONTRIBUTING.md") {
		return true
	}
	return strings.HasPrefix(rel, "docs/") || strings.HasPrefix(rel, ".github/ISSUE_TEMPLATE/") ||
		rel == ".github/pull_request_template.md"
}

// isTextFile reports whether rel has a text extension. A README without an
// extension is text too.
func isTextFile(rel string) bool {
	ext := strings.ToLower(path.Ext(rel))
	return textExts[ext] || (ext == "" && matchGlob("README*", path.Base(rel)))
}

// mcpHits returns one hit per MCP server of the config at rel. A config that
// is not JSON (VS Code allows comments) yields none.
func mcpHits(root, rel string) ([]hit, error) {
	data, err := readRepoFile(root, rel)
	if err != nil {
		return nil, err
	}
	var cfg mcpConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, nil
	}
	var out []hit
	for _, servers := range []map[string]mcpServer{cfg.MCPServers, cfg.Servers} {
		for name, s := range servers {
			n := strings.Trim(mcpNameChars.ReplaceAllString(name, "-"), "._-")
			if n == "" {
				continue
			}
			instance := mcpInstance(s)
			out = append(out, hit{
				tool: "mcp:" + n, instance: instance, uses: mcpUses(n + " " + instance),
				kind: kindMCP, location: rel, count: 1, strength: strengthStrong,
			})
		}
	}
	return out, nil
}

// mcpInstance is the host of a server's URL, without port or user info, or
// the base name of its command.
func mcpInstance(s mcpServer) string {
	if s.URL != "" {
		u, err := url.Parse(s.URL)
		if err != nil {
			return ""
		}
		return validInstance(u.Hostname())
	}
	return validInstance(path.Base(strings.ReplaceAll(s.Command, "\\", "/")))
}

// mcpUses returns the uses an MCP server with the text in its name and
// instance suggests. Only whole words count: "streams" is not "teams".
func mcpUses(text string) []string {
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		words[w] = true
	}
	for _, entry := range mcpUseWords {
		for _, word := range entry.words {
			if words[word] {
				return entry.uses
			}
		}
	}
	return usesKnowledge
}

// fileHits returns the hits of a Backstage catalog or a TechDocs MkDocs file.
func fileHits(root, rel string) ([]hit, error) {
	switch path.Base(rel) {
	case "catalog-info.yaml", "catalog-info.yml":
		return []hit{{tool: "backstage", uses: usesCatalog, kind: kindFile, location: rel, count: 1, strength: strengthNormal}}, nil
	case "mkdocs.yml", "mkdocs.yaml":
		data, err := readRepoFile(root, rel)
		if err != nil {
			return nil, err
		}
		if strings.Contains(strings.ToLower(string(data)), "techdocs") {
			return []hit{{tool: "techdocs", uses: usesKnowledge, kind: kindFile, location: rel, count: 1, strength: strengthNormal}}, nil
		}
	}
	return nil, nil
}

// workflowSteps is the part of a workflow the analyzer reads for its actions.
type workflowSteps struct {
	Jobs map[string]struct {
		Steps []struct {
			Uses string `yaml:"uses"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// ciHits returns one hit per notify or release step of a workflow. A workflow
// that does not parse yields none.
func ciHits(data []byte, rel string) []hit {
	var wf workflowSteps
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil
	}
	var out []hit
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			if h, ok := ciActions[actionRepo(step.Uses)]; ok {
				h.kind, h.location, h.count, h.strength = kindCI, rel, 1, strengthNormal
				out = append(out, h)
			}
		}
	}
	return out
}

// actionRepo returns owner/repo of a step's uses value, lower case: the
// action of "owner/repo/sub@v1" is "owner/repo".
func actionRepo(uses string) string {
	if strings.HasPrefix(uses, "./") || strings.Contains(uses, "://") {
		return "" // a local action or a Docker image
	}
	before, _, _ := strings.Cut(uses, "@")
	parts := strings.Split(before, "/")
	if len(parts) < 2 {
		return ""
	}
	return strings.ToLower(parts[0] + "/" + parts[1])
}

// gitIssueKeys returns guesses for the issue trackers of the checkout: the
// keys in its local branch names and its recent commit subjects. Each prefix
// fits both Jira and Linear, so both get a hit. A checkout without .git, or a
// git that fails, yields none: git is optional.
func gitIssueKeys(ctx context.Context, root string) []hit {
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return nil
	}
	var out []hit
	for _, src := range []struct {
		location string
		args     []string
	}{
		{"git:branches", []string{"branch", "--format=%(refname:short)"}},
		{"git:commits", []string{"log", fmt.Sprintf("-n%d", recentCommits), "--format=%s"}},
	} {
		lines, err := gitLines(ctx, root, src.args...)
		if err != nil {
			continue
		}
		counts := map[string]int{}
		for _, line := range lines {
			for _, m := range issueKey.FindAllStringSubmatch(line, -1) {
				if !notKeys[m[1]] {
					counts[m[1]]++
				}
			}
		}
		for prefix, n := range counts {
			for _, tool := range []string{"jira", "linear"} {
				out = append(out, hit{
					tool: tool, uses: usesTracker, kind: kindIssueKey, location: src.location + ":" + prefix,
					count: n, strength: strengthGuess,
				})
			}
		}
	}
	return out
}

// gitLines runs git in root and returns its output lines.
func gitLines(ctx context.Context, root string, args ...string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...).Output() // #nosec G204 -- fixed arguments
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
}
