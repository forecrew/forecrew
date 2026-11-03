package analyzer

import (
	_ "embed"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/forecrew/forecrew/internal/schema"
)

// profileSchema is a copy of docs/contracts/profile.schema.json; a test fails
// when the two differ.
//
//go:embed profile.schema.json
var profileSchema string

// ErrInvalidProfile is wrapped by every error Validate returns.
var ErrInvalidProfile = errors.New("invalid project profile")

// Profile is the content of .forecrew/profile.yaml, as described by
// docs/contracts/profile.schema.json.
type Profile struct {
	Version           int                `yaml:"version"`
	Name              string             `yaml:"name"`
	Summary           string             `yaml:"summary"`
	Languages         []string           `yaml:"languages,omitempty"`
	Components        []ProfileComponent `yaml:"components,omitempty"`
	Platforms         []string           `yaml:"platforms,omitempty"`
	Services          []ProfileService   `yaml:"services,omitempty"`
	CI                ProfileCI          `yaml:"ci,omitempty"`
	AgentInstructions string             `yaml:"agent_instructions,omitempty"`
	SuggestedRoles    []ProfileRole      `yaml:"suggested_roles,omitempty"`
	OpenQuestions     []string           `yaml:"open_questions,omitempty"`
	Integrations      []Integration      `yaml:"integrations,omitempty"`
}

// Integration is a tool the team uses outside the repo, in a Profile.
type Integration struct {
	Tool       string                `yaml:"tool" json:"tool"`                             // jira, confluence, ..., or mcp:<name>
	Instance   string                `yaml:"instance,omitempty" json:"instance,omitempty"` // host or organization, never credentials
	Uses       []string              `yaml:"uses" json:"uses"`                             // knowledge, tracker, notifications, monitoring, catalog
	Evidence   []IntegrationEvidence `yaml:"evidence" json:"evidence"`
	Confidence string                `yaml:"confidence" json:"confidence"` // low, medium or high
	// Mapping maps knowledge sections to the tool's locations.
	Mapping map[string][]string `yaml:"mapping,omitempty" json:"mapping,omitempty"`
	Status  string              `yaml:"status" json:"status,omitempty"` // proposed, approved or connected
}

// IntegrationEvidence is where onboarding saw an integration.
type IntegrationEvidence struct {
	Kind     string `yaml:"kind" json:"kind"` // mcp_config, link, autolink, file, ci_step, issue_key
	Location string `yaml:"location" json:"location"`
	Count    int    `yaml:"count" json:"count"`
}

// ProfileComponent is a component entry of a Profile.
type ProfileComponent struct {
	Path      string   `yaml:"path"`
	Stack     string   `yaml:"stack"`
	Manifests []string `yaml:"manifests,omitempty"`
	Build     string   `yaml:"build,omitempty"`
	Test      string   `yaml:"test,omitempty"`
	Platforms []string `yaml:"platforms,omitempty"`
	Needs     []string `yaml:"needs,omitempty"`
}

// ProfileService is a service entry of a Profile.
type ProfileService struct {
	Name  string `yaml:"name"`
	Image string `yaml:"image,omitempty"`
	File  string `yaml:"file,omitempty"`
}

// ProfileCI describes CI in a Profile.
type ProfileCI struct {
	Workflows    []string `yaml:"workflows,omitempty"`
	Runners      []string `yaml:"runners,omitempty"`
	GatingChecks []string `yaml:"gating_checks,omitempty"`
}

// ProfileRole is a role adjustment in a Profile.
type ProfileRole struct {
	Role       string   `yaml:"role"`
	WritePaths []string `yaml:"write_paths,omitempty"`
	Tools      []string `yaml:"tools,omitempty"`
	Tier       string   `yaml:"tier,omitempty"`
}

// toolchains are the toolchain worker labels, which equal the stack names
// that have one.
var toolchains = []string{"go", "node", "rust", "python", "java", "docker"}

