package tracker_test

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/gates"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/tracker"
	"github.com/forecrew/forecrew/internal/tracker/trackertest"
)

func TestScenarios(t *testing.T) {
	for _, name := range names() {
		t.Run(name, func(t *testing.T) {
			st := testkit.NewStore(nil)
			acc := domain.Account{Name: "default"}
			must(t, st.Accounts().Create(t.Context(), &acc))
			scenarios()[name](t, newWorld(t, st, acc.ID))
		})
	}
}

// A new tracker is one package: the core must know none of them.
func TestCoreImportsNoAdapter(t *testing.T) {
	files, err := filepath.Glob("*.go")
	must(t, err)
	const module = "github.com/forecrew/forecrew/"
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		must(t, err)
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			must(t, err)
			if strings.HasPrefix(path, module+"internal/tracker/") || path == module+"internal/github" {
				t.Errorf("%s imports %s", name, path)
			}
		}
	}
	if len(files) == 0 {
		t.Fatal("no files")
	}
}

func TestRegistry(t *testing.T) {
	r := tracker.NewRegistry()
	f := trackertest.NewFake("s")
	r.Register("b", f.Factory())
	r.Register("a", f.Factory())
	if got := r.Kinds(); len(got) != 2 || got[0] != "a" {
		t.Errorf("Kinds = %v", got)
	}
	if ad, err := r.Build(domain.TrackerConnection{Kind: "a"}); err != nil || ad != tracker.Adapter(f) {
		t.Errorf("Build = %v, %v", ad, err)
	}
	if _, err := r.Build(domain.TrackerConnection{Kind: "c"}); !errors.Is(err, tracker.ErrUnknownKind) {
		t.Errorf("unknown kind: %v", err)
	}
}

