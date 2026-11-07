// Package tracker turns tickets of the tracker a team already uses into goals
// and sends their status back (docs/contracts/tracker.md). This is the core:
// it knows no tracker. Each tracker is one package that implements Adapter and
// passes the conformance suite in trackertest; the core branches on
// Capabilities, never on Kind.
package tracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

// Errors of adapters and of the registry.
var (
	// ErrNotFound means the ticket does not exist, or no longer does.
	ErrNotFound = errors.New("tracker: ticket not found")
	// ErrBadSignature means a webhook delivery failed its signature check.
	ErrBadSignature = errors.New("tracker: bad webhook signature")
	// ErrUnknownKind means no adapter is registered for a connection's kind.
	ErrUnknownKind = errors.New("tracker: unknown kind")
	// ErrInvalid means a connection's config or triggers cannot work.
	ErrInvalid = errors.New("tracker: invalid connection")
	// ErrNotWaiting means a ticket is not in the Inbox any more.
	ErrNotWaiting = errors.New("tracker: ticket is not waiting")
	// ErrBlocked means the tracker will not take a state: no transition
	// reaches it, or one needs fields Forecrew does not have. The core says
	// so on the ticket and the route, and does not try again.
	ErrBlocked = errors.New("tracker: transition blocked")
)

// Ref names a ticket.
type Ref struct {
	Connection uuid.UUID
	Key        string // the tracker's key: acme/app#12, ENG-7
}

// User is a person on a tracker. ID is stable: never a name or an email that
// can change.
type User struct {
	ID   string
	Name string
	Bot  bool
}

// Trigger is a way to hand a ticket over.
type Trigger string

// Handover triggers.
const (
	TriggerAssignee Trigger = "assignee"
	TriggerLabel    Trigger = "label"
	TriggerState    Trigger = "state"
)

// Handover is a person giving a ticket a trigger's value. By is empty when
// the tracker cannot say who did it.
type Handover struct {
	Trigger Trigger
	Value   string // the user, label or state it was set to
	By      User
	At      time.Time
}

// Comment is a comment on a ticket.
type Comment struct {
	ID     string
	Author User
	Body   string
	At     time.Time
}

// RepoIssue is the GitHub issue a ticket is.
type RepoIssue struct {
	Repo   string
	Number int
}

// Ticket is a ticket in neutral form: adapters convert to and from their own
// formats. Everything in it is untrusted text.
type Ticket struct {
	Ref        Ref
	Project    string // the tracker's project: a repo, a Jira key
	Title      string
	Body       string // Markdown
	Type       string
	Labels     []string
	Components []string
	Assignee   User
	Reporter   User
	State      string // a name of States
	Closed     bool   // the tracker counts it as finished, done or canceled
	URL        string
	Parent     string
	Updated    time.Time
	Handovers  []Handover // oldest first
	Comments   []Comment  // the latest ones, oldest first
	Issue      *RepoIssue // nil unless the ticket is a GitHub issue
}

// Capabilities says what a tracker can do.
type Capabilities struct {
	Webhooks    bool // it sends events
	Assignment  bool // a ticket can be assigned to a bot user
	Labels      bool
	Columns     bool // a state or column can hand a ticket over
	Transitions bool // Forecrew can write a state
	Comments    bool
}

// State is a state or column of a tracker.
type State struct {
	Name   string
	Closed bool
}

// Request is a webhook delivery.
type Request struct {
	Header http.Header
	// Query is the query of the URL it came to, for a tracker that cannot
	// sign its deliveries and is given a secret URL instead.
	Query url.Values
	Body  []byte
}

// Event says a ticket changed. It is a hint: the core reads the ticket with
// Get, and believes nothing else about it.
type Event struct {
	ID   string // the delivery, for deduplication; empty for polling
	Key  string
	Self bool // Forecrew's own write, as far as the tracker says
}

// Adapter is one connection to a tracker.
type Adapter interface {
	Kind() string
	Capabilities() Capabilities
	// Verify reports whether the connection can work; it runs when the
	// connection is created.
	Verify(ctx context.Context) error
	// ParseEvent checks the delivery's signature, wrapping ErrBadSignature,
	// and returns the tickets it is about. A delivery it does not use has no
	// events.
	ParseEvent(r Request) ([]Event, error)
	// Changes returns the tickets changed since cursor and the next cursor.
	// An empty cursor returns no events and a cursor of now, so a new
	// connection does not read the backlog.
	Changes(ctx context.Context, cursor string) (events []Event, next string, err error)
	// Get reads a ticket, wrapping ErrNotFound when it is gone.
	Get(ctx context.Context, key string) (Ticket, error)
	// Comment posts a comment and returns its ID.
	Comment(ctx context.Context, key, body string) (id string, err error)
	// Transition moves a ticket to a state of States.
	Transition(ctx context.Context, key, state string) error
	States(ctx context.Context) ([]State, error)
}

// Linker is an optional part of an Adapter: it finds the Forecrew user of a
// tracker user through an identity the organization already has.
type Linker interface {
	LinkedUser(ctx context.Context, accountID uuid.UUID, u User) (uuid.UUID, bool, error)
}

