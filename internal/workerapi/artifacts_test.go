package workerapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/evidence"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/testkit"
)

const maxArtifact = 64

func withEvidence(ev evidence.Store) option {
	return func(c *Config) { c.Evidence, c.MaxArtifactSize = ev, maxArtifact }
}

// upload PUTs body as an artifact of job; a nil body sends no Content-Length.
func (e *env) upload(t *testing.T, token string, job uuid.UUID, name string, body io.Reader, contentType string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, e.http.URL+"/worker/v1/jobs/"+job.String()+"/artifacts/"+name, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := e.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// runningJob leases a job to a new worker.
func (e *env) runningJob(t *testing.T, name string) (domain.Job, string) {
	t.Helper()
	w, token := e.addWorker(t, name)
	j := e.newJob(t, jobOpts{})
	if status, _ := e.poll(t, token); status != http.StatusOK {
		t.Fatalf("poll status = %d", status)
	}
	if got := e.job(t, j.ID); got.WorkerID != w.ID {
		t.Fatalf("job leased to %s", got.WorkerID)
	}
	return j, token
}

func (e *env) artifacts(t *testing.T, job uuid.UUID) []domain.Artifact {
	t.Helper()
	list, err := e.st.Artifacts().ListByJob(t.Context(), e.account, job)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestUploadArtifact(t *testing.T) {
	ev := testkit.NewEvidence()
	e := newEnv(t, withEvidence(ev))
	j, token := e.runningJob(t, "box")

	status, body := e.upload(t, token, j.ID, "reports/unit.xml", strings.NewReader("<testsuite/>"), "text/xml")
	wantStatus(t, status, http.StatusCreated, body)
	var got artifactResponse
	decodeBody(t, body, &got)
	sum := sha256.Sum256([]byte("<testsuite/>"))
	if got.Name != "reports/unit.xml" || got.Size != 12 || got.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("response = %s", body)
	}
	list := e.artifacts(t, j.ID)
	if len(list) != 1 || list[0].ID.String() != got.ID || list[0].ProjectID != e.project.ID || list[0].ContentType != "text/xml" ||
		!strings.HasPrefix(list[0].StorageKey, evidence.JobKey(e.account, j.ID, "reports/unit.xml")+"~") {
		t.Fatalf("rows = %+v", list)
	}
	if o, ok := ev.Object(list[0].StorageKey); !ok || string(o.Data) != "<testsuite/>" || o.ContentType != "text/xml" {
		t.Fatalf("stored object = %+v, %v", o, ok)
	}

	// Uploading the name again replaces the artifact.
	status, body = e.upload(t, token, j.ID, "reports/unit.xml", strings.NewReader("v2"), "")
	wantStatus(t, status, http.StatusCreated, body)
	var again artifactResponse
	decodeBody(t, body, &again)
	list = e.artifacts(t, j.ID)
	if again.ID != got.ID || len(list) != 1 || list[0].Size != 2 || list[0].ContentType != DefaultContentType || len(ev.Keys()) != 1 {
		t.Fatalf("after re-upload: %s, rows %+v, keys %v", body, list, ev.Keys())
	}
	if o, _ := ev.Object(list[0].StorageKey); string(o.Data) != "v2" {
		t.Fatalf("object = %q", o.Data)
	}

	// An empty file and one of exactly the limit are fine.
	for name, size := range map[string]int{"empty.txt": 0, "full.bin": maxArtifact} {
		status, body = e.upload(t, token, j.ID, name, strings.NewReader(strings.Repeat("x", size)), "")
		wantStatus(t, status, http.StatusCreated, body)
	}
}

func TestUploadArtifactTooLarge(t *testing.T) {
	ev := testkit.NewEvidence()
	e := newEnv(t, withEvidence(ev))
	j, token := e.runningJob(t, "box")
	big := strings.Repeat("x", maxArtifact+1)

	// Declared length over the limit.
	status, body := e.upload(t, token, j.ID, "big.log", strings.NewReader(big), "")
	wantStatus(t, status, http.StatusRequestEntityTooLarge, body)
	if code := errorCode(t, body); code != "too_large" {
		t.Fatalf("code = %s", code)
	}
	if len(ev.Keys()) != 0 || len(e.artifacts(t, j.ID)) != 0 {
		t.Fatalf("something was stored: keys %v, rows %v", ev.Keys(), e.artifacts(t, j.ID))
	}

	// A failed replacement leaves the old artifact alone.
	wantStatus(t, mustUpload(t, e, token, j.ID, "a.log", "old"), http.StatusCreated, nil)
	status, body = e.upload(t, token, j.ID, "a.log", strings.NewReader(big), "")
	wantStatus(t, status, http.StatusRequestEntityTooLarge, body)
	if o, _ := ev.Object(e.artifacts(t, j.ID)[0].StorageKey); string(o.Data) != "old" {
		t.Fatalf("old artifact = %q", o.Data)
	}
}

func mustUpload(t *testing.T, e *env, token string, job uuid.UUID, name, content string) int {
	t.Helper()
	status, _ := e.upload(t, token, job, name, strings.NewReader(content), "")
	return status
}

func TestUploadArtifactNeedsLease(t *testing.T) {
	ev := testkit.NewEvidence()
	e := newEnv(t, withEvidence(ev))
	j, token := e.runningJob(t, "box")
	_, other := e.addWorker(t, "other")
	queued := e.newJob(t, jobOpts{needs: []string{"unavailable-label"}})

	tests := []struct {
		name  string
		token string
		job   uuid.UUID
		want  int
		code  string
	}{
		{"no token", "", j.ID, http.StatusUnauthorized, "unauthorized"},
		{"other worker", other, j.ID, http.StatusConflict, "lease_lost"},
		{"queued job", token, queued.ID, http.StatusConflict, "lease_lost"},
		{"unknown job", token, domain.NewID(), http.StatusNotFound, "not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := e.upload(t, tt.token, tt.job, "a.txt", strings.NewReader("x"), "")
			wantStatus(t, status, tt.want, body)
			if code := errorCode(t, body); code != tt.code {
				t.Errorf("code = %s, want %s", code, tt.code)
			}
		})
	}

	// After the job completed the lease is gone too.
	status, body := e.call(t, token, jobPath(j.ID, "complete"), map[string]any{"status": "failed", "error": "x"})
	wantStatus(t, status, http.StatusOK, body)
	status, body = e.upload(t, token, j.ID, "late.txt", strings.NewReader("x"), "")
	wantStatus(t, status, http.StatusConflict, body)

	if len(ev.Keys()) != 0 || len(e.artifacts(t, j.ID)) != 0 {
		t.Fatalf("refused uploads stored something: %v", ev.Keys())
	}
}

