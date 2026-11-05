package verification_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
	"github.com/forecrew/forecrew/internal/verification"
)

// pubFixture adds a tracker with the PR of the fixture's change, an evidence
// store and a log.
type pubFixture struct {
	*fixture
	tr  *testkit.Tracker
	ev  *testkit.Evidence
	log *bytes.Buffer
	pub *verification.Publisher
}

func newPubFixture(t *testing.T) *pubFixture {
	t.Helper()
	f := newFixture(t)
	tr := testkit.NewTracker(f.clock.Now)
	tr.Install("acme/app")
	must(t, tr.AddBranch("acme/app", "agent/task-1"))
	pr, err := tr.CreatePR(t.Context(), "acme/app", github.PRInput{Title: "T", Head: "agent/task-1", Base: "main"})
	must(t, err)
	if pr.Number != f.change.Number {
		t.Fatalf("PR number %d", pr.Number)
	}
	ev := testkit.NewEvidence()
	var buf bytes.Buffer
	pub := &verification.Publisher{Store: f.st, Tracker: tr, Evidence: ev, Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	return &pubFixture{fixture: f, tr: tr, ev: ev, log: &buf, pub: pub}
}

func (f *pubFixture) markerComments() []github.Comment {
	f.t.Helper()
	all, err := f.tr.ListComments(f.t.Context(), "acme/app", f.change.Number, time.Time{})
	must(f.t, err)
	var out []github.Comment
	for _, c := range all {
		if strings.Contains(c.Body, verification.Marker) {
			out = append(out, c)
		}
	}
	return out
}

func (f *pubFixture) publish(rep verification.Report) domain.VerificationReport {
	f.t.Helper()
	stored, err := f.pub.Publish(f.t.Context(), f.change, rep)
	must(f.t, err)
	return stored
}

func TestPublishStoresRendersAndPublishes(t *testing.T) {
	f := newPubFixture(t)
	f.add(run{env: "linux-go", cell: linux(), results: pass("a::T1")})
	rep := f.build(verification.Inputs{Expected: []verification.Expected{req("linux-go", linux(), "unit")}})
	stored := f.publish(rep)

	got, err := f.st.VerificationReports().GetBySHA(t.Context(), f.acc.ID, f.change.ID, sha)
	if err != nil || got.ID != stored.ID || got.Status != domain.VerificationPassed {
		t.Fatalf("stored = %+v, %v", got, err)
	}
	if parsed, err := verification.Parse(got.Report); err != nil || parsed.Status != domain.VerificationPassed {
		t.Fatalf("stored report = %s, %v", got.Report, err)
	}
	art, err := f.st.Artifacts().Get(t.Context(), f.acc.ID, got.ArtifactID)
	if err != nil || art.JobID.String() != "00000000-0000-0000-0000-000000000000" || art.Name != "verification/"+sha+".md" || !strings.HasPrefix(art.ContentType, "text/markdown") {
		t.Fatalf("artifact = %+v, %v", art, err)
	}
	obj, ok := f.ev.Object(art.StorageKey)
	if !ok || string(obj.Data) != verification.Render(rep) || int64(len(obj.Data)) != art.Size {
		t.Fatalf("object = %q, %v", obj.Data, ok)
	}
	comments := f.markerComments()
	if len(comments) != 1 || comments[0].Body != verification.Render(rep) {
		t.Fatalf("comments = %+v", comments)
	}
	run, ok := f.tr.CheckRun("acme/app", sha, verification.CheckName)
	if !ok || run.Conclusion != github.ConclusionSuccess || run.Title != "Verified" || strings.Contains(run.Summary, verification.Marker) || !strings.Contains(run.Summary, "Passed") {
		t.Fatalf("check run = %+v, %v", run, ok)
	}
}

func TestPublishConclusions(t *testing.T) {
	tests := []struct {
		name string
		runs []run
		want github.CheckConclusion
	}{
		{"passed is success", []run{{env: "e", cell: linux(), results: pass("a::T1")}}, github.ConclusionSuccess},
		{"failed is failure", []run{{env: "e", cell: linux(), results: fail("a::T1")}}, github.ConclusionFailure},
		{"incomplete is neutral", nil, github.ConclusionNeutral},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPubFixture(t)
			for _, r := range tt.runs {
				f.add(r)
			}
			f.publish(f.build(verification.Inputs{}))
			if run, ok := f.tr.CheckRun("acme/app", sha, verification.CheckName); !ok || run.Conclusion != tt.want {
				t.Errorf("check run = %+v, %v; want %s", run, ok, tt.want)
			}
		})
	}
}

