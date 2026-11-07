package advisor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"uuid"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/verification"
	"github.com/forecrew/forecrew/internal/workflow"
)

// Playbook names.
const (
	// PlaybookAdvise takes the input repo.
	PlaybookAdvise = "advise"
	// PlaybookProposal takes the inputs repo and proposal_id.
	PlaybookProposal = "proposal"
)

// Windows of the rules on proposals.
const (
	capWindow      = 7 * 24 * time.Hour
	rejectedWindow = 90 * 24 * time.Hour
	// MeasureAfter is how long after a proposal is done its detector runs
	// again.
	MeasureAfter = 14 * 24 * time.Hour
	// retroWindow is how long a measured outcome is shown to retros.
	retroWindow = 30 * 24 * time.Hour
)

// Bounds of what Collect reads from the repo and what a brief lists.
const (
	maxCIFiles   = 30
	maxCIFile    = 256 << 10
	maxReports   = 100
	maxSignals   = 40
	maxListed    = 30
	generatedDep = "generated/dependencies"
	matrixFile   = ".forecrew/matrix.yaml"
)

// botFiles are the configs of update bots, which scan without a workflow.
var botFiles = []string{".github/dependabot.yml", ".github/dependabot.yaml", ".github/renovate.json", "renovate.json"}

// Starter starts playbook runs; *workflow.Engine implements it.
type Starter interface {
	Start(ctx context.Context, playbook string, req workflow.StartRequest) (domain.WorkflowRun, error)
}

// Config holds the dependencies of a Service.
type Config struct {
	Store   store.Store
	Gates   gates.Service
	GitHub  github.Tracker
	Runs    Starter
	Catalog Catalog
	// MaxPerWeek is advisor.max_per_week; config.DefaultMaxProposalsPerWeek
	// when zero.
	MaxPerWeek int
	// GoalPlaybook says which playbook the goals of a project run.
	GoalPlaybook func(domain.Project) string
	Now          func() time.Time // defaults to the real UTC time
	Log          *slog.Logger     // defaults to discarding
}

// Service files proposals and follows them through.
type Service struct {
	Config
}

// New returns a Service.
func New(cfg Config) *Service {
	if cfg.MaxPerWeek == 0 {
		cfg.MaxPerWeek = config.DefaultMaxProposalsPerWeek
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Service{Config: cfg}
}

// deriveID is a UUID made from parts, so that a repeated action finds what
// the first call stored.
func deriveID(parts ...string) uuid.UUID {
	sum := sha256.Sum256([]byte("forecrew-advisor\x00" + strings.Join(parts, "\x00")))
	var id uuid.UUID
	copy(id[:], sum[:16])
	id[6] = id[6]&0x0f | 0x80 // version 8: application-defined
	id[8] = id[8]&0x3f | 0x80 // variant RFC 9562
	return id
}

// Collect gathers what the detectors read for p. A repo that cannot be read
// leaves out only what comes from it.
func (s *Service) Collect(ctx context.Context, p domain.Project) (Data, error) {
	d := Data{Now: s.Now()}
	var err error
	if d.Jobs, err = s.Store.Jobs().List(ctx, p.AccountID, store.JobFilter{ProjectID: p.ID}); err != nil {
		return Data{}, fmt.Errorf("list jobs: %w", err)
	}
	if d.Workers, err = s.Store.Workers().List(ctx, p.AccountID); err != nil {
		return Data{}, fmt.Errorf("list workers: %w", err)
	}
	if d.Gates, err = s.Store.Gates().List(ctx, p.AccountID, store.GateFilter{ProjectID: p.ID}); err != nil {
		return Data{}, fmt.Errorf("list gates: %w", err)
	}
	if d.Changes, err = s.Store.Changes().List(ctx, p.AccountID, store.ChangeFilter{ProjectID: p.ID}); err != nil {
		return Data{}, fmt.Errorf("list changes: %w", err)
	}
	if d.Reports, err = s.reports(ctx, p, d.Changes); err != nil {
		return Data{}, err
	}
	if d.Dependencies, err = s.dependencies(ctx, p); err != nil {
		return Data{}, err
	}
	if len(p.Profile) > 0 {
		var prof analyzer.Profile
		if err := yaml.Unmarshal(p.Profile, &prof); err != nil {
			return Data{}, fmt.Errorf("read the profile of %s: %w", p.Name, err)
		}
		d.Profile = &prof
		d.Platforms = prof.Platforms
	}
	s.readRepo(ctx, p, &d)
	return d, nil
}

// reports returns the latest verification report of the newest changes.
func (s *Service) reports(ctx context.Context, p domain.Project, changes []domain.Change) ([]Report, error) {
	var out []Report
	for _, c := range changes[max(0, len(changes)-maxReports):] {
		r, err := s.Store.VerificationReports().Latest(ctx, p.AccountID, c.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("verification report of PR #%d: %w", c.Number, err)
		}
		rep, err := verification.Parse(r.Report)
		if err != nil {
			s.Log.WarnContext(ctx, "skip a verification report", "report_id", r.ID, "error", err)
			continue
		}
		out = append(out, Report{Ref: fmt.Sprintf("PR #%d", c.Number), Report: rep})
	}
	return out, nil
}

// dependencies reads the generated dependency entry of p, whose body has a
// heading per component and an item per dependency.
func (s *Service) dependencies(ctx context.Context, p domain.Project) (map[string][]string, error) {
	entries, err := s.Store.Knowledge().List(ctx, p.AccountID, store.KnowledgeFilter{ProjectID: p.ID, Section: domain.SectionGenerated})
	if err != nil {
		return nil, fmt.Errorf("list the generated knowledge: %w", err)
	}
	i := slices.IndexFunc(entries, func(e domain.KnowledgeEntry) bool { return e.Path == generatedDep })
	if i < 0 {
		return nil, nil
	}
	return parseDependencies(entries[i].Body), nil
}

func parseDependencies(body string) map[string][]string {
	out := map[string][]string{}
	component := ""
	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(line, "## "); ok {
			component = name
		} else if item, ok := strings.CutPrefix(line, "- "); ok && component != "" {
			if name, _, _ := strings.Cut(item, " "); name != "" {
				out[component] = append(out[component], name)
			}
		}
	}
	return out
}

