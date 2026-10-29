// Package analyzer computes repo facts from a checkout without running an
// agent: languages, manifests, CI and docs. The onboarding analyst and jobs
// with include_repo_facts get them in their prompt. internal/testkit has a
// fake.
package analyzer

import "context"

// Language is one language found in the repo.
type Language struct {
	Name  string `json:"name"` // e.g. Go, TypeScript
	Files int    `json:"files"`
	Lines int    `json:"lines"`
	Bytes int64  `json:"bytes"`
}

// Facts describes a repo checkout. Paths are relative to the repo root,
// with forward slashes, sorted.
type Facts struct {
	Name      string     `json:"name"`      // checkout directory name
	Languages []Language `json:"languages"` // most bytes first
	Manifests []string   `json:"manifests"` // go.mod, package.json, Cargo.toml, ...
	CIFiles   []string   `json:"ci_files"`  // .github/workflows/*.yml, ...
	DocFiles  []string   `json:"doc_files"` // README*, AGENTS.md, CONTRIBUTING*, docs/**
	TopLevel  []string   `json:"top_level"` // entries of the repo root, directories end in "/"
}

// Analyzer computes repo facts.
type Analyzer interface {
	// Analyze inspects the checkout at dir.
	Analyze(ctx context.Context, dir string) (Facts, error)
}
