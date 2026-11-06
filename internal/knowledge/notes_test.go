package knowledge_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

type notesEnv struct {
	*syncEnv
	notes  *knowledge.Notes
	search *knowledge.Search
	g1, g2 domain.Goal
}

func newNotesEnv(t *testing.T) *notesEnv {
	t.Helper()
	e := newSyncEnv(t)
	// The policy comes from the repo: give it a head to read it at.
	e.commit(t, "h0", map[string]string{"README.md": "# app\n"})
	must(t, e.tr.SetHeadOfBranch("acme/app", "main", "h0"))
	idx := &knowledge.Index{Store: e.st}
	return &notesEnv{
		syncEnv: e,
		notes:   &knowledge.Notes{Store: e.st, Index: idx, Now: e.clock.Now},
		search:  &knowledge.Search{Index: idx, Policy: e.syncer.Policy},
		g1:      testkit.Goal(t, e.st, e.project.AccountID, e.project.ID, "one"),
		g2:      testkit.Goal(t, e.st, e.project.AccountID, e.project.ID, "two"),
	}
}

// job is a job of the goal running under a worker; n is unused.
func (e *notesEnv) job(t *testing.T, _ int64, role string, goal domain.Goal) domain.Job {
	t.Helper()
	j := domain.Job{AccountID: e.project.AccountID, ProjectID: e.project.ID, GoalID: goal.ID, Role: role, Runtime: "claude-code"}
	if err := e.st.Jobs().Create(t.Context(), &j); err != nil {
		t.Fatal(err)
	}
	return testkit.Lease(t, e.st, j)
}

func (e *notesEnv) find(t *testing.T, j domain.Job, text string) []knowledge.Result {
	t.Helper()
	res, err := e.search.Run(t.Context(), knowledge.Query{Project: e.project, Text: text, Role: j.Role, GoalID: j.GoalID})
	must(t, err)
	return res
}

func TestNotesAreSharedWithinTheGoal(t *testing.T) {
	e := newNotesEnv(t)
	ctx := t.Context()
	writer, sameGoal, otherGoal := e.job(t, 1, "developer", e.g1), e.job(t, 2, "reviewer", e.g1), e.job(t, 3, "developer", e.g2)

	n, err := e.notes.Write(ctx, writer, knowledge.NoteInput{Scope: domain.NoteScopeGoal, Title: "Flaky disk test", Body: "The spool test fails on slow disks.", Tags: []string{"flaky"}})
	must(t, err)
	if n.Path != "notes/"+n.ID.String() || n.Provenance.Source != "JOB-1" || n.Provenance.AuthorRole != "developer" || !n.ExpiresAt.IsZero() || n.GoalID != e.g1.ID {
		t.Errorf("note = %+v", n)
	}
	if _, err := e.notes.Write(ctx, otherGoal, knowledge.NoteInput{Scope: domain.NoteScopeProject, Title: "Cache", Body: "The cache is warmed at boot."}); err != nil {
		t.Fatal(err)
	}

	got := e.find(t, sameGoal, "spool")
	if len(got) != 1 || got[0].Kind != domain.KnowledgeNoteKind || !got[0].Untrusted || got[0].Provider != domain.ProviderInternal || got[0].Provenance.Source != "JOB-1" {
		t.Errorf("same goal found %+v", got)
	}
	if got := e.find(t, otherGoal, "spool"); len(got) != 0 {
		t.Errorf("a job of another goal found %+v", got)
	}
	if got := e.find(t, sameGoal, "cache"); len(got) != 1 {
		t.Errorf("a project note is seen by every job: %+v", got)
	}
	// A human sees the notes of every goal.
	all, err := e.search.Run(ctx, knowledge.Query{Project: e.project, Text: "spool", AllGoals: true})
	if err != nil || len(all) != 1 {
		t.Errorf("human search = %+v, %v", all, err)
	}

	// Editing: the same goal may, another goal may not.
	edit, err := e.notes.Write(ctx, sameGoal, knowledge.NoteInput{Scope: domain.NoteScopeGoal, Title: "Flaky disk test", Body: "Fixed by retrying.", Note: n.ID})
	if err != nil || edit.ID != n.ID || !edit.Provenance.Created.Equal(n.Provenance.Created) || edit.Provenance.AuthorRole != "reviewer" {
		t.Fatalf("edit = %+v, %v", edit, err)
	}
	if got := e.find(t, sameGoal, "retrying"); len(got) != 1 {
		t.Errorf("edited note not found: %+v", got)
	}
	_, err = e.notes.Write(ctx, otherGoal, knowledge.NoteInput{Scope: domain.NoteScopeProject, Title: "x", Body: "stolen", Note: n.ID})
	if !errors.Is(err, knowledge.ErrNoteForbidden) {
		t.Errorf("editing a note of another goal: %v", err)
	}
	if _, err = e.notes.Write(ctx, writer, knowledge.NoteInput{Scope: domain.NoteScopeGoal, Title: "x", Body: "y", Note: uuid.New()}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("editing an unknown note: %v", err)
	}
	// Curated entries are not notes.
	e.commit(t, "h1", sourceFiles)
	_, err = e.syncer.Sync(ctx, e.project, "", "h1")
	must(t, err)
	adr := e.entries(t)["docs/adr/0001-q.md"]
	if _, err = e.notes.Write(ctx, writer, knowledge.NoteInput{Scope: domain.NoteScopeProject, Title: "x", Body: "y", Note: adr.ID}); !errors.Is(err, knowledge.ErrNoteForbidden) {
		t.Errorf("editing an entry through notes: %v", err)
	}
	if err := e.notes.Delete(ctx, e.project.AccountID, adr.ID); !errors.Is(err, knowledge.ErrNotNote) {
		t.Errorf("deleting an entry as a note: %v", err)
	}
	if err := e.notes.Delete(ctx, e.project.AccountID, n.ID); err != nil {
		t.Errorf("deleting a note: %v", err)
	}
	if got := e.find(t, sameGoal, "retrying"); len(got) != 0 {
		t.Errorf("deleted note found: %+v", got)
	}
}

