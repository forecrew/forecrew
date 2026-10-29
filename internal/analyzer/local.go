package analyzer

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Local analyzes a checkout on the local file system. It reads files only;
// it never runs a build tool or touches the network.
type Local struct{}

var _ Analyzer = Local{}

// excludedDirs are skipped at any depth: dependencies, build output and git
// metadata.
var excludedDirs = map[string]bool{
	"vendor":       true,
	"node_modules": true,
	"dist":         true,
	"build":        true,
	".git":         true,
}

// composePatterns match docker-compose files by base name.
var composePatterns = []string{"docker-compose*.yml", "docker-compose*.yaml", "compose.yml", "compose.yaml"}

// languageByExt maps lower-case file extensions to language names.
var languageByExt = map[string]string{
	".go": "Go", ".py": "Python", ".rs": "Rust", ".java": "Java", ".kt": "Kotlin",
	".ts": "TypeScript", ".tsx": "TypeScript",
	".js": "JavaScript", ".jsx": "JavaScript", ".mjs": "JavaScript", ".cjs": "JavaScript",
	".c": "C", ".h": "C", ".cc": "C++", ".cpp": "C++", ".cxx": "C++", ".hpp": "C++",
	".cs": "C#", ".rb": "Ruby", ".php": "PHP", ".swift": "Swift", ".sh": "Shell",
	".sql": "SQL", ".html": "HTML", ".css": "CSS", ".scss": "SCSS",
}

// generatedRe matches the marker Go and protoc-style generators write before
// the code: "// Code generated ... DO NOT EDIT.", or the "#" form.
var generatedRe = regexp.MustCompile(`(?m)^(?://|#) Code generated .* DO NOT EDIT\.\r?$`)

// Analyze implements Analyzer.
func (Local) Analyze(ctx context.Context, dir string) (Facts, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return Facts{}, fmt.Errorf("analyze %s: %w", dir, err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return Facts{}, fmt.Errorf("analyze %s: %w", dir, err)
	}
	files, err := listFiles(ctx, root)
	if err != nil {
		return Facts{}, fmt.Errorf("analyze %s: %w", dir, err)
	}
	langs, err := languages(root, files)
	if err != nil {
		return Facts{}, fmt.Errorf("analyze %s: %w", dir, err)
	}
	top, err := topLevel(root)
	if err != nil {
		return Facts{}, fmt.Errorf("analyze %s: %w", dir, err)
	}
	comps, err := components(root, files)
	if err != nil {
		return Facts{}, fmt.Errorf("analyze %s: %w", dir, err)
	}
	runners, err := ciRunners(root, files)
	if err != nil {
		return Facts{}, fmt.Errorf("analyze %s: %w", dir, err)
	}
	services, err := composeServices(root, files)
	if err != nil {
		return Facts{}, fmt.Errorf("analyze %s: %w", dir, err)
	}
	return Facts{
		Name:       filepath.Base(root),
		Languages:  nonNil(langs),
		Manifests:  filter(files, isManifest),
		CIFiles:    filter(files, isWorkflow),
		DocFiles:   filter(files, isDoc),
		TopLevel:   nonNil(top),
		Components: comps,
		CIRunners:  runners,
		AgentFiles: filter(files, isAgentFile),
		Services:   services,
	}, nil
}

// listFiles returns the slash-separated paths of the regular files under
// root, skipping excludedDirs at any depth.
func listFiles(ctx context.Context, root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if p == root {
			return nil
		}
		if d.IsDir() {
			if excludedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(files)
	return files, nil
}

// topLevel lists the entries of the repo root, directories with a trailing
// slash. The .git entry is left out.
func topLevel(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		switch {
		case e.Name() == ".git":
		case e.IsDir():
			out = append(out, e.Name()+"/")
		default:
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out, nil
}

// languages counts files, lines and bytes per language, skipping generated
// files. The result is sorted by bytes, most first.
func languages(root string, files []string) ([]Language, error) {
	byName := map[string]*Language{}
	for _, rel := range files {
		name, ok := languageByExt[strings.ToLower(path.Ext(rel))]
		if !ok {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) // #nosec G304 -- rel comes from a walk under root
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", rel, err)
		}
		if generatedRe.Match(data) {
			continue
		}
		l := byName[name]
		if l == nil {
			l = &Language{Name: name}
			byName[name] = l
		}
		l.Files++
		l.Bytes += int64(len(data))
		l.Lines += countLines(data)
	}
	out := make([]Language, 0, len(byName))
	for _, l := range byName {
		out = append(out, *l)
	}
	slices.SortFunc(out, func(a, b Language) int {
		return cmp.Or(cmp.Compare(b.Bytes, a.Bytes), strings.Compare(a.Name, b.Name))
	})
	return out, nil
}

// countLines counts lines, counting a final line without a newline.
func countLines(data []byte) int {
	n := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// isManifest reports whether rel is a build, package or deployment file.
func isManifest(rel string) bool {
	base := path.Base(rel)
	return buildFileIndex(base) >= 0 || matchAny(composePatterns, base) ||
		strings.HasPrefix(rel, ".devcontainer/")
}

// isWorkflow reports whether rel is a GitHub Actions workflow.
func isWorkflow(rel string) bool {
	return strings.HasPrefix(rel, ".github/workflows/") &&
		(strings.HasSuffix(rel, ".yml") || strings.HasSuffix(rel, ".yaml"))
}

// isAgentFile reports whether rel is an agent instruction file.
func isAgentFile(rel string) bool {
	return matchAny([]string{"CLAUDE.md", "AGENTS.md"}, path.Base(rel)) ||
		rel == ".cursor/rules" || strings.HasPrefix(rel, ".cursor/rules/") ||
		rel == ".github/copilot-instructions.md"
}

// isDoc reports whether rel is a README, contributing guide, AGENTS.md or
// a file under docs/.
func isDoc(rel string) bool {
	base := path.Base(rel)
	return matchAny([]string{"README*", "CONTRIBUTING*", "AGENTS.md"}, base) ||
		strings.HasPrefix(rel, "docs/")
}

// matchGlob reports whether name matches the glob pattern.
func matchGlob(pattern, name string) bool {
	ok, _ := path.Match(pattern, name)
	return ok
}

// matchAny reports whether name matches one of the glob patterns.
func matchAny(patterns []string, name string) bool {
	return slices.ContainsFunc(patterns, func(p string) bool { return matchGlob(p, name) })
}

// filter returns the items for which keep is true.
func filter(items []string, keep func(string) bool) []string {
	var out []string
	for _, it := range items {
		if keep(it) {
			out = append(out, it)
		}
	}
	return nonNil(out)
}

// nonNil returns s, or an empty slice when s is nil, so JSON shows [].
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
