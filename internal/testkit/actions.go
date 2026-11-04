package testkit

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/github"
)

var (
	keyOnce sync.Once
	keyPEM  []byte
)

// appKey returns a GitHub App private key, generated once per test binary.
func appKey() []byte {
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	})
	return keyPEM
}

// Zip returns a zip archive of files (name -> content), like the artifact of
// a workflow run.
func Zip(files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		w, err := zw.Create(name)
		if err == nil {
			_, err = w.Write([]byte(files[name]))
		}
		if err != nil {
			panic(err)
		}
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// Dispatch is a workflow_dispatch request the fake GitHub received.
type Dispatch struct {
	Repo, File, Ref string
	Inputs          map[string]string
}

// ActionsRun is a workflow run of the fake GitHub. Its status is
// in_progress for the first Polls reads and completed afterwards.
type ActionsRun struct {
	ID         int64
	Repo       string
	Title      string
	Conclusion string
	HeadSHA    string
	Attempt    int
	// Polls is how many reads of the run still answer in_progress.
	Polls int
	// Artifact is the zip of the run's artifact, ArtifactName its name; no
	// Artifact means the run uploaded none.
	Artifact     []byte
	ArtifactName string
	// Expired makes the artifact gone (HTTP 410) on download.
	Expired   bool
	Cancelled bool
	Created   time.Time
}

// FakeActions is an httptest GitHub with the Actions, contents and App
// endpoints of internal/github's Actions, and a Client pointed at it. Every
// repo is installed.
type FakeActions struct {
	// Client talks to the fake.
	Client *github.Client
	// OnDispatch decides what a dispatch starts: it returns the run, or
	// nil for no run (the dispatch is recorded all the same).
	OnDispatch func(d Dispatch) *ActionsRun
	// DispatchStatus, when not 0, answers every dispatch with it and no run.
	DispatchStatus int
	// NoRunDetails answers a dispatch with 204 and no run id, as GitHub
	// Enterprise Server does.
	NoRunDetails bool
	// ContentsStatus, when not 0, answers every read of a file with it.
	ContentsStatus int
	// GetRunStatus, when not 0, answers every read of a run with it.
	GetRunStatus int

	mu         sync.Mutex
	now        func() time.Time
	nextID     int64
	runs       map[int64]*ActionsRun
	dispatches []Dispatch
	files      map[string][]byte
	canceled   []int64
	gets       map[int64]int
}

// NewFakeActions starts the fake; it stops with the test.
func NewFakeActions(t testing.TB, now func() time.Time) *FakeActions {
	t.Helper()
	f := &FakeActions{now: orNow(now), nextID: 1000, runs: map[int64]*ActionsRun{}, files: map[string][]byte{}, gets: map[int64]int{}}
	srv := httptest.NewServer(f.routes())
	t.Cleanup(srv.Close)
	c, err := github.New(github.Config{AppID: 1, PrivateKey: appKey(), BaseURL: srv.URL, Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	f.Client = c
	return f
}

// Dispatches returns the dispatch requests received so far.
func (f *FakeActions) Dispatches() []Dispatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.dispatches)
}

// Canceled returns the ids of the runs cancelled so far.
func (f *FakeActions) Canceled() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.canceled)
}

// File returns the content written at path of repo, if any.
func (f *FakeActions) File(repo, path string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[repo+":"+path]
	return b, ok
}

// SetFile sets the content of path in repo.
func (f *FakeActions) SetFile(repo, path string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[repo+":"+path] = content
}

// Run returns a copy of a run.
func (f *FakeActions) Run(id int64) (ActionsRun, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return ActionsRun{}, false
	}
	return *r, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (r *ActionsRun) json(status string) map[string]any {
	m := map[string]any{
		"id": r.ID, "display_title": r.Title, "status": status, "head_sha": r.HeadSHA, "run_attempt": max(r.Attempt, 1),
		"html_url": fmt.Sprintf("https://github.com/%s/actions/runs/%d", r.Repo, r.ID), "created_at": r.Created.Format(time.RFC3339),
	}
	if status == "completed" {
		m["conclusion"] = r.Conclusion
	}
	return m
}

