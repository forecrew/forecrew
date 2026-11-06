package playbooks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/contextpack"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// A tool the profile has is "much more used" when the discovery counts at
// least toolGrowthFactor times the links the profile recorded for it, and at
// least toolGrowthMin.
const (
	toolGrowthFactor = 3
	toolGrowthMin    = 10
	// toolPRsPerRun bounds the PRs one run opens: discovery reads what
	// anyone can write in an issue.
	toolPRsPerRun = 3
)

// toolMarker is the audit action that records a tool the curator proposed.
const toolMarker = "profile.tool_proposed"

// toolPR is a profile PR the curator opened.
type toolPR struct {
	Tool     string `json:"tool"`
	Instance string `json:"instance,omitempty"`
	Reason   string `json:"reason"` // new or more_used
	Number   int    `json:"number"`
	URL      string `json:"url"`
}

// toolsOutput is the output of curate.tools.
type toolsOutput struct {
	Opened  []toolPR `json:"opened"`
	Skipped []string `json:"skipped"`
}

func toolKey(in analyzer.Integration) string { return strings.ToLower(in.Tool + "|" + in.Instance) }

// links is how many times the evidence saw the tool.
func links(in analyzer.Integration) int {
	n := 0
	for _, e := range in.Evidence {
		n += max(e.Count, 1)
	}
	return n
}

// toolSubject is the subject of the audit entry of a tool of a project: the
// same tool is the same subject whenever it is looked at.
func toolSubject(project uuid.UUID, key string) uuid.UUID {
	sum := sha256.Sum256([]byte("forecrew-profile-tool\x00" + project.String() + "\x00" + key))
	var id uuid.UUID
	copy(id[:], sum[:16])
	id[6], id[8] = id[6]&0x0f|0x80, id[8]&0x3f|0x80
	return id
}

// toolBranch is the branch of a tool's profile PR.
func toolBranch(in analyzer.Integration) string {
	slug := strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, strings.ToLower(in.Tool+" "+in.Instance)), "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	sum := sha256.Sum256([]byte(toolKey(in)))
	return "agent/tool-" + slug + "-" + hex.EncodeToString(sum[:3])
}

// toolChange is a discovered tool the profile should get.
type toolChange struct {
	in     analyzer.Integration
	reason string // new or more_used
}

// planTools returns the discovered tools that are new to the profile or much
// more used than it recorded, in the order found.
func planTools(profile, found []analyzer.Integration) []toolChange {
	have := map[string]analyzer.Integration{}
	for _, in := range profile {
		have[toolKey(in)] = in
	}
	var out []toolChange
	for _, in := range found {
		cur, ok := have[toolKey(in)]
		switch {
		case !ok:
			out = append(out, toolChange{in, "new"})
		case links(in) >= max(toolGrowthMin, toolGrowthFactor*links(cur)):
			out = append(out, toolChange{in, "more_used"})
		}
	}
	return out
}

