//go:build integration

package app

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/evidence"
	"github.com/forecrew/forecrew/internal/mcp"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/store/postgres"
	"github.com/forecrew/forecrew/internal/testkit"
)

// TestEvidenceOnGarage runs the artifact life cycle on Postgres and a real
// S3 server (Garage in Docker): upload by a worker, listing and download by a
// user, the size limit, the safe-type rule and retention.
func TestEvidenceOnGarage(t *testing.T) {
	ctx := t.Context()
	pool := testkit.NewDatabase(t)
	must(t, postgres.Migrate(ctx, pool))
	st := postgres.New(pool)
	accounts, err := st.Accounts().List(ctx)
	must(t, err)
	account := accounts[0]
	project := domain.Project{AccountID: account.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(ctx, &project))
	const userToken = "fcm_user"
	must(t, st.MCPTokens().Create(ctx, &domain.MCPToken{AccountID: account.ID, Name: "me", TokenHash: mcp.HashToken(userToken)}))

	cfg := testConfig()
	cfg.Evidence = testkit.NewGarage(t)
	cfg.Evidence.MaxArtifactSize, cfg.Evidence.RetentionDays = 1<<20, 30
	tracker := testkit.NewTracker(nil)
	tracker.Install("acme/app")
	var logs bytes.Buffer
	a, err := New(Deps{Config: cfg, Store: st, GitHub: tracker, Logger: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	must(t, err)
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	job, workerToken := runningJob(t, st, account, project, srv.URL)
	up := func(name, content string) (int, string) {
		status, _, body := request(t, srv.URL, "PUT", "/worker/v1/jobs/"+job.ID.String()+"/artifacts/"+name, workerToken, strings.NewReader(content))
		return status, body
	}

	// Upload; the content type comes from the request, here none.
	html := "<html><script>alert(1)</script></html>"
	status, body := up("reports/index.html", html)
	if status != http.StatusCreated {
		t.Fatalf("upload: %d %s", status, body)
	}
	var created struct{ ID, SHA256 string }
	must(t, json.Unmarshal([]byte(body), &created))
	if status, body := up("big.bin", strings.Repeat("x", 1<<20+1)); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the limit: %d %s", status, body)
	}
	if status, _ := up("ok.txt", "fine"); status != http.StatusCreated {
		t.Fatal("upload of ok.txt failed")
	}

	list, err := st.Artifacts().ListByJob(ctx, account.ID, job.ID)
	must(t, err)
	if len(list) != 2 || list[0].Name != "ok.txt" || list[1].Name != "reports/index.html" || list[1].ID.String() != created.ID {
		t.Fatalf("rows = %+v (the rejected upload must leave nothing)", list)
	}

	status, h, got := request(t, srv.URL, "GET", "/api/v1/artifacts/"+created.ID, userToken, nil)
	if status != http.StatusOK || got != html || !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("download: %d %q %v", status, got, h)
	}

	// Retention deletes the objects and rows after the period and audits it.
	a.retention.Now = func() time.Time { return time.Now().AddDate(0, 0, 31) }
	n, err := a.retention.Sweep(ctx)
	if n != 2 || err != nil {
		t.Fatalf("Sweep = %d, %v", n, err)
	}
	if status, _, _ := request(t, srv.URL, "GET", "/api/v1/artifacts/"+created.ID, userToken, nil); status != http.StatusNotFound {
		t.Errorf("download after retention: %d", status)
	}
	s3, err := evidence.NewS3(cfg.Evidence)
	must(t, err)
	if _, err := s3.Get(ctx, list[0].StorageKey); err == nil {
		t.Error("the object is still in the bucket")
	}
	audit, err := st.Audit().List(ctx, account.ID, store.AuditFilter{})
	must(t, err)
	deleted := 0
	for _, e := range audit {
		if e.Action == evidence.ActionArtifactDeleted && e.Actor == "system:forecrewd" {
			deleted++
		}
	}
	if deleted != 2 {
		t.Errorf("%d deletions audited, want 2", deleted)
	}

	if out := logs.String(); strings.Contains(out, cfg.Evidence.SecretAccessKey) || strings.Contains(out, cfg.Evidence.AccessKeyID) {
		t.Errorf("a storage key is in the logs:\n%s", out)
	}
}
