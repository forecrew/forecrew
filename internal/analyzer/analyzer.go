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
	// Components are the directories with their own build file, sorted by path.
	Components []Component `json:"components"`
	// CIRunners are the runner values workflows declare in runs-on and matrix
	// os entries, e.g. ubuntu-latest. Unique and sorted.
	CIRunners []string `json:"ci_runners"`
	// AgentFiles are the agent instruction files: CLAUDE.md, AGENTS.md,
	// .cursor/rules and .github/copilot-instructions.md.
	AgentFiles []string `json:"agent_files"`
	// Services are the services of the docker-compose files.
	Services []Service `json:"services"`
	// Integrations are the tools the checkout shows the team uses outside the
	// repo, one per tool and instance, sorted. GitHub metadata is discovered
	// by the discover.github action, not here.
	Integrations []Integration `json:"integrations"`
	// Knowledge are the repo-native knowledge sources: ADR folders, the
	// contributing guide and CODEOWNERS. They are not integrations.
	Knowledge []KnowledgeSource `json:"knowledge"`
}

// Service is a service declared in a docker-compose file.
type Service struct {
	File  string `json:"file"`  // compose file, relative to the repo root
	Name  string `json:"name"`  // service name
	Image string `json:"image"` // "" when the service is built from a Dockerfile
}

// Component is a directory with its own build file.
type Component struct {
	Path      string   `json:"path"`      // "." for the repo root
	Stack     string   `json:"stack"`     // go, node, rust, python, java, cmake, make or docker
	Manifests []string `json:"manifests"` // build files in the directory
	Build     string   `json:"build"`     // likely build command, run in Path; "" when unknown
	Test      string   `json:"test"`      // likely test command, run in Path; "" when unknown
}

// Analyzer computes repo facts.
type Analyzer interface {
	// Analyze inspects the checkout at dir.
	Analyze(ctx context.Context, dir string) (Facts, error)
}
