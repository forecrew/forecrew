package advisor

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testsys"
	"github.com/forecrew/forecrew/internal/workflow"
)

// Bounds of what the advisor writes.
const (
	maxTitle       = 200
	maxText        = 8000
	maxConfigFiles = 10
	maxConfigFile  = 64 << 10
)

// Draft is one proposal as the advisor wrote it
// (packs/schemas/proposals.json).
type Draft struct {
	DedupeKey string                  `json:"dedupe_key"`
	Signals   []string                `json:"signals"`
	Scope     domain.ProposalScope    `json:"scope"`
	Category  domain.ProposalCategory `json:"category"`
	Title     string                  `json:"title"`
	Rationale string                  `json:"rationale"`
	Benefit   string                  `json:"benefit"`
	Effort    domain.Effort           `json:"effort"`
	Risk      domain.Level            `json:"risk"`
	Tools     []string                `json:"tools"`
	Measure   string                  `json:"measure"`
	Action    domain.ProposalAction   `json:"action"`
}

// Filed says what became of the advisor's drafts.
type Filed struct {
	Created []string `json:"created"` // refs
	Updated []string `json:"updated"` // refs of open proposals with new evidence
	Dropped []string `json:"dropped"` // why, per draft
}

// File turns the drafts of a run, best first, into proposals: a draft whose
// key has an open proposal updates its evidence, one held back by a rejected
// proposal is dropped, and so is one past the weekly cap or that breaks a
// rule of check. key is the action's, which makes repeating it harmless.
// Each new proposal gets its gate and its proposal run.
func (s *Service) File(ctx context.Context, p domain.Project, key string, signals []Signal, drafts []Draft) (Filed, error) {
	all, err := s.Store.Proposals().List(ctx, p.AccountID, store.ProposalFilter{ProjectID: p.ID})
	if err != nil {
		return Filed{}, fmt.Errorf("list proposals: %w", err)
	}
	now := s.Now()
	mine := map[uuid.UUID]bool{}
	for _, d := range drafts {
		mine[draftID(key, d)] = true
	}
	f := &filing{Service: s, project: p, key: key, signals: signals, all: all, now: now, used: createdSince(all, now.Add(-capWindow), mine),
		out: Filed{Created: []string{}, Updated: []string{}, Dropped: []string{}}}
	for _, d := range drafts {
		if err := f.draft(ctx, d); err != nil {
			return f.out, err
		}
	}
	return f.out, nil
}

// draftID is the ID of the proposal of d, the same for a repeated call.
func draftID(key string, d Draft) uuid.UUID { return deriveID("proposal", key, d.DedupeKey) }

// filing is one call of File.
type filing struct {
	*Service
	project domain.Project
	key     string
	signals []Signal
	all     []domain.Proposal
	now     time.Time
	used    int // proposals that count against the cap
	out     Filed
}

func (f *filing) drop(d Draft, why string) {
	f.out.Dropped = append(f.out.Dropped, contextpack.OneLine(d.DedupeKey)+": "+why)
}

// draft files one draft.
func (f *filing) draft(ctx context.Context, d Draft) error {
	id := draftID(f.key, d)
	if i := slices.IndexFunc(f.all, func(x domain.Proposal) bool { return x.ID == id }); i >= 0 { // an earlier call filed it
		f.used++
		f.out.Created = append(f.out.Created, f.all[i].Ref())
		return f.startRun(ctx, f.project, f.all[i])
	}
	sig, ok := find(f.signals, d.DedupeKey)
	if !ok {
		f.drop(d, "it answers no listed signal")
		return nil
	}
	open, err := f.Store.Proposals().GetOpen(ctx, f.project.AccountID, f.project.ID, d.DedupeKey)
	if err == nil {
		f.out.Updated = append(f.out.Updated, open.Ref())
		return f.tend(ctx, &open, f.signals, f.now)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("find the open proposal of %s: %w", d.DedupeKey, err)
	}
	if answered(f.all, sig, f.now) {
		f.drop(d, "a rejected or just finished proposal holds it back")
		return nil
	}
	if f.used >= f.MaxPerWeek {
		f.drop(d, "the weekly cap is reached")
		return nil
	}
	x, why := f.build(f.project, id, d, sig, f.signals)
	if why != "" {
		f.drop(d, why)
		return nil
	}
	if err := f.open(ctx, f.project, &x); err != nil {
		return err
	}
	f.used++
	f.out.Created = append(f.out.Created, x.Ref())
	return nil
}

