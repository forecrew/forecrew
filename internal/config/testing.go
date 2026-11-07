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

// GoalV3 switches projects to the goal playbook with councils and learning
// (goal-v3: goal-v2 with triage, the product and technical councils and the
// learn step). It is off for every project until it is listed; a project
// listed here and in Testing runs goal-v3.
type GoalV3 struct {
	// Projects are the names of the projects whose new goals run goal-v3.
	// Goals that already run keep their playbook.
	Projects []string `yaml:"projects"`
}

func (c *Config) validateTesting(p *problems) {
	validateProjectNames(p, "testing.projects", c.Testing.Projects)
	validateProjectNames(p, "goal_v3.projects", c.GoalV3.Projects)
}

// validateProjectNames checks a list of project names called key.
func validateProjectNames(p *problems, key string, names []string) {
	seen := map[string]bool{}
	for _, name := range names {
		switch {
		case strings.TrimSpace(name) == "" || name != strings.TrimSpace(name):
			p.addf("%s: %q must be a project name without surrounding spaces", key, name)
		case seen[name]:
			p.addf("%s: %q is listed twice", key, name)
		}
		seen[name] = true
	}
}
