package councils

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/roles"
	"github.com/forecrew/forecrew/internal/schema"
)

// ErrInvalidTurn means a council job was asked for a kind, step, round or
// seat its pack does not have.
var ErrInvalidTurn = errors.New("invalid council turn")

// memberName is the format of a member reference in council outputs and
// in council.read: role@runtime, the role and the runtime adapter its
// jobs run on (common.json#/$defs/member).
var memberName = regexp.MustCompile(`^[a-z][a-z0-9-]*@[a-z0-9][a-z0-9-]*$`)

// MemberName returns how council outputs and council.read name the member
// of role on runtime (an adapter): role@runtime, e.g. architect@claude-code.
// One role may sit on several runtimes, so the role alone does not name a
// member.
func MemberName(role, runtime string) string { return role + "@" + runtime }

// DevilsAdvocateDissent starts the brief's dissent of the devil's
// advocate, so a reader can tell the dissent its seat requires from a
// real split in the council.
const DevilsAdvocateDissent = "Devil's advocate (mandated dissent): "

// Turn is one council job: a step of a council, with the seat that runs
// it.
type Turn struct {
	Kind  domain.CouncilKind
	Step  Step
	Round int // from 1; see MaxRound
	// Seat is one of the pack's members for a member step, and the
	// facilitator (Seat{Role: pack.Facilitator}) for a facilitator step.
	Seat Seat
	// BudgetExhausted tells the synthesis that the budget ran out.
	BudgetExhausted bool
}

// MaxRound returns the highest round of step in a council of pack: one
// framing, proposal and critique; a check before each revision round the
// council runs on its own; revision and synthesis rounds as
// domain.CouncilStage.MaxRound, with the human's round.
func (p Pack) MaxRound(step Step) int {
	switch step {
	case StepFrame:
		return 1
	case StepCheck:
		return p.MaxRounds
	}
	return domain.CouncilStage(step).MaxRound()
}

// promptData is what the step prompts render.
type promptData struct {
	Kind        domain.CouncilKind
	About       string // the pack's question
	Step        Step
	Round       int
	Numbered    bool // the round is worth naming
	MaxRounds   int
	Role        string
	Member      string // role@runtime; empty for the facilitator
	Priority    string // empty for the facilitator
	Focus       string
	Others      []Seat // the members, without the seat itself
	Persona     string // the role's own prompt, for roles written for councils
	Assumptions bool   // the step's output lists assumptions
	// BudgetExhausted is set for a synthesis written early.
	BudgetExhausted bool
}

// Job returns r, the resolved role of the turn's seat, made into the
// council job of the turn: the step prompt as instructions, the step's
// output schema, and no write access. For a member step, r.Runtime is the
// runtime the member runs on and must be one of the seat's runtimes when
// it lists any. A role written for councils (its output_schema is the
// proposal or brief schema) keeps its own prompt, placed first, and its
// escalation conditions, and its definition of done in the steps it was
// written for (the facilitator's framing and checks drop it, since it is
// about the synthesis); any other role sits on the council through its
// seat's priority and focus only, so its usual job's checklists are
// dropped.
func (s *Set) Job(t Turn, r roles.Role) (roles.Role, error) {
	p, err := s.Pack(t.Kind)
	if err != nil {
		return roles.Role{}, err
	}
	if !slices.Contains(Steps(), t.Step) {
		return roles.Role{}, fmt.Errorf("%w: unknown step %q", ErrInvalidTurn, t.Step)
	}
	if t.Round < 1 || t.Round > p.MaxRound(t.Step) {
		return roles.Role{}, fmt.Errorf("%w: round of a %s in a %s council must be from 1 to %d, got %d",
			ErrInvalidTurn, t.Step, t.Kind, p.MaxRound(t.Step), t.Round)
	}
	if r.Name != t.Seat.Role {
		return roles.Role{}, fmt.Errorf("%w: role %q does not hold the seat of %q", ErrInvalidTurn, r.Name, t.Seat.Role)
	}
	data := promptData{
		Kind: p.Kind, About: p.Question, Step: t.Step, Round: t.Round, MaxRounds: p.MaxRounds,
		Numbered:    t.Step == StepRevision || t.Step == StepCheck || t.Round > 1,
		Role:        t.Seat.Role,
		Assumptions: t.Step != StepCheck, BudgetExhausted: t.BudgetExhausted,
	}
	if err := p.seat(t, r.Runtime, &data); err != nil {
		return roles.Role{}, err
	}
	persona := s.writes(r.Name, StepProposal) || s.writes(r.Name, StepSynthesis)
	if persona {
		data.Persona = strings.TrimSpace(r.Instructions)
	}
	var b strings.Builder
	if err := s.prompts.ExecuteTemplate(&b, string(t.Step)+".md", data); err != nil {
		return roles.Role{}, fmt.Errorf("render %s prompt: %w", t.Step, err)
	}
	out := r.WithoutWrites()
	out.Instructions = strings.TrimSpace(b.String())
	out.OutputSchema = s.schemas[t.Step]
	if !persona {
		out.DefinitionOfDone, out.EscalateWhen = nil, nil
	}
	if t.Step == StepFrame || t.Step == StepCheck {
		out.DefinitionOfDone = nil
	}
	return out, nil
}