// build makes the proposal of d, or says why it cannot be one. The evidence
// comes from the cited signals, never from the draft.
func (s *Service) build(p domain.Project, id uuid.UUID, d Draft, sig Signal, signals []Signal) (domain.Proposal, string) {
	x := domain.Proposal{ID: id, AccountID: p.AccountID, ProjectID: p.ID, Status: domain.ProposalProposed, DedupeKey: sig.Key, EvidenceWeight: sig.Weight}
	cited := slices.Clone(d.Signals)
	if !slices.Contains(cited, sig.Key) {
		cited = append(cited, sig.Key)
	}
	for _, key := range cited {
		c, ok := find(signals, key)
		if !ok {
			return x, "it cites " + contextpack.OneLine(key) + ", which is not a listed signal"
		}
		x.Evidence = bounded(append(x.Evidence, c.Evidence...))
		if why := s.missingTool(c, d); why != "" {
			return x, why
		}
	}
	if d.Measure != "" && d.Measure == sig.Detector {
		x.Measure = &domain.ProposalMeasure{Detector: sig.Detector, Value: sig.Weight}
	}
	return x, s.apply(&x, d)
}

// missingTool refuses a draft answering a tooling signal with a tool outside
// the catalog: it must name a catalog tool, and its action must not use the
// missing tool unless that is in the catalog.
func (s *Service) missingTool(c Signal, d Draft) string {
	if c.Detector != DetectorTooling {
		return ""
	}
	if len(d.Tools) == 0 {
		return "it answers a missing tool with no tool of the catalog"
	}
	if !s.Catalog.Has(c.Subject) && mentions(actionText(d.Action), c.Subject) {
		return "its action uses " + c.Subject + ", which is not in the catalog"
	}
	return ""
}

// mentions reports whether text has word as a whole word, ignoring case.
func mentions(text, word string) bool {
	re, err := regexp.Compile(`(?i)(^|[^\w.+-])` + regexp.QuoteMeta(word) + `($|[^\w+-])`)
	return err == nil && re.MatchString(text)
}

// actionText is everything an action says.
func actionText(a domain.ProposalAction) string {
	var b strings.Builder
	if a.Goal != nil {
		b.WriteString(a.Goal.Title + "\n" + a.Goal.Body + "\n")
	}
	if a.Setup != nil {
		b.WriteString(a.Setup.Instructions + "\n" + a.Setup.Issue + "\n")
	}
	if a.ConfigPR != nil {
		for _, f := range a.ConfigPR.Files {
			b.WriteString(f.Path + "\n" + f.Content + "\n")
		}
	}
	return b.String()
}

// apply writes the fields a draft decides into x and says what is wrong with
// them, "" when nothing is.
func (s *Service) apply(x *domain.Proposal, d Draft) string {
	x.Scope, x.Category, x.Title, x.Rationale, x.Benefit = d.Scope, d.Category, contextpack.OneLine(d.Title), d.Rationale, d.Benefit
	x.Effort, x.Risk, x.Action = d.Effort, d.Risk, d.Action
	for _, t := range d.Tools {
		if !s.Catalog.Has(t) {
			return contextpack.OneLine(t) + " is not in the catalog"
		}
	}
	if utf8.RuneCountInString(x.Title) > maxTitle || len(x.Rationale) > maxText || len(x.Benefit) > maxText || len(actionText(d.Action)) > maxConfigFiles*maxConfigFile+2*maxText {
		return "it is too long"
	}
	if err := x.Validate(); err != nil {
		return err.Error()
	}
	if x.Scope == domain.ScopePlatform && strings.TrimSpace(x.Action.Setup.Issue) == "" {
		return "a platform proposal needs the text of an issue"
	}
	if c := x.Action.ConfigPR; c != nil {
		if len(c.Files) > maxConfigFiles {
			return "it changes too many files"
		}
		for _, f := range c.Files {
			if err := checkConfigFile(f); err != nil {
				return err.Error()
			}
		}
	}
	return ""
}

