package contextpack

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/store"
)

// Searcher is the knowledge search; *knowledge.Search implements it. The
// pack uses the search of the job's role, so a role never gets an entry of a
// section it may not read.
type Searcher interface {
	Run(ctx context.Context, q knowledge.Query) ([]knowledge.Result, error)
}

// TaskAssumptions lists the assumptions a task rests on, its goal's
// included; *registers.Service implements it (internal/registers imports
// this package, so it cannot be used here).
type TaskAssumptions interface {
	OfTask(ctx context.Context, accountID uuid.UUID, t domain.Task) ([]domain.Assumption, error)
}

// WithKnowledge makes the builder fill the knowledge, decisions and notes
// sections from s; without it they are empty.
func (b *StoreBuilder) WithKnowledge(s Searcher) *StoreBuilder {
	b.search = s
	return b
}

// WithAssumptions makes the builder fill the assumptions section of a task
// from the register; without it only the assumptions of the goal are listed.
func (b *StoreBuilder) WithAssumptions(a TaskAssumptions) *StoreBuilder {
	b.assumptions = a
	return b
}

// Limits of the candidates of each section; the budget cuts further.
const (
	maxKnowledge   = 6
	maxADRs        = 8
	maxDecisions   = 10
	maxAssumptions = 20
	maxSimilar     = 5
	maxNotes       = 20
	maxFiles       = 30
	maxAreas       = 20
	// scanNotes is how many of the project's notes are read to find the
	// goal's newest ones.
	scanNotes = 100
	// excerpt is how much of a task or decision text a pack shows, in runes.
	excerpt = 600
	// minSharedWords is how many words a past task shares with the current
	// one to be called similar.
	minSharedWords = 2
)

// testEngineerRole is the role pointed at the assumptions to verify by test.
const testEngineerRole = "test-engineer"

// ADRSection is the knowledge section the default mapping gives the
// project's architecture decision records.
const ADRSection = "architecture"

// packSource gathers the candidates of the sections of one job's pack.
type packSource struct {
	b       *StoreBuilder
	role    roles.Role
	project domain.Project
	task    *domain.Task
	goal    *domain.Goal
	topic   string // the words the job is about
}

// sections returns the candidates of the sections role lists, except the
// test plan.
func (b *StoreBuilder) sections(ctx context.Context, role roles.Role, project domain.Project, task *domain.Task, goal *domain.Goal) ([]Section, error) {
	s := packSource{b: b, role: role, project: project, task: task, goal: goal}
	switch {
	case task != nil:
		s.topic = task.Title + " " + task.Body
	case goal != nil:
		s.topic = goal.Title + " " + goal.Body
	}
	var out []Section
	for _, name := range role.Context {
		var sec Section
		var err error
		switch name {
		case SectionKnowledge:
			sec, err = s.knowledge(ctx)
		case SectionDecisions:
			sec, err = s.decisions(ctx)
		case SectionAssumptions:
			sec, err = s.assumptionsSection(ctx)
		case SectionSimilarTasks:
			sec, err = s.similarTasks(ctx)
		case SectionNotes:
			sec, err = s.notes(ctx)
		case SectionRelatedFiles:
			sec, err = s.relatedFiles(ctx)
		default:
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s section: %w", name, err)
		}
		sec.Name = name
		out = append(out, sec)
	}
	return out, nil
}

// unavailable is the note of a section that needs the knowledge policy when
// it cannot be read now. The job still starts: it has the kb_search tool,
// which refuses the same way, and the prompt says so.
const unavailable = "Not available now: the knowledge policy of the project cannot be read."

func (s packSource) goalID() uuid.UUID {
	if s.goal != nil {
		return s.goal.ID
	}
	return uuid.Nil()
}

