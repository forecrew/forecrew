package domain

import (
	"encoding/json"
	"slices"
	"time"
	"uuid"
)

// ConfigPlanStatus is the state of a change plan (docs/contracts/config-resources.md).
type ConfigPlanStatus string

// Change plan states. A plan past ExpiresAt that was not applied reads as
// ConfigPlanExpired (ConfigPlan.StatusAt); that state is never stored.
const (
	ConfigPlanPending  ConfigPlanStatus = "pending"
	ConfigPlanAwaiting ConfigPlanStatus = "awaiting_approval" // in the Inbox of people who can confirm it
	ConfigPlanApplied  ConfigPlanStatus = "applied"
	ConfigPlanCanceled ConfigPlanStatus = "canceled"
	ConfigPlanReverted ConfigPlanStatus = "reverted" // a later plan put the previous values back
	ConfigPlanExpired  ConfigPlanStatus = "expired"
)

// ConfigPlanStatuses returns every stored state.
func ConfigPlanStatuses() []ConfigPlanStatus {
	return []ConfigPlanStatus{ConfigPlanPending, ConfigPlanAwaiting, ConfigPlanApplied, ConfigPlanCanceled, ConfigPlanReverted}
}

var configPlanTransitions = transitions[ConfigPlanStatus]{
	ConfigPlanPending:  {ConfigPlanAwaiting, ConfigPlanApplied, ConfigPlanCanceled},
	ConfigPlanAwaiting: {ConfigPlanApplied, ConfigPlanCanceled},
	ConfigPlanApplied:  {ConfigPlanReverted},
}

// Transition returns to when s may move there, and an error wrapping
// ErrInvalidTransition otherwise.
func (s ConfigPlanStatus) Transition(to ConfigPlanStatus) (ConfigPlanStatus, error) {
	return configPlanTransitions.apply("config plan", s, to)
}

// Open reports whether the plan can still be applied.
func (s ConfigPlanStatus) Open() bool { return s == ConfigPlanPending || s == ConfigPlanAwaiting }

// ConfigPlanSource says which path made a plan.
type ConfigPlanSource string

// Change plan sources.
const (
	ConfigFromForm      ConfigPlanSource = "form"
	ConfigFromAssistant ConfigPlanSource = "assistant"
	ConfigFromYAML      ConfigPlanSource = "yaml"
	ConfigFromCLI       ConfigPlanSource = "cli"
	ConfigFromMCP       ConfigPlanSource = "mcp"
)

// ConfigPlanSources returns every source.
func ConfigPlanSources() []ConfigPlanSource {
	return []ConfigPlanSource{ConfigFromForm, ConfigFromAssistant, ConfigFromYAML, ConfigFromCLI, ConfigFromMCP}
}

// BrowserOnly reports whether only a person in the web UI confirms plans of
// this source: a model wrote them, so no token can.
func (s ConfigPlanSource) BrowserOnly() bool {
	return s == ConfigFromMCP || s == ConfigFromAssistant
}

// ConfigOp is what a change does to a resource.
type ConfigOp string

// Change operations.
const (
	ConfigCreate ConfigOp = "create"
	ConfigUpdate ConfigOp = "update"
	ConfigDelete ConfigOp = "delete"
)

// ConfigFieldChange is one changed field; Old and New are shown to people.
type ConfigFieldChange struct {
	Path string `json:"path"`
	Old  string `json:"old,omitempty"`
	New  string `json:"new,omitempty"`
}

// ConfigChange is one resource change of a plan. Old holds the spec before
// the change, which is what a revert puts back; New the spec after it.
type ConfigChange struct {
	Op     ConfigOp            `json:"op"`
	Kind   string              `json:"kind"`
	Name   string              `json:"name"`
	Old    json.RawMessage     `json:"old,omitempty"`
	New    json.RawMessage     `json:"new,omitempty"`
	Fields []ConfigFieldChange `json:"fields"`
	// Line is the change in one readable line, with its sign: + ~ or -.
	Line string `json:"line"`
}

// ConfigStepKind is something a plan needs besides its changes.
type ConfigStepKind string

// Plan step kinds. Only the server writes steps, and only they make the UI
// draw a secure field or a connect card.
const (
	// ConfigStepSecret asks for a value in a secure field; the plan cannot
	// be confirmed until the secret store holds it.
	ConfigStepSecret ConfigStepKind = "secret_required"
	// ConfigStepConnect is a command to run once the plan is applied. The
	// credential it leads to travels over the machine's own connection and
	// passes through no person.
	ConfigStepConnect ConfigStepKind = "connect"
)

// ConfigStep is one step of a plan.
type ConfigStep struct {
	Kind     ConfigStepKind `json:"kind"`
	Resource string         `json:"resource"` // kind/name
	Title    string         `json:"title"`
	Detail   string         `json:"detail,omitempty"`
	// Secret is the name in the secret store of a secret_required step.
	Secret string `json:"secret,omitempty"`
	// Filled says the secret store holds Secret; set when the plan is read.
	Filled bool `json:"filled"`
}

