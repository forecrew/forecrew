package knowledge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
)

// Note limits (job-mcp.md, Limits).
const (
	MaxNoteBody = 16 << 10
	MaxNoteTags = 10
	// NoteGrace is how long a goal's notes outlive the goal.
	NoteGrace = 7 * 24 * time.Hour
	// SweepEvery is how often expired notes are deleted.
	SweepEvery = 24 * time.Hour
)

// Errors of Notes.
var (
	// ErrNoGoal means a goal note was asked for by a job without a goal.
	ErrNoGoal = errors.New("the job has no goal: write a project note")
	// ErrNoteForbidden means the note belongs to another goal or project.
	ErrNoteForbidden = errors.New("the note is not of this job's goal or project")
	// ErrNoteTooLarge means a note body or its tags are over the limits.
	ErrNoteTooLarge = errors.New("note too large")
)

// Notes is the internal provider: agent notes in forecrewd's own store,
// indexed at once.
type Notes struct {
	Store store.Store
	Index *Index
	Now   func() time.Time
	Log   *slog.Logger
}

func (n *Notes) now() time.Time {
	if n.Now == nil {
		return time.Now().UTC()
	}
	return n.Now()
}

// NoteInput is a note a job writes.
type NoteInput struct {
	Scope domain.NoteScope
	Title string
	Body  string
	Tags  []string
	// Note replaces this note of the job's goal or project; the zero value
	// adds one.
	Note uuid.UUID
}

// Write adds or replaces a note of job j. A goal note belongs to the job's
// goal and, when that goal is closed already, expires NoteGrace from now; a
// project note never expires. A job edits only notes of its own goal or
// project.
func (n *Notes) Write(ctx context.Context, j domain.Job, in NoteInput) (domain.KnowledgeEntry, error) {
	if len(in.Body) > MaxNoteBody || len(in.Tags) > MaxNoteTags {
		return domain.KnowledgeEntry{}, ErrNoteTooLarge
	}
	e := domain.KnowledgeEntry{
		AccountID: j.AccountID, ProjectID: j.ProjectID, Provider: domain.ProviderInternal, Section: domain.SectionNotes,
		Title: strings.TrimSpace(in.Title), Body: in.Body, Scope: in.Scope, Tags: in.Tags,
		Provenance: domain.Provenance{AuthorRole: j.Role, Source: j.Ref(), Created: n.now()},
	}
	if in.Scope == domain.NoteScopeGoal {
		if j.GoalID == uuid.Nil() {
			return domain.KnowledgeEntry{}, ErrNoGoal
		}
		e.GoalID = j.GoalID
		g, err := n.Store.Goals().Get(ctx, j.AccountID, j.GoalID)
		if err != nil {
			return domain.KnowledgeEntry{}, fmt.Errorf("goal of %s: %w", j.Ref(), err)
		}
		if g.Status == domain.GoalDone || g.Status == domain.GoalCanceled {
			e.ExpiresAt = n.now().Add(NoteGrace)
		}
	}
	e.ID = domain.NewID()
	if in.Note != uuid.Nil() {
		old, err := n.Store.Knowledge().Get(ctx, j.AccountID, in.Note)
		if err != nil {
			return domain.KnowledgeEntry{}, fmt.Errorf("note %s: %w", in.Note, err)
		}
		if old.Provider != domain.ProviderInternal || old.ProjectID != j.ProjectID ||
			(old.Scope == domain.NoteScopeGoal && old.GoalID != j.GoalID) {
			return domain.KnowledgeEntry{}, ErrNoteForbidden
		}
		e.ID, e.Provenance.Created = old.ID, old.Provenance.Created
	}
	e.Path = "notes/" + e.ID.String()
	if err := n.Index.Put(ctx, &e); err != nil {
		return domain.KnowledgeEntry{}, err
	}
	return e, nil
}

// Delete removes a note, for a human. Anything but a note is refused.
func (n *Notes) Delete(ctx context.Context, accountID, id uuid.UUID) error {
	e, err := n.Store.Knowledge().Get(ctx, accountID, id)
	if err != nil {
		return err
	}
	if e.Provider != domain.ProviderInternal {
		return ErrNotNote
	}
	return n.Store.Knowledge().Delete(ctx, accountID, id)
}

// ErrNotNote means the entry is not an agent note, which is all a human can
// delete here.
var ErrNotNote = errors.New("only agent notes can be deleted; curated entries change through their provider")

// Sweep gives the notes of closed goals their expiry and deletes the notes
// that expired, in every account. It returns how many it deleted.
func (n *Notes) Sweep(ctx context.Context) (int, error) {
	accounts, err := n.Store.Accounts().List(ctx)
	if err != nil {
		return 0, err
	}
	deleted := 0
	var errs []error
	for _, a := range accounts {
		if _, err := n.Store.Knowledge().ExpireGoalNotes(ctx, a.ID, NoteGrace); err != nil {
			errs = append(errs, err)
			continue
		}
		c, err := n.Store.Knowledge().DeleteExpired(ctx, a.ID, n.now())
		deleted += c
		errs = append(errs, err)
	}
	return deleted, errors.Join(errs...)
}

// RunSweep sweeps at once and then every SweepEvery until ctx is done.
func (n *Notes) RunSweep(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = SweepEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		deleted, err := n.Sweep(ctx)
		switch {
		case err != nil && n.Log != nil:
			n.Log.ErrorContext(ctx, "sweep expired notes", "error", err)
		case deleted > 0 && n.Log != nil:
			n.Log.InfoContext(ctx, "expired notes deleted", "count", deleted)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