// TestPublishUpdatesTheCommentInPlace: a second build edits the comment, and
// the report and its artifact are replaced, not added.
func TestPublishUpdatesTheCommentInPlace(t *testing.T) {
	f := newPubFixture(t)
	exp := []verification.Expected{req("linux-go", linux(), "unit"), req("mac-go", mac(), "unit")}
	f.add(run{env: "linux-go", cell: linux(), results: pass("a::T1")})
	first := f.publish(f.build(verification.Inputs{Expected: exp}))
	if len(f.markerComments()) != 1 || first.Status != domain.VerificationIncomplete {
		t.Fatalf("first: %+v, %d comments", first, len(f.markerComments()))
	}
	firstComment := f.markerComments()[0]
	firstArtifact := first.ArtifactID

	f.add(run{env: "mac-go", cell: mac(), results: pass("a::T1")})
	second := f.publish(f.build(verification.Inputs{Expected: exp}))
	comments := f.markerComments()
	if len(comments) != 1 || comments[0].ID != firstComment.ID || !strings.Contains(comments[0].Body, "passed") || comments[0].Body == firstComment.Body {
		t.Fatalf("the comment was not updated in place: %+v", comments)
	}
	if second.ID != first.ID || second.Status != domain.VerificationPassed || second.ArtifactID == firstArtifact {
		t.Fatalf("second = %+v, first %+v", second, first)
	}
	if _, err := f.st.Artifacts().Get(t.Context(), f.acc.ID, firstArtifact); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the replaced artifact is still there: %v", err)
	}
	if keys := f.ev.Keys(); len(keys) != 1 {
		t.Errorf("objects = %v, want only the new one", keys)
	}
	if run, _ := f.tr.CheckRun("acme/app", sha, verification.CheckName); run.Conclusion != github.ConclusionSuccess {
		t.Errorf("check run = %+v", run)
	}

	// The same report again changes nothing, and uploads nothing.
	third := f.publish(f.build(verification.Inputs{Expected: exp}))
	if third.ArtifactID != second.ArtifactID || len(f.ev.Keys()) != 1 || len(f.markerComments()) != 1 {
		t.Errorf("an identical publish changed things: %+v, %v", third, f.ev.Keys())
	}
}

func TestPublishNewHeadKeepsOneComment(t *testing.T) {
	f := newPubFixture(t)
	f.add(run{env: "e", cell: linux(), results: pass("a::T1")})
	f.publish(f.build(verification.Inputs{}))
	other, err := f.b.Build(t.Context(), f.change, "ffff0000", verification.Inputs{})
	must(t, err)
	f.publish(other)
	comments := f.markerComments()
	if len(comments) != 1 || !strings.Contains(comments[0].Body, "ffff0000") {
		t.Fatalf("comments = %+v", comments)
	}
	if _, ok := f.tr.CheckRun("acme/app", "ffff0000", verification.CheckName); !ok {
		t.Error("no check run on the new head")
	}
	if _, err := f.st.VerificationReports().GetBySHA(t.Context(), f.acc.ID, f.change.ID, sha); err != nil {
		t.Errorf("the report of the old head is gone: %v", err)
	}
}

// TestPublishIgnoresAMarkerFromSomeoneElse: a user who posts the marker first
// cannot stop the report from being published.
func TestPublishIgnoresAMarkerFromSomeoneElse(t *testing.T) {
	f := newPubFixture(t)
	_, err := f.tr.AddComment("acme/app", f.change.Number, "mallory", verification.Marker+"\nall good")
	must(t, err)
	f.add(run{env: "e", cell: linux(), results: fail("a::T1")})
	rep := f.build(verification.Inputs{})
	f.publish(rep)
	f.publish(rep)
	all := f.markerComments()
	if len(all) != 2 || all[0].Author != "mallory" || all[0].Body != verification.Marker+"\nall good" || all[1].Author != "forecrew[bot]" || all[1].Body != verification.Render(rep) {
		t.Fatalf("comments = %+v", all)
	}
	// An edit of the report goes to the App's comment.
	f.add(run{env: "e", cell: linux(), results: pass("a::T1")})
	f.publish(f.build(verification.Inputs{}))
	all = f.markerComments()
	if len(all) != 2 || all[0].Body != verification.Marker+"\nall good" || !strings.Contains(all[1].Body, "Passed") {
		t.Fatalf("comments = %+v", all)
	}
}

func TestPublishWithoutChecksPermission(t *testing.T) {
	f := newPubFixture(t)
	must(t, f.tr.ForbidChecks("acme/app"))
	f.add(run{env: "e", cell: linux(), results: pass("a::T1")})
	rep := f.build(verification.Inputs{})
	f.publish(rep)
	f.add(run{env: "e", cell: linux(), suite: "lint", results: pass("a::L1")})
	f.publish(f.build(verification.Inputs{}))
	if len(f.markerComments()) != 1 {
		t.Errorf("comment lost: %+v", f.markerComments())
	}
	if _, ok := f.tr.CheckRun("acme/app", sha, verification.CheckName); ok {
		t.Error("a check run was set without permission")
	}
	if n := strings.Count(f.log.String(), "Checks: write"); n != 1 {
		t.Errorf("the warning was logged %d times:\n%s", n, f.log)
	}
}