// ConfigPlan is a stored change plan: what would change, what it needs and
// what it affects. A plan is applied in one step.
type ConfigPlan struct {
	ID        uuid.UUID        `json:"id"`
	AccountID uuid.UUID        `json:"account_id"`
	Status    ConfigPlanStatus `json:"status"`
	Source    ConfigPlanSource `json:"source"`
	// Client is the name of the MCP client that sent the plan.
	Client  string `json:"client,omitempty"`
	Title   string `json:"title"`
	Request string `json:"request,omitempty"` // the sentence that asked for it
	// Applies is "now", or "pr:owner/name" for repo files.
	Applies string         `json:"applies"`
	Changes []ConfigChange `json:"changes"`
	Steps   []ConfigStep   `json:"steps"`
	// Needs lists the permissions confirming takes.
	Needs   []Permission `json:"needs"`
	Affects string       `json:"affects"`
	Cost    string       `json:"cost"`
	Undo    string       `json:"undo"`
	// Reversible is false when something in Undo cannot come back.
	Reversible bool `json:"reversible"`
	// LowersProtection gives the reason of each drop in protection; with
	// any, confirming takes typing ConfirmWord.
	LowersProtection []string `json:"lowers_protection"`
	ConfirmWord      string   `json:"confirm_word,omitempty"`
	// BaseDigest is the digest of what the plan changes as it was planned;
	// a plan whose resources moved since is not applied.
	BaseDigest string `json:"base_digest"`
	// Untrusted names text the plan draws on that nobody verified.
	Untrusted      string    `json:"untrusted,omitempty"`
	CreatedBy      string    `json:"created_by"` // Actor.String()
	ConversationID uuid.UUID `json:"conversation_id,omitzero"`
	JobID          uuid.UUID `json:"job_id,omitzero"`
	// Reverts is the plan this one undoes.
	Reverts   uuid.UUID `json:"reverts,omitzero"`
	ExpiresAt time.Time `json:"expires_at"`
	AppliedBy string    `json:"applied_by,omitempty"`
	AppliedAt time.Time `json:"applied_at,omitzero"`
	// PRURL is the pull request of a repo plan.
	PRURL     string    `json:"pr_url,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// StatusAt is the status of the plan at now: an open plan past its expiry
// reads as expired.
func (p ConfigPlan) StatusAt(now time.Time) ConfigPlanStatus {
	if p.Status.Open() && !now.Before(p.ExpiresAt) {
		return ConfigPlanExpired
	}
	return p.Status
}

// Kinds returns the resource kinds the plan touches, sorted.
func (p ConfigPlan) Kinds() []string {
	var out []string
	for _, c := range p.Changes {
		out = append(out, c.Kind)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ConfigSetting is a stored resource that has no table of its own: a server
// setting over the file's default, or a setting of one organization. The
// zero AccountID is the scope of the whole server.
type ConfigSetting struct {
	ID        uuid.UUID       `json:"id"`
	AccountID uuid.UUID       `json:"account_id,omitzero"`
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	Spec      json.RawMessage `json:"spec"`
	UpdatedBy string          `json:"updated_by"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// StoredSecret is an encrypted value of the secret store. The zero
// AccountID is the scope of the whole server.
type StoredSecret struct {
	ID         uuid.UUID `json:"id"`
	AccountID  uuid.UUID `json:"account_id,omitzero"`
	Name       string    `json:"name"`
	Nonce      []byte    `json:"-"`
	Ciphertext []byte    `json:"-"`
	UpdatedBy  string    `json:"updated_by"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// AssistantConversation is a conversation with the assistant, personal to
// the subject that started it. Its turns run as jobs of ProjectID.
type AssistantConversation struct {
	ID        uuid.UUID `json:"id"`
	AccountID uuid.UUID `json:"account_id"`
	ProjectID uuid.UUID `json:"project_id"`
	Owner     string    `json:"owner"` // Actor.String()
	// Credential is what the owner came with (Credential.String()): the
	// assistant reads with no more than that credential reaches.
	Credential string    `json:"-"`
	Title      string    `json:"title"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// AssistantAuthor is who wrote a message.
type AssistantAuthor string

// Message authors.
const (
	AssistantUser  AssistantAuthor = "user"
	AssistantReply AssistantAuthor = "assistant"
)

// AssistantMessage is one message of a conversation. JobID is the turn it
// belongs to: the job a user message started, or the job that answered.
type AssistantMessage struct {
	ID             uuid.UUID       `json:"id"`
	AccountID      uuid.UUID       `json:"account_id"`
	ConversationID uuid.UUID       `json:"conversation_id"`
	Author         AssistantAuthor `json:"author"`
	Body           string          `json:"body"`
	JobID          uuid.UUID       `json:"job_id,omitzero"`
	CreatedAt      time.Time       `json:"created_at"`
}
