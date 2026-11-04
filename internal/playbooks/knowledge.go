package playbooks

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/workflow"
)

// seedOutput is the output of knowledge.seed.
type seedOutput struct {
	Indexed int `json:"indexed"` // entries read from the repo
	// Sections are the sections that have no location yet.
	Sections []string `json:"sections"`
	// Curate holds one item when the curator has sections to fill and none
	// otherwise, so a foreach over it skips the curation.
	Curate []string `json:"curate"`
	// Text tells the curator what to fill and what already has a home.
	Text string `json:"text"`
	// Error is why the repo could not be indexed; seeding then stops.
	Error string `json:"error,omitempty"`
}

// seedKnowledge is the action knowledge.seed. It indexes the project's
// mapped knowledge paths (the policy file, else the repo's ADR folders,
// contributing guide and knowledge/ folders) and outputs the sections the
// repo gives no location, which the knowledge curator then proposes
// entries for. A repo that cannot be read, or an installation without a
// knowledge base, ends the seeding quietly: onboarding is done and does
// not depend on it, and the poller indexes the repo later. Idempotent: it
// indexes what changed and reads the same policy again.
func (a *actions) seedKnowledge(ctx context.Context, call workflow.ActionCall) (any, error) {
	out := seedOutput{Sections: []string{}, Curate: []string{}}
	if a.Knowledge == nil {
		return out, nil
	}
	p, err := a.Store.Projects().Get(ctx, call.Run.AccountID, call.Run.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	res, err := a.Knowledge.Sync(ctx, p, "", "")
	if err != nil {
		out.Error = err.Error()
		return out, nil
	}
	out.Indexed = res.Indexed
	policy, err := a.Knowledge.Policy(ctx, p)
	if err != nil {
		out.Error = err.Error()
		return out, nil
	}
	out.Sections = knowledge.MissingSections(policy)
	if len(out.Sections) == 0 {
		return out, nil
	}
	out.Curate = []string{"curator"}
	out.Text = seedText(policy, out.Sections)
	return out, nil
}

// seedText is what the curator is told: the sections to fill and the ones
// that have a location already, which it must leave alone.
func seedText(p knowledge.Policy, missing []string) string {
	var b strings.Builder
	b.WriteString("## Sections to fill\n\n")
	b.WriteString("Write entries only for these sections, and only what the sources say: ")
	b.WriteString(strings.Join(missing, ", "))
	b.WriteString(".\n\n## Sections that already have a location\n\n")
	b.WriteString("Do not write entries for these; their owners keep them.\n\n")
	names := make([]string, 0, len(p.Sections))
	for name, s := range p.Sections {
		if !knowledge.Reserved(name) && len(s.Paths) > 0 {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Fprintf(&b, "- %s: %s\n", name, strings.Join(p.Sections[name].Paths, ", "))
	}
	if len(names) == 0 {
		b.WriteString("None.\n")
	}
	return b.String()
}