// search runs a knowledge search as the job's role. unavailable is true when
// the policy cannot be read.
func (s packSource) search(ctx context.Context, q knowledge.Query) (found []knowledge.Result, unavailable bool, err error) {
	if s.b.search == nil || s.role.Name == "" { // no role would search as a human
		return nil, false, nil
	}
	q.Project, q.Role, q.GoalID = s.project, s.role.Name, s.goalID()
	found, err = s.b.search.Run(ctx, q)
	if errors.Is(err, knowledge.ErrPolicyUnknown) {
		return nil, true, nil
	}
	return found, false, err
}

// knowledge returns the entries that best match the job's text.
func (s packSource) knowledge(ctx context.Context) (Section, error) {
	query := orQuery(s.topic, 10)
	if query == "" {
		return Section{}, nil
	}
	found, down, err := s.search(ctx, knowledge.Query{
		Text: query, Kinds: []domain.KnowledgeKind{domain.KnowledgeEntryKind, domain.KnowledgeGeneratedKind}, Limit: maxKnowledge,
	})
	if err != nil || down {
		return noteIf(down), err
	}
	var items []Item
	for _, r := range found {
		items = append(items, knowledgeItem(r, true))
	}
	return Section{Items: items}, nil
}

func noteIf(down bool) Section {
	if down {
		return Section{Note: unavailable}
	}
	return Section{}
}

// knowledgeItem turns a search result into an item. Text of an untrusted
// result, the title and path included, reaches the prompt as data. snippet
// says whether to show the snippet.
func knowledgeItem(r knowledge.Result, snippet bool) Item {
	var body strings.Builder
	body.WriteString(OneLine(r.Title))
	if r.Path != "" {
		body.WriteString("\n" + OneLine(r.Path))
	}
	if snippet && r.Snippet != "" {
		body.WriteString("\n\n" + r.Snippet)
	}
	if r.NeedsReverify {
		body.WriteString("\n\nMay be out of date: " + OneLine(r.ReverifyReason))
	}
	label := string(r.Kind)
	if domain.ValidName(r.Section) {
		label += " in " + r.Section
	}
	return Item{
		ID: r.ID.String(), Path: r.Path, Label: label, Body: body.String(),
		Untrusted: r.Untrusted, Source: "knowledge " + string(r.Kind) + " " + r.Provider,
	}
}

// decisions returns the goal's decisions, then the project's ADRs by path.
func (s packSource) decisions(ctx context.Context) (Section, error) {
	var sec Section
	if s.goal != nil {
		ds, err := s.b.store.Decisions().List(ctx, s.project.AccountID, store.DecisionFilter{ProjectID: s.project.ID, GoalID: s.goal.ID, Limit: maxDecisions})
		if err != nil {
			return Section{}, fmt.Errorf("list the decisions of %s: %w", s.goal.Ref(), err)
		}
		slices.SortFunc(ds, func(a, b domain.Decision) int { return cmp.Compare(a.Number, b.Number) })
		for _, d := range ds {
			sec.Items = append(sec.Items, decisionItem(d))
		}
	}
	found, down, err := s.search(ctx, knowledge.Query{
		Sections: []string{ADRSection}, Kinds: []domain.KnowledgeKind{domain.KnowledgeEntryKind}, Limit: maxADRs,
	})
	if err != nil {
		return Section{}, err
	}
	for _, r := range found {
		sec.Items = append(sec.Items, knowledgeItem(r, false))
	}
	if down {
		sec.Note = unavailable
	}
	return sec, nil
}

func decisionItem(d domain.Decision) Item {
	var body strings.Builder
	fmt.Fprintf(&body, "Question: %s\nChoice: %s\nReasons: %s\n", clip(OneLine(d.Question)), clip(OneLine(d.Choice)), clip(OneLine(d.Reasons)))
	if d.Dissent != "" {
		fmt.Fprintf(&body, "Dissent: %s\n", clip(OneLine(d.Dissent)))
	}
	return Item{ID: d.Ref(), Label: d.Ref(), Body: body.String(), Untrusted: true, Source: "decision " + d.Ref()}
}