func (f *FakeActions) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"id": 42})
	})
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		// #nosec G101 -- a fake token
		writeJSON(w, http.StatusCreated, map[string]any{"token": "ghs_fake_actions", "expires_at": f.now().Add(time.Hour).Format(time.RFC3339)})
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/actions/workflows/{file}/dispatches", f.dispatch)
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/workflows/{file}/runs", f.listRuns)
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runs/{id}", f.getRun)
	mux.HandleFunc("POST /repos/{owner}/{repo}/actions/runs/{id}/cancel", f.cancelRun)
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runs/{id}/artifacts", f.listArtifacts)
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/artifacts/{id}/zip", f.artifactRedirect)
	mux.HandleFunc("GET /storage/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_, _ = w.Write(f.runs[runID(r)].Artifact)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/{path...}", f.getContents)
	mux.HandleFunc("PUT /repos/{owner}/{repo}/contents/{path...}", f.putContents)
	return mux
}

func runID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func (f *FakeActions) listRuns(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	runs := []map[string]any{}
	for _, id := range slices.Backward(slices.Sorted(maps.Keys(f.runs))) {
		if run := f.runs[id]; run.Repo == repo {
			runs = append(runs, run.json("in_progress"))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(runs), "workflow_runs": runs})
}

func (f *FakeActions) cancelRun(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.runs[runID(r)]
	if !ok {
		http.NotFound(w, r)
		return
	}
	run.Cancelled = true
	f.canceled = append(f.canceled, run.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{})
}

func (f *FakeActions) listArtifacts(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.runs[runID(r)]
	if !ok {
		http.NotFound(w, r)
		return
	}
	list := []map[string]any{}
	if run.Artifact != nil {
		list = append(list, map[string]any{"id": run.ID, "name": run.ArtifactName, "size_in_bytes": len(run.Artifact), "expired": false})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(list), "artifacts": list})
}

func (f *FakeActions) artifactRedirect(w http.ResponseWriter, r *http.Request) {
	id := runID(r)
	f.mu.Lock()
	run, ok := f.runs[id]
	expired := ok && run.Expired
	f.mu.Unlock()
	switch {
	case !ok:
		http.NotFound(w, r)
	case expired:
		w.WriteHeader(http.StatusGone)
	default:
		// #nosec G710 -- the fake redirects to its own storage path
		http.Redirect(w, r, fmt.Sprintf("http://%s/storage/%d", r.Host, id), http.StatusFound)
	}
}

func contentsKey(r *http.Request) string {
	return r.PathValue("owner") + "/" + r.PathValue("repo") + ":" + r.PathValue("path")
}

func (f *FakeActions) getContents(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ContentsStatus != 0 {
		writeJSON(w, f.ContentsStatus, map[string]string{"message": "refused"})
		return
	}
	b, ok := f.files[contentsKey(r)]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"type": "file", "encoding": "base64", "sha": "sha1", "content": base64.StdEncoding.EncodeToString(b)})
}

func (f *FakeActions) putContents(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content []byte `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[contentsKey(r)] = body.Content
	writeJSON(w, http.StatusCreated, map[string]any{"content": map[string]any{}, "commit": map[string]any{}})
}

func (f *FakeActions) dispatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Ref    string            `json:"ref"`
		Inputs map[string]string `json:"inputs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d := Dispatch{Repo: r.PathValue("owner") + "/" + r.PathValue("repo"), File: r.PathValue("file"), Ref: body.Ref, Inputs: body.Inputs}
	f.mu.Lock()
	f.dispatches = append(f.dispatches, d)
	status, noDetails, hook := f.DispatchStatus, f.NoRunDetails, f.OnDispatch
	f.mu.Unlock()
	if status != 0 {
		writeJSON(w, status, map[string]string{"message": "refused"})
		return
	}
	var run *ActionsRun
	if hook != nil {
		run = hook(d)
	}
	if run == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	f.mu.Lock()
	f.nextID++
	run.ID, run.Repo, run.Created = f.nextID, d.Repo, f.now()
	if run.Title == "" {
		run.Title = "forecrew-matrix " + d.Inputs["job_id"]
	}
	f.runs[run.ID] = run
	f.mu.Unlock()
	if noDetails {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflow_run_id": run.ID})
}

func (f *FakeActions) getRun(w http.ResponseWriter, r *http.Request) {
	id := runID(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.GetRunStatus != 0 {
		writeJSON(w, f.GetRunStatus, map[string]string{"message": "broken"})
		return
	}
	run, ok := f.runs[id]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	status := "completed"
	if f.gets[id]++; f.gets[id] <= run.Polls && !run.Cancelled {
		status = "in_progress"
	}
	if run.Cancelled {
		run.Conclusion = "cancelled"
	}
	writeJSON(w, http.StatusOK, run.json(status))
}
