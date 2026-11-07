package domain

import (
	"encoding/json"
	"time"
	"uuid"
)

// JobKindModelCheck lists the models of a connection with its key, on a
// worker, so a model the server cannot reach is checked where it runs
// (config-resources.md, Model checks). It runs no agent and gets no repo.
const JobKindModelCheck JobKind = "model_check"

// ModelCheckSpec is what a model_check job checks. URL and KeyEnv are filled
// in at lease time from the connection as it is then, so the key only ever
// goes to the connection's own endpoint.
type ModelCheckSpec struct {
	Connection string `json:"connection"`
	// Adapter is the agent of the connection, whose version the worker
	// reports.
	Adapter string `json:"adapter"`
	// URL lists the models: the connection's endpoint and the catalog's path.
	URL string `json:"url,omitempty"`
	// Auth is how the key is sent: bearer, x-api-key, api-key or none.
	Auth string `json:"auth,omitempty"`
	// Format is the list's: anthropic or openai.
	Format string `json:"format,omitempty"`
	// KeyEnv names the variable of the spec's Env that holds the key.
	KeyEnv string `json:"key_env,omitempty"`
}

// What a key check found.
const (
	ModelCheckQueued       = "queued"        // the job has not ended yet
	ModelCheckReachable    = "reachable"     // the key works and the models are listed
	ModelCheckRefused      = "refused"       // the provider refused the key
	ModelCheckUnreachable  = "unreachable"   // no answer, or an address the worker may not reach
	ModelCheckWrongAddress = "wrong_address" // an answer, but not a list of models
	ModelCheckUnsupported  = "unsupported"   // the provider has no list; enter models by hand
	ModelCheckFailed       = "failed"        // the job failed or was canceled
)

// ModelCheckResult is the output of a model_check job. Models are the
// provider's names, which the worker keeps to a plain character set.
type ModelCheckResult struct {
	Status       string   `json:"status"`
	Detail       string   `json:"detail,omitempty"`
	Models       []string `json:"models,omitempty"`
	AgentVersion string   `json:"agent_version,omitempty"`
}

// Abilities a suitability test checks.
const (
	AbilityRead    = "read"    // read a file
	AbilityEdit    = "edit"    // edit a file
	AbilityCommand = "command" // run a command
	AbilityMCP     = "mcp"     // call a job MCP tool
	AbilityJSON    = "json"    // answer with JSON matching a schema
)

// Abilities returns the abilities a suitability test checks.
func Abilities() []string {
	return []string{AbilityRead, AbilityEdit, AbilityCommand, AbilityMCP, AbilityJSON}
}

// Levels of a tested model: what roles it may serve.
const (
	LevelCode = "code" // every ability: any role
	LevelRead = "read" // read, mcp and json: roles that only read
	LevelNone = "none"
)

// SuitabilitySpec makes an agent job the suitability test of a model on a
// connection: the worker runs it on the fixture repo of the packs, never on a
// project's repo, and on that connection whatever the role's assignment.
type SuitabilitySpec struct {
	Connection string `json:"connection"`
	// Nonce is the code word the worker writes into the fixture.
	Nonce string `json:"nonce"`
}

// SuitabilityReport is what the worker reports of a suitability test.
type SuitabilityReport struct {
	// Output is the agent's answer, when it gave JSON matching the schema.
	Output json.RawMessage `json:"output,omitempty"`
	// Error says why the run failed or the answer did not match.
	Error string `json:"error,omitempty"`
	// Edited and Ran are what the worker found in the fixture afterwards.
	Edited       bool   `json:"edited"`
	Ran          bool   `json:"ran"`
	Turns        int    `json:"turns"`
	AgentVersion string `json:"agent_version,omitempty"`
}

// SuitabilityResult is a test as stored: each ability, its level, and what
// it took.
type SuitabilityResult struct {
	Abilities    map[string]bool `json:"abilities"`
	Level        string          `json:"level"`
	Tokens       int64           `json:"tokens"`
	DurationMS   int64           `json:"duration_ms"`
	Turns        int             `json:"turns"`
	CostUSD      float64         `json:"cost_usd"`
	AgentVersion string          `json:"agent_version,omitempty"`
	Error        string          `json:"error,omitempty"`
}

// LevelOf is the level of a set of abilities.
func LevelOf(abilities map[string]bool) string {
	read := abilities[AbilityRead] && abilities[AbilityMCP] && abilities[AbilityJSON]
	switch {
	case read && abilities[AbilityEdit] && abilities[AbilityCommand]:
		return LevelCode
	case read:
		return LevelRead
	}
	return LevelNone
}

// Serves reports whether a model of level may serve a role that writes code
// (writes) or only reads.
func Serves(level string, writes bool) bool {
	return level == LevelCode || (!writes && level == LevelRead)
}

// ModelCheck is the last key check of a connection (Model empty) or the last
// suitability test of one of its models.
type ModelCheck struct {
	ID           uuid.UUID `json:"id"`
	AccountID    uuid.UUID `json:"account_id"`
	ConnectionID uuid.UUID `json:"connection_id"`
	Connection   string    `json:"connection"`
	Model        string    `json:"model,omitempty"`
	JobID        uuid.UUID `json:"job_id"`
	// Status is a ModelCheck* value for a key check; for a test queued,
	// done or failed.
	Status string             `json:"status"`
	Key    *ModelCheckResult  `json:"key,omitempty"`
	Test   *SuitabilityResult `json:"test,omitempty"`
	// Stale marks a test of an agent version that is no longer the one
	// workers run; it is run again.
	Stale     bool      `json:"stale,omitempty"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Statuses of a suitability test.
const (
	SuitabilityQueued = "queued"
	SuitabilityDone   = "done"
	SuitabilityFailed = "failed"
)
