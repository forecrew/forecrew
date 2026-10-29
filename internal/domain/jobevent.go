package domain

import (
	"encoding/json"
	"time"
	"uuid"
)

// JobEventType is the kind of a normalized job event. Runtimes and the
// worker emit the same types.
type JobEventType string

// Job event types and the shape of their Data.
const (
	EventStatus     JobEventType = "status"      // worker phase in Text: cloning, running, pushing, ...
	EventLog        JobEventType = "log"         // a log line in Text
	EventMessage    JobEventType = "message"     // assistant text in Text
	EventToolCall   JobEventType = "tool_call"   // Data: {"name": string, "input": any}
	EventToolResult JobEventType = "tool_result" // Data: {"name": string, "output": string, "is_error": bool}
	EventUsage      JobEventType = "usage"       // Data: Usage plus "cost_usd"
	EventError      JobEventType = "error"       // error message in Text
)

// JobEventTypes returns every job event type.
func JobEventTypes() []JobEventType {
	return []JobEventType{EventStatus, EventLog, EventMessage, EventToolCall, EventToolResult, EventUsage, EventError}
}

// JobEvent is one entry of a job's log. Runtimes emit events with only Time,
// Type, Text and Data set; the store assigns ID, JobID, AccountID and Seq.
type JobEvent struct {
	ID        uuid.UUID       `json:"id,omitzero"`
	AccountID uuid.UUID       `json:"account_id,omitzero"`
	JobID     uuid.UUID       `json:"job_id,omitzero"`
	Seq       int64           `json:"seq,omitempty"` // 1, 2, ... per job
	Time      time.Time       `json:"time"`
	Type      JobEventType    `json:"type"`
	Text      string          `json:"text,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}
