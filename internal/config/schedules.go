package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/cron"
	"github.com/forecrew/forecrew/internal/domain"
)

// DefaultVerifyWindow is how old an entry's last_verified may be before the
// curator looks at it again: 180 days.
const DefaultVerifyWindow = 180 * 24 * time.Hour

// Knowledge holds the settings of the knowledge base's upkeep.
type Knowledge struct {
	// VerifyWindow is the age after which the curator re-verifies an entry
	// that no change flagged, and the age after which an unread project note
	// is proposed for promotion or deleted. Zero means DefaultVerifyWindow.
	VerifyWindow time.Duration `yaml:"verify_window"`
}

// Schedule starts a playbook on a cron schedule for the project of a repo.
type Schedule struct {
	// Playbook is the playbook to start; it takes the input repo only.
	Playbook string `yaml:"playbook"`
	// Project is the repo of the project, owner/name: the schedule runs for
	// that project in every organization that has it. "*" is every project.
	Project string `yaml:"project"`
	// Cron is a five-field cron expression in UTC (internal/cron).
	Cron string `yaml:"cron"`
}

// DefaultMaxProposalsPerWeek is how many new improvement proposals a project
// gets in seven days.
const DefaultMaxProposalsPerWeek = 3

// Advisor holds the settings of the improvement advisor.
type Advisor struct {
	// MaxPerWeek is how many new proposals a project gets in seven days;
	// zero means DefaultMaxProposalsPerWeek.
	MaxPerWeek int `yaml:"max_per_week"`
}

// DefaultSchedules are the schedules of a forecrew.yaml that has no
// schedules key: the curator's upkeep and the advisor, weekly, for every
// onboarded project. An explicit empty list, schedules: [], turns them off.
func DefaultSchedules() []Schedule {
	return []Schedule{
		{Playbook: "curate", Project: AllProjects, Cron: "0 3 * * 1"},
	}
}

// AllProjects as a schedule's project means every onboarded project.
const AllProjects = "*"

func (c *Config) applyKnowledgeDefaults() {
	if c.Schedules == nil {
		c.Schedules = DefaultSchedules()
	}
	if c.Knowledge.VerifyWindow == 0 {
		c.Knowledge.VerifyWindow = DefaultVerifyWindow
	}
	if c.Advisor.MaxPerWeek == 0 {
		c.Advisor.MaxPerWeek = DefaultMaxProposalsPerWeek
	}
}

func (c *Config) validateSchedules(p *problems) {
	if c.Knowledge.VerifyWindow < 0 {
		p.addf("knowledge.verify_window must be positive")
	}
	if c.Advisor.MaxPerWeek < 0 {
		p.addf("advisor.max_per_week must be positive")
	}
	for i, s := range c.Schedules {
		if !domain.ValidName(s.Playbook) {
			p.addf("schedules[%d].playbook %q is not a playbook name", i, s.Playbook)
		}
		if owner, name, ok := strings.Cut(s.Project, "/"); s.Project != AllProjects && (!ok || owner == "" || name == "" || strings.Contains(name, "/")) {
			p.addf("schedules[%d].project %q must be a repo, owner/name, or %s", i, s.Project, AllProjects)
		}
		parsed, err := cron.Parse(s.Cron)
		if err != nil {
			p.addf("schedules[%d].cron: %v", i, err)
		} else if _, ok := parsed.Next(time.Now()); !ok {
			p.addf("schedules[%d].cron %q never fires", i, s.Cron)
		}
	}
}

// String shows a schedule in messages.
func (s Schedule) String() string {
	return fmt.Sprintf("%s on %s at %q", s.Playbook, s.Project, s.Cron)
}
