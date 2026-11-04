package github_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/github"
)

func TestDispatchWorkflow(t *testing.T) {
	e := newEnv(t)
	var body map[string]any
	e.handle("POST /repos/acme/app/actions/workflows/forecrew-matrix.yml/dispatches", func(w http.ResponseWriter, r *http.Request) {
		body = readJSON(t, r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"workflow_run_id": 77, "run_url": "u", "html_url": "h"}`)
	})
	id, err := e.client.DispatchWorkflow(t.Context(), "acme/app", "forecrew-matrix.yml", "main", map[string]string{"job_id": "j1"})
	noErr(t, err)
	if id != 77 || body["ref"] != "main" || body["return_run_details"] != true || body["inputs"].(map[string]any)["job_id"] != "j1" {
		t.Errorf("id = %d, body = %v", id, body)
	}
}

func TestDispatchWorkflowErrors(t *testing.T) {
	for status, want := range map[int]error{404: github.ErrNotFound, 422: github.ErrInvalid, 403: github.ErrForbidden} {
		e := newEnv(t)
		e.handle("POST /repos/acme/app/actions/workflows/forecrew-matrix.yml/dispatches", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message": "no"}`, status)
		})
		_, err := e.client.DispatchWorkflow(t.Context(), "acme/app", "forecrew-matrix.yml", "main", nil)
		wantErr(t, err, want)
	}
	e := newEnv(t)
	_, err := e.client.DispatchWorkflow(t.Context(), "bad", "f.yml", "main", nil)
	wantErr(t, err, github.ErrInvalid)
}

const runJSON = `{"id": 5, "display_title": "forecrew-matrix j1", "status": "completed", "conclusion": "success", "head_sha": "abc", "run_attempt": 2, "html_url": "https://x/5", "created_at": "2026-03-01T12:00:00Z"}`

func TestWorkflowRuns(t *testing.T) {
	e := newEnv(t)
	e.handle("GET /repos/acme/app/actions/workflows/forecrew-matrix.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("event") != "workflow_dispatch" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		fmt.Fprintf(w, `{"total_count": 1, "workflow_runs": [%s]}`, runJSON)
	})
	e.handle("GET /repos/acme/app/actions/runs/5", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, runJSON) })
	e.handle("GET /repos/acme/app/actions/runs/6", func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) })

	runs, err := e.client.ListWorkflowRuns(t.Context(), "acme/app", "forecrew-matrix.yml")
	noErr(t, err)
	want := github.WorkflowRun{ID: 5, Title: "forecrew-matrix j1", Status: "completed", Conclusion: "success", HeadSHA: "abc", Attempt: 2, URL: "https://x/5", CreatedAt: t0}
	if len(runs) != 1 || runs[0] != want || !runs[0].Completed() {
		t.Errorf("runs = %+v", runs)
	}
	got, err := e.client.GetWorkflowRun(t.Context(), "acme/app", 5)
	noErr(t, err)
	if got != want {
		t.Errorf("run = %+v", got)
	}
	_, err = e.client.GetWorkflowRun(t.Context(), "acme/app", 6)
	wantErr(t, err, github.ErrNotFound)
	_, err = e.client.ListWorkflowRuns(t.Context(), "acme/other", "forecrew-matrix.yml")
	wantErr(t, err, github.ErrNotInstalled)
}

func TestCancelWorkflowRun(t *testing.T) {
	for status, wantOK := range map[int]bool{202: true, 409: true, 404: false} {
		e := newEnv(t)
		e.handle("POST /repos/acme/app/actions/runs/5/cancel", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"message": "x"}`)
		})
		if err := e.client.CancelWorkflowRun(t.Context(), "acme/app", 5); (err == nil) != wantOK {
			t.Errorf("status %d: err = %v", status, err)
		}
	}
	e := newEnv(t)
	wantErr(t, e.client.CancelWorkflowRun(t.Context(), "bad", 5), github.ErrInvalid)
}

func TestRunArtifactsAndDownload(t *testing.T) {
	e := newEnv(t)
	e.handle("GET /repos/acme/app/actions/runs/5/artifacts", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"total_count": 2, "artifacts": [{"id": 9, "name": "forecrew-j1", "size_in_bytes": 10, "expired": false}, {"id": 10, "name": "old", "size_in_bytes": 1, "expired": true}]}`)
	})
	e.handle("GET /repos/acme/app/actions/artifacts/9/zip", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+"/storage/9", http.StatusFound)
	})
	e.handle("GET /storage/9", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("the storage download carries credentials")
		}
		fmt.Fprint(w, "0123456789")
	})
	e.handle("GET /repos/acme/app/actions/artifacts/10/zip", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGone) })
	e.handle("GET /repos/acme/app/actions/artifacts/11/zip", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+"/storage/11", http.StatusFound)
	})
	e.handle("GET /storage/11", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "x", http.StatusForbidden) })

	list, err := e.client.ListRunArtifacts(t.Context(), "acme/app", 5)
	noErr(t, err)
	if want := []github.RunArtifact{{ID: 9, Name: "forecrew-j1", Size: 10}, {ID: 10, Name: "old", Size: 1, Expired: true}}; !slices.Equal(list, want) {
		t.Errorf("artifacts = %+v", list)
	}
	data, err := e.client.DownloadArtifact(t.Context(), "acme/app", 9, 10)
	noErr(t, err)
	if string(data) != "0123456789" {
		t.Errorf("data = %q", data)
	}
	_, err = e.client.DownloadArtifact(t.Context(), "acme/app", 9, 9)
	wantErr(t, err, github.ErrTooLarge)
	_, err = e.client.DownloadArtifact(t.Context(), "acme/app", 10, 10)
	wantErr(t, err, github.ErrGone)
	if _, err = e.client.DownloadArtifact(t.Context(), "acme/app", 11, 10); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("storage error = %v", err)
	}
	_, err = e.client.ListRunArtifacts(t.Context(), "bad", 5)
	wantErr(t, err, github.ErrInvalid)
}

