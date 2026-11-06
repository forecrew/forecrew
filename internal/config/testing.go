package config

import "strings"

// Testing switches projects to the goal playbook with the testing system
// (goal-v2: tests from the spec before the code, a matrix on every PR head
// and a verification report that gates the merge). It is off for every
// project until it is listed.
type Testing struct {
	// Projects are the names of the projects whose new goals run goal-v2.
	// Goals that already run keep their playbook.
	Projects []string `yaml:"projects"`
}

func (c *Config) validateTesting(p *problems) {
	seen := map[string]bool{}
	for _, name := range c.Testing.Projects {
		switch {
		case strings.TrimSpace(name) == "" || name != strings.TrimSpace(name):
			p.addf("testing.projects: %q must be a project name without surrounding spaces", name)
		case seen[name]:
			p.addf("testing.projects: %q is listed twice", name)
		}
		seen[name] = true
	}
}
