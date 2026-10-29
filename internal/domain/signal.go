package domain

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"uuid"
)

// SignalKind is the type of a fact the workflow engine reacts to.
type SignalKind string

// Signal kinds.
const (
	SignalJobCompleted    SignalKind = "job.completed"
	SignalGateResolved    SignalKind = "gate.resolved"
	SignalPRMerged        SignalKind = "pr.merged"
	SignalPRClosed        SignalKind = "pr.closed"
	SignalChecksCompleted SignalKind = "checks.completed"
)

// SignalKinds returns every signal kind.
func SignalKinds() []SignalKind {
	return []SignalKind{SignalJobCompleted, SignalGateResolved, SignalPRMerged, SignalPRClosed, SignalChecksCompleted}
}

// Signal is a stored fact. Key correlates it with the step waiting for it;
// DedupeKey is unique, so the same fact reported twice (webhook and poll) is
// stored once.
type Signal struct {
	ID        uuid.UUID       `json:"id"`
	AccountID uuid.UUID       `json:"account_id"`
	ProjectID uuid.UUID       `json:"project_id"`
	Kind      SignalKind      `json:"kind"`
	Key       string          `json:"key"`
	DedupeKey string          `json:"dedupe_key"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

// SignalPayload is implemented by the payload type of each signal kind.
type SignalPayload interface {
	SignalKind() SignalKind
	// SignalKey is the correlation key, e.g. "job:<id>".
	SignalKey() string
	validate() error
}

// NewSignal builds a signal with a new ID, its key, dedupe key and JSON payload.
func NewSignal(accountID, projectID uuid.UUID, p SignalPayload, now time.Time) (Signal, error) {
	if err := p.validate(); err != nil {
		return Signal{}, fmt.Errorf("%s: %w", p.SignalKind(), err)
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return Signal{}, fmt.Errorf("marshal %s: %w", p.SignalKind(), err)
	}
	return Signal{
		ID:        NewID(),
		AccountID: accountID,
		ProjectID: projectID,
		Kind:      p.SignalKind(),
		Key:       p.SignalKey(),
		DedupeKey: DedupeKey(p),
		Payload:   payload,
		CreatedAt: now.UTC(),
	}, nil
}

// DedupeKey returns the semantic dedupe key of a payload: "<kind>:<key body>",
// e.g. "pr.merged:owner/repo#12" or "checks.completed:owner/repo@<sha>".
func DedupeKey(p SignalPayload) string {
	_, body, _ := strings.Cut(p.SignalKey(), ":")
	return string(p.SignalKind()) + ":" + body
}

// Decode returns the typed payload of the signal.
func (s Signal) Decode() (SignalPayload, error) {
	var p SignalPayload
	switch s.Kind {
	case SignalJobCompleted:
		p = &JobCompleted{}
	case SignalGateResolved:
		p = &GateResolved{}
	case SignalPRMerged:
		p = &PRMerged{}
	case SignalPRClosed:
		p = &PRClosed{}
	case SignalChecksCompleted:
		p = &ChecksCompleted{}
	default:
		return nil, fmt.Errorf("kind %q: %w", s.Kind, ErrInvalidSignal)
	}
	if err := json.Unmarshal(s.Payload, p); err != nil {
		return nil, fmt.Errorf("decode %s: %w: %w", s.Kind, ErrInvalidSignal, err)
	}
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", s.Kind, err)
	}
	return p, nil
}

// JobKey, GateKey, PRKey and ChecksKey build correlation keys. Repos are
// lowercased because GitHub repo names are case-insensitive.

// JobKey returns "job:<id>".
func JobKey(jobID uuid.UUID) string { return "job:" + jobID.String() }

// GateKey returns "gate:<id>".
func GateKey(gateID uuid.UUID) string { return "gate:" + gateID.String() }

// PRKey returns "pr:<repo>#<number>".
func PRKey(repo string, number int) string {
	return "pr:" + strings.ToLower(repo) + "#" + strconv.Itoa(number)
}

// ChecksKey returns "checks:<repo>@<sha>".
func ChecksKey(repo, sha string) string {
	return "checks:" + strings.ToLower(repo) + "@" + strings.ToLower(sha)
}

func missing(field string) error {
	return fmt.Errorf("missing %s: %w", field, ErrInvalidSignal)
}

// JobCompleted is the payload of job.completed, sent when a job reaches a
// terminal state.
type JobCompleted struct {
	JobID  uuid.UUID `json:"job_id"`
	Status JobStatus `json:"status"` // succeeded, failed or canceled
}

// SignalKind implements SignalPayload.
func (JobCompleted) SignalKind() SignalKind { return SignalJobCompleted }

// SignalKey implements SignalPayload.
func (p JobCompleted) SignalKey() string { return JobKey(p.JobID) }

func (p JobCompleted) validate() error {
	if p.JobID == uuid.Nil() {
		return missing("job_id")
	}
	if !p.Status.Terminal() {
		return fmt.Errorf("status %q is not terminal: %w", p.Status, ErrInvalidSignal)
	}
	return nil
}

// GateResolved is the payload of gate.resolved.
type GateResolved struct {
	GateID   uuid.UUID  `json:"gate_id"`
	Decision GateStatus `json:"decision"` // approved, changes_requested or rejected
	Feedback string     `json:"feedback"`
}

// SignalKind implements SignalPayload.
func (GateResolved) SignalKind() SignalKind { return SignalGateResolved }

// SignalKey implements SignalPayload.
func (p GateResolved) SignalKey() string { return GateKey(p.GateID) }

func (p GateResolved) validate() error {
	if p.GateID == uuid.Nil() {
		return missing("gate_id")
	}
	if !GatePending.CanTransition(p.Decision) {
		return fmt.Errorf("decision %q: %w", p.Decision, ErrInvalidSignal)
	}
	return nil
}

// PRMerged is the payload of pr.merged.
type PRMerged struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	SHA    string `json:"sha"` // merge commit
}

// SignalKind implements SignalPayload.
func (PRMerged) SignalKind() SignalKind { return SignalPRMerged }

// SignalKey implements SignalPayload.
func (p PRMerged) SignalKey() string { return PRKey(p.Repo, p.Number) }

func (p PRMerged) validate() error {
	if err := validatePR(p.Repo, p.Number); err != nil {
		return err
	}
	if p.SHA == "" {
		return missing("sha")
	}
	return nil
}

// PRClosed is the payload of pr.closed, sent when a PR is closed without merging.
type PRClosed struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

// SignalKind implements SignalPayload.
func (PRClosed) SignalKind() SignalKind { return SignalPRClosed }

// SignalKey implements SignalPayload.
func (p PRClosed) SignalKey() string { return PRKey(p.Repo, p.Number) }

func (p PRClosed) validate() error { return validatePR(p.Repo, p.Number) }

func validatePR(repo string, number int) error {
	if repo == "" {
		return missing("repo")
	}
	if number < 1 {
		return missing("number")
	}
	return nil
}

// CheckConclusion is the aggregate result of a commit's checks.
type CheckConclusion string

// Check conclusions. None means the commit has no checks at all.
const (
	ChecksSuccess CheckConclusion = "success"
	ChecksFailure CheckConclusion = "failure"
	ChecksNone    CheckConclusion = "none"
)

// ChecksCompleted is the payload of checks.completed. The first one for a SHA
// is final for that SHA.
type ChecksCompleted struct {
	Repo       string          `json:"repo"`
	Number     int             `json:"number"` // PR number
	SHA        string          `json:"sha"`
	Conclusion CheckConclusion `json:"conclusion"`
}

// SignalKind implements SignalPayload.
func (ChecksCompleted) SignalKind() SignalKind { return SignalChecksCompleted }

// SignalKey implements SignalPayload.
func (p ChecksCompleted) SignalKey() string { return ChecksKey(p.Repo, p.SHA) }

func (p ChecksCompleted) validate() error {
	if err := validatePR(p.Repo, p.Number); err != nil {
		return err
	}
	if p.SHA == "" {
		return missing("sha")
	}
	switch p.Conclusion {
	case ChecksSuccess, ChecksFailure, ChecksNone:
		return nil
	}
	return fmt.Errorf("conclusion %q: %w", p.Conclusion, ErrInvalidSignal)
}