// seat fills data with the turn's seat, which runs on runtime, and the
// other seats of p.
func (p Pack) seat(t Turn, runtime string, data *promptData) error {
	if t.Step.Facilitator() {
		if t.Seat.Role != p.Facilitator {
			return fmt.Errorf("%w: %s is run by the facilitator %q, not %q", ErrInvalidTurn, t.Step, p.Facilitator, t.Seat.Role)
		}
		data.Others = p.Members
		return nil
	}
	i := slices.IndexFunc(p.Members, func(m Seat) bool { return m.Role == t.Seat.Role && m.Priority == t.Seat.Priority })
	if i < 0 {
		return fmt.Errorf("%w: the %s council has no seat %s with priority %q", ErrInvalidTurn, t.Kind, t.Seat.Role, t.Seat.Priority)
	}
	seat := p.Members[i]
	if !slices.Contains(adapters, runtime) || (len(seat.Runtimes) > 0 && !slices.Contains(seat.Runtimes, runtime)) {
		return fmt.Errorf("%w: the %s seat %s (%s) cannot run on runtime %q", ErrInvalidTurn, t.Kind, seat.Role, seat.Priority, runtime)
	}
	data.Member = MemberName(seat.Role, runtime)
	data.Priority, data.Focus = seat.Priority, seat.Focus
	data.Others = slices.Delete(slices.Clone(p.Members), i, i+1)
	return nil
}

// CheckOutput checks the output of a council job of step against the
// step's schema and the rules it cannot express: option names are
// unique and name the recommendation, revision is null exactly at the
// proposal stage, prompted_by is empty when the position did not change,
// every evidence source is one Forecrew can check, and every
// member is named as role@runtime (MemberName). Briefs go through
// ParseBrief, and the devil's advocate's dissent, and only it, starts
// with DevilsAdvocateDissent. It wraps ErrInvalid.
func (s *Set) CheckOutput(step Step, output []byte) error {
	sch, err := s.Schema(step)
	if err != nil {
		return err
	}
	if step == StepSynthesis {
		b, err := ParseBrief(output)
		if err != nil {
			return fmt.Errorf("council %s: %w", step, err)
		}
		if p := briefProblems(b); len(p) > 0 {
			return fmt.Errorf("council %s: %w: %s", step, ErrInvalid, strings.Join(p, "; "))
		}
		return nil
	}
	inst, err := schema.DecodeJSON(output)
	if err != nil {
		return fmt.Errorf("council %s: %w", step, err)
	}
	if err := schema.Check(stepSchemas[step], string(sch), inst); err != nil {
		return fmt.Errorf("council %s: %w", step, err)
	}
	var out stepOutput
	if err := json.Unmarshal(output, &out); err != nil {
		return fmt.Errorf("council %s: %w: %w", step, ErrInvalid, err)
	}
	var p []string
	if step == StepProposal || step == StepRevision {
		p = out.positionProblems(step)
	}
	for _, e := range out.sources() {
		if problem := e.check(); problem != "" {
			p = append(p, problem)
		}
	}
	if len(p) > 0 {
		return fmt.Errorf("council %s: %w: %s", step, ErrInvalid, strings.Join(p, "; "))
	}
	return nil
}