func TestUploadArtifactBadRequests(t *testing.T) {
	e := newEnv(t, withEvidence(testkit.NewEvidence()))
	j, token := e.runningJob(t, "box")
	for _, name := range []string{".hidden", "a%20b", "dir/.env", strings.Repeat("a", 256)} {
		status, body := e.upload(t, token, j.ID, name, strings.NewReader("x"), "")
		wantStatus(t, status, http.StatusBadRequest, body)
	}
	status, body := e.upload(t, token, uuid.UUID{}, "a.txt", strings.NewReader("x"), "")
	wantStatus(t, status, http.StatusNotFound, body)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, e.http.URL+"/worker/v1/jobs/not-a-uuid/artifacts/a.txt", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("bad job id: status %d", resp.StatusCode)
	}

	// A content type that is not a media type becomes the default.
	if s, b := e.upload(t, token, j.ID, "t.bin", strings.NewReader("x"), "not a media type;;"); s != http.StatusCreated {
		t.Fatalf("status %d: %s", s, b)
	}
	if list := e.artifacts(t, j.ID); len(list) != 1 || list[0].ContentType != DefaultContentType {
		t.Fatalf("rows = %+v", list)
	}
}

func TestUploadArtifactUnavailable(t *testing.T) {
	e := newEnv(t) // no evidence storage
	j, token := e.runningJob(t, "box")
	status, body := e.upload(t, token, j.ID, "a.txt", strings.NewReader("x"), "")
	wantStatus(t, status, http.StatusServiceUnavailable, body)
	if code := errorCode(t, body); code != "unavailable" {
		t.Fatalf("code = %s", code)
	}
	if got := New(Config{}).cfg.MaxArtifactSize; got != config.DefaultMaxArtifactSize {
		t.Errorf("default max = %d", got)
	}
}