// failing wraps a tracker and fails one call.
type failing struct {
	verification.Tracker
	list   error
	update error
	create error
	check  error
}

func (f *failing) ListComments(ctx context.Context, repo string, n int, since time.Time) ([]github.Comment, error) {
	if f.list != nil {
		return nil, f.list
	}
	return f.Tracker.ListComments(ctx, repo, n, since)
}

func (f *failing) UpdateComment(ctx context.Context, repo string, id int64, body string) (github.Comment, error) {
	if f.update != nil {
		return github.Comment{}, f.update
	}
	return f.Tracker.UpdateComment(ctx, repo, id, body)
}

func (f *failing) CreateComment(ctx context.Context, repo string, n int, body string) (github.Comment, error) {
	if f.create != nil {
		return github.Comment{}, f.create
	}
	return f.Tracker.CreateComment(ctx, repo, n, body)
}

func (f *failing) SetCheckRun(ctx context.Context, repo string, in github.CheckRunInput) error {
	if f.check != nil {
		return f.check
	}
	return f.Tracker.SetCheckRun(ctx, repo, in)
}

func TestPublishErrors(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name string
		prep func(f *pubFixture, w *failing)
		want string
	}{
		{"list comments", func(_ *pubFixture, w *failing) { w.list = boom }, "list the comments"},
		{"create comment", func(_ *pubFixture, w *failing) { w.create = boom }, "comment on acme/app#1"},
		{"update comment", func(f *pubFixture, w *failing) {
			f.publish(f.build(verification.Inputs{}))
			f.add(run{env: "e", cell: linux(), suite: "lint", results: pass("a::L1")})
			w.update = boom
		}, "update the verification comment"},
		{"check run", func(_ *pubFixture, w *failing) { w.check = boom }, "check run"},
		{"evidence", func(f *pubFixture, _ *failing) { f.ev.PutErr = boom }, "store the rendered report"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPubFixture(t)
			f.add(run{env: "e", cell: linux(), results: pass("a::T1")})
			w := &failing{Tracker: f.tr}
			f.pub.Tracker = w
			tt.prep(f, w)
			_, err := f.pub.Publish(t.Context(), f.change, f.build(verification.Inputs{}))
			if err == nil || !strings.Contains(err.Error(), tt.want) || !errors.Is(err, boom) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

// TestPublishKeepsTheReportWhenGitHubFails: the stored report is the source of
// truth; a retry publishes it.
func TestPublishKeepsTheReportWhenGitHubFails(t *testing.T) {
	f := newPubFixture(t)
	f.add(run{env: "e", cell: linux(), results: pass("a::T1")})
	w := &failing{Tracker: f.tr, create: errors.New("down")}
	f.pub.Tracker = w
	rep := f.build(verification.Inputs{})
	if _, err := f.pub.Publish(t.Context(), f.change, rep); err == nil {
		t.Fatal("no error")
	}
	if _, err := f.st.VerificationReports().GetBySHA(t.Context(), f.acc.ID, f.change.ID, sha); err != nil {
		t.Fatalf("report not stored: %v", err)
	}
	w.create = nil
	f.publish(rep)
	if len(f.markerComments()) != 1 || len(f.ev.Keys()) != 1 {
		t.Errorf("after the retry: %d comments, %d objects", len(f.markerComments()), len(f.ev.Keys()))
	}
}

func TestPublishWithoutTrackerOrEvidence(t *testing.T) {
	f := newFixture(t)
	f.add(run{env: "e", cell: linux(), results: pass("a::T1")})
	pub := &verification.Publisher{Store: f.st}
	stored, err := pub.Publish(t.Context(), f.change, f.build(verification.Inputs{}))
	if err != nil || stored.ArtifactID.String() != "00000000-0000-0000-0000-000000000000" || stored.Status != domain.VerificationPassed {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
}

func TestPublishRejectsAnInvalidReport(t *testing.T) {
	f := newPubFixture(t)
	_, err := f.pub.Publish(t.Context(), f.change, verification.Report{Version: 1, SHA: "NOPE", Status: domain.VerificationPassed})
	if err == nil {
		t.Fatal("an invalid report was published")
	}
	if len(f.markerComments()) != 0 {
		t.Error("a comment was posted")
	}
}

func TestPublishDeletedArtifactIsRecreated(t *testing.T) {
	f := newPubFixture(t)
	f.add(run{env: "e", cell: linux(), results: pass("a::T1")})
	rep := f.build(verification.Inputs{})
	first := f.publish(rep)
	must(t, f.st.Artifacts().Delete(t.Context(), f.acc.ID, first.ArtifactID))
	second := f.publish(rep)
	if second.ArtifactID == first.ArtifactID || second.ArtifactID.String() == "00000000-0000-0000-0000-000000000000" {
		t.Errorf("artifact after retention = %+v", second)
	}
}
