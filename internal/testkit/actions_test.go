package testkit_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/testkit"
)

func TestZip(t *testing.T) {
	data := testkit.Zip(map[string]string{"b.txt": "bb", "a/c.txt": "c"})
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(zr.File) != 2 || zr.File[0].Name != "a/c.txt" {
		t.Fatalf("zip = %v, %v", zr, err)
	}
	f, _ := zr.File[1].Open()
	if b, _ := io.ReadAll(f); string(b) != "bb" {
		t.Errorf("content = %q", b)
	}
}

func TestFakeActionsRuns(t *testing.T) {
	f := testkit.NewFakeActions(t, nil)
	c, ctx := f.Client, t.Context()
	f.OnDispatch = func(d testkit.Dispatch) *testkit.ActionsRun {
		return &testkit.ActionsRun{Conclusion: "success", HeadSHA: "abc", Polls: 1, Artifact: testkit.Zip(map[string]string{"x": "1"}), ArtifactName: "forecrew-" + d.Inputs["job_id"]}
	}
	id, err := c.DispatchWorkflow(ctx, "acme/app", "f.yml", "main", map[string]string{"job_id": "j1"})
	if err != nil || id != 1001 {
		t.Fatalf("id = %d, %v", id, err)
	}
	if ds := f.Dispatches(); len(ds) != 1 || ds[0].Inputs["job_id"] != "j1" || ds[0].Ref != "main" {
		t.Errorf("dispatches = %+v", ds)
	}
	runs, err := c.ListWorkflowRuns(ctx, "acme/app", "f.yml")
	if err != nil || len(runs) != 1 || runs[0].Title != "forecrew-matrix j1" {
		t.Errorf("runs = %+v, %v", runs, err)
	}
	if r, _ := c.GetWorkflowRun(ctx, "acme/app", id); r.Completed() {
		t.Errorf("first read = %+v, want in progress", r)
	}
	if r, _ := c.GetWorkflowRun(ctx, "acme/app", id); !r.Completed() || r.Conclusion != "success" {
		t.Errorf("second read = %+v", r)
	}
	arts, err := c.ListRunArtifacts(ctx, "acme/app", id)
	if err != nil || len(arts) != 1 || arts[0].Name != "forecrew-j1" {
		t.Fatalf("artifacts = %+v, %v", arts, err)
	}
	run, _ := f.Run(id)
	data, err := c.DownloadArtifact(ctx, "acme/app", arts[0].ID, 1<<20)
	if err != nil || !bytes.Equal(data, run.Artifact) {
		t.Errorf("download: %v", err)
	}
	if err := c.CancelWorkflowRun(ctx, "acme/app", id); err != nil {
		t.Fatal(err)
	}
	if r, _ := c.GetWorkflowRun(ctx, "acme/app", id); r.Conclusion != "cancelled" || len(f.Canceled()) != 1 {
		t.Errorf("cancelled run = %+v", r)
	}
	if err := c.CancelWorkflowRun(ctx, "acme/app", 999); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("cancel unknown = %v", err)
	}
	if _, err := c.GetWorkflowRun(ctx, "acme/app", 999); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("get unknown = %v", err)
	}
	if _, err := c.ListRunArtifacts(ctx, "acme/app", 999); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("artifacts of unknown = %v", err)
	}
	if _, ok := f.Run(999); ok {
		t.Error("unknown run found")
	}
}

func TestFakeActionsFailures(t *testing.T) {
	f := testkit.NewFakeActions(t, nil)
	c, ctx := f.Client, t.Context()
	f.OnDispatch = func(testkit.Dispatch) *testkit.ActionsRun {
		return &testkit.ActionsRun{Artifact: []byte("z"), ArtifactName: "n", Expired: true}
	}
	id, _ := c.DispatchWorkflow(ctx, "acme/app", "f.yml", "main", nil)
	if _, err := c.DownloadArtifact(ctx, "acme/app", id, 10); !errors.Is(err, github.ErrGone) {
		t.Errorf("expired download = %v", err)
	}
	if _, err := c.DownloadArtifact(ctx, "acme/app", 999, 10); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("unknown download = %v", err)
	}
	f.GetRunStatus = 500
	if _, err := c.GetWorkflowRun(ctx, "acme/app", id); err == nil {
		t.Error("broken read succeeded")
	}
	f.DispatchStatus = 404
	if _, err := c.DispatchWorkflow(ctx, "acme/app", "f.yml", "main", nil); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("refused dispatch = %v", err)
	}
	f.DispatchStatus, f.NoRunDetails = 0, true
	if id, err := c.DispatchWorkflow(ctx, "acme/app", "f.yml", "main", nil); err != nil || id != 0 {
		t.Errorf("dispatch without details = %d, %v", id, err)
	}
	f.OnDispatch = func(testkit.Dispatch) *testkit.ActionsRun { return nil }
	if id, err := c.DispatchWorkflow(ctx, "acme/app", "f.yml", "main", nil); err != nil || id != 0 {
		t.Errorf("dispatch without run = %d, %v", id, err)
	}
}

func TestFakeActionsFiles(t *testing.T) {
	f := testkit.NewFakeActions(t, nil)
	c, ctx := f.Client, t.Context()
	changed, err := c.WriteFile(ctx, "acme/app", "main", ".github/workflows/f.yml", []byte("v1"), "Add f")
	if err != nil || !changed {
		t.Fatalf("write = %v, %v", changed, err)
	}
	if b, ok := f.File("acme/app", ".github/workflows/f.yml"); !ok || string(b) != "v1" {
		t.Errorf("file = %q, %v", b, ok)
	}
	if changed, err = c.WriteFile(ctx, "acme/app", "main", ".github/workflows/f.yml", []byte("v1"), "Add f"); err != nil || changed {
		t.Errorf("rewrite = %v, %v", changed, err)
	}
	f.SetFile("acme/app", "g.yml", []byte("old"))
	if changed, err = c.WriteFile(ctx, "acme/app", "main", "g.yml", []byte("new"), "Update g"); err != nil || !changed {
		t.Errorf("update = %v, %v", changed, err)
	}
	f.ContentsStatus = 403
	if _, err = c.WriteFile(ctx, "acme/app", "main", "g.yml", nil, "m"); !errors.Is(err, github.ErrForbidden) {
		t.Errorf("refused write = %v", err)
	}
	if _, ok := f.File("acme/app", "missing"); ok {
		t.Error("missing file found")
	}
}