// curateTools is the action curate.tools. It compares the tools discovery
// found (with.integrations, the output of discover.github) with the
// integrations of the profile on the default branch, and for each tool that is
// new or much more used opens a PR on .forecrew/profile.yaml: a new tool is
// added with status proposed and its evidence, a tool the profile has gets its
// evidence updated and keeps its status. A tool gets a PR once, ever: the
// audit log remembers it, so a PR that was closed is not opened again. A human
// merges the PR; connecting a tool that needs credentials stays a human step.
// At most three PRs per run. Idempotent: the branch, the file and the PR are
// the same on a repeat.
func (a *actions) curateTools(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Repo         string                 `json:"repo"`
		Integrations []analyzer.Integration `json:"integrations"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	out := toolsOutput{Opened: []toolPR{}, Skipped: []string{}}
	p, err := a.Store.Projects().Get(ctx, call.Run.AccountID, call.Run.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	file, err := a.Tracker.GetFile(ctx, p.Repo, p.DefaultBranch, profilePath)
	if errors.Is(err, github.ErrNotFound) {
		out.Skipped = append(out.Skipped, profilePath+" does not exist: onboard the project first")
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", profilePath, err)
	}
	var profile analyzer.Profile
	if err := yaml.Unmarshal(file, &profile); err != nil {
		out.Skipped = append(out.Skipped, fmt.Sprintf("%s does not parse: %v", profilePath, err))
		return out, nil
	}
	for _, c := range planTools(profile.Integrations, with.Integrations) {
		if len(out.Opened) == toolPRsPerRun {
			out.Skipped = append(out.Skipped, "more tools than one run opens PRs for: the rest wait for the next run")
			break
		}
		done, err := a.toolProposed(ctx, call.Run, c.in)
		if err != nil {
			return nil, err
		}
		if done {
			continue
		}
		pr, why, err := a.openToolPR(ctx, p, file, c)
		switch {
		case err != nil:
			return nil, err
		case why != "":
			out.Skipped = append(out.Skipped, contextpack.OneLine(c.in.Tool)+": "+why)
		default:
			out.Opened = append(out.Opened, pr)
		}
	}
	return out, nil
}

// toolProposed reports whether the curator proposed the tool before.
func (a *actions) toolProposed(ctx context.Context, run domain.WorkflowRun, in analyzer.Integration) (bool, error) {
	es, err := a.Store.Audit().List(ctx, run.AccountID, store.AuditFilter{ProjectID: run.ProjectID, SubjectID: toolSubject(run.ProjectID, toolKey(in))})
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(es, func(e domain.AuditEntry) bool { return e.Action == toolMarker }), nil
}

// openToolPR writes the PR of c. why is set, with no error, when the tool
// cannot be proposed: the profile with it would be invalid.
func (a *actions) openToolPR(ctx context.Context, p domain.Project, file []byte, c toolChange) (toolPR, string, error) {
	edited, err := editProfile(file, c)
	if err != nil {
		return toolPR{}, "the profile cannot be edited: " + err.Error(), nil
	}
	if err := analyzer.Validate(edited); err != nil {
		return toolPR{}, "the profile would be invalid with it: " + err.Error(), nil
	}
	branch := toolBranch(c.in)
	head, err := a.Tracker.HeadSHA(ctx, p.Repo, p.DefaultBranch)
	if err != nil {
		return toolPR{}, "", err
	}
	if err := a.Tracker.CreateBranch(ctx, p.Repo, branch, head); err != nil {
		return toolPR{}, "", err
	}
	verb := "Propose"
	if c.reason == "more_used" {
		verb = "Update the evidence of"
	}
	if _, err := a.Tracker.WriteFile(ctx, p.Repo, branch, profilePath, edited, verb+" the integration "+contextpack.OneLine(c.in.Tool)); err != nil {
		return toolPR{}, "", err
	}
	title := "Profile: " + verb + " " + contextpack.OneLine(c.in.Tool)
	pr, err := a.Tracker.FindPR(ctx, p.Repo, branch)
	if errors.Is(err, github.ErrNotFound) {
		pr, err = a.Tracker.CreatePR(ctx, p.Repo, github.PRInput{Title: title, Head: branch, Base: p.DefaultBranch, Body: toolPRBody(c)})
	}
	if err != nil {
		return toolPR{}, "", err
	}
	data := fmt.Sprintf(`{"tool":%q,"instance":%q,"reason":%q,"number":%d}`, c.in.Tool, c.in.Instance, c.reason, pr.Number)
	if err := a.Store.Audit().Append(ctx, &domain.AuditEntry{
		AccountID: p.AccountID, ProjectID: p.ID, Actor: domain.SystemActor(p.AccountID, domain.ForecrewdName).String(), Action: toolMarker,
		SubjectType: "profile_tool", SubjectID: toolSubject(p.ID, toolKey(c.in)), Data: []byte(data),
	}); err != nil {
		return toolPR{}, "", err
	}
	return toolPR{Tool: c.in.Tool, Instance: c.in.Instance, Reason: c.reason, Number: pr.Number, URL: pr.URL}, "", nil
}

// toolPRBody tells a human why the PR is there. What was found comes from
// issues and links anyone can write: marked as data.
func toolPRBody(c toolChange) string {
	var b strings.Builder
	if c.reason == "more_used" {
		fmt.Fprintf(&b, "The curator saw %d links to %s in recent issues and pull requests, much more than the profile recorded. This PR updates the evidence of its integration; its status stays.\n\n", links(c.in), contextpack.OneLine(c.in.Tool))
	} else {
		fmt.Fprintf(&b, "The curator found %s in recent issues and pull requests and in the repo's autolinks, and it is not in the profile. This PR adds it as `proposed`: merging says the team uses it. Connecting it, when it needs credentials, is a separate step that only a human does.\n\n", contextpack.OneLine(c.in.Tool))
	}
	var ev strings.Builder
	for _, e := range c.in.Evidence {
		fmt.Fprintf(&ev, "- %s %s, %d\n", contextpack.OneLine(e.Kind), contextpack.OneLine(e.Location), e.Count)
	}
	b.WriteString(contextpack.WrapUntrusted("discovered evidence", ev.String()))
	return b.String()
}

// editProfile returns the profile file with c applied, keeping its comments
// and the order of everything else.
func editProfile(file []byte, c toolChange) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(file, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("it is not a mapping")
	}
	root := doc.Content[0]
	list := mappingValue(root, "integrations")
	if list == nil {
		list = &yaml.Node{Kind: yaml.SequenceNode}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "integrations"}, list)
	}
	if list.Kind != yaml.SequenceNode {
		return nil, errors.New("integrations is not a list")
	}
	in := c.in
	if c.reason == "new" {
		in.Status = "proposed"
		var n yaml.Node
		if err := n.Encode(in); err != nil {
			return nil, err
		}
		list.Content = append(list.Content, &n)
	} else {
		i := slices.IndexFunc(list.Content, func(n *yaml.Node) bool {
			var cur analyzer.Integration
			return n.Decode(&cur) == nil && toolKey(cur) == toolKey(in)
		})
		if i < 0 {
			return nil, errors.New("the integration is not in the profile")
		}
		var ev, conf yaml.Node
		if err := ev.Encode(in.Evidence); err != nil {
			return nil, err
		}
		if err := conf.Encode(in.Confidence); err != nil {
			return nil, err
		}
		setMapping(list.Content[i], "evidence", &ev)
		setMapping(list.Content[i], "confidence", &conf)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

// mappingValue returns the value of key in a mapping node, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setMapping sets key of a mapping node to value, adding it when missing.
func setMapping(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, value)
}