// assumptionsSection returns the open assumptions the task rests on, or the
// open assumptions of the goal for a job without a task; the test engineer
// gets those to verify by test.
func (s packSource) assumptionsSection(ctx context.Context) (Section, error) {
	var as []domain.Assumption
	var err error
	switch {
	case s.task != nil && s.b.assumptions != nil:
		as, err = s.b.assumptions.OfTask(ctx, s.project.AccountID, *s.task)
	case s.goal != nil:
		as, err = s.b.store.Assumptions().List(ctx, s.project.AccountID, store.AssumptionFilter{ProjectID: s.project.ID, GoalID: s.goal.ID})
	}
	if err != nil {
		return Section{}, fmt.Errorf("list assumptions: %w", err)
	}
	as = slices.DeleteFunc(as, func(a domain.Assumption) bool {
		return !a.Status.Open() || (s.role.Name == testEngineerRole && a.VerifyBy != domain.VerifyByTest)
	})
	rank := func(a domain.Assumption) int { return slices.Index(domain.Levels(), a.Impact) }
	slices.SortStableFunc(as, func(a, b domain.Assumption) int {
		return cmp.Or(cmp.Compare(rank(b), rank(a)), cmp.Compare(a.Number, b.Number))
	})
	var sec Section
	for _, a := range as[:min(len(as), maxAssumptions)] {
		sec.Items = append(sec.Items, Item{
			ID: a.Ref(), Label: fmt.Sprintf("%s (impact %s, confidence %s, verify by %s)", a.Ref(), a.Impact, a.Confidence, a.VerifyBy),
			Body: a.Statement, Untrusted: true, Source: "assumption " + a.Ref(),
		})
	}
	return sec, nil
}

// notes returns the notes agents wrote for the goal, newest first.
func (s packSource) notes(ctx context.Context) (Section, error) {
	if s.goal == nil {
		return Section{}, nil
	}
	found, down, err := s.search(ctx, knowledge.Query{
		Sections: []string{domain.SectionNotes}, Kinds: []domain.KnowledgeKind{domain.KnowledgeNoteKind}, Limit: scanNotes,
	})
	if err != nil || down {
		return noteIf(down), err
	}
	found = slices.DeleteFunc(found, func(r knowledge.Result) bool { return r.Scope != domain.NoteScopeGoal || r.GoalID != s.goal.ID })
	slices.SortStableFunc(found, func(a, b knowledge.Result) int { return b.Provenance.Created.Compare(a.Provenance.Created) })
	var sec Section
	for _, r := range found[:min(len(found), maxNotes)] {
		// The search gives a snippet; the note itself is read whole.
		e, err := s.b.store.Knowledge().Get(ctx, s.project.AccountID, r.ID)
		if err != nil {
			return Section{}, fmt.Errorf("read note %s: %w", r.ID, err)
		}
		// forecrewd writes the author role and job ref of a note itself.
		sec.Items = append(sec.Items, Item{
			ID: r.ID.String(), Path: r.Path,
			Label: fmt.Sprintf("Note by %s (%s), %s", r.Provenance.AuthorRole, r.Provenance.Source, r.Provenance.Created.UTC().Format("2006-01-02")),
			Body:  e.Title + "\n\n" + e.Body, Untrusted: true, Source: "agent note by " + r.Provenance.AuthorRole,
		})
	}
	return sec, nil
}