// DraftProfile builds a baseline profile from repo facts. It uses only the
// facts: the onboarding analyst refines it and asks the human about the
// open questions it lists.
func DraftProfile(f Facts) Profile {
	platforms := osLabels(f.CIRunners)
	p := Profile{
		Version:           1,
		Name:              f.Name,
		Summary:           summary(f),
		Platforms:         platforms,
		AgentInstructions: agentInstructions(f.AgentFiles),
		OpenQuestions:     openQuestions(f, platforms),
	}
	for _, l := range f.Languages {
		p.Languages = append(p.Languages, l.Name)
	}
	for _, c := range f.Components {
		p.Components = append(p.Components, ProfileComponent{
			Path:      c.Path,
			Stack:     c.Stack,
			Manifests: c.Manifests,
			Build:     c.Build,
			Test:      c.Test,
			Platforms: platforms,
			Needs:     needsOf(c.Manifests),
		})
	}
	for _, s := range f.Services {
		p.Services = append(p.Services, ProfileService{Name: s.Name, Image: s.Image, File: s.File})
	}
	if len(f.CIFiles) > 0 {
		p.CI = ProfileCI{Workflows: f.CIFiles, Runners: f.CIRunners}
	}
	return p
}

// summary describes the repo in one line from its facts.
func summary(f Facts) string {
	langs := "none detected"
	if len(f.Languages) > 0 {
		names := make([]string, 0, len(f.Languages))
		for _, l := range f.Languages {
			names = append(names, l.Name)
		}
		langs = strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s has %d component(s). Languages: %s.", f.Name, len(f.Components), langs)
}

// agentInstructions points agents at the instruction files the repo has.
func agentInstructions(files []string) string {
	if len(files) == 0 {
		return ""
	}
	return "Follow the instructions in " + strings.Join(files, ", ") + "."
}

// openQuestions lists what the facts leave unanswered.
func openQuestions(f Facts, platforms []string) []string {
	var qs []string
	if len(f.Components) == 0 {
		qs = append(qs, "Which commands build and test this project? No build file was found.")
	}
	for _, c := range f.Components {
		if c.Build == "" || c.Test == "" {
			qs = append(qs, fmt.Sprintf("What are the build and test commands of %s?", c.Path))
		}
	}
	if len(f.CIFiles) == 0 {
		qs = append(qs, "Which checks must pass before a merge? The repo has no CI workflow.")
	} else {
		qs = append(qs, "Which CI checks must pass before a merge?")
	}
	if len(platforms) == 0 {
		qs = append(qs, "Which operating systems do jobs need?")
	}
	if len(f.AgentFiles) == 0 {
		qs = append(qs, "Do agents need repo-specific instructions?")
	}
	return qs
}

// osLabels maps runner values to OS worker labels (linux, macos, windows),
// unique and sorted. Other runners, such as self-hosted, map to nothing.
func osLabels(runners []string) []string {
	var out []string
	for _, r := range runners {
		r = strings.ToLower(r)
		switch {
		case strings.Contains(r, "ubuntu"), strings.Contains(r, "linux"):
			out = append(out, "linux")
		case strings.Contains(r, "macos"):
			out = append(out, "macos")
		case strings.Contains(r, "windows"):
			out = append(out, "windows")
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// needsOf returns the toolchain labels the build files in manifests need,
// sorted and unique.
func needsOf(manifests []string) []string {
	var out []string
	for _, m := range manifests {
		if i := buildFileIndex(path.Base(m)); i >= 0 {
			if stack := buildFiles[i].stack; slices.Contains(toolchains, stack) {
				out = append(out, stack)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Validate checks that data, the YAML of a .forecrew/profile.yaml, follows
// profile.schema.json. Errors wrap ErrInvalidProfile.
func Validate(data []byte) error {
	_, inst, err := schema.DecodeYAML(data)
	if err == nil {
		err = schema.Check("profile.schema.json", profileSchema, inst)
	}
	if errors.Is(err, schema.ErrInvalid) {
		return fmt.Errorf("%w: %w", ErrInvalidProfile, err)
	}
	return err
}
