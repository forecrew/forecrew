package domain

import (
	"regexp"
	"strings"
	"time"
	"uuid"
)

// RuntimeNone is the runtime of a queued agent job that has no usable model
// connection: no worker advertises it, so the job waits, and no machine starts
// for it. internal/models gives the job its runtime when a connection is there.
const RuntimeNone = "none"

// OrganizationModelLabel is required by a job on an organization's model
// connection. forecrewd gives it to a worker that polls with the capability
// OrganizationModelCapability, and to every machine shape: such a worker
// keeps its own keys away from the job. A worker never sets it itself.
const OrganizationModelLabel = "models:organization"

// OrganizationModelCapability is what a worker that keeps its own keys away
// from an organization's connection says when it polls.
const OrganizationModelCapability = "organization_models"

// ModelSecretPrefix starts the names of the secret store that hold model keys.
// A model connection reaches this namespace only, and nothing else reaches it:
// whoever manages a connection decides where its key is sent, so it must not
// name, say, the client secret of a sign-in connection, and the other way round.
const ModelSecretPrefix = "models/"

// ValidModelSecretRef reports whether ref is "store:models/<name>".
func ValidModelSecretRef(ref string) bool {
	name, ok := StoreSecretName(ref)
	return ok && len(name) > len(ModelSecretPrefix) && strings.HasPrefix(name, ModelSecretPrefix)
}

// IsModelSecretRef reports whether ref names the namespace of model keys.
func IsModelSecretRef(ref string) bool {
	return strings.HasPrefix(ref, SecretRefStore+ModelSecretPrefix)
}

var modelKeyEnvRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*_KEY$`)

// ValidModelKeyEnv reports whether name may hold an organization's model key
// in a job's environment. The form keeps a key out of variables that change
// how a program runs (PATH, LD_PRELOAD, NODE_OPTIONS) and out of forecrew's own.
func ValidModelKeyEnv(name string) bool {
	return modelKeyEnvRE.MatchString(name) && !strings.HasPrefix(name, "FORECREW_")
}

// ModelConnection is a runtime owned by an organization: an agent and a model
// vendor with the key to use them (config-resources.md).
type ModelConnection struct {
	ID        uuid.UUID `json:"id"`
	AccountID uuid.UUID `json:"account_id"`
	Name      string    `json:"name"`
	Vendor    string    `json:"vendor"`
	Adapter   string    `json:"adapter"`
	// BaseURL sends the model calls to a custom endpoint; empty means the
	// vendor's own.
	BaseURL string `json:"base_url,omitempty"`
	// KeyEnv is the variable the key goes in. Claude Code and Codex have
	// their own, so it is only for opencode.
	KeyEnv string `json:"key_env,omitempty"`
	// SecretRef is a ValidModelSecretRef, resolved at lease time.
	SecretRef string    `json:"secret_ref"`
	Enabled   bool      `json:"enabled"`
	UpdatedBy string    `json:"updated_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ModelRole gives a role a connection and a tier.
type ModelRole struct {
	Connection string `json:"connection"`
	Tier       string `json:"tier"`
}

// ModelAssignment says which model serves what, for an organization or, with
// ProjectID, as the overrides of one project. It lives in the database and
// never in a repo: the model decides cost and where code goes.
type ModelAssignment struct {
	ID        uuid.UUID `json:"id"`
	AccountID uuid.UUID `json:"account_id"`
	// ProjectID is zero on the organization's row.
	ProjectID uuid.UUID `json:"project_id,omitzero"`
	// Tiers maps tier -> vendor -> model.
	Tiers map[string]map[string]string `json:"tiers"`
	Roles map[string]ModelRole         `json:"roles"`
	// ProjectOverrides is the policy models.project_overrides, on the
	// organization's row only; nil means on.
	ProjectOverrides *bool     `json:"project_overrides,omitempty"`
	UpdatedBy        string    `json:"updated_by"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// OverridesAllowed reports whether a's organization lets projects override it.
func (a ModelAssignment) OverridesAllowed() bool {
	return a.ProjectOverrides == nil || *a.ProjectOverrides
}