func TestCheckTriggers(t *testing.T) {
	all := tracker.Capabilities{Assignment: true, Labels: true, Columns: true}
	for _, tt := range []struct {
		name string
		tr   domain.TrackerTriggers
		caps tracker.Capabilities
		ok   bool
	}{
		{"all", domain.TrackerTriggers{Assignee: "a", Label: "l", State: "s"}, all, true},
		{"assignee", domain.TrackerTriggers{Assignee: "a"}, tracker.Capabilities{Labels: true}, false},
		{"label", domain.TrackerTriggers{Label: "l"}, tracker.Capabilities{Assignment: true}, false},
		{"state", domain.TrackerTriggers{State: "s"}, tracker.Capabilities{Transitions: true}, false},
	} {
		err := tracker.CheckTriggers(domain.TrackerConnection{Kind: "x", Triggers: tt.tr}, tt.caps)
		if (err == nil) != tt.ok || (err != nil && !errors.Is(err, tracker.ErrInvalid)) {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
}

func TestDeliverGoesThroughTheAdaptersOfAKind(t *testing.T) {
	st := testkit.NewStore(nil)
	acc := domain.Account{Name: "default"}
	must(t, st.Accounts().Create(t.Context(), &acc))
	w := newWorld(t, st, acc.ID)
	w.route(t, w.app.ID, domain.TrackerMatch{}, nil)
	w.fake.Put(labeled("A-1", w.ann))
	body, sig := w.fake.Payload("d", "A-1")
	h := http.Header{}
	h.Set(trackertest.SignatureHeader, "00")
	if err := w.svc.Deliver(t.Context(), trackertest.Kind, tracker.Request{Header: h, Body: body}); !errors.Is(err, tracker.ErrBadSignature) {
		t.Errorf("forged: %v", err)
	}
	w.noLink(t, "A-1")
	h.Set(trackertest.SignatureHeader, sig)
	must(t, w.svc.Deliver(t.Context(), trackertest.Kind, tracker.Request{Header: h, Body: body}))
	w.noLink(t, "A-1") // the caller does not wait for the tracker
	w.svc.Drain(t.Context())
	if l := w.link(t, "A-1"); l.Status != domain.LinkActive {
		t.Errorf("link = %+v", l)
	}
	if w.svc.Registry() == nil {
		t.Error("no registry")
	}
}

// A full queue drops the hint, not the delivery: polling recovers it.
func TestDeliverWithAFullQueue(t *testing.T) {
	st := testkit.NewStore(nil)
	acc := domain.Account{Name: "default"}
	must(t, st.Accounts().Create(t.Context(), &acc))
	w := newWorld(t, st, acc.ID)
	h := http.Header{}
	body, sig := w.fake.Payload("d", "A-1")
	h.Set(trackertest.SignatureHeader, sig)
	for range 300 {
		must(t, w.svc.Deliver(t.Context(), trackertest.Kind, tracker.Request{Header: h, Body: body}))
	}
	w.svc.Drain(t.Context())
}

// flakyGates fails the next Resolve.
type flakyGates struct {
	gates.Service
	fail bool
}

func (f *flakyGates) Resolve(ctx context.Context, by domain.Actor, id uuid.UUID, d domain.GateStatus, feedback string) (domain.Gate, error) {
	if f.fail {
		f.fail = false
		return domain.Gate{}, errors.New("the database went away")
	}
	return f.Service.Resolve(ctx, by, id, d, feedback)
}

// An answer that fails is taken back, and the same comment cannot answer the
// next question when it is read again.
func TestAReplyThatFailsAnswersNothingLater(t *testing.T) {
	st := testkit.NewStore(nil)
	acc := domain.Account{Name: "default"}
	must(t, st.Accounts().Create(t.Context(), &acc))
	flaky := &flakyGates{}
	w := newWorld(t, st, acc.ID, func(c *tracker.Config) {
		flaky.Service = c.Gates
		c.Gates = flaky
	})
	ctx := t.Context()
	w.route(t, w.app.ID, domain.TrackerMatch{}, nil)
	w.put(t, labeled("A-1", w.ann))
	q := w.question(t, w.link(t, "A-1"), "Which?")
	flaky.fail = true
	w.fake.PersonComment("A-1", w.ann, "@forecrew reply Postgres")
	if err := w.svc.Hint(ctx, w.conn, tracker.Event{Key: "A-1"}); err == nil {
		t.Fatal("a failed answer was not reported")
	}
	must(t, w.svc.Hint(ctx, w.conn, tracker.Event{Key: "A-1"})) // read again: claimed
	if g, _ := w.gates.Get(ctx, w.account, q.ID); g.Status != domain.GatePending {
		t.Fatalf("the comment answered after all: %+v", g)
	}
	w.sync(t)
	text := comments(w.fake.Ticket("A-1"))
	if strings.Contains(text, "is answered") || !strings.Contains(text, "did not go through") {
		t.Errorf("comments = %q", text)
	}
	w.fake.PersonComment("A-1", w.ann, "@forecrew reply Postgres, again")
	w.hint(t, "A-1")
	if g, _ := w.gates.Get(ctx, w.account, q.ID); g.Status != domain.GateApproved || !strings.Contains(g.Feedback, "again") {
		t.Errorf("a new reply: %+v", g)
	}
}

// The bounds apply to what an adapter returns.
func TestTicketsAreBounded(t *testing.T) {
	st := testkit.NewStore(nil)
	acc := domain.Account{Name: "default"}
	must(t, st.Accounts().Create(t.Context(), &acc))
	w := newWorld(t, st, acc.ID)
	w.route(t, w.app.ID, domain.TrackerMatch{}, nil)
	k := labeled("A-1", w.ann)
	k.Title = strings.Repeat("é", 500) + "\n\nsecond line"
	k.Body = strings.Repeat("x", 1<<20)
	for i := range 300 {
		k.Comments = append(k.Comments, tracker.Comment{ID: strconv.Itoa(i), Body: strings.Repeat("y", 1<<16), At: time.Now()})
	}
	w.put(t, k)
	l := w.link(t, "A-1")
	g := w.goal(t, l)
	if len(g.Title) > 200 || strings.Contains(g.Title, "\n") || len(g.Body) > 65<<10 {
		t.Errorf("title %d bytes, body %d bytes", len(g.Title), len(g.Body))
	}
	if !strings.HasSuffix(g.Title, "é") {
		t.Errorf("title cut inside a character: %q", g.Title[len(g.Title)-4:])
	}
	if l.CommentCursor != "299" || l.GoalID == uuid.Nil() {
		t.Errorf("link = %+v", l)
	}
}
