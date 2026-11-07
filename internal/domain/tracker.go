package domain

import (
	"fmt"
	"regexp"
	"slices"
	"time"
	"uuid"
)

// TicketState is a logical state of a ticket's work, the same on every
// tracker (docs/contracts/tracker.md). A route maps each to a state of its
// tracker.
type TicketState string

// Logical ticket states.
const (
	TicketQueued     TicketState = "queued"
	TicketInProgress TicketState = "in_progress"
	TicketInReview   TicketState = "in_review"
	TicketDone       TicketState = "done"
	TicketCanceled   TicketState = "canceled"
)

// TicketStates returns every logical ticket state.
func TicketStates() []TicketState {
	return []TicketState{TicketQueued, TicketInProgress, TicketInReview, TicketDone, TicketCanceled}
}

// TicketLinkStatus is where a ticket stands with Forecrew.
type TicketLinkStatus string

// Ticket link states. Waiting, queued and active are live: a ticket has at
// most one live link, so at most one goal at work.
const (
	LinkWaiting   TicketLinkStatus = "waiting"   // in the Inbox, for a person to accept
	LinkQueued    TicketLinkStatus = "queued"    // accepted, waiting under trackers.max_active_goals
	LinkActive    TicketLinkStatus = "active"    // a goal works on it
	LinkDone      TicketLinkStatus = "done"      // its goal is done
	LinkCanceled  TicketLinkStatus = "canceled"  // its goal was canceled, or the ticket closed first
	LinkDismissed TicketLinkStatus = "dismissed" // a person turned it down in the Inbox
)

// TicketLinkStatuses returns every ticket link state.
func TicketLinkStatuses() []TicketLinkStatus {
	return []TicketLinkStatus{LinkWaiting, LinkQueued, LinkActive, LinkDone, LinkCanceled, LinkDismissed}
}

// Live reports whether the link still counts as the ticket's one link.
func (s TicketLinkStatus) Live() bool {
	return s == LinkWaiting || s == LinkQueued || s == LinkActive
}

// TicketWaitReason says why a ticket waits in the Inbox.
type TicketWaitReason string

// Reasons a ticket waits in the Inbox.
const (
	WaitNoRoute       TicketWaitReason = "no_route"       // no route matches
	WaitManyProjects  TicketWaitReason = "many_projects"  // routes to more than one project match
	WaitUnknownPerson TicketWaitReason = "unknown_person" // who handed it over maps to no Forecrew user
	WaitNotPermitted  TicketWaitReason = "not_permitted"  // that user may not create goals in the project
)

// TrackerTriggers are the handovers a connection listens for. Each set one
// is a way for a person to hand a ticket to Forecrew; a connection needs at
// least one, within the capabilities of its adapter.
type TrackerTriggers struct {
	Assignee string `json:"assignee,omitempty"` // assignment to this tracker user, the Forecrew user
	Label    string `json:"label,omitempty"`    // this label added
	State    string `json:"state,omitempty"`    // moved to this state or column
}

// Empty reports whether no trigger is set.
func (t TrackerTriggers) Empty() bool { return t == TrackerTriggers{} }

// trackerKindRE is what a tracker kind looks like. The list of kinds is open:
// a new tracker is one package.
var trackerKindRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// TrackerConnection is an organization's link to one tracker. Config holds
// the adapter's settings and never a secret.
type TrackerConnection struct {
	ID        uuid.UUID       `json:"id"`
	AccountID uuid.UUID       `json:"account_id"`
	Name      string          `json:"name"` // a ValidName, unique in the organization
	Kind      string          `json:"kind"` // the adapter, for example github
	Config    map[string]any  `json:"config"`
	Triggers  TrackerTriggers `json:"triggers"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Validate checks the connection's name, kind and triggers.
func (c TrackerConnection) Validate() error {
	switch {
	case !ValidName(c.Name):
		return fmt.Errorf("tracker connection name %q is not a valid name", c.Name)
	case !trackerKindRE.MatchString(c.Kind):
		return fmt.Errorf("tracker kind %q is not a valid kind", c.Kind)
	case c.Triggers.Empty():
		return fmt.Errorf("tracker connection %s needs a trigger: an assignee, a label or a state", c.Name)
	}
	return nil
}

// TrackerMatch says which tickets a route takes. Every field that is set must
// match; a ticket matches a list when it holds one of its values. The zero
// match takes every ticket.
type TrackerMatch struct {
	Project    string   `json:"project,omitempty"` // the tracker's project: a repo, a Jira key
	Types      []string `json:"types,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	Components []string `json:"components,omitempty"`
}

