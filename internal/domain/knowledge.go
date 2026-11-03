package domain

import (
	"time"
	"uuid"
)

// Knowledge provider kinds (docs/contracts/knowledge.md). A project's
// .forecrew/knowledge-policy.yaml names its providers; ProviderInternal is
// built in and never declared there.
const (
	ProviderKindRepo       = "repo"
	ProviderKindConfluence = "confluence"
	ProviderKindNotion     = "notion"
	ProviderKindGDrive     = "gdrive"
	ProviderKindSharePoint = "sharepoint"
	// ProviderInternal is the name and kind of forecrewd's own store of
	// agent notes.
	ProviderInternal = "internal"
)

// Reserved knowledge sections.
const (
	// SectionGenerated holds entries forecrewd rebuilds from the analyzer
	// (components, packages, dependencies): no provider, no owner, never
	// proposed to.
	SectionGenerated = "generated"
	// SectionNotes holds agent notes, the only section of ProviderInternal.
	SectionNotes = "notes"
)

// NoteScope is who sees an agent note.
type NoteScope string

// Note scopes.
const (
	NoteScopeGoal    NoteScope = "goal"    // jobs of the same goal; expires after the goal closes
	NoteScopeProject NoteScope = "project" // every job of the project
)

// NoteScopes returns every note scope.
func NoteScopes() []NoteScope { return []NoteScope{NoteScopeGoal, NoteScopeProject} }

// KnowledgeKind is what a search result is.
type KnowledgeKind string

// Knowledge result kinds.
const (
	KnowledgeEntryKind     KnowledgeKind = "entry"     // curated, from a provider
	KnowledgeNoteKind      KnowledgeKind = "note"      // an agent note
	KnowledgeGeneratedKind KnowledgeKind = "generated" // rebuilt by forecrewd
)

// Provenance is where a knowledge entry comes from: the frontmatter of a
// repo entry (docs/contracts/knowledge.md), or derived from git or the
// provider when there is none.
type Provenance struct {
	AuthorRole   string    `json:"author_role" yaml:"author_role"`                        // role, or "human"
	Source       string    `json:"source" yaml:"source"`                                  // ref (JOB-41, GOAL-3), URL or "human"
	Created      time.Time `json:"created" yaml:"created"`                                // first written
	LastVerified time.Time `json:"last_verified,omitzero" yaml:"last_verified,omitempty"` // last checked against the code
}

