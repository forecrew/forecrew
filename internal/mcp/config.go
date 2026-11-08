package mcp

import (
	"encoding/json"

	"github.com/forecrew/forecrew/internal/domain"
)

// ConfigResource is one setting (docs/contracts/config-resources.md). Spec
// is JSON with its keys sorted and never holds a secret, only references.
type ConfigResource struct {
	Kind string          `json:"kind"`
	Name string          `json:"name"`
	Spec json.RawMessage `json:"spec"`
	// Scope is where it applies; Origin says, for a server setting, whether
	// the file alone sets it. Neither is part of the spec.
	Scope  string `json:"scope,omitempty"`
	Origin string `json:"origin,omitempty"`
	// Local marks a model_connection whose base_url is on your own network
	// (models.LocalEndpoint): its code stays there, and it needs no
	// authorization for private code. The server decides it; it is not part
	// of the spec.
	Local bool `json:"local,omitempty"`
}

// Ref is how a plan and its steps name the resource.
func (r ConfigResource) Ref() string { return r.Kind + "/" + r.Name }

// ConfigSchema describes one kind of resource.
type ConfigSchema struct {
	Kind   string          `json:"kind"`
	Title  string          `json:"title"`
	Scope  string          `json:"scope"`
	Schema json.RawMessage `json:"schema"`
}

// ConfigWanted is one change someone asks for: a spec, or Delete.
type ConfigWanted struct {
	Kind   string          `json:"kind"`
	Name   string          `json:"name"`
	Spec   json.RawMessage `json:"spec,omitempty"`
	Delete bool            `json:"delete,omitempty"`
}

// ConfigPlanView is a plan as the server shows it: Status is as of now, and
// CanConfirm says whether the reader holds what confirming takes.
type ConfigPlanView struct {
	domain.ConfigPlan
	CanConfirm bool `json:"can_confirm"`
	// Stale means the configuration moved since the plan was made: it has to
	// be rebuilt.
	Stale bool `json:"stale"`
}

// ConfigShowOnce is a value Forecrew generated, given to the person who
// confirmed the plan and stored nowhere.
type ConfigShowOnce struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// ConfigApplied is the answer of applying a plan.
type ConfigApplied struct {
	Plan     ConfigPlanView   `json:"plan"`
	ShowOnce []ConfigShowOnce `json:"show_once"`
}

// ConfigKindInput is the input of config_schema.
type ConfigKindInput struct {
	Kind string `json:"kind,omitempty"`
}

// ConfigGetInput is the input of config_get.
type ConfigGetInput struct {
	Kind string `json:"kind,omitempty"`
	Name string `json:"name,omitempty"`
}

// ConfigPlanInput is the input of config_plan. Source is set by the entry
// point, never by the caller of a tool.
type ConfigPlanInput struct {
	Title   string                  `json:"title,omitempty"`
	Request string                  `json:"request,omitempty"`
	Changes []ConfigWanted          `json:"changes"`
	Source  domain.ConfigPlanSource `json:"-"`
}

// ConfigPlanRefInput names a plan.
type ConfigPlanRefInput struct {
	Plan string `json:"plan"` // UUID
	// Source is the source of a plan the call makes; see ConfigPlanInput.
	Source domain.ConfigPlanSource `json:"-"`
}

// ConfigApplyInput is the input of applying a plan over REST. Browser is set
// by the API when the request came with the web UI's session.
type ConfigApplyInput struct {
	Plan string `json:"plan"`
	// Word is what the person typed in the second confirmation.
	Word    string `json:"word,omitempty"`
	Browser bool   `json:"-"`
}

// ConfigConfirmation is the answer of config_apply over MCP. A client never
// confirms a plan: this tells the person where to.
type ConfigConfirmation struct {
	Status string         `json:"status"` // confirmation_required
	Plan   ConfigPlanView `json:"plan"`
	// ConfirmURL opens the plan in the web UI, where the person signs in as
	// themselves. It grants nothing by itself.
	ConfirmURL string `json:"confirm_url,omitempty"`
	Message    string `json:"message"`
}

// ConfigExport is the answer of config_export.
type ConfigExport struct {
	YAML string `json:"yaml"`
}

// ConfigImportInput is the input of config_import.
type ConfigImportInput struct {
	YAML   string                  `json:"yaml"`
	Source domain.ConfigPlanSource `json:"-"`
}

// ConfigPlansInput is the input of config_plans.
type ConfigPlansInput struct {
	// Inbox keeps the plans waiting for the caller's confirmation.
	Inbox bool `json:"inbox,omitempty"`
	Limit int  `json:"limit,omitempty"`
}

// ConfigSecretInput saves the value of a secure field. It has no MCP tool:
// what passes through a client passes through its model.
type ConfigSecretInput struct {
	Plan   string `json:"plan"`
	Secret string `json:"secret"`
	Value  string `json:"value"`
}

// AssistantSendInput is the input of assistant_send.
type AssistantSendInput struct {
	// Conversation continues one; empty starts a new one.
	Conversation string `json:"conversation,omitempty"`
	Message      string `json:"message"`
	// Project is the project the conversation's jobs run in; it defaults to
	// the first the caller may read. Only for a new conversation.
	Project string `json:"project,omitempty"`
	// Page is the page the person is on, as context.
	Page string `json:"page,omitempty"`
}

// AssistantRefInput names a conversation.
type AssistantRefInput struct {
	Conversation string `json:"conversation"` // UUID
}

// AssistantListInput is the input of assistant_list.
type AssistantListInput struct {
	Limit int `json:"limit,omitempty"`
}

// AssistantView is a conversation with its messages. Plans are the plans its
// turns made, drawn by the server: nothing the model writes puts a Confirm
// button or a secure field on the screen.
type AssistantView struct {
	domain.AssistantConversation
	Messages []domain.AssistantMessage `json:"messages"`
	Plans    []ConfigPlanView          `json:"plans"`
	// Thinking is true while a turn's job runs.
	Thinking bool `json:"thinking"`
}
