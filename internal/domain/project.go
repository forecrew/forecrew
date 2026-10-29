package domain

import (
	"encoding/json"
	"time"
	"uuid"
)

// ProjectStatus is the onboarding state of a project.
type ProjectStatus string

// Project states.
const (
	ProjectRegistered ProjectStatus = "registered"
	ProjectOnboarding ProjectStatus = "onboarding"
	ProjectOnboarded  ProjectStatus = "onboarded"
)

// ProjectStatuses returns every project state.
func ProjectStatuses() []ProjectStatus {
	return []ProjectStatus{ProjectRegistered, ProjectOnboarding, ProjectOnboarded}
}

var projectTransitions = transitions[ProjectStatus]{
	ProjectRegistered: {ProjectOnboarding},
	ProjectOnboarding: {ProjectOnboarded, ProjectRegistered}, // back to registered when rejected or failed
}

// CanTransition reports whether a project may move from s to to.
func (s ProjectStatus) CanTransition(to ProjectStatus) bool {
	return projectTransitions.can(s, to)
}

// Transition returns to, or an error wrapping ErrInvalidTransition.
func (s ProjectStatus) Transition(to ProjectStatus) (ProjectStatus, error) {
	return projectTransitions.apply("project", s, to)
}

// Project is a GitHub repository managed by Forecrew.
type Project struct {
	ID            uuid.UUID     `json:"id"`
	AccountID     uuid.UUID     `json:"account_id"`
	Name          string        `json:"name"`           // unique per account, defaults to the repo name
	Repo          string        `json:"repo"`           // "owner/name", unique per account
	DefaultBranch string        `json:"default_branch"` // e.g. "main"
	Status        ProjectStatus `json:"status"`
	// Profile is the approved .forecrew/profile.yaml as JSON
	// (docs/contracts/profile.schema.json); empty until onboarded.
	Profile   json.RawMessage `json:"profile,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}