// Preparer is an optional part of an Adapter: it reads what the tracker says
// of itself when a connection is set up, such as its version, and returns the
// config to store, with the keys of Derived filled in.
type Preparer interface {
	Prepare(ctx context.Context) (map[string]any, error)
	// Derived lists the config keys Prepare fills in: a person never sets
	// them.
	Derived() []string
}

// Check is one result of a connection test.
type Check struct {
	Name   string `json:"name"` // for example address, certificate, login
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Checker is an optional part of an Adapter: a connection test that reports
// each thing that can fail on its own.
type Checker interface {
	Check(ctx context.Context) []Check
}

// Test runs the connection test of ad: its checks, or Verify as one.
func Test(ctx context.Context, ad Adapter) []Check {
	if c, ok := ad.(Checker); ok {
		return c.Check(ctx)
	}
	if err := ad.Verify(ctx); err != nil {
		return []Check{{Name: "connection", Detail: err.Error()}}
	}
	return []Check{{Name: "connection", OK: true}}
}

// Failed returns an error for the checks that failed, nil when all passed.
func Failed(checks []Check) error {
	var errs []error
	for _, c := range checks {
		if !c.OK {
			errs = append(errs, fmt.Errorf("%s: %s", c.Name, c.Detail))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrInvalid, errors.Join(errs...))
}

// Factory builds the adapter of a connection.
type Factory func(domain.TrackerConnection) (Adapter, error)

// Registry holds the adapters by kind.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{factories: map[string]Factory{}} }

// Register adds the factory of a kind, replacing any earlier one.
func (r *Registry) Register(kind string, f Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[kind] = f
}

// Kinds returns the registered kinds, sorted.
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.factories))
	for k := range r.factories {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Build returns the adapter of a connection.
func (r *Registry) Build(c domain.TrackerConnection) (Adapter, error) {
	r.mu.RLock()
	f, ok := r.factories[c.Kind]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%q: %w", c.Kind, ErrUnknownKind)
	}
	return f(c)
}

// CheckTriggers reports a trigger of c the adapter cannot serve.
func CheckTriggers(c domain.TrackerConnection, caps Capabilities) error {
	t := c.Triggers
	switch {
	case t.Assignee != "" && !caps.Assignment:
		return fmt.Errorf("%s tickets cannot be assigned to a bot: %w", c.Kind, ErrInvalid)
	case t.Label != "" && !caps.Labels:
		return fmt.Errorf("%s tickets have no labels: %w", c.Kind, ErrInvalid)
	case t.State != "" && !caps.Columns:
		return fmt.Errorf("%s tickets cannot be handed over by a state: %w", c.Kind, ErrInvalid)
	}
	return nil
}

// Bounds of what an adapter returns; everything in a ticket comes from
// outside.
const (
	maxTitle    = 200
	maxBody     = 64 << 10
	maxComment  = 16 << 10
	maxComments = 100
	maxLabels   = 100
)

// clip cuts s to at most limit bytes, on a rune boundary.
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return strings.ToValidUTF8(s[:limit], "")
}

// bound keeps t within the bounds.
func bound(t Ticket) Ticket {
	t.Title = clip(strings.Join(strings.Fields(t.Title), " "), maxTitle)
	t.Body = clip(t.Body, maxBody)
	t.Labels = t.Labels[:min(len(t.Labels), maxLabels)]
	t.Comments = t.Comments[max(0, len(t.Comments)-maxComments):]
	t.Comments = slices.Clone(t.Comments)
	for i := range t.Comments {
		t.Comments[i].Body = clip(t.Comments[i].Body, maxComment)
	}
	return t
}

// fold reports whether list holds v, ignoring case.
func fold(list []string, v string) bool {
	return slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, v) })
}

// matches reports whether a route takes t.
func matches(m domain.TrackerMatch, t Ticket) bool {
	anyOf := func(want, have []string) bool {
		return len(want) == 0 || slices.ContainsFunc(have, func(v string) bool { return fold(want, v) })
	}
	return (m.Project == "" || strings.EqualFold(m.Project, t.Project)) &&
		anyOf(m.Types, []string{t.Type}) && anyOf(m.Labels, t.Labels) && anyOf(m.Components, t.Components)
}

// handover returns the newest handover of t that still holds: the ticket
// still has what the trigger set.
func handover(tr domain.TrackerTriggers, t Ticket) (Handover, bool) {
	for _, h := range slices.Backward(t.Handovers) {
		switch h.Trigger {
		case TriggerAssignee:
			if tr.Assignee != "" && strings.EqualFold(h.Value, tr.Assignee) &&
				(strings.EqualFold(t.Assignee.Name, tr.Assignee) || strings.EqualFold(t.Assignee.ID, tr.Assignee)) {
				return h, true
			}
		case TriggerLabel:
			if tr.Label != "" && strings.EqualFold(h.Value, tr.Label) && fold(t.Labels, tr.Label) {
				return h, true
			}
		case TriggerState:
			if tr.State != "" && strings.EqualFold(h.Value, tr.State) && strings.EqualFold(t.State, tr.State) {
				return h, true
			}
		}
	}
	return Handover{}, false
}