// similarTasks returns the merged tasks of the project closest to the job's
// text, with their PR and review outcome. The closeness is the share of
// distinctive words the two texts have in common: tasks have no search
// index of their own.
func (s packSource) similarTasks(ctx context.Context) (Section, error) {
	want := wordSet(s.topic)
	if len(want) == 0 {
		return Section{}, nil
	}
	merged, err := s.b.store.Tasks().List(ctx, s.project.AccountID, store.TaskFilter{ProjectID: s.project.ID, Status: domain.TaskMerged})
	if err != nil {
		return Section{}, fmt.Errorf("list merged tasks: %w", err)
	}
	type scored struct {
		task  domain.Task
		score float64
	}
	var best []scored
	for _, t := range merged {
		if s.task != nil && t.ID == s.task.ID {
			continue
		}
		have := wordSet(t.Title + " " + t.Body)
		shared := 0
		for w := range want {
			if _, ok := have[w]; ok {
				shared++
			}
		}
		if shared >= minSharedWords {
			best = append(best, scored{t, float64(shared) / math.Sqrt(float64(len(want)*len(have)))})
		}
	}
	slices.SortFunc(best, func(a, b scored) int {
		return cmp.Or(cmp.Compare(b.score, a.score), cmp.Compare(b.task.Number, a.task.Number))
	})
	var sec Section
	for _, c := range best[:min(len(best), maxSimilar)] {
		it, err := s.similarItem(ctx, c.task)
		if err != nil {
			return Section{}, err
		}
		sec.Items = append(sec.Items, it)
	}
	return sec, nil
}

func (s packSource) similarItem(ctx context.Context, t domain.Task) (Item, error) {
	label := t.Ref() + ", merged"
	changes, err := s.b.store.Changes().List(ctx, s.project.AccountID, store.ChangeFilter{ProjectID: s.project.ID, TaskID: t.ID, State: domain.ChangeMerged, Limit: 1})
	if err != nil {
		return Item{}, fmt.Errorf("list the changes of %s: %w", t.Ref(), err)
	}
	if len(changes) > 0 && safeURL(changes[0].URL) {
		label += ", PR " + changes[0].URL
	}
	jobs, err := s.b.store.Jobs().List(ctx, s.project.AccountID, store.JobFilter{ProjectID: s.project.ID, TaskID: t.ID, Status: domain.JobSucceeded, Limit: 50})
	if err != nil {
		return Item{}, fmt.Errorf("list the jobs of %s: %w", t.Ref(), err)
	}
	if reviews, last := reviewOutcome(jobs); reviews > 0 {
		label += fmt.Sprintf(", reviewed %d time(s), last verdict %s", reviews, last)
	}
	return Item{
		ID: t.Ref(), Label: label, Body: clip(OneLine(t.Title)) + "\n\n" + clip(t.Body),
		Untrusted: true, Source: "task " + t.Ref(),
	}, nil
}

// reviewOutcome counts the reviewer's finished jobs among jobs (newest first)
// and returns the verdict of the latest one with a valid verdict.
func reviewOutcome(jobs []domain.Job) (count int, last string) {
	for _, j := range jobs {
		if j.Role != "reviewer" {
			continue
		}
		var out struct {
			Verdict string `json:"verdict"`
		}
		if json.Unmarshal(j.Output, &out) != nil || (out.Verdict != "approve" && out.Verdict != "request_changes") {
			continue
		}
		if count == 0 {
			last = out.Verdict
		}
		count++
	}
	return count, last
}

// safeURL reports whether u can sit in a heading: an https URL without
// white space, control characters or backticks.
func safeURL(u string) bool {
	return strings.HasPrefix(u, "https://") && !strings.ContainsFunc(u, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r == '`'
	})
}

// relatedFiles returns the paths the task names, then the areas triage found
// for the goal.
func (s packSource) relatedFiles(ctx context.Context) (Section, error) {
	var sec Section
	if s.task != nil {
		for _, p := range pathsIn(s.task.Title+"\n"+s.task.Body, maxFiles) {
			sec.Items = append(sec.Items, Item{Path: p, Label: "`" + p + "`"})
		}
	}
	if s.goal == nil {
		return sec, nil
	}
	areas, err := s.areas(ctx)
	if err != nil {
		return Section{}, err
	}
	if len(areas) > 0 {
		var body strings.Builder
		for _, a := range areas {
			body.WriteString("- " + a + "\n")
		}
		sec.Items = append(sec.Items, Item{
			ID: s.goal.Ref(), Label: "Areas the goal touches, from triage", Body: body.String(),
			Untrusted: true, Source: "triage of " + s.goal.Ref(),
		})
	}
	return sec, nil
}

