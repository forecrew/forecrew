package analyzer

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// workflow is the part of a GitHub Actions workflow the analyzer reads.
type workflow struct {
	Jobs map[string]struct {
		RunsOn   any `yaml:"runs-on"`
		Strategy struct {
			Matrix map[string]any `yaml:"matrix"`
		} `yaml:"strategy"`
	} `yaml:"jobs"`
}

// ciRunners returns the unique, sorted runner values of the workflow files in
// files: runs-on values and matrix os entries. Expressions such as
// ${{ matrix.os }} are skipped, since their values come from the matrix.
// A workflow that does not parse contributes no runners.
func ciRunners(root string, files []string) ([]string, error) {
	seen := map[string]bool{}
	for _, rel := range files {
		if !isWorkflow(rel) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) // #nosec G304 -- rel comes from a walk under root
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", rel, err)
		}
		var wf workflow
		if err := yaml.Unmarshal(data, &wf); err != nil {
			continue
		}
		for _, job := range wf.Jobs {
			for _, v := range append(stringsOf(job.RunsOn), matrixOS(job.Strategy.Matrix)...) {
				if v != "" && !strings.Contains(v, "${{") {
					seen[v] = true
				}
			}
		}
	}
	return nonNil(slices.Sorted(maps.Keys(seen))), nil
}

// matrixOS returns the os values of a strategy matrix, including the os
// entries of its include list.
func matrixOS(matrix map[string]any) []string {
	out := stringsOf(matrix["os"])
	include, _ := matrix["include"].([]any)
	for _, item := range include {
		if entry, ok := item.(map[string]any); ok {
			out = append(out, stringsOf(entry["os"])...)
		}
	}
	return out
}

// stringsOf returns v as a list of strings: a string, or the string items of
// a list. Other values yield none.
func stringsOf(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
