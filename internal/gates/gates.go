// Package gates opens gates and records human decisions on them. Keeper is
// the implementation; internal/testkit re-exports it for tests.
package gates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
)

// ErrNotFound means the gate does not exist in the account.
var ErrNotFound = errors.New("gate not found")

// OpenRequest opens a gate. Its json keys are the `with:` keys of a
// playbook gate step; the engine sets AccountID, ProjectID and ID.
type OpenRequest struct {
	// ID, when set, is the gate's ID and makes Open idempotent: opening an
	// existing ID returns that gate.
	ID            uuid.UUID       `json:"-"`
	AccountID     uuid.UUID       `json:"-"`
	ProjectID     uuid.UUID       `json:"-"`
	Kind          domain.GateKind `json:"kind"`
	Title         string          `json:"title"`
	Summary       string          `json:"summary"`
	SubjectRepo   string          `json:"subject_repo"`
	SubjectNumber int             `json:"subject_number"`
	GoalID        uuid.UUID       `json:"goal_id,omitzero"`
	SpecID        uuid.UUID       `json:"spec_id,omitzero"`
	TaskID        uuid.UUID       `json:"task_id,omitzero"`
	JobID         uuid.UUID       `json:"job_id,omitzero"`
	// Origin is what opens the gate; playbooks leave it empty
	// (domain.GateFromPlaybook).
	Origin domain.GateOrigin `json:"-"`
	// Opened, when set, runs in the transaction that stores a new gate, so
	// what must refer to the gate does so before anyone can resolve it.
	Opened func(ctx context.Context, tx store.Store, g domain.Gate) error `json:"-"`
	// Before, when set, runs first in the transaction of Open, before the
	// gate counter is taken. A caller that locks a job row (Jobs().LockLeased)
	// does it here: every other writer takes the job row before the counter,
	// and the reverse order deadlocks. Its error aborts Open.
	Before func(ctx context.Context, tx store.Store) error `json:"-"`
}

// Service manages gates.
type Service interface {
	// Open stores a pending gate and writes an audit entry. An unknown kind
	// wraps mcp.ErrInvalidInput.
	Open(ctx context.Context, req OpenRequest) (domain.Gate, error)
	// Resolve records the decision of by (domain.Gate.Resolve), writes an
	// audit entry and publishes a gate.resolved signal. Errors wrap
	// domain.ErrInvalidDecision or domain.ErrInvalidTransition.
	Resolve(ctx context.Context, by domain.Actor, gateID uuid.UUID, decision domain.GateStatus, feedback string) (domain.Gate, error)
	// Decide resolves the gate named by in.Gate (Find) with the decision of
	// a gate tool (mcp.GateDecision): approve, request_changes or reject.
	Decide(ctx context.Context, by domain.Actor, tool string, in mcp.GateDecisionInput) (domain.Gate, error)
	Get(ctx context.Context, accountID, gateID uuid.UUID) (domain.Gate, error)
	// Find returns a gate by human ref (GATE-7) or UUID. An empty or
	// malformed ref wraps mcp.ErrInvalidInput.
	Find(ctx context.Context, accountID uuid.UUID, ref string) (domain.Gate, error)
	// ListPending returns the pending gates of a project, or of the whole
	// account when projectID is nil.
	ListPending(ctx context.Context, accountID, projectID uuid.UUID) ([]domain.Gate, error)
}

// Publisher publishes signals; workflow.SignalBus implements it.
type Publisher interface {
	Publish(ctx context.Context, s domain.Signal) (bool, error)
}

// Recorder is told of every gate a Keeper resolves, in the transaction that
// resolves it (the assumption and decision registers).
type Recorder interface {
	// GateResolved records what the resolution of g by by means. The
	// function it returns, if any, runs after the transaction has committed
	// and the gate.resolved signal is published; its error is returned by
	// Resolve, the gate being resolved all the same.
	GateResolved(ctx context.Context, tx store.Store, by domain.Actor, g domain.Gate) (func(ctx context.Context) error, error)
}

// LessonSaver turns the feedback of a resolved gate into a lesson: a
// proposal to the lessons section with the gate as its source.
type LessonSaver interface {
	SaveLesson(ctx context.Context, by domain.Actor, g domain.Gate, feedback string) error
}

var _ Service = (*Keeper)(nil)

