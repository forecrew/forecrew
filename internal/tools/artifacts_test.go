package tools_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/evidence"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/tools"
)

func (f *fixture) newArtifact(t *testing.T, j domain.Job, name, content string) domain.Artifact {
	t.Helper()
	a := domain.Artifact{AccountID: j.AccountID, ProjectID: j.ProjectID, JobID: j.ID, Name: name, ContentType: "text/plain",
		Size: int64(len(content)), StorageKey: evidence.JobKey(j.AccountID, j.ID, name)}
	must(t, f.st.Artifacts().Put(t.Context(), &a))
	must(t, f.ev.Put(t.Context(), a.StorageKey, strings.NewReader(content), a.Size, a.ContentType))
	return a
}

func TestListArtifacts(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	j := f.newJob(t, "reviewer", domain.Goal{}, domain.Task{}, 0)
	f.newArtifact(t, j, "b.log", "b")
	f.newArtifact(t, j, "a/unit.xml", "a")

	for _, ref := range []string{j.Ref(), j.ID.String()} {
		got, err := f.svc.ListArtifacts(ctx, f.by, mcp.JobRefInput{Job: ref})
		if err != nil || len(got) != 2 || got[0].Name != "a/unit.xml" {
			t.Fatalf("%s: artifacts = %+v, %v", ref, got, err)
		}
	}
	empty := f.newJob(t, "reviewer", domain.Goal{}, domain.Task{}, 0)
	if got, err := f.svc.ListArtifacts(ctx, f.by, mcp.JobRefInput{Job: empty.Ref()}); err != nil || len(got) != 0 {
		t.Fatalf("empty = %+v, %v", got, err)
	}

	_, err := f.svc.ListArtifacts(ctx, f.by, mcp.JobRefInput{Job: "JOB-99"})
	wantErr(t, err, mcp.ErrNotFound)
	_, err = f.svc.ListArtifacts(ctx, f.by, mcp.JobRefInput{Job: "later"})
	wantErr(t, err, mcp.ErrInvalidInput)
	other, _, otherJob := f.otherAccount(t)
	_, err = f.svc.ListArtifacts(ctx, other, mcp.JobRefInput{Job: j.ID.String()})
	wantErr(t, err, mcp.ErrNotFound)
	if got, err := f.svc.ListArtifacts(ctx, other, mcp.JobRefInput{Job: otherJob.ID.String()}); err != nil || len(got) != 0 {
		t.Fatalf("other account sees %+v, %v", got, err)
	}
}

func TestOpenArtifact(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	j := f.newJob(t, "reviewer", domain.Goal{}, domain.Task{}, 0)
	a := f.newArtifact(t, j, "run.log", "hello")

	got, body, err := f.svc.OpenArtifact(ctx, f.by, a.ID.String())
	must(t, err)
	data, _ := io.ReadAll(body)
	_ = body.Close()
	if got.ID != a.ID || string(data) != "hello" {
		t.Fatalf("open = %+v, %q", got, data)
	}

	_, _, err = f.svc.OpenArtifact(ctx, f.by, "not-a-uuid")
	wantErr(t, err, mcp.ErrInvalidInput)
	_, _, err = f.svc.OpenArtifact(ctx, f.by, domain.NewID().String())
	wantErr(t, err, mcp.ErrNotFound)
	other, _, _ := f.otherAccount(t)
	_, _, err = f.svc.OpenArtifact(ctx, other, a.ID.String())
	wantErr(t, err, mcp.ErrNotFound)

	// The row is there but the object is gone, or storage is down.
	must(t, f.ev.Delete(ctx, a.StorageKey))
	_, _, err = f.svc.OpenArtifact(ctx, f.by, a.ID.String())
	wantErr(t, err, mcp.ErrNotFound)
	boom := errors.New("boom")
	f.ev.GetErr = boom
	_, _, err = f.svc.OpenArtifact(ctx, f.by, a.ID.String())
	wantErr(t, err, boom)

	noStorage := tools.New(tools.Config{Store: f.st})
	_, _, err = noStorage.OpenArtifact(ctx, f.by, a.ID.String())
	wantErr(t, err, mcp.ErrUnavailable)
}
