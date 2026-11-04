package registers

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"
	"uuid"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Paths in the repo where the learn step (T39) writes the registers.
const (
	AssumptionsDir = ".forecrew/assumptions"
	DecisionsDir   = "docs/decisions"
)

// flow is a YAML list written on one line.
type flow []string

func (f flow) MarshalYAML() (any, error) {
	n := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	for _, s := range f {
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: s})
	}
	return n, nil
}

// assumptionFile is the file of an assumption, in the format of the design
// doc (Assumptions and decisions).
type assumptionFile struct {
	ID         string `yaml:"id"`
	Statement  string `yaml:"statement"`
	MadeBy     string `yaml:"made_by"`
	Source     string `yaml:"source,omitempty"`
	Confidence string `yaml:"confidence"`
	Impact     string `yaml:"impact"`
	Status     string `yaml:"status"`
	VerifyBy   string `yaml:"verify_by"`
	Evidence   string `yaml:"evidence,omitempty"`
	ResolvedBy string `yaml:"resolved_by,omitempty"`
	ResolvedAt string `yaml:"resolved_at,omitempty"`
	Dependents flow   `yaml:"dependents"`
}

// Export returns the register files of a goal: .forecrew/assumptions/A-n.yaml
// for each of its assumptions and docs/decisions/DEC-n.md for each of its
// decisions, by path. Output is deterministic. The learn step commits them.
func (s *Service) Export(ctx context.Context, accountID, goalID uuid.UUID) (map[string][]byte, error) {
	goal, err := s.st.Goals().Get(ctx, accountID, goalID)
	if err != nil {
		return nil, fmt.Errorf("get goal: %w", err)
	}
	as, err := s.st.Assumptions().List(ctx, accountID, store.AssumptionFilter{ProjectID: goal.ProjectID, GoalID: goalID})
	if err != nil {
		return nil, fmt.Errorf("list assumptions: %w", err)
	}
	ds, err := s.st.Decisions().List(ctx, accountID, store.DecisionFilter{ProjectID: goal.ProjectID, GoalID: goalID})
	if err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	files := map[string][]byte{}
	refs := map[uuid.UUID]string{}
	for _, a := range as {
		refs[a.ID] = a.Ref()
		source, err := s.SourceRef(ctx, accountID, a)
		if err != nil {
			return nil, err
		}
		f := assumptionFile{
			ID: a.Ref(), Statement: a.Statement, MadeBy: a.MadeBy, Source: source, Confidence: string(a.Confidence),
			Impact: string(a.Impact), Status: string(a.Status), VerifyBy: string(a.VerifyBy), Evidence: a.Evidence,
			ResolvedBy: a.ResolvedBy, Dependents: flow{},
		}
		if !a.ResolvedAt.IsZero() {
			f.ResolvedAt = a.ResolvedAt.UTC().Format(time.RFC3339)
		}
		for _, d := range a.Dependents {
			f.Dependents = append(f.Dependents, d.ID)
		}
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(f); err != nil {
			return nil, fmt.Errorf("encode %s: %w", a.Ref(), err)
		}
		files[AssumptionsDir+"/"+a.Ref()+".yaml"] = buf.Bytes()
	}
	for _, d := range ds {
		files[DecisionsDir+"/"+d.Ref()+".md"] = []byte(s.decisionMarkdown(ctx, accountID, d, refs))
	}
	return files, nil
}

// SourceRef names where an assumption came from: its job, else its council,
// else its spec.
func (s *Service) SourceRef(ctx context.Context, accountID uuid.UUID, a domain.Assumption) (string, error) {
	switch {
	case a.JobID != uuid.Nil():
		j, err := s.st.Jobs().Get(ctx, accountID, a.JobID)
		if err != nil {
			return "", fmt.Errorf("get the job of %s: %w", a.Ref(), err)
		}
		return j.Ref(), nil
	case a.CouncilID != uuid.Nil():
		return a.CouncilID.String(), nil
	case a.SpecID != uuid.Nil():
		sp, err := s.st.Specs().Get(ctx, accountID, a.SpecID)
		if err != nil {
			return "", fmt.Errorf("get the spec of %s: %w", a.Ref(), err)
		}
		return sp.Ref(), nil
	}
	return "", nil
}

// decisionMarkdown is the file of a decision. refs names the assumptions of
// the goal; others are looked up.
func (s *Service) decisionMarkdown(ctx context.Context, accountID uuid.UUID, d domain.Decision, refs map[uuid.UUID]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s: %s\n\n", d.Ref(), contextpack.OneLine(d.Question))
	fmt.Fprintf(&b, "- Decided by: %s\n- Date: %s\n", d.DecidedBy, d.CreatedAt.UTC().Format("2006-01-02"))
	b.WriteString("\n## Choice\n\n" + contextpack.OneLine(d.Choice) + "\n")
	if len(d.Options) > 0 {
		b.WriteString("\n## Options\n\n")
		for _, o := range d.Options {
			if o.Summary != "" {
				fmt.Fprintf(&b, "- **%s**: %s\n", contextpack.OneLine(o.Name), contextpack.OneLine(o.Summary))
			} else {
				fmt.Fprintf(&b, "- **%s**\n", contextpack.OneLine(o.Name))
			}
		}
	}
	if r := strings.TrimSpace(d.Reasons); r != "" {
		b.WriteString("\n## Reasons\n\n" + quote(r) + "\n")
	}
	if r := strings.TrimSpace(d.Dissent); r != "" {
		b.WriteString("\n## Dissent\n\n" + quote(r) + "\n")
	}
	if len(d.Assumptions) > 0 {
		b.WriteString("\n## Assumptions\n\n")
		for _, id := range d.Assumptions {
			ref, ok := refs[id]
			if !ok {
				if a, err := s.st.Assumptions().Get(ctx, accountID, id); err == nil {
					ref = a.Ref()
				} else {
					ref = id.String()
				}
			}
			fmt.Fprintf(&b, "- %s\n", ref)
		}
	}
	return b.String()
}

// quote is text written by someone else as a block quote, so that none of
// its lines is a heading or a list item of the file.
func quote(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight("> "+l, " ")
	}
	return strings.Join(lines, "\n")
}