// Keeper is the Service on top of a store and a signal publisher.
type Keeper struct {
	st  store.Store
	pub Publisher
	now func() time.Time
	rec Recorder
	les LessonSaver
}

// New returns a gate service. now defaults to the real UTC time.
func New(st store.Store, pub Publisher, now func() time.Time) *Keeper {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Keeper{st: st, pub: pub, now: now}
}

// WithRecorder makes the keeper tell r of every gate it resolves.
func (k *Keeper) WithRecorder(r Recorder) *Keeper {
	k.rec = r
	return k
}

// WithLessons makes the keeper save the feedback of gates decided with
// save_as_lesson.
func (k *Keeper) WithLessons(l LessonSaver) *Keeper {
	k.les = l
	return k
}

// Open implements Service.
func (k *Keeper) Open(ctx context.Context, req OpenRequest) (domain.Gate, error) {
	if !slices.Contains(domain.GateKinds(), req.Kind) {
		return domain.Gate{}, fmt.Errorf("open %s gate: unknown kind: %w", req.Kind, mcp.ErrInvalidInput)
	}
	g := domain.Gate{
		ID: req.ID, AccountID: req.AccountID, ProjectID: req.ProjectID, Kind: req.Kind, Status: domain.GatePending,
		Title: req.Title, Summary: req.Summary, SubjectRepo: req.SubjectRepo, SubjectNumber: req.SubjectNumber,
		GoalID: req.GoalID, SpecID: req.SpecID, TaskID: req.TaskID, JobID: req.JobID, Origin: req.Origin,
	}
	err := k.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if req.Before != nil {
			if err := req.Before(ctx, tx); err != nil {
				return err
			}
		}
		if existing, found, err := existingGate(ctx, tx, req); err != nil || found {
			if found {
				g = existing
			}
			return err
		}
		if err := tx.Gates().Create(ctx, &g); err != nil {
			return err
		}
		if req.Opened != nil {
			if err := req.Opened(ctx, tx, g); err != nil {
				return err
			}
		}
		return audit(ctx, tx, domain.SystemActor(g.AccountID, domain.ForecrewdName), "gate.opened", g, nil)
	})
	if err != nil {
		return domain.Gate{}, fmt.Errorf("open %s gate: %w", req.Kind, err)
	}
	return g, nil
}

// existingGate returns the gate of req.ID, which makes Open idempotent.
func existingGate(ctx context.Context, tx store.Store, req OpenRequest) (domain.Gate, bool, error) {
	if req.ID == uuid.Nil() {
		return domain.Gate{}, false, nil
	}
	g, err := tx.Gates().Get(ctx, req.AccountID, req.ID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.Gate{}, false, nil
	}
	return g, err == nil, err
}

// Resolve implements Service. The gate and its audit entry are written in
// one transaction; the signal is published after it commits.
func (k *Keeper) Resolve(ctx context.Context, by domain.Actor, gateID uuid.UUID, decision domain.GateStatus, feedback string) (domain.Gate, error) {
	now := k.now()
	var g domain.Gate
	var then func(context.Context) error
	err := k.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		var err error
		if g, err = get(ctx, tx, by.AccountID, gateID); err != nil {
			return err
		}
		if err := checkCouncilRound(ctx, tx, g, decision); err != nil {
			return err
		}
		if err := g.Resolve(by, decision, feedback, now); err != nil {
			return err
		}
		if err := tx.Gates().Resolve(ctx, &g); err != nil {
			return err
		}
		if err := audit(ctx, tx, by, "gate.resolved", g, map[string]string{"decision": string(decision), "feedback": feedback}); err != nil {
			return err
		}
		if k.rec == nil {
			return nil
		}
		if then, err = k.rec.GateResolved(ctx, tx, by, g); err != nil {
			return fmt.Errorf("record %s: %w", g.Ref(), err)
		}
		return nil
	})
	if err != nil {
		return domain.Gate{}, fmt.Errorf("resolve gate: %w", err)
	}
	sig, err := domain.NewSignal(g.AccountID, g.ProjectID, &domain.GateResolved{GateID: g.ID, Decision: decision, Feedback: feedback}, now)
	if err == nil {
		_, err = k.pub.Publish(ctx, sig)
	}
	if err != nil {
		return g, fmt.Errorf("publish %s resolution: %w", g.Ref(), err)
	}
	if then != nil {
		if err := then(ctx); err != nil {
			return g, fmt.Errorf("follow up %s: %w", g.Ref(), err)
		}
	}
	return g, nil
}

