package contextpack

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/testsys"
)

// RepoFiles reads files of a repo at a ref; github.Tracker implements it.
type RepoFiles interface {
	GetFile(ctx context.Context, repo, ref, path string) ([]byte, error)
}

// testPlanRoles get the Test plan section. The role packs' context
// settings (T38) will take this choice over.
var testPlanRoles = []string{"test-architect", "environment-engineer", "test-engineer"}

// testPlan returns the Test plan section: the test matrix, the test
// strategy and the environments the matrix names, read at the job branch
// when the matrix is there and at the base otherwise. Missing files are
// said to be missing; other read errors fail.
func (b *StoreBuilder) testPlan(ctx context.Context, repo, baseRef, branch string) (Text, error) {
	refs := []string{baseRef}
	if branch != "" {
		refs = []string{branch, baseRef}
	}
	plan := plan{ref: baseRef}
	for _, ref := range refs {
		data, err := b.file(ctx, repo, ref, testsys.MatrixFile)
		if err != nil {
			return Text{}, err
		}
		if data != nil {
			plan.ref, plan.matrix = ref, data
			break
		}
	}
	var err error
	if plan.strategy, err = b.file(ctx, repo, plan.ref, testsys.StrategyFile); err != nil {
		return Text{}, err
	}
	names, listErr := environmentNames(plan.matrix)
	plan.listErr = listErr
	for _, name := range names {
		env := environment{name: name, file: path.Join(testsys.EnvironmentsDir, name+".yaml")}
		if domain.ValidName(name) {
			if env.data, err = b.file(ctx, repo, plan.ref, env.file); err != nil {
				return Text{}, err
			}
		}
		plan.environments = append(plan.environments, env)
	}
	return Text{Heading: "Test plan", Body: plan.render()}, nil
}

// file reads a file of repo at ref; nil when it does not exist.
func (b *StoreBuilder) file(ctx context.Context, repo, ref, name string) ([]byte, error) {
	data, err := b.files.GetFile(ctx, repo, ref, name)
	switch {
	case errors.Is(err, github.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read %s at %s: %w", name, ref, err)
	case data == nil:
		return []byte{}, nil
	}
	return data, nil
}

// environmentNames returns the environments the matrix's rules name, in
// rule order without repeats. The matrix is only decoded, not validated:
// the test roles see the plan as it is, mistakes included.
func environmentNames(matrix []byte) ([]string, error) {
	if matrix == nil {
		return nil, nil
	}
	var m struct {
		Environments []struct {
			Environment string `yaml:"environment"`
		} `yaml:"environments"`
	}
	if err := yaml.Unmarshal(matrix, &m); err != nil {
		return nil, err
	}
	var names []string
	for _, r := range m.Environments {
		if r.Environment != "" && !slices.Contains(names, r.Environment) {
			names = append(names, r.Environment)
		}
	}
	return names, nil
}

// plan is the content of the Test plan section. A nil file is missing.
type plan struct {
	ref          string
	matrix       []byte
	strategy     []byte
	environments []environment
	listErr      error // why the environments could not be listed
}

type environment struct {
	name, file string
	data       []byte
}

func (p plan) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "As committed at `%s`; the checkout is authoritative where it differs.\n", p.ref)
	fmt.Fprintf(&b, "\n## Matrix: `%s`\n\n", testsys.MatrixFile)
	fileBody(&b, "yaml", p.matrix)
	fmt.Fprintf(&b, "\n## Strategy: `%s`\n\n", testsys.StrategyFile)
	fileBody(&b, "markdown", p.strategy)
	b.WriteString("\n## Environments\n")
	switch {
	case p.listErr != nil:
		fmt.Fprintf(&b, "\nThe matrix's environments cannot be listed: %v.\n", p.listErr)
	case p.matrix == nil:
		b.WriteString("\nNone: there is no matrix to name them.\n")
	case len(p.environments) == 0:
		b.WriteString("\nNone: the matrix names no environment.\n")
	}
	for _, e := range p.environments {
		fmt.Fprintf(&b, "\n### `%s`: `%s`\n\n", e.name, e.file)
		if !domain.ValidName(e.name) {
			b.WriteString("Not a valid environment name.\n")
			continue
		}
		fileBody(&b, "yaml", e.data)
	}
	return b.String()
}

// fileBody writes a file's content fenced, or that it does not exist.
func fileBody(b *strings.Builder, lang string, data []byte) {
	switch {
	case data == nil:
		b.WriteString("Does not exist yet.\n")
	case strings.TrimSpace(string(data)) == "":
		b.WriteString("Empty.\n")
	default:
		b.WriteString(fenced(lang, string(data)))
	}
}
