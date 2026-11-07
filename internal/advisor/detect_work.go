package advisor

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/testreport"
	"github.com/forecrew/forecrew/internal/verification"
)

// Thresholds of the detectors over what happened.
const (
	workerWindow     = 14 * 24 * time.Hour
	workerWait       = time.Hour
	historyWindow    = 90 * 24 * time.Hour
	toolMentions     = 2
	questionGoals    = 2
	questionMinScore = 0.5
	maxQuestions     = 200
	untestedCriteria = 2
	flakyReports     = 3
	incompleteMin    = 3
	themePRs         = 3
	costMinJobs      = 20
	costRatio        = 2.0
	detailLen        = 200
)

// detail is agent or repo text cut to a line for evidence.
func detail(s string) string { return testreport.Clip(contextpack.OneLine(s), detailLen) }

// recent reports whether t is within window of now.
func recent(t, now time.Time, window time.Duration) bool { return !t.Before(now.Add(-window)) }

// detectWorkers finds jobs that waited longer than workerWait in the last
// workerWindow for a pool or labels no online worker has now.
func detectWorkers(d Data) []Signal {
	groups := map[string]*Signal{}
	for _, j := range d.Jobs {
		if !recent(j.CreatedAt, d.Now, workerWindow) || waited(j, d.Now) <= workerWait {
			continue
		}
		labels := jobLabels(j)
		if slices.ContainsFunc(d.Workers, func(w domain.Worker) bool { return canRun(w, j.Pool, labels, d.Now) }) {
			continue
		}
		key := "workers:" + poolOf(j.Pool) + ":" + strings.Join(labels, ",")
		s := groups[key]
		if s == nil {
			s = &Signal{Key: key, Category: domain.CategoryWorkers, Subject: poolOf(j.Pool),
				Summary: fmt.Sprintf("Jobs wait for a worker in pool %s with labels [%s]; none is online.", poolOf(j.Pool), strings.Join(labels, ", "))}
			groups[key] = s
		}
		s.Weight++
		s.Evidence = bounded(append(s.Evidence, domain.ProposalEvidence{Kind: DetectorWorkers, Ref: j.Ref(), Detail: "waited " + waited(j, d.Now).Round(time.Minute).String()}))
	}
	return sorted(groups)
}

func poolOf(p string) string { return cmp.Or(p, domain.PoolBuild) }

// waited is how long j was ready and not leased.
func waited(j domain.Job, now time.Time) time.Duration {
	from := j.CreatedAt
	if j.RunAfter.After(from) {
		from = j.RunAfter
	}
	to := now
	if !j.StartedAt.IsZero() {
		to = j.StartedAt
	}
	return to.Sub(from)
}

// jobLabels are the labels a worker needs for j, sorted.
func jobLabels(j domain.Job) []string {
	labels := slices.Clone(j.Needs)
	if j.Runtime != "" {
		labels = append(labels, "runtime:"+j.Runtime)
	}
	slices.Sort(labels)
	return slices.Compact(labels)
}

// canRun reports whether w is online in pool with all labels.
func canRun(w domain.Worker, pool string, labels []string, now time.Time) bool {
	if !w.Online(now) || poolOf(w.Pool) != poolOf(pool) {
		return false
	}
	return !slices.ContainsFunc(labels, func(l string) bool { return !slices.Contains(w.Labels, l) })
}

// sorted returns groups in key order.
func sorted(groups map[string]*Signal) []Signal {
	out := make([]Signal, 0, len(groups))
	for _, k := range slices.Sorted(maps.Keys(groups)) {
		out = append(out, *groups[k])
	}
	return out
}

// toolRules recognize a missing tool, command, service or environment in the
// text of a gate; the first group is its name. docs/playbooks.md lists them.
var toolRules = []*regexp.Regexp{
	regexp.MustCompile(`(?i)([\w.+-]+): command not found`),
	regexp.MustCompile(`(?i)command not found: ([\w.+-]+)`),
	regexp.MustCompile(`(?i)exec: "([\w.+-]+)": executable file not found`),
	regexp.MustCompile("(?i)`?\\b([\\w.+-]+)`? is not installed"),
	regexp.MustCompile(`(?i)missing (?:tool|command|binary|service|environment)[: ]+` + "`?" + `([\w.+-]+)`),
	regexp.MustCompile(`(?i)(?:no|without) access to ` + "`?" + `([\w.+-]+)`),
	regexp.MustCompile(`(?i)(?:cannot|could not|can't) (?:connect|reach) (?:to )?` + "`?" + `([\w.+-]+)`),
	regexp.MustCompile(`(?i)(?:needs?|requires?) (?:an? |the )?` + "`?" + `([\w.+-]+)` + "`?" + ` (?:tool|cli|service|server|environment)\b`),
}