// readRepo reads the CI files and the matrix at the default branch, all of
// it repo text: bounded in number and size, and only searched.
func (s *Service) readRepo(ctx context.Context, p domain.Project, d *Data) {
	if s.GitHub == nil {
		return
	}
	var paths []string
	if d.Profile != nil {
		paths = d.Profile.CI.Workflows[:min(len(d.Profile.CI.Workflows), maxCIFiles)]
	}
	files := map[string]string{}
	for _, path := range append(slices.Clone(paths), botFiles...) {
		data, err := s.GitHub.GetFile(ctx, p.Repo, p.DefaultBranch, path)
		if errors.Is(err, github.ErrNotFound) {
			continue
		}
		if err != nil {
			s.Log.WarnContext(ctx, "read a CI file", "repo", p.Repo, "path", path, "error", err)
			return
		}
		files[path] = string(data[:min(len(data), maxCIFile)])
	}
	d.CIFiles = files
	data, err := s.GitHub.GetFile(ctx, p.Repo, p.DefaultBranch, matrixFile)
	if err != nil {
		return
	}
	var m struct {
		Axes map[string][]string `yaml:"axes"`
	}
	if yaml.Unmarshal(data[:min(len(data), maxCIFile)], &m) != nil {
		return
	}
	if os := append(slices.Clone(m.Axes["os"]), m.Axes["platform"]...); len(os) > 0 {
		d.Platforms = os
	}
}

// Brief is what the advisor job is given.
type Brief struct {
	Signals []Signal // what it may answer, heaviest first
	Room    int      // how many proposals it may write
	Text    string
}

// Prepare is the upkeep of a project's proposals and the advisor's brief: it
// shows proposals whose snooze ended again, measures the ones due, gives the
// open ones their new evidence, and lists the signals nothing answers yet.
// Repeating it changes nothing further.
func (s *Service) Prepare(ctx context.Context, p domain.Project) (Brief, error) {
	data, err := s.Collect(ctx, p)
	if err != nil {
		return Brief{}, err
	}
	signals := Detect(data)
	all, err := s.Store.Proposals().List(ctx, p.AccountID, store.ProposalFilter{ProjectID: p.ID})
	if err != nil {
		return Brief{}, fmt.Errorf("list proposals: %w", err)
	}
	now := s.Now()
	for i := range all {
		if err := s.tend(ctx, &all[i], signals, now); err != nil {
			return Brief{}, err
		}
	}
	var fresh []Signal
	for _, sig := range signals {
		if !answered(all, sig, now) && len(fresh) < maxSignals {
			fresh = append(fresh, sig)
		}
	}
	room := s.MaxPerWeek - createdSince(all, now.Add(-capWindow), nil)
	return Brief{Signals: fresh, Room: max(room, 0), Text: s.briefText(all, fresh, room, now)}, nil
}

