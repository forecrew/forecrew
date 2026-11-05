package councils

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/roles"
)

// ErrInvalidPack means a council pack, a step prompt or a step schema is
// malformed or inconsistent.
var ErrInvalidPack = errors.New("invalid council pack")

// DevilsAdvocate is the role every council seats exactly once.
const DevilsAdvocate = "devils-advocate"

// Paths under roles.PacksDir.
const (
	packsDir   = "councils"
	promptsDir = "councils/prompts"
	schemasDir = "schemas"
)

// minMembers is the smallest council: two priorities and the devil's
// advocate.
const minMembers = 3

var priorityName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// adapters are the runtimes a seat may prefer.
var adapters = []string{config.AdapterClaudeCode, config.AdapterCodex, config.AdapterOpencode}

// newSeatAdapters are the runtimes a seat made for an ad hoc member rotates
// through. opencode is left out: it is opt-in, and a seat falls back to its
// first runtime when no worker offers any.
var newSeatAdapters = adapters[:2]

// Step is one kind of council job: the member stages of
// domain.CouncilStage, plus the facilitator's framing and its checks
// whether positions still move, which are jobs of the council's run but not
// contributions.
type Step string

// Council steps, in protocol order.
const (
	StepFrame     Step = "frame"
	StepProposal       = Step(domain.StageProposal)
	StepCritique       = Step(domain.StageCritique)
	StepRevision       = Step(domain.StageRevision)
	StepCheck     Step = "check"
	StepSynthesis      = Step(domain.StageSynthesis)
)

// Steps returns every step in protocol order.
func Steps() []Step {
	return []Step{StepFrame, StepProposal, StepCritique, StepRevision, StepCheck, StepSynthesis}
}

// stepSchemas are the output schemas of the steps, under packs/schemas.
// A revision is a proposal written again, with revision set.
var stepSchemas = map[Step]string{
	StepFrame:     "council-frame.json",
	StepProposal:  "council-proposal.json",
	StepCritique:  "council-critique.json",
	StepRevision:  "council-proposal.json",
	StepCheck:     "council-check.json",
	StepSynthesis: "council-brief.json",
}

// Facilitator reports whether the facilitator runs the step; members run
// the others.
func (s Step) Facilitator() bool {
	return s == StepFrame || s == StepCheck || s == StepSynthesis
}

// Budget is a council's limit; when either runs out the facilitator
// synthesizes from what exists.
type Budget struct {
	Tokens int64         `yaml:"tokens" json:"tokens"`
	Time   time.Duration `yaml:"time" json:"time"`
}

// Seat is a member of a council: a role arguing from a priority.
type Seat struct {
	Role     string `yaml:"role" json:"role"`
	Priority string `yaml:"priority" json:"priority"`
	// Focus says what the priority means on this council; the member's
	// prompt quotes it.
	Focus string `yaml:"focus" json:"focus"`
	// Runtimes are the runtime adapters the seat prefers, in order, so
	// members of one council run on different vendors. Empty means the
	// role's runtime in forecrew.yaml.
	Runtimes []string `yaml:"runtimes" json:"runtimes"`
}

// Pack is a standard council from packs/councils/<kind>.yaml.
type Pack struct {
	Kind domain.CouncilKind `yaml:"kind" json:"kind"`
	// Question is what the council answers, in general terms; each council
	// has its own question.
	Question    string `yaml:"question" json:"question"`
	Facilitator string `yaml:"facilitator" json:"facilitator"`
	Members     []Seat `yaml:"members" json:"members"`
	// MaxRounds is the number of revision rounds the council runs on its
	// own while positions still move, at most domain.MaxCouncilRevisions.
	MaxRounds int    `yaml:"max_rounds" json:"max_rounds"`
	Budget    Budget `yaml:"budget" json:"budget"`
}

// RoleLookup returns a role definition by name, wrapping
// roles.ErrUnknownRole when there is none (roles.Catalog.Definition).
type RoleLookup func(name string) (roles.Definition, error)

// Set is the loaded council packs with the step prompts and schemas.
type Set struct {
	packs   map[domain.CouncilKind]Pack
	prompts *template.Template
	schemas map[Step]json.RawMessage
	role    RoleLookup
}