func TestUploadArtifactStorageFailure(t *testing.T) {
	ev := testkit.NewEvidence()
	ev.PutErr = errors.New("disk on fire")
	e := newEnv(t, withEvidence(ev))
	j, token := e.runningJob(t, "box")
	status, body := e.upload(t, token, j.ID, "a.txt", strings.NewReader("x"), "")
	wantStatus(t, status, http.StatusInternalServerError, body)
	if strings.Contains(string(body), "fire") || len(e.artifacts(t, j.ID)) != 0 {
		t.Fatalf("body %s, rows %v", body, e.artifacts(t, j.ID))
	}
	if !strings.Contains(e.logs.String(), "disk on fire") {
		t.Errorf("the cause is not logged: %s", e.logs)
	}
}

// The storage keys are given to the S3 client only: a storage that refuses
// them must not leak them to the worker or the logs.
func TestUploadArtifactKeepsStorageKeysSecret(t *testing.T) {
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
	}))
	t.Cleanup(s3.Close)
	const keyID, secret = "GKaccesskeyid0123456789", "s3cr3t-key-material-9f8e7d6c5b4a"
	store, err := evidence.NewS3(&config.Evidence{Endpoint: s3.URL, Bucket: "evidence", Region: "garage", PathStyle: true, AccessKeyID: keyID, SecretAccessKey: secret})
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, withEvidence(store))
	j, token := e.runningJob(t, "box")
	status, body := e.upload(t, token, j.ID, "a.txt", strings.NewReader("x"), "")
	wantStatus(t, status, http.StatusInternalServerError, body)
	for _, out := range []string{string(body), e.logs.String()} {
		if strings.Contains(out, keyID) || strings.Contains(out, secret) {
			t.Fatalf("a storage key leaked: %s", out)
		}
	}
	if !strings.Contains(e.logs.String(), "AccessDenied") && !strings.Contains(e.logs.String(), "Access Denied") {
		t.Errorf("the failure is not logged: %s", e.logs)
	}
}

func TestUploadArtifactNeedsLength(t *testing.T) {
	ev := testkit.NewEvidence()
	e := newEnv(t, withEvidence(ev))
	j, token := e.runningJob(t, "box")
	// A body of unknown length is sent chunked, without Content-Length.
	status, body := e.upload(t, token, j.ID, "a.log", io.MultiReader(strings.NewReader("x")), "")
	wantStatus(t, status, http.StatusLengthRequired, body)
	if code := errorCode(t, body); code != "length_required" || len(ev.Keys()) != 0 {
		t.Fatalf("code %s, objects %v", code, ev.Keys())
	}
	// Known length, even zero, is accepted.
	wantStatus(t, mustUpload(t, e, token, j.ID, "empty.log", ""), http.StatusCreated, nil)
}

func TestLimitedHash(t *testing.T) {
	l := &limitedHash{r: strings.NewReader("abcdef"), max: 4, h: sha256.New()}
	buf := make([]byte, 3)
	if n, err := l.Read(buf); n != 3 || err != nil {
		t.Fatalf("first read = %d, %v", n, err)
	}
	if n, err := l.Read(buf); n != 0 || !errors.Is(err, errTooLarge) || !l.exceeded {
		t.Fatalf("read past the limit = %d, %v, exceeded %v", n, err, l.exceeded)
	}
}

// The lease can end while the body is being stored: no row, and no object a
// first upload left behind; the worker is told the lease is lost.
func TestUploadArtifactLeaseEndsDuringUpload(t *testing.T) {
	ev := testkit.NewEvidence()
	e := newEnv(t, withEvidence(ev))
	j, token := e.runningJob(t, "box")
	cancel := func() {
		if status, body := e.call(t, token, jobPath(j.ID, "complete"), map[string]any{"status": "failed", "error": "x"}); status != http.StatusOK {
			t.Errorf("complete: %d %s", status, body)
		}
	}
	ev.AfterPut = cancel
	status, body := e.upload(t, token, j.ID, "late.log", strings.NewReader("x"), "")
	wantStatus(t, status, http.StatusConflict, body)
	if code := errorCode(t, body); code != "lease_lost" || len(ev.Keys()) != 0 || len(e.artifacts(t, j.ID)) != 0 {
		t.Fatalf("code %s, objects %v, rows %v", code, ev.Keys(), e.artifacts(t, j.ID))
	}
}

