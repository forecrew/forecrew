package analyzer

import (
	"cmp"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Evidence kinds, as in profile.schema.json.
const (
	kindMCP      = "mcp_config"
	kindLink     = "link"
	kindAutolink = "autolink"
	kindFile     = "file"
	kindCI       = "ci_step"
	kindIssueKey = "issue_key"
)

// Strengths of a hit. A strong hit (an MCP server, an autolink) is high
// confidence alone; a normal one (a link, a file, a CI step) counts with the
// others; a guess (a self-hosted URL shape, an issue key) never rises above low.
const (
	strengthGuess = iota
	strengthNormal
	strengthStrong
)

// Knowledge and tracker tools use these uses values.
var (
	usesKnowledge     = []string{"knowledge"}
	usesTracker       = []string{"tracker"}
	usesNotifications = []string{"notifications"}
	usesMonitoring    = []string{"monitoring"}
	usesCatalog       = []string{"catalog"}
)

// hit is one sighting of a tool: a link, a file, a CI step, an MCP server or
// a GitHub signal. mergeHits adds the hits of one tool and instance together.
type hit struct {
	tool     string
	instance string
	uses     []string
	kind     string
	location string
	count    int
	strength int
	// space is the Confluence space or Notion workspace a link shows, "" if none.
	space string
}

// instancePattern is the instance shape of profile.schema.json.
var instancePattern = regexp.MustCompile(`^[A-Za-z0-9][^\s@:]*$`)

// mappingKeyPattern is the shape of a knowledge section name in a mapping.
var mappingKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// validInstance returns s when it can be an instance, "" otherwise.
func validInstance(s string) string {
	if instancePattern.MatchString(s) {
		return s
	}
	return ""
}

// toolKey identifies one Integration.
type toolKey struct{ tool, instance string }

// group collects the hits of one Integration.
type group struct {
	in       Integration
	level    int               // the highest strength of its hits
	normal   int               // occurrences of its normal hits
	evidence map[[2]string]int // by kind and location
	uses     []string
	mapping  map[string][]string
}

// mergeHits merges hits into one Integration per tool and instance, sorted by
// tool then instance, with evidence sorted by kind then location. Status is
// left empty: a profile sets it.
func mergeHits(hits []hit) []Integration {
	groups := map[toolKey]*group{}
	for _, h := range hits {
		k := toolKey{h.tool, h.instance}
		g := groups[k]
		if g == nil {
			g = &group{in: Integration{Tool: h.tool, Instance: h.instance}, evidence: map[[2]string]int{}}
			groups[k] = g
		}
		g.add(h)
	}
	keys := slices.SortedFunc(maps.Keys(groups), func(a, b toolKey) int {
		return cmp.Or(strings.Compare(a.tool, b.tool), strings.Compare(a.instance, b.instance))
	})
	out := make([]Integration, 0, len(keys))
	for _, k := range keys {
		out = append(out, groups[k].integration())
	}
	return out
}

// add counts one hit in the group.
func (g *group) add(h hit) {
	g.level = max(g.level, h.strength)
	if h.strength == strengthNormal {
		g.normal += h.count
	}
	g.evidence[[2]string{h.kind, h.location}] += h.count
	g.uses = append(g.uses, h.uses...)
	if h.space != "" {
		g.addSpace(h.space)
	}
}

// addSpace maps the space to a knowledge section named after it, when the
// name is a valid section name.
func (g *group) addSpace(space string) {
	key := strings.ToLower(space)
	if !mappingKeyPattern.MatchString(key) {
		return
	}
	if g.mapping == nil {
		g.mapping = map[string][]string{}
	}
	loc := "space:" + space
	if !slices.Contains(g.mapping[key], loc) {
		g.mapping[key] = append(g.mapping[key], loc)
	}
}

// integration builds the Integration of the group.
func (g *group) integration() Integration {
	in := g.in
	slices.Sort(g.uses)
	in.Uses = slices.Compact(g.uses)
	for k, n := range g.evidence {
		in.Evidence = append(in.Evidence, IntegrationEvidence{Kind: k[0], Location: k[1], Count: n})
	}
	slices.SortFunc(in.Evidence, func(a, b IntegrationEvidence) int {
		return cmp.Or(strings.Compare(a.Kind, b.Kind), strings.Compare(a.Location, b.Location))
	})
	in.Confidence = confidence(g.level, g.normal)
	if len(g.mapping) > 0 {
		in.Mapping = g.mapping
	}
	return in
}

// confidence is high for a strong hit, medium for normal hits seen at least
// twice, and low otherwise.
func confidence(level, normal int) string {
	switch {
	case level == strengthStrong:
		return "high"
	case normal >= 2:
		return "medium"
	}
	return "low"
}

// linkRule maps the links of one domain (and path prefix) to a tool.
type linkRule struct {
	domain string // the host or one of its parents; a trailing dot matches a prefix of the host
	path   string // path prefix, "" for any path
	tool   string
	uses   []string
	// org makes the instance the organization in the first path segment.
	org bool
}

// linkRules are checked in order; the first matching rule wins.
var linkRules = []linkRule{
	{domain: "atlassian.net", path: "/wiki", tool: "confluence", uses: usesKnowledge},
	{domain: "atlassian.net", path: "/browse", tool: "jira", uses: usesTracker},
	{domain: "notion.so", tool: "notion", uses: usesKnowledge},
	{domain: "notion.site", tool: "notion", uses: usesKnowledge},
	{domain: "docs.google.com", tool: "gdrive", uses: usesKnowledge},
	{domain: "drive.google.com", tool: "gdrive", uses: usesKnowledge},
	{domain: "sharepoint.com", tool: "sharepoint", uses: usesKnowledge},
	{domain: "linear.app", tool: "linear", uses: usesTracker, org: true},
	{domain: "sentry.io", tool: "sentry", uses: usesMonitoring},
	{domain: "app.datadoghq.", tool: "datadog", uses: usesMonitoring},
	{domain: "grafana.net", tool: "grafana", uses: usesMonitoring},
	{domain: "slack.com", tool: "slack", uses: usesNotifications},
	{domain: "pagerduty.com", tool: "pagerduty", uses: usesMonitoring},
}

// selfHosted are URL shapes of self-hosted Confluence and Jira: guesses.
var selfHosted = []struct {
	re   *regexp.Regexp
	tool string
	uses []string
}{
	{regexp.MustCompile(`^/(wiki/spaces|display)/`), "confluence", usesKnowledge},
	{regexp.MustCompile(`^/browse/[A-Z][A-Z0-9]+-\d+`), "jira", usesTracker},
}

// confluenceSpace finds the space key in a Confluence path.
var confluenceSpace = regexp.MustCompile(`/(?:wiki/spaces|display)/([A-Za-z0-9~_-]+)`)

// notionSpace is a workspace name in a Notion path: a word that is not a page
// id (32 hex digits).
var notionSpace = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{1,63}$`)
var pageID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// hostIs reports whether host is domain or a subdomain of it. A domain with a
// trailing dot matches the host by prefix (app.datadoghq. matches
// app.datadoghq.com).
func hostIs(host, domain string) bool {
	if strings.HasSuffix(domain, ".") {
		return strings.HasPrefix(host, domain)
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// classifyURL returns the tool a URL points to, without the kind, location and
// count, which the caller sets. It never contacts the host.
func classifyURL(raw string) (hit, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return hit{}, false
	}
	host := strings.TrimSuffix(strings.TrimPrefix(strings.ToLower(u.Hostname()), "www."), ".")
	if host == "" {
		return hit{}, false
	}
	for _, r := range linkRules {
		if hostIs(host, r.domain) && strings.HasPrefix(u.Path, r.path) {
			return ruleHit(r, host, u.Path), true
		}
	}
	for _, s := range selfHosted {
		if s.re.MatchString(u.Path) {
			h := hit{tool: s.tool, uses: s.uses, instance: validInstance(host), strength: strengthGuess}
			if s.tool == "confluence" {
				h.space = spaceIn(confluenceSpace, u.Path)
			}
			return h, h.instance != ""
		}
	}
	return hit{}, false
}

// ruleHit builds the hit of a link that matched rule r.
func ruleHit(r linkRule, host, path string) hit {
	h := hit{tool: r.tool, uses: r.uses, instance: validInstance(host), strength: strengthNormal}
	seg := firstSegment(path)
	switch {
	case r.org && validInstance(seg) != "":
		h.instance = seg
	case r.tool == "confluence":
		h.space = spaceIn(confluenceSpace, path)
	case r.tool == "notion" && notionSpace.MatchString(seg) && !pageID.MatchString(seg):
		h.space = seg
	}
	return h
}

// spaceIn returns the first group of re in path, or "".
func spaceIn(re *regexp.Regexp, path string) string {
	if m := re.FindStringSubmatch(path); m != nil {
		return m[1]
	}
	return ""
}

// firstSegment returns the first path segment of p.
func firstSegment(p string) string {
	seg, _, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/")
	return seg
}

// urlPattern finds URLs in text. It stops at whitespace and at the characters
// that close a Markdown or HTML link.
var urlPattern = regexp.MustCompile(`https?://[^\s<>"'\x60\])]+`)

// linkHits returns the hits of the URLs in text, located at location.
func linkHits(text []byte, location string) []hit {
	var out []hit
	for _, raw := range urlPattern.FindAll(text, -1) {
		if h, ok := classifyURL(string(raw)); ok {
			h.kind, h.location, h.count = kindLink, location, 1
			out = append(out, h)
		}
	}
	return out
}

// FromGitHub returns the integrations of GitHub metadata: the autolink URL
// templates of the repo, which hold <num> where the number goes (strong: an
// admin configured them), and the link hosts in the bodies of its recent
// issues and pull requests. Nothing is fetched.
func FromGitHub(autolinkTemplates []string, issues, pulls []string) []Integration {
	var hits []hit
	for _, tmpl := range autolinkTemplates {
		h, ok := classifyURL(strings.ReplaceAll(tmpl, "<num>", "1"))
		if !ok {
			continue
		}
		h.strength = strengthStrong
		h.kind, h.location, h.count = kindAutolink, "github:autolinks", 1
		hits = append(hits, h)
	}
	for _, body := range issues {
		hits = append(hits, linkHits([]byte(body), "github:issues")...)
	}
	for _, body := range pulls {
		hits = append(hits, linkHits([]byte(body), "github:pulls")...)
	}
	return mergeHits(hits)
}