func TestNoteLimitsAndGoals(t *testing.T) {
	e := newNotesEnv(t)
	ctx := t.Context()
	noGoal := e.job(t, 4, "developer", domain.Goal{})
	if _, err := e.notes.Write(ctx, noGoal, knowledge.NoteInput{Scope: domain.NoteScopeGoal, Title: "t", Body: "b"}); !errors.Is(err, knowledge.ErrNoGoal) {
		t.Errorf("goal note without a goal: %v", err)
	}
	if _, err := e.notes.Write(ctx, noGoal, knowledge.NoteInput{Scope: domain.NoteScopeProject, Title: "t", Body: "b"}); err != nil {
		t.Errorf("project note without a goal: %v", err)
	}
	j := e.job(t, 5, "developer", e.g1)
	big := knowledge.NoteInput{Scope: domain.NoteScopeProject, Title: "t", Body: strings.Repeat("x", knowledge.MaxNoteBody+1)}
	if _, err := e.notes.Write(ctx, j, big); !errors.Is(err, knowledge.ErrNoteTooLarge) {
		t.Errorf("big note: %v", err)
	}
	many := knowledge.NoteInput{Scope: domain.NoteScopeProject, Title: "t", Body: "b", Tags: make([]string, knowledge.MaxNoteTags+1)}
	if _, err := e.notes.Write(ctx, j, many); !errors.Is(err, knowledge.ErrNoteTooLarge) {
		t.Errorf("many tags: %v", err)
	}
	gone := e.job(t, 6, "developer", domain.Goal{ID: uuid.New()})
	if _, err := e.notes.Write(ctx, gone, knowledge.NoteInput{Scope: domain.NoteScopeGoal, Title: "t", Body: "b"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("note of an unknown goal: %v", err)
	}
}

func TestNoteExpirySweep(t *testing.T) {
	e := newNotesEnv(t)
	ctx := t.Context()
	j1, j2 := e.job(t, 1, "developer", e.g1), e.job(t, 2, "developer", e.g2)
	goalNote, err := e.notes.Write(ctx, j1, knowledge.NoteInput{Scope: domain.NoteScopeGoal, Title: "goal one note", Body: "alpha"})
	must(t, err)
	_, err = e.notes.Write(ctx, j2, knowledge.NoteInput{Scope: domain.NoteScopeGoal, Title: "goal two note", Body: "beta"})
	must(t, err)
	_, err = e.notes.Write(ctx, j1, knowledge.NoteInput{Scope: domain.NoteScopeProject, Title: "project note", Body: "gamma"})
	must(t, err)

	// Open goals: nothing expires, however long we wait.
	e.clock.Advance(100 * 24 * time.Hour)
	if n, err := e.notes.Sweep(ctx); err != nil || n != 0 {
		t.Fatalf("sweep with open goals = %d, %v", n, err)
	}
	// Goal one is canceled: its notes expire a week after the cancellation.
	must(t, e.st.Goals().UpdateStatus(ctx, e.project.AccountID, e.g1.ID, domain.GoalDraft, domain.GoalCanceled))
	closed, _ := e.st.Goals().Get(ctx, e.project.AccountID, e.g1.ID)
	if n, err := e.notes.Sweep(ctx); err != nil || n != 0 {
		t.Fatalf("sweep on the day = %d, %v", n, err)
	}
	stamped, _ := e.st.Knowledge().Get(ctx, e.project.AccountID, goalNote.ID)
	if !stamped.ExpiresAt.Equal(closed.UpdatedAt.Add(knowledge.NoteGrace)) {
		t.Errorf("expires_at = %v, want close + 7 days", stamped.ExpiresAt)
	}
	e.clock.Advance(stamped.ExpiresAt.Add(-time.Minute).Sub(e.clock.Now()))
	if n, _ := e.notes.Sweep(ctx); n != 0 {
		t.Errorf("a note expired early")
	}
	e.clock.Advance(2 * time.Minute)
	if n, err := e.notes.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("sweep after the week = %d, %v", n, err)
	}
	left := e.entries(t)
	if len(left) != 2 {
		t.Errorf("notes left = %d, want goal two's and the project note", len(left))
	}
	// A note written for a goal that is closed already expires a week later.
	late, err := e.notes.Write(ctx, e.job(t, 7, "developer", e.g1), knowledge.NoteInput{Scope: domain.NoteScopeGoal, Title: "late", Body: "delta"})
	must(t, err)
	if !late.ExpiresAt.Equal(e.clock.Now().Add(knowledge.NoteGrace)) {
		t.Errorf("late note expires %v", late.ExpiresAt)
	}
}

func TestRunSweep(t *testing.T) {
	e := newNotesEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { e.notes.RunSweep(ctx, time.Hour); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunSweep did not stop")
	}
}

func TestNoteOfAFinishedJobIsRefused(t *testing.T) {
	e := newNotesEnv(t)
	j := e.job(t, 1, "developer", e.g1)
	_, err := e.st.Jobs().Complete(t.Context(), j.AccountID, j.ID, j.WorkerID, store.JobResult{Status: domain.JobSucceeded})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.notes.Write(t.Context(), j, knowledge.NoteInput{Scope: domain.NoteScopeProject, Title: "late", Body: "too late"}); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("err = %v, want a lost lease", err)
	}
}