// A replacement that loses the lease keeps the row of the first upload and
// the object it points at.
func TestUploadArtifactLeaseEndsDuringReplacement(t *testing.T) {
	ev := testkit.NewEvidence()
	e := newEnv(t, withEvidence(ev))
	j, token := e.runningJob(t, "box")
	wantStatus(t, mustUpload(t, e, token, j.ID, "a.log", "v1"), http.StatusCreated, nil)
	ev.AfterPut = func() { e.call(t, token, jobPath(j.ID, "complete"), map[string]any{"status": "failed", "error": "x"}) }
	status, body := e.upload(t, token, j.ID, "a.log", strings.NewReader("v2"), "")
	wantStatus(t, status, http.StatusConflict, body)
	if rows := e.artifacts(t, j.ID); len(rows) != 1 || len(ev.Keys()) != 1 {
		t.Fatalf("rows %v, objects %v", rows, ev.Keys())
	}
}

type failingArtifacts struct{ store.Store }

func (s failingArtifacts) Artifacts() store.ArtifactRepo { return failingRepo{s.Store.Artifacts()} }

type failingRepo struct{ store.ArtifactRepo }

func (failingRepo) PutLeased(context.Context, *domain.Artifact, uuid.UUID) (string, error) {
	return "", errors.New("db down")
}

func (failingRepo) ListByJob(context.Context, uuid.UUID, uuid.UUID) ([]domain.Artifact, error) {
	return nil, errors.New("db down")
}

func TestUploadArtifactRowFailureRemovesObject(t *testing.T) {
	ev := testkit.NewEvidence()
	e := newEnv(t, withEvidence(ev))
	j, token := e.runningJob(t, "box")
	cfg := e.server.cfg
	cfg.Store = failingArtifacts{e.st}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/worker/v1/jobs/"+j.ID.String()+"/artifacts/a.log", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	New(cfg).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || len(ev.Keys()) != 0 {
		t.Fatalf("status %d, objects %v", rec.Code, ev.Keys())
	}
}

// A re-upload that fails to record leaves the first upload's row and object
// alone, even when every read of the artifacts fails as well.
func TestUploadArtifactFailedReplacementKeepsTheOldObject(t *testing.T) {
	ev := testkit.NewEvidence()
	e := newEnv(t, withEvidence(ev))
	j, token := e.runningJob(t, "box")
	wantStatus(t, mustUpload(t, e, token, j.ID, "a.log", "v1"), http.StatusCreated, nil)
	old := e.artifacts(t, j.ID)[0]

	cfg := e.server.cfg
	cfg.Store = failingArtifacts{e.st}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/worker/v1/jobs/"+j.ID.String()+"/artifacts/a.log", strings.NewReader("v2"))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	New(cfg).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	if o, ok := ev.Object(old.StorageKey); !ok || string(o.Data) != "v1" || len(ev.Keys()) != 1 {
		t.Fatalf("old object %+v %v, objects %v", o, ok, ev.Keys())
	}
}

// Parallel uploads of one name end with one row whose object is intact and
// no other object left.
func TestUploadArtifactParallelSameName(t *testing.T) {
	ev := testkit.NewEvidence()
	e := newEnv(t, withEvidence(ev))
	j, token := e.runningJob(t, "box")
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			status, body := e.upload(t, token, j.ID, "a.log", strings.NewReader(strings.Repeat(string(rune('a'+i)), 10)), "")
			if status != http.StatusCreated {
				t.Errorf("upload %d: %d %s", i, status, body)
			}
		})
	}
	wg.Wait()
	rows := e.artifacts(t, j.ID)
	if len(rows) != 1 || len(ev.Keys()) != 1 {
		t.Fatalf("rows %v, objects %v", rows, ev.Keys())
	}
	o, ok := ev.Object(rows[0].StorageKey)
	sum := sha256.Sum256(o.Data)
	if !ok || hex.EncodeToString(sum[:]) != rows[0].SHA256 || int64(len(o.Data)) != rows[0].Size {
		t.Fatalf("row %+v does not match its object %+v", rows[0], o)
	}
}
