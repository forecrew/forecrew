package domain

import (
	"testing"
	"uuid"
)

func TestKnowledgeProposalTransitions(t *testing.T) {
	checkTable(t, KnowledgeProposalStatuses(), []pair[KnowledgeProposalStatus]{
		{KnowledgeQueued, KnowledgeOpen}, {KnowledgeQueued, KnowledgeRejected},
		{KnowledgeOpen, KnowledgeAccepted}, {KnowledgeOpen, KnowledgeRejected},
	}, KnowledgeProposalStatus.CanTransition, KnowledgeProposalStatus.Transition)
}

func TestKnowledgeEntry(t *testing.T) {
	goal := NewID()
	entry := KnowledgeEntry{Provider: "repo", Path: "docs/adr/0001-queue.md", Section: "architecture", Title: "Queue"}
	note := KnowledgeEntry{Provider: ProviderInternal, Path: "notes/" + NewID().String(), Section: SectionNotes, Title: "Flaky test",
		Scope: NoteScopeGoal, GoalID: goal, ExpiresAt: now, Tags: []string{"ci"}}
	gen := KnowledgeEntry{Provider: "repo", Path: "generated/components", Section: SectionGenerated, Title: "Components", Generated: true}
	for _, e := range []KnowledgeEntry{entry, note, gen} {
		wantInvalid(t, e.Path, e.Validate())
	}
	if entry.Kind() != KnowledgeEntryKind || note.Kind() != KnowledgeNoteKind || gen.Kind() != KnowledgeGeneratedKind {
		t.Fatal("Kind")
	}
	tests := map[string]struct {
		e    KnowledgeEntry
		want []string
	}{
		"empty":             {KnowledgeEntry{}, []string{"provider must be set", "path must be set", "title must be set", `section ""`}},
		"note elsewhere":    {func() KnowledgeEntry { e := note; e.Section = "lessons"; return e }(), []string{"only section"}},
		"notes of repo":     {func() KnowledgeEntry { e := entry; e.Section = SectionNotes; return e }(), []string{"only section"}},
		"goal note no goal": {func() KnowledgeEntry { e := note; e.GoalID = uuid.Nil(); return e }(), []string{"needs its goal"}},
		"project note goal": {func() KnowledgeEntry { e := note; e.Scope = NoteScopeProject; return e }(), []string{"needs its goal"}},
		"note no scope":     {func() KnowledgeEntry { e := note; e.Scope = ""; return e }(), []string{"scope must be one of"}},
		"entry with scope":  {func() KnowledgeEntry { e := entry; e.Tags = []string{"x"}; return e }(), []string{"for notes only"}},
		"generated outside": {func() KnowledgeEntry { e := entry; e.Generated = true; return e }(), []string{"only they"}},
		"generated note":    {func() KnowledgeEntry { e := note; e.Generated = true; return e }(), []string{"only they"}},
	}
	for name, tt := range tests {
		wantInvalid(t, name, tt.e.Validate(), tt.want...)
	}
}

func TestKnowledgeProposalValidate(t *testing.T) {
	ok := KnowledgeProposal{Provider: "repo", Section: "conventions", Path: "CONTRIBUTING.md", Action: KnowledgeEdit, Body: "# Rules",
		Reason: "PRs stay small", ProposedBy: "reviewer", Batch: BatchPerGoal, GoalID: NewID(), Status: KnowledgeQueued}
	wantInvalid(t, "valid", ok.Validate())
	retire := ok
	retire.Action, retire.Body, retire.Batch, retire.GoalID = KnowledgeRetire, "", BatchStandalone, uuid.Nil()
	wantInvalid(t, "retire", retire.Validate())
	for _, section := range []string{SectionGenerated, SectionNotes} {
		k := ok
		k.Section = section
		wantInvalid(t, section, k.Validate(), "takes no proposals")
	}
	k := ok
	k.Provider = ProviderInternal
	wantInvalid(t, "internal", k.Validate(), "takes no proposals")
	k = ok
	k.GoalID = uuid.Nil()
	wantInvalid(t, "per_goal without goal", k.Validate(), "needs its goal")
	wantInvalid(t, "empty", KnowledgeProposal{}.Validate(), "provider must be set", "path must be set", "reason must be set",
		"proposed_by must be set", `section ""`, "action must be one of", "body must be set", "batch must be one of", "status must be one of")
}
