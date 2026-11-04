package jobmcp

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
)

func TestAttachments(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	a := NewAttachments(func() time.Time { return now })
	j := domain.Job{ID: domain.NewID(), Attempts: 1}
	x, y := Attachment{Path: "x", Name: "evidence/x"}, Attachment{Path: "y", Name: "evidence/y"}
	a.Add(j, x)
	a.Add(j, y)
	// Delivered until acknowledged: a lost answer costs nothing.
	for range 2 {
		if got := a.Pending(j); !slices.Equal(got, []Attachment{x, y}) {
			t.Fatalf("pending = %v", got)
		}
	}
	a.Ack(j, []Attachment{x, {Path: "other", Name: "evidence/y"}})
	if got := a.Pending(j); !slices.Equal(got, []Attachment{y}) {
		t.Fatalf("after ack = %v", got)
	}
	// A later mark of the same name replaces the earlier one.
	moved := Attachment{Path: "z", Name: "evidence/y"}
	a.Add(j, moved)
	if got := a.Pending(j); !slices.Equal(got, []Attachment{moved}) {
		t.Fatalf("after a new mark = %v", got)
	}
	a.Ack(j, []Attachment{moved})
	if len(a.m) != 0 || a.Pending(j) != nil {
		t.Fatalf("all acknowledged, kept %v", a.m)
	}
	a.Ack(j, []Attachment{moved}) // nothing left: a repeated ack is harmless

	// A new attempt drops what the earlier one left.
	a.Add(j, x)
	next := domain.Job{ID: j.ID, Attempts: 2}
	a.Add(next, y)
	if a.Pending(j) != nil {
		t.Fatal("an earlier attempt kept its marks")
	}
	a.Drop(j.ID)
	if a.Pending(next) != nil {
		t.Fatal("dropped job kept its marks")
	}

	// Marks of a worker that vanished do not stay for ever.
	a.Add(j, x)
	now = now.Add(attachmentTTL + time.Hour)
	a.Add(domain.Job{ID: domain.NewID()}, y)
	if a.Pending(j) != nil || len(a.m) != 1 {
		t.Fatalf("stale marks kept: %v", a.m)
	}

	var none *Attachments
	none.Drop(j.ID)
	none.Ack(j, []Attachment{x})
	if none.Pending(j) != nil {
		t.Fatal("nil queue returned attachments")
	}
	if NewAttachments(nil).now().IsZero() {
		t.Fatal("no default clock")
	}
}

func TestLimiterPrunesIdleAttempts(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	l := newLimiter(func() time.Time { return now })
	old := domain.Job{ID: domain.NewID()}
	l.allow(old, Tool{Name: "x"})
	now = now.Add(2 * idleAfter)
	l.allow(domain.Job{ID: domain.NewID()}, Tool{Name: "x"})
	if _, ok := l.attempts[attemptOf(old)]; ok || len(l.attempts) != 1 {
		t.Fatalf("attempts = %v", l.attempts)
	}
}

func TestToolError(t *testing.T) {
	err := error(Errorf(CodeNotFound, "no note %d", 3))
	if err.Error() != "not_found: no note 3" {
		t.Fatalf("Error = %q", err)
	}
	if te, ok := errors.AsType[*ToolError](err); !ok || te.Code != CodeNotFound {
		t.Fatal("not a ToolError")
	}
	if res := internalError(); !res.IsError || res.StructuredContent != nil {
		t.Fatalf("internal error = %+v", res)
	}
	if _, err := (Result{Data: func() {}}).result(); err == nil {
		t.Fatal("unencodable result accepted")
	}
	c := &Call{}
	c.Notify(context.Background(), 1, "nobody asked") // no progress token: nothing to do
}