// writeFileEnv fakes a file holding current (absent when empty) and returns
// what the client wrote and by which call.
func writeFileEnv(t *testing.T, current string) (e *env, wrote func() (map[string]any, string)) {
	t.Helper()
	e = newEnv(t)
	var body map[string]any
	method := ""
	e.handle("GET /repos/acme/app/contents/.github/workflows/f.yml", func(w http.ResponseWriter, _ *http.Request) {
		if current == "" {
			http.Error(w, `{"message": "Not Found"}`, http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"type": "file", "encoding": "base64", "sha": "s1", "content": %q}`, base64.StdEncoding.EncodeToString([]byte(current)))
	})
	e.handle("PUT /repos/acme/app/contents/.github/workflows/f.yml", func(w http.ResponseWriter, r *http.Request) {
		body = readJSON(t, r)
		method = "create"
		if body["sha"] != nil {
			method = "update"
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"content": {}, "commit": {}}`)
	})
	return e, func() (map[string]any, string) { return body, method }
}

func TestWriteFile(t *testing.T) {
	tests := []struct {
		name    string
		current string // empty: the file does not exist
		method  string // the write expected, empty for none
	}{
		{"new file", "", "create"},
		{"changed file", "old", "update"},
		{"same file", "new", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, wrote := writeFileEnv(t, tt.current)
			changed, err := e.client.WriteFile(t.Context(), "acme/app", "main", ".github/workflows/f.yml", []byte("new"), "Add f")
			noErr(t, err)
			body, method := wrote()
			if changed != (tt.method != "") || method != tt.method {
				t.Errorf("changed = %v, method = %q", changed, method)
			}
			if tt.method == "" {
				return
			}
			if body["branch"] != "main" || body["message"] != "Add f" || body["content"] != base64.StdEncoding.EncodeToString([]byte("new")) {
				t.Errorf("body = %v", body)
			}
			if tt.method == "update" && body["sha"] != "s1" {
				t.Errorf("sha = %v", body["sha"])
			}
		})
	}
}

func TestWriteFileErrors(t *testing.T) {
	e := newEnv(t)
	e.handle("GET /repos/acme/app/contents/a.yml", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, `{"message": "x"}`, http.StatusForbidden) })
	_, err := e.client.WriteFile(t.Context(), "acme/app", "main", "a.yml", nil, "m")
	wantErr(t, err, github.ErrForbidden)

	e = newEnv(t)
	e.handle("GET /repos/acme/app/contents/a.yml", func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) })
	e.handle("PUT /repos/acme/app/contents/a.yml", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message": "x"}`, http.StatusUnprocessableEntity)
	})
	_, err = e.client.WriteFile(t.Context(), "acme/app", "gone", "a.yml", nil, "m")
	wantErr(t, err, github.ErrInvalid)

	e = newEnv(t)
	_, err = e.client.WriteFile(t.Context(), "bad", "main", "a.yml", nil, "m")
	if !errors.Is(err, github.ErrInvalid) {
		t.Errorf("err = %v", err)
	}
}

// A failed download must not leak the signed URL GitHub redirected to.
func TestDownloadArtifactErrorsHideTheSignedURL(t *testing.T) {
	e := newEnv(t)
	e.handle("GET /repos/acme/app/actions/artifacts/9/zip", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+"/storage/9?sig=SECRET-SIGNATURE", http.StatusFound)
	})
	e.handle("GET /storage/9", func(w http.ResponseWriter, _ *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close() // the connection drops mid-request
	})
	_, err := e.client.DownloadArtifact(t.Context(), "acme/app", 9, 10)
	if err == nil || strings.Contains(err.Error(), "SECRET-SIGNATURE") || strings.Contains(err.Error(), "/storage/") {
		t.Errorf("err = %v, want one without the URL", err)
	}
}
