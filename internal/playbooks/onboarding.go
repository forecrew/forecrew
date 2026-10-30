package playbooks

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/workflow"
)

// assumption is an assumption of the analyst's output.
type assumption struct {
	Text       string `json:"text"`
	Confidence string `json:"confidence"`
	Impact     string `json:"impact"`
}

// proposalOutput is the output of onboarding.proposal.
type proposalOutput struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// onboardingProposal is the action onboarding.proposal. It writes the title
// and body of the profile PR from with.profile (a project profile as JSON),
// with.summary and with.assumptions (the analyst's output): the detected
// components with their commands, platforms and needs tags, the assumptions
// and the open questions.
func (a *actions) onboardingProposal(_ context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Profile     json.RawMessage `json:"profile"`
		Summary     string          `json:"summary"`
		Assumptions []assumption    `json:"assumptions"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	var p analyzer.Profile
	if err := yaml.Unmarshal(with.Profile, &p); err != nil { // JSON is YAML
		return nil, fmt.Errorf("with: profile: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Adds `.forecrew/profile.yaml` and `.forecrew/README.md`, the profile Forecrew proposes for **%s**.\n", p.Name)
	b.WriteString("Merge this PR to approve the profile, close it to reject it, or ask for changes in Forecrew.\n")
	section(&b, "Summary", []string{strings.TrimSpace(with.Summary)}, "")
	var components []string
	for _, c := range p.Components {
		components = append(components, componentLine(c))
	}
	section(&b, "Components", components, "- ")
	section(&b, "Platforms", []string{strings.Join(p.Platforms, ", ")}, "")
	var assumptions []string
	for _, as := range with.Assumptions {
		assumptions = append(assumptions, fmt.Sprintf("%s (confidence %s, impact %s)", as.Text, as.Confidence, as.Impact))
	}
	section(&b, "Assumptions", assumptions, "- ")
	section(&b, "Open questions", p.OpenQuestions, "- ")
	return proposalOutput{Title: "Add the Forecrew project profile", Body: b.String()}, nil
}

// componentLine describes a component in one markdown line.
func componentLine(c analyzer.ProfileComponent) string {
	parts := []string{fmt.Sprintf("`%s` (%s)", c.Path, c.Stack)}
	for _, f := range []struct{ label, value string }{
		{"build", code(c.Build)},
		{"test", code(c.Test)},
		{"platforms", strings.Join(c.Platforms, ", ")},
		{"needs", strings.Join(c.Needs, ", ")},
	} {
		if f.value != "" {
			parts = append(parts, f.label+": "+f.value)
		}
	}
	return strings.Join(parts, "; ")
}

func code(s string) string {
	if s == "" {
		return ""
	}
	return "`" + s + "`"
}

// section writes a titled section of items, each with prefix, skipping the
// empty items and the section when none is left.
func section(b *strings.Builder, title string, items []string, prefix string) {
	var kept []string
	for _, it := range items {
		if it != "" {
			kept = append(kept, prefix+it)
		}
	}
	if len(kept) > 0 {
		fmt.Fprintf(b, "\n## %s\n\n%s\n", title, strings.Join(kept, "\n"))
	}
}

// settleOnboarding is the action onboarding.settle, which the onboarding
// playbook runs when it ends failed or canceled: a project still onboarding
// goes back to registered, so it can be onboarded again. Any other status is
// left alone, so it is safe to call twice.
func (a *actions) settleOnboarding(ctx context.Context, call workflow.ActionCall) (any, error) {
	p, err := a.Store.Projects().Get(ctx, call.Run.AccountID, call.Run.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	if p.Status != domain.ProjectOnboarding {
		return map[string]string{"status": string(p.Status)}, nil
	}
	if err := a.Store.Projects().UpdateStatus(ctx, p.AccountID, p.ID, domain.ProjectOnboarding, domain.ProjectRegistered); err != nil {
		return nil, fmt.Errorf("put %s back to registered: %w", p.Name, err)
	}
	return map[string]string{"status": string(domain.ProjectRegistered)}, nil
}

// completeOnboarding is the action onboarding.complete. It reads the profile
// file in with.repo at the merge commit with.sha, or at the branch with.ref
// (the base branch) when GitHub merged the PR itself and there is no commit
// to name, so edits made in the PR count. It validates the file, stores it as the run's project
// profile and moves the project from onboarding to onboarded; a project
// already onboarded keeps going, so a repeated call succeeds.
func (a *actions) completeOnboarding(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		Repo string `json:"repo"`
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	p, err := a.Store.Projects().Get(ctx, call.Run.AccountID, call.Run.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	if p.Status != domain.ProjectOnboarding && p.Status != domain.ProjectOnboarded {
		return nil, fmt.Errorf("project %s is %s, not onboarding", p.Name, p.Status)
	}
	file, err := a.Tracker.GetFile(ctx, with.Repo, cmp.Or(with.SHA, with.Ref), profilePath)
	if err != nil {
		return nil, fmt.Errorf("read the merged profile: %w", err)
	}
	if err := analyzer.Validate(file); err != nil {
		return nil, fmt.Errorf("the merged %s: %w", profilePath, err)
	}
	var doc any
	if err := yaml.Unmarshal(file, &doc); err != nil { // Validate parsed it
		return nil, fmt.Errorf("parse the merged profile: %w", err)
	}
	profile, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("convert the merged profile: %w", err)
	}
	if err := a.Store.Projects().SetProfile(ctx, p.AccountID, p.ID, profile); err != nil {
		return nil, fmt.Errorf("store the profile of %s: %w", p.Name, err)
	}
	if p.Status == domain.ProjectOnboarding {
		if err := a.Store.Projects().UpdateStatus(ctx, p.AccountID, p.ID, domain.ProjectOnboarding, domain.ProjectOnboarded); err != nil {
			return nil, fmt.Errorf("onboard %s: %w", p.Name, err)
		}
	}
	return map[string]string{"status": string(domain.ProjectOnboarded)}, nil
}