// Load reads every packs/councils/*.yaml file, the step prompts of
// packs/councils/prompts and the step schemas from fsys, and checks every
// pack against the roles role finds. It reports every problem at once,
// wrapping ErrInvalidPack.
func Load(fsys fs.FS, role RoleLookup) (*Set, error) {
	s := &Set{packs: map[domain.CouncilKind]Pack{}, schemas: map[Step]json.RawMessage{}, role: role}
	var errs []error
	for _, step := range Steps() {
		sch, err := roles.BundleSchema(fsys, path.Join(roles.PacksDir, schemasDir, stepSchemas[step]))
		if err != nil {
			errs = append(errs, fmt.Errorf("%w: step %s: %w", ErrInvalidPack, step, err))
		}
		s.schemas[step] = sch
	}
	prompts, err := template.New("").Option("missingkey=error").ParseFS(fsys, path.Join(roles.PacksDir, promptsDir, "*.md"))
	if err != nil {
		errs = append(errs, fmt.Errorf("%w: prompts: %w", ErrInvalidPack, err))
	} else {
		for _, step := range Steps() {
			if prompts.Lookup(string(step)+".md") == nil {
				errs = append(errs, fmt.Errorf("%w: no prompt %s/%s/%s.md", ErrInvalidPack, roles.PacksDir, promptsDir, step))
			}
		}
		s.prompts = prompts
	}
	names, err := fs.Glob(fsys, path.Join(roles.PacksDir, packsDir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("list council packs: %w", err)
	}
	if len(names) == 0 {
		errs = append(errs, fmt.Errorf("%w: no council packs in %s/%s", ErrInvalidPack, roles.PacksDir, packsDir))
	}
	for _, name := range names {
		p, err := s.loadPack(fsys, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.packs[p.Kind] = p
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return s, nil
}

// Kinds returns the kinds of the loaded packs, sorted.
func (s *Set) Kinds() []domain.CouncilKind {
	return slices.Sorted(maps.Keys(s.packs))
}

// Pack returns the pack of a council kind.
func (s *Set) Pack(kind domain.CouncilKind) (Pack, error) {
	p, ok := s.packs[kind]
	if !ok {
		return Pack{}, fmt.Errorf("%w: no council pack %q", ErrInvalidTurn, kind)
	}
	return p, nil
}

// Pool returns the worker pool of role's jobs, "" for an unknown role.
func (s *Set) Pool(role string) string {
	d, err := s.role(role)
	if err != nil {
		return ""
	}
	return d.Pool
}

// Schema returns the bundled output schema of a step.
func (s *Set) Schema(step Step) (json.RawMessage, error) {
	sch, ok := s.schemas[step]
	if !ok {
		return nil, fmt.Errorf("%w: unknown step %q", ErrInvalidTurn, step)
	}
	return sch, nil
}

func (s *Set) loadPack(fsys fs.FS, name string) (Pack, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return Pack{}, fmt.Errorf("%w: %w", ErrInvalidPack, err)
	}
	var p Pack
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return Pack{}, fmt.Errorf("%w: %s: %w", ErrInvalidPack, name, err)
	}
	if problems := s.check(p, strings.TrimSuffix(path.Base(name), ".yaml")); len(problems) > 0 {
		return Pack{}, fmt.Errorf("%w: %s: %s", ErrInvalidPack, name, strings.Join(problems, "; "))
	}
	return p, nil
}

// check returns the problems of p, read from the file <stem>.yaml.
func (s *Set) check(p Pack, stem string) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	if string(p.Kind) != stem {
		add("kind %q must match the file name %q", p.Kind, stem)
	}
	if !slices.Contains(domain.CouncilKinds(), p.Kind) || p.Kind == domain.CouncilAdhoc {
		add("kind %q is not a standard council kind", p.Kind)
	}
	if strings.TrimSpace(p.Question) == "" {
		add("question must be set")
	}
	if p.MaxRounds < 0 || p.MaxRounds > domain.MaxCouncilRevisions {
		add("max_rounds must be from 0 to %d, got %d", domain.MaxCouncilRevisions, p.MaxRounds)
	}
	if p.Budget.Tokens <= 0 || p.Budget.Time <= 0 {
		add("budget needs positive tokens and time, such as {tokens: 400000, time: 45m}")
	}
	switch {
	case p.Facilitator == "":
		add("facilitator must be set")
	case !s.writes(p.Facilitator, StepSynthesis):
		add("facilitator %q must be a role whose output_schema is schemas/%s", p.Facilitator, stepSchemas[StepSynthesis])
	}
	if len(p.Members) < minMembers {
		add("a council needs at least %d members, got %d", minMembers, len(p.Members))
	}
	out = append(out, s.checkMembers(p)...)
	return out
}

// checkMembers returns the problems of p's members as a whole and of each
// seat.
func (s *Set) checkMembers(p Pack) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	priorities := map[string]bool{}
	advocates := 0
	firstRuntimes := map[string]bool{}
	for i, m := range p.Members {
		for _, problem := range s.checkSeat(p, m) {
			add("members[%d] (%s): %s", i, m.Role, problem)
		}
		if priorities[m.Priority] {
			add("priority %q is held by two members; each member needs its own", m.Priority)
		}
		priorities[m.Priority] = true
		if m.Role == DevilsAdvocate {
			advocates++
		}
		if len(m.Runtimes) > 0 {
			firstRuntimes[m.Runtimes[0]] = true
		}
		for _, other := range p.Members[:i] {
			if other.Role == m.Role && !disjoint(other.Runtimes, m.Runtimes) {
				add("role %s sits twice; both seats need runtimes, and no runtime in common", m.Role)
			}
		}
	}
	if advocates != 1 {
		add("a council needs exactly one %s, got %d", DevilsAdvocate, advocates)
	}
	if len(firstRuntimes) < 2 {
		add("members' first runtimes must name at least two runtimes, for vendor diversity")
	}
	return out
}

func (s *Set) checkSeat(p Pack, m Seat) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	if _, err := s.role(m.Role); err != nil {
		add("role: %v", err)
	}
	if m.Role == p.Facilitator {
		add("the facilitator cannot be a member")
	}
	if !priorityName.MatchString(m.Priority) {
		add("priority %q must match %s", m.Priority, priorityName)
	}
	if strings.TrimSpace(m.Focus) == "" {
		add("focus must be set")
	}
	for i, r := range m.Runtimes {
		if !slices.Contains(adapters, r) {
			add("runtime %q must be one of %s", r, strings.Join(adapters, ", "))
		}
		if slices.Contains(m.Runtimes[:i], r) {
			add("runtime %q is listed twice", r)
		}
	}
	return out
}

// writes reports whether the role exists and its output schema is the
// step's: such roles are written for councils (council personas).
func (s *Set) writes(role string, step Step) bool {
	d, err := s.role(role)
	return err == nil && len(s.schemas[step]) > 0 && bytes.Equal(d.OutputSchema, s.schemas[step])
}

// disjoint reports whether a and b are both non-empty and share nothing.
func disjoint(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	return !slices.ContainsFunc(a, func(x string) bool { return slices.Contains(b, x) })
}