// notTools are words the rules catch that name nothing.
var notTools = []string{"a", "an", "the", "this", "that", "it", "which", "any", "some", "my", "our", "your", "to", "sh", "bash", "zsh", "dash", "fish", "make"}

// missingTools returns the tools text says are missing, lowercase.
func missingTools(text string) []string {
	var out []string
	for _, rule := range toolRules {
		for _, m := range rule.FindAllStringSubmatch(text, -1) {
			if name := strings.ToLower(strings.Trim(m[1], ".-")); name != "" && len(name) <= 40 && !slices.Contains(notTools, name) {
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// detectTooling clusters by tool the escalations and ask_human questions
// that name something missing; a tool named toolMentions times is a signal.
func detectTooling(d Data) []Signal {
	groups := map[string]*Signal{}
	for _, g := range d.Gates {
		if g.Kind != domain.GateNeedsYou || !recent(g.CreatedAt, d.Now, historyWindow) {
			continue
		}
		for _, tool := range missingTools(g.Title + "\n" + g.Summary) {
			s := groups[tool]
			if s == nil {
				s = &Signal{Key: "tooling:" + tool, Category: domain.CategoryTooling, Subject: tool, Summary: "Agents keep lacking " + tool + "."}
				groups[tool] = s
			}
			s.Weight++
			s.Evidence = bounded(append(s.Evidence, domain.ProposalEvidence{Kind: string(g.Origin.OrDefault()), Ref: g.Ref(), Detail: detail(g.Summary)}))
		}
	}
	maps.DeleteFunc(groups, func(_ string, s *Signal) bool { return s.Weight < toolMentions })
	return sorted(groups)
}

var (
	wordRE = regexp.MustCompile(`[\p{L}\p{N}_-]{3,}`)
	// stopWords carry no topic.
	stopWords = strings.Fields(`the and for are was were has have had not but with this that these those what which who whom when where why how
		can could should would will shall may might must does did doing our your you they them their its from into about there here than then
		use using used need needs want wants please any some all one two also just like get got`)
)

// terms returns the set of words of a question that carry its topic.
func terms(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range wordRE.FindAllString(strings.ToLower(text), -1) {
		if !slices.Contains(stopWords, w) {
			out[w] = true
		}
	}
	return out
}

// similarity is the share of terms two questions have in common (Jaccard).
func similarity(a, b map[string]bool) float64 {
	shared := 0
	for w := range a {
		if b[w] {
			shared++
		}
	}
	if total := len(a) + len(b) - shared; total > 0 {
		return float64(shared) / float64(total)
	}
	return 0
}

// question is an ask_human question with the goal it was asked in.
type question struct {
	gate  domain.Gate
	goal  uuid.UUID
	terms map[string]bool
}

// questions returns the latest ask_human questions asked in a goal.
func questions(d Data) []question {
	goalOf := make(map[uuid.UUID]uuid.UUID, len(d.Jobs))
	for _, j := range d.Jobs {
		goalOf[j.ID] = j.GoalID
	}
	var out []question
	for _, g := range d.Gates {
		if g.Origin != domain.GateFromAskHuman || !recent(g.CreatedAt, d.Now, historyWindow) {
			continue
		}
		goal := g.GoalID
		if goal == uuid.Nil() {
			goal = goalOf[g.JobID]
		}
		if goal != uuid.Nil() {
			out = append(out, question{gate: g, goal: goal, terms: terms(g.Summary)})
		}
	}
	if len(out) > maxQuestions {
		out = out[len(out)-maxQuestions:]
	}
	return out
}

// detectKnowledge finds a topic asked about in questionGoals goals or more:
// the answer belongs in the knowledge base. Questions join the first cluster
// with a question similar enough.
func detectKnowledge(d Data) []Signal {
	var clusters [][]question
next:
	for _, q := range questions(d) {
		for i, c := range clusters {
			if slices.ContainsFunc(c, func(o question) bool { return similarity(o.terms, q.terms) >= questionMinScore }) {
				clusters[i] = append(c, q)
				continue next
			}
		}
		clusters = append(clusters, []question{q})
	}
	var out []Signal
	for _, c := range clusters {
		goals := map[uuid.UUID]bool{}
		for _, q := range c {
			goals[q.goal] = true
		}
		if len(goals) < questionGoals {
			continue
		}
		topic := topicOf(c)
		s := Signal{Key: "knowledge:" + topic, Category: domain.CategoryKnowledge, Subject: topic, Weight: float64(len(goals)),
			Summary: fmt.Sprintf("The same question came up in %d goals: the answer is missing from the knowledge base.", len(goals))}
		for _, q := range c {
			s.Evidence = bounded(append(s.Evidence, domain.ProposalEvidence{Kind: string(domain.GateFromAskHuman), Ref: q.gate.Ref(), Detail: detail(q.gate.Summary)}))
		}
		out = append(out, s)
	}
	return out
}

// topicOf names a cluster by the terms its first two questions share, so the
// name stays the same as the cluster grows.
func topicOf(c []question) string {
	var shared []string
	for w := range c[0].terms {
		if c[1%len(c)].terms[w] {
			shared = append(shared, w)
		}
	}
	slices.Sort(shared)
	return strings.Join(shared[:min(len(shared), 3)], "-")
}

// detectTesting reads the verification reports: criteria nothing tests,
// tests flaky in flakyReports reports or more, and required cells that are
// incomplete in most reports.
func detectTesting(d Data) []Signal {
	var out []Signal
	if s := untestedCriteriaSignal(d.Reports); s.Weight >= untestedCriteria {
		out = append(out, s)
	}
	return append(append(out, flakySignals(d.Reports)...), incompleteSignals(d.Reports)...)
}

// untestedCriteriaSignal counts the criteria left untested or untestable.
func untestedCriteriaSignal(reports []Report) Signal {
	s := Signal{Key: "testing:untested", Category: domain.CategoryTesting, Summary: "Acceptance criteria end up untested or untestable."}
	for _, r := range reports {
		for _, id := range slices.Sorted(maps.Keys(r.Report.Criteria)) {
			c := r.Report.Criteria[id]
			if c.Status == verification.CriterionUntested || c.Untestable != "" {
				s.Weight++
				s.Evidence = bounded(append(s.Evidence, domain.ProposalEvidence{Kind: DetectorTesting, Ref: r.Ref, Detail: id + ": " + detail(cmp.Or(c.Untestable, c.Status))}))
			}
		}
	}
	return s
}

// flakySignals are the tests flaky in flakyReports reports or more.
func flakySignals(reports []Report) []Signal {
	flaky := map[string]*Signal{}
	for _, r := range reports {
		for _, f := range r.Report.Flaky {
			s := flaky[f.Test]
			if s == nil {
				s = &Signal{Key: "testing:flaky:" + f.Test, Category: domain.CategoryTesting, Subject: f.Test, Summary: "A test keeps being flaky."}
				flaky[f.Test] = s
			}
			s.Weight++
			s.Evidence = bounded(append(s.Evidence, domain.ProposalEvidence{Kind: DetectorTesting, Ref: r.Ref, Detail: "flaky: " + detail(f.Test)}))
		}
	}
	maps.DeleteFunc(flaky, func(_ string, s *Signal) bool { return s.Weight < flakyReports })
	return sorted(flaky)
}

// incompleteSignals are the required suites with no result in incompleteMin
// reports or more, and in most of the reports that have them.
func incompleteSignals(reports []Report) []Signal {
	seen := map[string]int{}
	groups := map[string]*Signal{}
	for _, r := range reports {
		for _, x := range r.Report.Results {
			if !x.Required {
				continue
			}
			key := x.Environment + "/" + x.Suite
			seen[key]++
			if x.Status != verification.ResultMissing {
				continue
			}
			if groups[key] == nil {
				groups[key] = &Signal{Key: "testing:incomplete:" + key, Category: domain.CategoryTesting, Subject: key, Summary: "A required cell often has no result."}
			}
			groups[key].Weight++
			groups[key].Evidence = bounded(append(groups[key].Evidence, domain.ProposalEvidence{Kind: DetectorTesting, Ref: r.Ref, Detail: "no result for " + detail(key)}))
		}
	}
	maps.DeleteFunc(groups, func(key string, s *Signal) bool { return s.Weight < incompleteMin || int(s.Weight)*2 <= seen[key] })
	return sorted(groups)
}

// themes sort review comments by keywords; the first that matches wins. The
// reviewer's comments carry a severity and no category, so the theme comes
// from the text.
var themes = []struct {
	name string
	re   *regexp.Regexp
}{
	{"security", regexp.MustCompile(`(?i)\b(injection|xss|csrf|secret|credential|sanitiz|escap|authoriz|authenticat|vulnerab)`)},
	{"error-handling", regexp.MustCompile(`(?i)\b(error handling|unhandled|swallow|ignored? (the )?err|wrap the error|panic|nil check|null check)`)},
	{"concurrency", regexp.MustCompile(`(?i)\b(race|deadlock|mutex|concurren|goroutine leak|thread.safe)`)},
	{"tests", regexp.MustCompile(`(?i)\b(missing tests?|no tests?|add (a )?tests?|untested|test coverage|regression test)`)},
	{"naming", regexp.MustCompile(`(?i)\b(naming|rename|misleading name|unclear name)`)},
	{"duplication", regexp.MustCompile(`(?i)\b(duplicat|copy.paste|already exists|reuse)`)},
	{"docs", regexp.MustCompile(`(?i)\b(document|docstring|comment explain|readme)`)},
	{"logging", regexp.MustCompile(`(?i)\b(logging|log line|logs? (the|a) )`)},
}

// themeOf returns the theme of a comment, "" when it has none.
func themeOf(body string) string {
	for _, t := range themes {
		if t.re.MatchString(body) {
			return t.name
		}
	}
	return ""
}

// detectRework finds review comments of one theme on themePRs PRs or more:
// a convention entry or a lint rule would catch them earlier.
func detectRework(d Data) []Signal {
	prs := map[string]map[uuid.UUID]bool{}
	groups := map[string]*Signal{}
	for _, j := range d.Jobs {
		if j.Role != "reviewer" || j.TaskID == uuid.Nil() || len(j.Output) == 0 || !recent(j.CreatedAt, d.Now, historyWindow) {
			continue
		}
		var out struct {
			Comments []struct {
				Severity string `json:"severity"`
				Body     string `json:"body"`
			} `json:"comments"`
		}
		if json.Unmarshal(j.Output, &out) != nil {
			continue
		}
		for _, c := range out.Comments {
			theme := themeOf(c.Body)
			if theme == "" || c.Severity == "nit" {
				continue
			}
			if prs[theme] == nil {
				prs[theme] = map[uuid.UUID]bool{}
				groups[theme] = &Signal{Key: "rework:" + theme, Category: domain.CategoryDocs, Subject: theme, Summary: "Reviews keep asking for the same thing: " + theme + "."}
			}
			prs[theme][j.TaskID] = true
			groups[theme].Evidence = bounded(append(groups[theme].Evidence, domain.ProposalEvidence{Kind: DetectorRework, Ref: j.Ref(), Detail: detail(c.Body)}))
		}
	}
	for theme, s := range groups {
		s.Weight = float64(len(prs[theme]))
	}
	maps.DeleteFunc(groups, func(_ string, s *Signal) bool { return s.Weight < themePRs })
	return sorted(groups)
}

// detectCost finds roles whose cost per merged PR is more than costRatio
// times the project's median, over costMinJobs jobs or more. The weight is
// that ratio.
func detectCost(d Data) []Signal {
	merged := map[uuid.UUID]bool{}
	for _, c := range d.Changes {
		if c.State == domain.ChangeMerged && c.TaskID != uuid.Nil() {
			merged[c.TaskID] = true
		}
	}
	if len(merged) == 0 {
		return nil
	}
	type total struct {
		jobs int
		cost float64
	}
	roles := map[string]*total{}
	for _, j := range d.Jobs {
		if j.Role == "" || !merged[j.TaskID] {
			continue
		}
		if roles[j.Role] == nil {
			roles[j.Role] = &total{}
		}
		roles[j.Role].jobs++
		roles[j.Role].cost += j.CostUSD
	}
	perPR := make([]float64, 0, len(roles))
	for _, t := range roles {
		perPR = append(perPR, t.cost/float64(len(merged)))
	}
	mid := median(perPR)
	if mid <= 0 {
		return nil
	}
	var out []Signal
	for _, role := range slices.Sorted(maps.Keys(roles)) {
		t := roles[role]
		cost := t.cost / float64(len(merged))
		if t.jobs < costMinJobs || cost <= costRatio*mid {
			continue
		}
		out = append(out, Signal{Key: "cost:" + role, Category: domain.CategoryCost, Subject: role, Weight: cost / mid,
			Summary:  fmt.Sprintf("Role %s costs $%.2f per merged PR, the project's median is $%.2f: a cheaper tier or runtime may do.", role, cost, mid),
			Evidence: []domain.ProposalEvidence{{Kind: DetectorCost, Ref: role, Detail: fmt.Sprintf("%d jobs over %d merged PRs, $%.2f", t.jobs, len(merged), t.cost)}}})
	}
	return out
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Sorted(slices.Values(v))
	if n := len(s); n%2 == 0 {
		return (s[n/2-1] + s[n/2]) / 2
	}
	return s[len(s)/2]
}