// checkConfigFile accepts only the .forecrew/ files that have a contract,
// valid against it.
func checkConfigFile(f domain.ConfigFile) error {
	data := []byte(f.Content)
	if len(data) > maxConfigFile {
		return fmt.Errorf("%s is too large", f.Path)
	}
	dir, name, _ := cutLast(f.Path)
	switch {
	case f.Path == ".forecrew/profile.yaml":
		return analyzer.Validate(data)
	case f.Path == matrixFile:
		_, err := testsys.ParseMatrix(data, nil)
		return err
	case f.Path == knowledge.PolicyFile:
		_, err := knowledge.ParsePolicy(data)
		return err
	case dir == ".forecrew/environments" && strings.HasSuffix(name, ".yaml"):
		_, err := testsys.ParseEnvironment(f.Path, data)
		return err
	}
	return fmt.Errorf("%s has no contract", f.Path)
}

func cutLast(path string) (dir, name string, ok bool) {
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return "", path, false
	}
	return path[:i], path[i+1:], true
}

// open stores x with its improvement_proposal gate and starts its run.
func (s *Service) open(ctx context.Context, p domain.Project, x *domain.Proposal) error {
	_, err := s.Gates.Open(ctx, gates.OpenRequest{
		ID: deriveID("gate", x.ID.String()), AccountID: p.AccountID, ProjectID: p.ID, Kind: domain.GateImprovementProposal,
		Title: x.Title, Summary: gateSummary(*x, ""), SubjectRepo: p.Repo,
		Opened: func(ctx context.Context, tx store.Store, g domain.Gate) error {
			x.GateID = g.ID
			return tx.Proposals().Create(ctx, x)
		},
	})
	if err != nil {
		return fmt.Errorf("open the gate of %s: %w", x.DedupeKey, err)
	}
	return s.startRun(ctx, p, *x)
}

// startRun starts the run that follows x; its ID is the proposal's.
func (s *Service) startRun(ctx context.Context, p domain.Project, x domain.Proposal) error {
	_, err := s.Runs.Start(ctx, PlaybookProposal, workflow.StartRequest{
		ID: x.ID, AccountID: p.AccountID, ProjectID: p.ID, Inputs: map[string]any{"repo": p.Repo, "proposal_id": x.ID.String()},
	})
	if err != nil {
		return fmt.Errorf("start the run of %s: %w", x.Ref(), err)
	}
	return nil
}

// gateSummary is the text of a proposal's gate. All of it comes from the
// advisor or from what agents and the repo wrote, so it is marked as data.
func gateSummary(x domain.Proposal, note string) string {
	var b strings.Builder
	if note != "" {
		b.WriteString(note + "\n\n")
	}
	fmt.Fprintf(&b, "%s, effort %s, risk %s, %s scope.\n\n", x.Category, x.Effort, x.Risk, x.Scope)
	var t strings.Builder
	fmt.Fprintf(&t, "Why: %s\n\nBenefit: %s\n\nAction: %s\n\nEvidence:\n", x.Rationale, x.Benefit, actionSummary(x.Action))
	for _, e := range x.Evidence {
		fmt.Fprintf(&t, "- %s %s: %s\n", e.Kind, detail(e.Ref), detail(e.Detail))
	}
	b.WriteString(contextpack.WrapUntrusted("improvement proposal", t.String()))
	return b.String()
}

// actionSummary says what accepting does.
func actionSummary(a domain.ProposalAction) string {
	switch {
	case a.Goal != nil:
		return "start the goal " + detail(a.Goal.Title)
	case a.ConfigPR != nil:
		paths := make([]string, len(a.ConfigPR.Files))
		for i, f := range a.ConfigPR.Files {
			paths[i] = f.Path
		}
		return "open a pull request on " + strings.Join(paths, ", ")
	case a.Setup != nil:
		return "a step for you: " + a.Setup.Instructions
	}
	return ""
}
