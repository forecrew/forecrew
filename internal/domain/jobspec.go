package domain

import (
	"regexp"
	"slices"
	"strings"
)

// providerModelRE is the provider/model form of an opencode model. Braces
// are refused because opencode expands {env:...} and {file:...} in the inline
// configuration the name ends up in.
var providerModelRE = regexp.MustCompile(`^([a-zA-Z0-9][a-zA-Z0-9._-]*)/([^\s{}]+)$`)

// SplitProviderModel splits a model in provider/model form, for example
// deepseek/deepseek-chat. The model part may hold further slashes.
func SplitProviderModel(s string) (provider, model string, ok bool) {
	m := providerModelRE.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// Validate checks that the spec fits its kind (docs/contracts/job-spec.md):
// an agent job has a runtime, model and prompt and none of the command
// fields; a command job has a command and no prompt, model, runtime,
// write paths or toolbox. It reports every problem at once, wrapping
// ErrInvalidSpec. Secrets (Env, RepoToken) are not checked.
func (s JobSpec) Validate() error {
	var p problems
	switch s.Kind.OrDefault() {
	case JobKindAgent:
		s.checkAgent(&p)
	case JobKindCommand:
		s.checkCommand(&p)
	case JobKindModelCheck:
		if s.ModelCheck == nil || s.ModelCheck.Connection == "" || s.Prompt != "" || s.Command != nil || len(s.WritePaths) > 0 || s.Toolbox != nil {
			p.addf("model_check job needs model_check with a connection, and has no prompt, command, write_paths or toolbox")
		}
	default:
		p.addf("kind must be agent, command or model_check, got %q", s.Kind)
	}
	return p.err("job spec")
}

func (s JobSpec) checkAgent(p *problems) {
	if s.Runtime == "" || s.Model == "" || s.Prompt == "" {
		p.addf("agent job needs runtime, model and prompt")
	}
	if s.Command != nil || s.Environment != nil || len(s.Cell) > 0 || s.Suite != "" || s.Overlay != nil {
		p.addf("command, environment, cell, suite and overlay are only for command jobs")
	}
	for _, l := range s.LockedPaths {
		if strings.HasPrefix(l, "!") || !RelativePath(l) {
			p.addf("locked_paths: %q must be a glob relative to the repo root, without !", l)
		}
	}
	if s.Toolbox != nil {
		if err := s.Toolbox.Validate(); err != nil {
			p.addf("toolbox: %v", err)
		}
	}
}

func (s JobSpec) checkCommand(p *problems) {
	if s.Runtime != "" || s.Model != "" || s.Prompt != "" || len(s.OutputSchema) > 0 ||
		len(s.AllowedTools) > 0 || s.MaxTurns != 0 || s.IncludeRepoFacts {
		p.addf("command job has no runtime, model, prompt, output_schema, allowed_tools, max_turns or include_repo_facts")
	}
	if len(s.WritePaths) > 0 || len(s.LockedPaths) > 0 || s.Toolbox != nil {
		p.addf("command job has no write_paths, locked_paths or toolbox: it never commits")
	}
	c := s.Command
	if c == nil {
		p.addf("command must be set")
		return
	}
	hasArgv := len(c.Argv) > 0 && c.Argv[0] != ""
	if hasArgv == (c.Shell != "") || (len(c.Argv) > 0 && !hasArgv) {
		p.addf("command needs exactly one of argv and shell")
	}
	p.checkEnv("command.env", c.Env)
	if c.Workdir != "" {
		p.checkPaths("command.workdir", []string{c.Workdir})
	}
	p.checkPaths("command.reports", c.Reports)
	p.checkPaths("command.artifacts", c.Artifacts)
	if o := s.Overlay; o != nil {
		if o.Ref == "" || len(o.Paths) == 0 {
			p.addf("overlay needs ref and paths")
		}
		p.checkPaths("overlay.paths", o.Paths)
	}
	if s.Suite != "" && !ValidName(s.Suite) {
		p.addf("suite %q must match %s", s.Suite, nameRE)
	}
	if slices.Contains(sortedKeys(s.Cell), "") {
		p.addf("cell: empty axis name")
	}
	if s.Environment != nil {
		if err := s.Environment.Validate(); err != nil {
			p.addf("environment: %v", err)
		}
	}
}