// areas returns the areas of the goal's latest finished triage.
func (s packSource) areas(ctx context.Context) ([]string, error) {
	jobs, err := s.b.store.Jobs().List(ctx, s.project.AccountID, store.JobFilter{ProjectID: s.project.ID, GoalID: s.goal.ID, Status: domain.JobSucceeded, Limit: 100})
	if err != nil {
		return nil, fmt.Errorf("list the jobs of %s: %w", s.goal.Ref(), err)
	}
	for _, j := range jobs {
		var out struct {
			Areas []string `json:"areas"`
		}
		if j.Role != "triage" || json.Unmarshal(j.Output, &out) != nil {
			continue
		}
		var areas []string
		for _, a := range out.Areas {
			if a = clip(OneLine(a)); a != "" && len(areas) < maxAreas {
				areas = append(areas, a)
			}
		}
		return areas, nil
	}
	return nil, nil
}

// clip cuts s to excerpt runes.
func clip(s string) string {
	if utf8.RuneCountInString(s) <= excerpt {
		return s
	}
	return string([]rune(s)[:excerpt-1]) + "…"
}

// stopWords are the words too common to say what a text is about.
var stopWords = map[string]bool{}

func init() {
	for w := range strings.FieldsSeq("this that with from have will should when then than into each them they their there which what where " +
		"these those your must been also only some such able make need needs using used uses task tasks goal spec criteria criterion " +
		"acceptance add adds added about after before because being between both does done more most other over same very while would") {
		stopWords[w] = true
	}
}

// wordSet returns the distinctive words of text: lower case letters and
// digits, at least four long, not common ones.
func wordSet(text string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, w := range words(text) {
		set[w] = struct{}{}
	}
	return set
}

func words(text string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if utf8.RuneCountInString(w) >= 4 && !stopWords[w] {
			out = append(out, w)
		}
	}
	return out
}

// orQuery returns a knowledge search that matches any of the first n
// distinctive words of text, in the order they appear: the search needs every
// word of a plain query.
func orQuery(text string, n int) string {
	var out []string
	for _, w := range words(text) {
		if !slices.Contains(out, w) && len(out) < n {
			out = append(out, w)
		}
	}
	return strings.Join(out, " or ")
}

var (
	// pathLike is a word that may be a repo path: letters, digits and
	// . _ - /
	pathLike = regexp.MustCompile(`[A-Za-z0-9_.][A-Za-z0-9_./-]*`)
	// fileExt ends a file name.
	fileExt = regexp.MustCompile(`\.[A-Za-z][A-Za-z0-9]{1,4}$`)
	// notPaths are pairs of words, not paths.
	notPaths = []string{"and/or", "w/o", "n/a", "i/o", "ci/cd", "tcp/ip", "client/server", "read/write", "true/false", "yes/no", "input/output"}
)

// pathsIn returns the repo paths text names, in order and without repeats, at
// most n: words with a slash or a file extension. Only the characters of a
// plain path pass, so a path cannot carry markup into the prompt.
func pathsIn(text string, n int) []string {
	var out []string
	for _, loc := range pathLike.FindAllStringIndex(text, -1) {
		if loc[0] > 0 && strings.ContainsRune("/:\\", rune(text[loc[0]-1])) {
			continue // part of a URL, an absolute path or a Windows path
		}
		w := strings.TrimPrefix(strings.TrimRight(text[loc[0]:loc[1]], ".,-"), "./")
		dir := strings.HasSuffix(w, "/")
		w = strings.TrimSuffix(w, "/")
		switch {
		case w == "", len(w) > 200, strings.Contains(w, "//"), !domain.RelativePath(w), slices.Contains(out, w), slices.Contains(notPaths, strings.ToLower(w)):
		case dir || strings.Contains(w, "/") || fileExt.MatchString(w):
			if len(out) < n {
				out = append(out, w)
			}
		}
	}
	return out
}