// checkCouncilRound refuses a request for changes on the gate of a council
// that had its human round (domain.MaxRevisionRounds): the gate of its second
// brief can only be approved or rejected.
func checkCouncilRound(ctx context.Context, tx store.Store, g domain.Gate, decision domain.GateStatus) error {
	if g.Kind != domain.GateCouncilDecision || decision != domain.GateChangesRequested {
		return nil
	}
	found, err := tx.Councils().List(ctx, g.AccountID, store.CouncilFilter{GateID: g.ID})
	if err != nil || len(found) == 0 {
		return err
	}
	briefs, err := tx.Councils().Contributions(ctx, g.AccountID, found[0].ID, domain.StageSynthesis)
	if err != nil {
		return err
	}
	if len(briefs) >= domain.MaxSynthesisRounds {
		return fmt.Errorf("%s had its human round: approve or reject its brief: %w", found[0].Ref(), mcp.ErrInvalidInput)
	}
	return nil
}

// Decide implements Service.
func (k *Keeper) Decide(ctx context.Context, by domain.Actor, tool string, in mcp.GateDecisionInput) (domain.Gate, error) {
	if in.Gate == "" {
		_, err := mcp.GateDecision(tool, "", in)
		return domain.Gate{}, err
	}
	g, err := k.Find(ctx, by.AccountID, in.Gate)
	if err != nil {
		return domain.Gate{}, err
	}
	decision, err := mcp.GateDecision(tool, g.Kind, in)
	if err != nil {
		return domain.Gate{}, err
	}
	if in.SaveAsLesson && k.les == nil {
		return domain.Gate{}, fmt.Errorf("%s: this forecrewd cannot save lessons: %w", tool, mcp.ErrInvalidInput)
	}
	g, err = k.Resolve(ctx, by, g.ID, decision, in.Feedback)
	if err != nil || !in.SaveAsLesson {
		return g, err
	}
	// The gate is resolved whatever happens to the lesson: say so.
	if err := k.les.SaveLesson(ctx, by, g, in.Feedback); err != nil {
		return g, fmt.Errorf("%s is %s, but its feedback was not saved as a lesson: %w", g.Ref(), g.Status, err)
	}
	return g, nil
}

// Get implements Service.
func (k *Keeper) Get(ctx context.Context, accountID, gateID uuid.UUID) (domain.Gate, error) {
	return get(ctx, k.st, accountID, gateID)
}

func get(ctx context.Context, st store.Store, accountID, gateID uuid.UUID) (domain.Gate, error) {
	g, err := st.Gates().Get(ctx, accountID, gateID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.Gate{}, fmt.Errorf("gate %s: %w", gateID, ErrNotFound)
	}
	return g, err
}

// Find implements Service.
func (k *Keeper) Find(ctx context.Context, accountID uuid.UUID, ref string) (domain.Gate, error) {
	number, id, err := domain.ParseRefOrID(domain.RefGate, ref)
	if err != nil {
		return domain.Gate{}, fmt.Errorf("gate %q is not a GATE ref or UUID: %w", ref, mcp.ErrInvalidInput)
	}
	if number == 0 {
		return k.Get(ctx, accountID, id)
	}
	g, err := k.st.Gates().GetByNumber(ctx, accountID, number)
	if errors.Is(err, store.ErrNotFound) {
		return domain.Gate{}, fmt.Errorf("gate %s: %w", ref, ErrNotFound)
	}
	return g, err
}

// ListPending implements Service.
func (k *Keeper) ListPending(ctx context.Context, accountID, projectID uuid.UUID) ([]domain.Gate, error) {
	return k.st.Gates().List(ctx, accountID, store.GateFilter{ProjectID: projectID, Status: domain.GatePending})
}

// audit appends an audit entry about g; data may be nil.
func audit(ctx context.Context, st store.Store, by domain.Actor, action string, g domain.Gate, data map[string]string) error {
	var raw json.RawMessage
	if data != nil {
		raw, _ = json.Marshal(data) // a map of strings always marshals
	}
	return st.Audit().Append(ctx, &domain.AuditEntry{
		AccountID: g.AccountID, ProjectID: g.ProjectID, Actor: by.String(), Credential: by.Credential.String(),
		Action: action, SubjectType: "gate", SubjectID: g.ID, Data: raw,
	})
}
