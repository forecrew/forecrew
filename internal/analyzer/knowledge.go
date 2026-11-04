package analyzer

import (
	"path"
	"slices"
	"strings"
)

// Knowledge source kinds.
const (
	knowledgeADR          = "adr"
	knowledgeContributing = "contributing"
	knowledgeCodeowners   = "codeowners"
)

// KnowledgeSource is a repo-native source of team knowledge. Section is the
// knowledge section it suggests: architecture for ADRs, conventions for the
// contributing guide and owners for CODEOWNERS. These are not integrations:
// they live in the repo.
type KnowledgeSource struct {
	Kind    string `json:"kind"`    // adr, contributing or codeowners
	Path    string `json:"path"`    // a folder (adr) or a file, relative to the repo root
	Section string `json:"section"` // architecture, conventions or owners
}

// adrDefaults are the ADR folders the repo may use when .adr-dir names none.
var adrDefaults = []string{"docs/adr", "docs/decisions", "doc/architecture/decisions"}

// codeownersPaths are GitHub's locations of CODEOWNERS, in order of precedence.
var codeownersPaths = []string{"CODEOWNERS", ".github/CODEOWNERS", "docs/CODEOWNERS"}

// knowledgeSources returns the ADR folders with files, the contributing guide
// and the CODEOWNERS file the checkout has. A .adr-dir file names an ADR
// folder (adr-tools); a target outside the repo is ignored.
func knowledgeSources(root string, files []string) ([]KnowledgeSource, error) {
	return KnowledgeSourcesOf(files, func(name string) ([]byte, error) { return readRepoFile(root, name) })
}

// KnowledgeSourcesOf is knowledgeSources for a repo that is not checked
// out: files are its paths and read returns one of them. forecrewd uses it
// on the file list of a GitHub tree.
func KnowledgeSourcesOf(files []string, read func(name string) ([]byte, error)) ([]KnowledgeSource, error) {
	var out []KnowledgeSource
	adrs, err := adrFolders(files, read)
	if err != nil {
		return nil, err
	}
	for _, d := range adrs {
		out = append(out, KnowledgeSource{Kind: knowledgeADR, Path: d, Section: "architecture"})
	}
	if slices.Contains(files, "CONTRIBUTING.md") {
		out = append(out, KnowledgeSource{Kind: knowledgeContributing, Path: "CONTRIBUTING.md", Section: "conventions"})
	}
	for _, p := range codeownersPaths {
		if slices.Contains(files, p) {
			out = append(out, KnowledgeSource{Kind: knowledgeCodeowners, Path: p, Section: "owners"})
			break
		}
	}
	return out, nil
}

// adrFolders returns the ADR folders that hold at least one file: the folder
// .adr-dir names first, then the defaults, each once.
func adrFolders(files []string, read func(name string) ([]byte, error)) ([]string, error) {
	candidates := adrDefaults
	if slices.Contains(files, ".adr-dir") {
		data, err := read(".adr-dir")
		if err != nil {
			return nil, err
		}
		if target := adrTarget(string(data)); target != "" {
			candidates = append([]string{target}, adrDefaults...)
		}
	}
	var out []string
	for _, d := range candidates {
		if !slices.Contains(out, d) && hasFileUnder(files, d) {
			out = append(out, d)
		}
	}
	return out, nil
}

// adrTarget returns the folder an .adr-dir file names: its first non-empty
// line, cleaned. "" when it names no folder inside the repo.
func adrTarget(content string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(content), "\n")
	t := path.Clean(strings.TrimSpace(line))
	if t == "." || t == ".." || strings.HasPrefix(t, "../") || path.IsAbs(t) {
		return ""
	}
	return t
}

// hasFileUnder reports whether a file lies under the folder dir.
func hasFileUnder(files []string, dir string) bool {
	for _, f := range files {
		if strings.HasPrefix(f, dir+"/") {
			return true
		}
	}
	return false
}