// KnowledgeEntry is one indexed entry of a project's knowledge base
// (docs/contracts/data-model.md, knowledge_entries): a curated entry of a
// provider, a generated entry or an agent note. (Project, Provider, Path) is
// unique.
type KnowledgeEntry struct {
	ID        uuid.UUID `json:"id"`
	AccountID uuid.UUID `json:"account_id"`
	ProjectID uuid.UUID `json:"project_id"`
	Provider  string    `json:"provider"` // provider name from the policy, or ProviderInternal
	// Path is the repo path of a repo entry, or the provider's stable id
	// (page id, note id).
	Path           string     `json:"path"`
	SourceURL      string     `json:"source_url,omitempty"`
	Version        string     `json:"version,omitempty"` // blob SHA, page version
	Section        string     `json:"section"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`      // Markdown without frontmatter
	BodyHash       string     `json:"body_hash"` // lowercase hex SHA-256 of Body
	Provenance     Provenance `json:"provenance"`
	Generated      bool       `json:"generated"`
	NeedsReverify  bool       `json:"needs_reverify"`
	ReverifyReason string     `json:"reverify_reason,omitempty"` // commit and paths that changed
	// Scope, GoalID, ExpiresAt and Tags are set on notes only.
	Scope     NoteScope `json:"scope,omitempty"`
	GoalID    uuid.UUID `json:"goal_id,omitzero"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	Tags      []string  `json:"tags,omitempty"`
	SyncedAt  time.Time `json:"synced_at"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Kind returns whether e is a curated entry, a note or a generated entry.
func (e KnowledgeEntry) Kind() KnowledgeKind {
	switch {
	case e.Provider == ProviderInternal:
		return KnowledgeNoteKind
	case e.Generated:
		return KnowledgeGeneratedKind
	}
	return KnowledgeEntryKind
}

// Validate checks the fields, wrapping ErrInvalidRecord: notes are the
// internal provider's section notes with a scope (and a goal when scoped
// to one); generated entries are in section generated and nothing else is.
func (e KnowledgeEntry) Validate() error {
	var p problems
	p.checkText("provider", e.Provider)
	p.checkText("path", e.Path)
	p.checkText("title", e.Title)
	if !ValidName(e.Section) {
		p.addf("section %q must match %s", e.Section, nameRE)
	}
	note := e.Provider == ProviderInternal
	if note != (e.Section == SectionNotes) {
		p.addf("section notes is the internal provider's only section")
	}
	if e.Generated != (e.Section == SectionGenerated) || (e.Generated && note) {
		p.addf("generated entries, and only they, are in section generated")
	}
	switch {
	case note:
		checkEnum(&p, "scope", e.Scope, NoteScopes())
		if (e.Scope == NoteScopeGoal) != (e.GoalID != uuid.Nil()) {
			p.addf("a goal note needs its goal, a project note none")
		}
	case e.Scope != "" || e.GoalID != uuid.Nil() || !e.ExpiresAt.IsZero() || len(e.Tags) > 0:
		p.addf("scope, goal_id, expires_at and tags are for notes only")
	}
	return p.errOf(ErrInvalidRecord, "knowledge entry")
}

// KnowledgeAction is what a knowledge proposal does to an entry.
type KnowledgeAction string

// Knowledge proposal actions.
const (
	KnowledgeAdd    KnowledgeAction = "add"
	KnowledgeEdit   KnowledgeAction = "edit"
	KnowledgeRetire KnowledgeAction = "retire"
)

// KnowledgeActions returns every knowledge proposal action.
func KnowledgeActions() []KnowledgeAction {
	return []KnowledgeAction{KnowledgeAdd, KnowledgeEdit, KnowledgeRetire}
}

// KnowledgeBatch says when a proposal is sent to its provider.
type KnowledgeBatch string

// Knowledge proposal batches.
const (
	BatchPerGoal    KnowledgeBatch = "per_goal"   // with the goal's learn PR
	BatchStandalone KnowledgeBatch = "standalone" // on its own when the job completes
)

// KnowledgeBatches returns every batch.
func KnowledgeBatches() []KnowledgeBatch { return []KnowledgeBatch{BatchPerGoal, BatchStandalone} }

// KnowledgeProposalStatus is the state of a knowledge proposal.
type KnowledgeProposalStatus string

// Knowledge proposal states.
const (
	KnowledgeQueued   KnowledgeProposalStatus = "queued"   // waiting for its batch
	KnowledgeOpen     KnowledgeProposalStatus = "open"     // at the provider (ExternalRef) or a gate, under review
	KnowledgeAccepted KnowledgeProposalStatus = "accepted" // merged or published
	KnowledgeRejected KnowledgeProposalStatus = "rejected" // closed, discarded or refused by policy
)

// KnowledgeProposalStatuses returns every knowledge proposal state.
func KnowledgeProposalStatuses() []KnowledgeProposalStatus {
	return []KnowledgeProposalStatus{KnowledgeQueued, KnowledgeOpen, KnowledgeAccepted, KnowledgeRejected}
}

var knowledgeProposalTransitions = transitions[KnowledgeProposalStatus]{
	KnowledgeQueued: {KnowledgeOpen, KnowledgeRejected},
	KnowledgeOpen:   {KnowledgeAccepted, KnowledgeRejected},
}

// CanTransition reports whether a knowledge proposal may move from s to to.
func (s KnowledgeProposalStatus) CanTransition(to KnowledgeProposalStatus) bool {
	return knowledgeProposalTransitions.can(s, to)
}

// Transition returns to, or an error wrapping ErrInvalidTransition.
func (s KnowledgeProposalStatus) Transition(to KnowledgeProposalStatus) (KnowledgeProposalStatus, error) {
	return knowledgeProposalTransitions.apply("knowledge proposal", s, to)
}

// KnowledgeProposal is a proposed change to a curated knowledge section
// (docs/contracts/data-model.md, knowledge_proposals).
type KnowledgeProposal struct {
	ID          uuid.UUID               `json:"id"`
	AccountID   uuid.UUID               `json:"account_id"`
	ProjectID   uuid.UUID               `json:"project_id"`
	Number      int64                   `json:"number"` // KB-<number>
	Provider    string                  `json:"provider"`
	Section     string                  `json:"section"`
	Path        string                  `json:"path"` // repo path or provider location
	Action      KnowledgeAction         `json:"action"`
	Body        string                  `json:"body"` // the new Markdown; empty to retire
	Reason      string                  `json:"reason"`
	ProposedBy  string                  `json:"proposed_by"` // role, or Actor.String() of a human
	JobID       uuid.UUID               `json:"job_id,omitzero"`
	GoalID      uuid.UUID               `json:"goal_id,omitzero"`
	Batch       KnowledgeBatch          `json:"batch"`
	ExternalRef string                  `json:"external_ref,omitempty"` // PR number, page id: what the review is followed by
	ChangeID    uuid.UUID               `json:"change_id,omitzero"`     // the PR, for the repo provider
	GateID      uuid.UUID               `json:"gate_id,omitzero"`       // knowledge_proposal gate, when a human decides
	Status      KnowledgeProposalStatus `json:"status"`
	CreatedAt   time.Time               `json:"created_at"`
	UpdatedAt   time.Time               `json:"updated_at"`
}

// Ref returns the proposal's human ref, e.g. KB-5.
func (k KnowledgeProposal) Ref() string { return Ref(RefKnowledge, k.Number) }

// Validate checks the fields, wrapping ErrInvalidRecord. Nothing is ever
// proposed to the generated section or to notes; per_goal proposals need
// their goal.
func (k KnowledgeProposal) Validate() error {
	var p problems
	p.checkText("provider", k.Provider)
	p.checkText("path", k.Path)
	p.checkText("reason", k.Reason)
	p.checkText("proposed_by", k.ProposedBy)
	if !ValidName(k.Section) {
		p.addf("section %q must match %s", k.Section, nameRE)
	}
	if k.Section == SectionGenerated || k.Section == SectionNotes || k.Provider == ProviderInternal {
		p.addf("section %s of provider %s takes no proposals", k.Section, k.Provider)
	}
	checkEnum(&p, "action", k.Action, KnowledgeActions())
	if k.Action != KnowledgeRetire {
		p.checkText("body", k.Body)
	}
	checkEnum(&p, "batch", k.Batch, KnowledgeBatches())
	if k.Batch == BatchPerGoal && k.GoalID == uuid.Nil() {
		p.addf("a per_goal proposal needs its goal")
	}
	checkEnum(&p, "status", k.Status, KnowledgeProposalStatuses())
	return p.errOf(ErrInvalidRecord, "knowledge proposal")
}
