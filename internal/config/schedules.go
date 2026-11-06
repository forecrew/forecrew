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
	// that project in every organization that has it.
	Project string `yaml:"project"`
	// Cron is a five-field cron expression in UTC (internal/cron).
	Cron string `yaml:"cron"`
}

func (c *Config) applyKnowledgeDefaults() {
	if c.Knowledge.VerifyWindow == 0 {
		c.Knowledge.VerifyWindow = DefaultVerifyWindow
	}
}

func (c *Config) validateSchedules(p *problems) {
	if c.Knowledge.VerifyWindow < 0 {
		p.addf("knowledge.verify_window must be positive")
	}
	for i, s := range c.Schedules {
		if !domain.ValidName(s.Playbook) {
			p.addf("schedules[%d].playbook %q is not a playbook name", i, s.Playbook)
		}
		if owner, name, ok := strings.Cut(s.Project, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			p.addf("schedules[%d].project %q must be a repo, owner/name", i, s.Project)
		}
		if _, err := cron.Parse(s.Cron); err != nil {
			p.addf("schedules[%d].cron: %v", i, err)
		}
	}
}

// String shows a schedule in messages.
func (s Schedule) String() string {
	return fmt.Sprintf("%s on %s at %q", s.Playbook, s.Project, s.Cron)
}