// briefProblems checks the members of a brief.
func briefProblems(b domain.CouncilBrief) []string {
	var p []string
	for _, v := range b.Votes {
		if !memberName.MatchString(v.Member) {
			p = append(p, fmt.Sprintf("vote member %q must be role@runtime, e.g. architect@claude-code", v.Member))
		}
	}
	for _, d := range b.Dissent {
		if !memberName.MatchString(d.Member) {
			p = append(p, fmt.Sprintf("dissent member %q must be role@runtime, e.g. architect@claude-code", d.Member))
			continue
		}
		role, _, _ := strings.Cut(d.Member, "@")
		marked := strings.HasPrefix(d.Position, DevilsAdvocateDissent)
		if role == DevilsAdvocate && !marked {
			p = append(p, fmt.Sprintf("the dissent of %s must start with %q", d.Member, DevilsAdvocateDissent))
		}
		if role != DevilsAdvocate && marked {
			p = append(p, fmt.Sprintf("the dissent of %s is not the devil's advocate's and must not start with %q", d.Member, DevilsAdvocateDissent))
		}
	}
	return p
}

// stepOutput holds what CheckOutput checks of the steps' outputs.
type stepOutput struct {
	Recommendation string `json:"recommendation"`
	Options        []struct {
		Name string `json:"name"`
	} `json:"options"`
	Revision  json.RawMessage `json:"revision"`
	Evidence  []evidence      `json:"evidence"`
	Context   []evidence      `json:"context"`
	Critiques []struct {
		Agree      []cited `json:"agree"`
		Disagree   []cited `json:"disagree"`
		Challenged []cited `json:"challenged_assumptions"`
	} `json:"critiques"`
}

// positionProblems checks a proposal or revision.
func (o stepOutput) positionProblems(step Step) []string {
	var p, names []string
	for _, opt := range o.Options {
		if slices.Contains(names, opt.Name) {
			p = append(p, fmt.Sprintf("option %q is listed twice", opt.Name))
		}
		names = append(names, opt.Name)
	}
	if !slices.Contains(names, o.Recommendation) {
		p = append(p, fmt.Sprintf("recommendation %q is not an option", o.Recommendation))
	}
	isNull := string(o.Revision) == "null"
	if step == StepProposal && !isNull {
		p = append(p, "revision must be null in a proposal")
	}
	if step == StepRevision && isNull {
		p = append(p, "revision must say whether and why the position changed")
	}
	if step == StepRevision && !isNull {
		var r struct {
			Changed    bool     `json:"changed"`
			PromptedBy []string `json:"prompted_by"`
		}
		// A change may come from a human's feedback rather than a
		// critique, so changed alone does not require prompted_by.
		if err := json.Unmarshal(o.Revision, &r); err == nil && !r.Changed && len(r.PromptedBy) > 0 {
			p = append(p, "revision.prompted_by must be empty when the position did not change")
		}
	}
	return p
}

// sources returns every evidence item of the output.
func (o stepOutput) sources() []evidence {
	all := slices.Concat(o.Evidence, o.Context)
	for _, c := range o.Critiques {
		for _, pt := range slices.Concat(c.Agree, c.Disagree, c.Challenged) {
			all = append(all, pt.Evidence...)
		}
	}
	return all
}

// cited is anything with evidence.
type cited struct {
	Evidence []evidence `json:"evidence"`
}

// evidence is an item of common.json#/$defs/evidence.
type evidence struct {
	Kind   domain.SourceKind `json:"kind"`
	Source string            `json:"source"`
	Date   *string           `json:"date"`
}

// check returns why Forecrew could not check the source, or "".
func (e evidence) check() string {
	switch e.Kind {
	case domain.SourceURL:
		u, err := url.Parse(e.Source)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Sprintf("url source %q must be an absolute http(s) URL", e.Source)
		}
		if e.Date == nil {
			return fmt.Sprintf("url source %q needs its date", e.Source)
		}
		return ""
	case domain.SourceJob:
		if k, _, err := domain.ParseRef(e.Source); err != nil || k != domain.RefJob {
			return fmt.Sprintf("job source %q must be a job ref such as JOB-12", e.Source)
		}
	default: // path and kb
		file, _, _ := strings.Cut(e.Source, ":")
		if file == "" || strings.HasPrefix(file, "/") || strings.Contains(e.Source, "://") ||
			slices.Contains(strings.Split(file, "/"), "..") {
			return fmt.Sprintf("%s source %q must be a path relative to the repo root", e.Kind, e.Source)
		}
	}
	if e.Date != nil {
		return fmt.Sprintf("%s source %q: date is only for url sources", e.Kind, e.Source)
	}
	return ""
}
