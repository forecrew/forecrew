package domain

import (
	"encoding/json"
	"time"
	"uuid"
)

// RunStatus is the state of a workflow run.
type RunStatus string

// Workflow run states.
const (
	RunRunning   RunStatus = "running"
	RunWaiting   RunStatus = "waiting" // a step waits for a signal
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCanceled  RunStatus = "canceled"
)

// WorkflowRun is one execution of a playbook.
type WorkflowRun struct {
	ID        uuid.UUID       `json:"id"`
	AccountID uuid.UUID       `json:"account_id"`
	ProjectID uuid.UUID       `json:"project_id"`
	Playbook  string          `json:"playbook"` // playbook name
	Status    RunStatus       `json:"status"`
	GoalID    uuid.UUID       `json:"goal_id,omitzero"`
	TaskID    uuid.UUID       `json:"task_id,omitzero"`
	Inputs    json.RawMessage `json:"inputs,omitempty"`
	State     json.RawMessage `json:"state,omitempty"` // step outputs and variables, owned by the engine
	Error     string          `json:"error,omitempty"`
	// CancelReason, when set, asks the engine to cancel the run the next
	// time it looks at it.
	CancelReason string    `json:"cancel_reason,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	FinishedAt   time.Time `json:"finished_at,omitzero"`
}

// StepStatus is the state of a step run.
type StepStatus string

// Step run states.
const (
	StepPending   StepStatus = "pending"
	StepRunning   StepStatus = "running"
	StepWaiting   StepStatus = "waiting" // waits for a signal with WaitKey
	StepSucceeded StepStatus = "succeeded"
	StepFailed    StepStatus = "failed"
	StepSkipped   StepStatus = "skipped"
)

// StepRun is one attempt of one playbook step.
type StepRun struct {
	ID         uuid.UUID       `json:"id"`
	AccountID  uuid.UUID       `json:"account_id"`
	RunID      uuid.UUID       `json:"run_id"`
	StepID     string          `json:"step_id"` // step id in the playbook
	Attempt    int             `json:"attempt"` // 1, 2, ...
	Status     StepStatus      `json:"status"`
	WaitKey    string          `json:"wait_key,omitempty"` // signal key awaited while waiting
	JobID      uuid.UUID       `json:"job_id,omitzero"`
	GateID     uuid.UUID       `json:"gate_id,omitzero"`
	SignalID   uuid.UUID       `json:"signal_id,omitzero"` // signal that resumed the step
	Output     json.RawMessage `json:"output,omitempty"`
	Error      string          `json:"error,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	StartedAt  time.Time       `json:"started_at,omitzero"`
	FinishedAt time.Time       `json:"finished_at,omitzero"`
}