// tend does what is due on one proposal.
func (s *Service) tend(ctx context.Context, p *domain.Proposal, signals []Signal, now time.Time) error {
	from := p.Status
	switch {
	case from == domain.ProposalSnoozed && !p.SnoozedUntil.After(now):
		p.Status, p.SnoozedUntil = domain.ProposalProposed, time.Time{}
	case from == domain.ProposalDone && p.Measure != nil && p.Outcome == nil && !now.Before(p.UpdatedAt.Add(MeasureAfter)):
		after, _ := find(signals, p.DedupeKey) // a signal that is gone weighs zero
		p.Outcome = &domain.ProposalOutcome{Before: p.Measure.Value, After: after.Weight, MeasuredAt: now}
	case from.Final():
		return nil
	default:
		sig, ok := find(signals, p.DedupeKey)
		if !ok || sig.Weight == p.EvidenceWeight {
			return nil
		}
		p.Evidence, p.EvidenceWeight = sig.Evidence, sig.Weight
	}
	if err := s.Store.Proposals().Update(ctx, p, from); err != nil {
		return fmt.Errorf("update %s: %w", p.Ref(), err)
	}
	return nil
}

// answered reports whether sig needs no new proposal: one is open, or one
// that was rejected or just carried out holds it back.
func answered(all []domain.Proposal, sig Signal, now time.Time) bool {
	return slices.ContainsFunc(all, func(p domain.Proposal) bool {
		return p.DedupeKey == sig.Key && (!p.Status.Final() || heldBack(p, sig.Weight, now))
	})
}

// heldBack reports whether p, final, keeps a proposal with its key and the
// evidence weight from coming back. A rejected one does unless the evidence
// doubled or rejectedWindow passed; a done one while its change has not had
// the time to show.
func heldBack(p domain.Proposal, weight float64, now time.Time) bool {
	switch p.Status {
	case domain.ProposalRejected:
		return weight < 2*p.EvidenceWeight && now.Before(p.UpdatedAt.Add(rejectedWindow))
	case domain.ProposalDone:
		return now.Before(p.UpdatedAt.Add(MeasureAfter))
	}
	return false
}

// createdSince counts the proposals created after t, leaving out skip.
func createdSince(all []domain.Proposal, t time.Time, skip map[uuid.UUID]bool) int {
	n := 0
	for _, p := range all {
		if p.CreatedAt.After(t) && !skip[p.ID] {
			n++
		}
	}
	return n
}

// briefText writes the advisor's work list. Signals and proposals hold text
// of agents and the repo: each sits in a block marked as data.
func (s *Service) briefText(all []domain.Proposal, fresh []Signal, room int, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Limit\n\nWrite at most %d proposals.\n\n## New signals (%d)\n", max(room, 0), len(fresh))
	for _, sig := range fresh {
		fmt.Fprintf(&b, "\n%s, weight %.1f:\n", sig.Detector, sig.Weight)
		b.WriteString(contextpack.WrapUntrusted("signal", signalText(sig)))
	}
	listed := func(title string, keep func(domain.Proposal) bool) {
		fmt.Fprintf(&b, "\n## %s\n", title)
		n := 0
		for _, p := range slices.Backward(all) {
			if !keep(p) {
				continue
			}
			if n++; n > maxListed {
				b.WriteString("\n(more left out)\n")
				break
			}
			text := "key: " + p.DedupeKey + "\ntitle: " + detail(p.Title)
			if p.Reason != "" {
				text += "\nreason: " + detail(p.Reason)
			}
			fmt.Fprintf(&b, "\n%s, %s:\n", p.Ref(), p.Status)
			b.WriteString(contextpack.WrapUntrusted(p.Ref(), text))
		}
		if n == 0 {
			b.WriteString("\nNone.\n")
		}
	}
	listed("Open and snoozed proposals: do not repeat them", func(p domain.Proposal) bool { return !p.Status.Final() })
	listed("Recently rejected proposals: do not repeat them", func(p domain.Proposal) bool {
		return p.Status == domain.ProposalRejected && now.Before(p.UpdatedAt.Add(rejectedWindow))
	})
	b.WriteString("\n## Tool catalog\n\n")
	b.WriteString(s.Catalog.text())
	return b.String()
}

// signalText is a signal as the advisor reads it.
func signalText(sig Signal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "key: %s\nsummary: %s\n", sig.Key, sig.Summary)
	for _, e := range sig.Evidence {
		fmt.Fprintf(&b, "- %s %s: %s\n", e.Kind, detail(e.Ref), detail(e.Detail))
	}
	return b.String()
}