// TrackerRoute sends the tickets it matches to a Forecrew project.
type TrackerRoute struct {
	ID           uuid.UUID    `json:"id"`
	AccountID    uuid.UUID    `json:"account_id"`
	ConnectionID uuid.UUID    `json:"connection_id"`
	Position     int          `json:"position"` // routes are tried in order; 0 on Create appends
	Match        TrackerMatch `json:"match"`
	ProjectID    uuid.UUID    `json:"project_id"`
	// GoalKind is the kind of the goal; empty goes by the ticket's type: a
	// bug type gives bug, anything else feature.
	GoalKind GoalKind `json:"goal_kind,omitempty"`
	// Playbook runs the goal; empty is the project's goal playbook.
	Playbook string `json:"playbook,omitempty"`
	// States maps logical states to the tracker's. A state left out is not
	// written to the ticket.
	States map[TicketState]string `json:"states"`
	// Warning says why the tracker did not take the last state Forecrew
	// wrote: no transition reaches it, or one needs fields Forecrew does not
	// have. A state that goes through clears it.
	Warning   string    `json:"warning,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Validate checks the route's goal kind and state mapping.
func (r TrackerRoute) Validate() error {
	if r.GoalKind != "" && !slices.Contains(GoalKinds(), r.GoalKind) {
		return fmt.Errorf("goal kind %q is not %v", r.GoalKind, GoalKinds())
	}
	for s := range r.States {
		if !slices.Contains(TicketStates(), s) {
			return fmt.Errorf("state %q is not one of %v", s, TicketStates())
		}
	}
	return nil
}

// TrackerIdentity maps a person on a tracker to a Forecrew user.
type TrackerIdentity struct {
	ID           uuid.UUID `json:"id"`
	AccountID    uuid.UUID `json:"account_id"`
	ConnectionID uuid.UUID `json:"connection_id"`
	ExternalID   string    `json:"external_id"` // the tracker's stable user ID
	ExternalName string    `json:"external_name,omitempty"`
	UserID       uuid.UUID `json:"user_id"`
	CreatedBy    string    `json:"created_by"` // Actor.String()
	CreatedAt    time.Time `json:"created_at"`
}

// TicketLink is a ticket Forecrew was handed, from the Inbox to the end of
// its goal. Title and URL are kept for lists; Title is ticket text and
// untrusted.
type TicketLink struct {
	ID           uuid.UUID        `json:"id"`
	AccountID    uuid.UUID        `json:"account_id"`
	ConnectionID uuid.UUID        `json:"connection_id"`
	Key          string           `json:"key"` // the ticket's key on its tracker
	Title        string           `json:"title"`
	URL          string           `json:"url,omitempty"`
	Status       TicketLinkStatus `json:"status"`
	Reason       TicketWaitReason `json:"reason,omitempty"` // why it waits, while waiting
	ProjectID    uuid.UUID        `json:"project_id,omitzero"`
	RouteID      uuid.UUID        `json:"route_id,omitzero"`
	GoalID       uuid.UUID        `json:"goal_id,omitzero"`
	// HandedBy is who handed it over on the tracker, for people to read.
	HandedBy string `json:"handed_by,omitempty"`
	// UserID is the Forecrew user the goal starts as.
	UserID uuid.UUID `json:"user_id,omitzero"`
	// GateID is the ticket_intake gate of a waiting ticket with a project.
	GateID uuid.UUID `json:"gate_id,omitzero"`
	// ChangeGateID is the last ticket_intake gate about an edit of the ticket.
	ChangeGateID uuid.UUID `json:"change_gate_id,omitzero"`
	// State is the last logical state the ticket was given.
	State TicketState `json:"state,omitempty"`
	// ContentHash is of the title and body the goal started from.
	ContentHash string `json:"-"`
	// CommentCursor is the ID of the last comment read.
	CommentCursor string `json:"-"`
	// IssueNumber is set when the ticket is itself the goal's GitHub issue.
	IssueNumber int       `json:"issue_number,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// TrackerWriteKind is what an outbox row does to a ticket.
type TrackerWriteKind string

// Outbox write kinds.
const (
	WriteComment    TrackerWriteKind = "comment"    // Value is the comment's text
	WriteTransition TrackerWriteKind = "transition" // Value is a state of the tracker
)

// TrackerWrite is a write to a ticket waiting in the outbox. (LinkID,
// DedupeKey) is unique, so a fact is written once.
type TrackerWrite struct {
	ID           uuid.UUID        `json:"id"`
	AccountID    uuid.UUID        `json:"account_id"`
	ConnectionID uuid.UUID        `json:"connection_id"`
	LinkID       uuid.UUID        `json:"link_id"`
	Key          string           `json:"key"` // the ticket
	DedupeKey    string           `json:"dedupe_key"`
	Kind         TrackerWriteKind `json:"kind"`
	Value        string           `json:"value"`
	Attempts     int              `json:"attempts"`
	NextAttempt  time.Time        `json:"next_attempt_at"`
	SentAt       time.Time        `json:"sent_at,omitzero"`
	// ExternalID is the tracker's ID of a comment that was sent, which tells
	// Forecrew's own comments from people's.
	ExternalID string    `json:"external_id,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}
