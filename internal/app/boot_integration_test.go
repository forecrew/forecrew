//go:build integration && !windows

package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/store/postgres"
	"github.com/forecrew/forecrew/internal/testkit"
)

// TestBoot starts forecrewd against real Postgres: the migrated schema, the
// routes and the loops are up, and SIGTERM stops everything within 10 s.
func TestBoot(t *testing.T) {
	pool := testkit.NewDatabase(t)
	u, err := url.Parse(os.Getenv(testkit.EnvTestDatabaseURL))
	must(t, err)
	u.Path = "/" + pool.Config().ConnConfig.Database

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	must(t, err)
	cfg := testConfig()
	cfg.DatabaseURL = u.String()
	cfg.GitHub.PrivateKey = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	ln := listen(t)
	cfg.Server.Listen = ln.Addr().String()
	must(t, ln.Close())

	var logs bytes.Buffer
	ctx, stop := signal.NotifyContext(t.Context(), syscall.SIGTERM)
	defer stop()
	a, err := Open(ctx, cfg, slog.New(slog.NewJSONHandler(&logs, nil)), "test", Options{})
	must(t, err)
	defer a.Close()
	errc := make(chan error, 1)
	go func() { errc <- a.Run(ctx) }()

	base := "http://" + cfg.Server.Listen
	waitFor(t, "healthz", func() bool {
		resp, err := http.Get(base + "/healthz") //nolint:noctx // polling a local server
		if err != nil {
			return false
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "ok"
	})

	// The webhook handler has the secret: a signed delivery is accepted, an unsigned one is not.
	mac := hmac.New(sha256.New, []byte(cfg.GitHub.WebhookSecret))
	body := []byte(`{}`)
	mac.Write(body)
	for sig, want := range map[string]int{"sha256=" + hex.EncodeToString(mac.Sum(nil)): http.StatusAccepted, "sha256=00": http.StatusUnauthorized} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/webhooks/github", bytes.NewReader(body))
		must(t, err)
		req.Header.Set("X-Hub-Signature-256", sig)
		req.Header.Set("X-GitHub-Delivery", "d1")
		req.Header.Set("X-GitHub-Event", "ping")
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("webhook with signature %q: status %d, want %d", sig[:9], resp.StatusCode, want)
		}
	}

	begin := time.Now()
	must(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
	select {
	case err := <-errc:
		must(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("forecrewd did not stop within 10 seconds of SIGTERM")
	}
	if d := time.Since(begin); d > 10*time.Second {
		t.Errorf("stopping took %s", d)
	}
	if !strings.Contains(logs.String(), "forecrewd stopped") {
		t.Errorf("logs = %s", logs.String())
	}
}

// TestLoopsRun proves that Run starts the background loops, each through an
// effect only that loop has: the poller merges a change whose PR GitHub
// merged, the webhook queue processes a signed delivery, and the reaper
// requeues a job whose lease expired.
func TestLoopsRun(t *testing.T) {
	ctx := t.Context()
	pool := testkit.NewDatabase(t)
	must(t, postgres.Migrate(ctx, pool))
	st := postgres.New(pool)
	acc := domain.Account{Name: "loops"}
	must(t, st.Accounts().Create(ctx, &acc))
	project := domain.Project{AccountID: acc.ID, Name: "app", Repo: "acme/app", DefaultBranch: "main", Status: domain.ProjectOnboarded}
	must(t, st.Projects().Create(ctx, &project))
	worker := domain.Worker{AccountID: acc.ID, Name: "box", TokenHash: []byte("h"), MaxJobs: 1}
	must(t, st.Workers().Create(ctx, &worker))

	tracker := testkit.NewTracker(nil)
	tracker.Install("acme/app")
	giveHead(tracker, "acme/app")
	openChange := func(branch string) domain.Change {
		must(t, tracker.AddBranch("acme/app", branch))
		pr, err := tracker.CreatePR(ctx, "acme/app", github.PRInput{Title: "x", Head: branch, Base: "main"})
		must(t, err)
		c := domain.Change{
			AccountID: acc.ID, ProjectID: project.ID, Repo: "acme/app", Number: pr.Number, Branch: branch, BaseRef: "main",
			HeadSHA: pr.HeadSHA, State: domain.ChangeOpen,
		}
		must(t, st.Changes().Upsert(ctx, &c))
		return c
	}
	polled, hooked := openChange("agent/polled"), openChange("agent/hooked")
	_, err := tracker.MergePR(ctx, "acme/app", polled.Number, github.MergeInput{})
	must(t, err)

	job := domain.Job{AccountID: acc.ID, ProjectID: project.ID, Role: "developer", Runtime: "claude-code", Spec: domain.JobSpec{Repo: "acme/app"}}
	must(t, st.Jobs().Create(ctx, &job))
	_, err = st.Jobs().LeaseNext(ctx, acc.ID, worker.ID, []string{"runtime:claude-code"}, time.Millisecond)
	must(t, err)

	cfg := testConfig()
	a, err := New(Deps{Config: cfg, Store: st, GitHub: tracker, Listener: listen(t), ReapEvery: 20 * time.Millisecond})
	must(t, err)
	runCtx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- a.Run(runCtx) }()
	defer func() {
		cancel()
		must(t, <-errc)
	}()

	body := []byte(`{"action":"closed","number":` + strconv.Itoa(hooked.Number) + `,"pull_request":{"number":` + strconv.Itoa(hooked.Number) +
		`,"merged":true,"head":{"sha":"merge-sha"}},"repository":{"full_name":"acme/app"}}`)
	mac := hmac.New(sha256.New, []byte(cfg.GitHub.WebhookSecret))
	mac.Write(body)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Delivery", "d1")
	req.Header.Set("X-GitHub-Event", "pull_request")
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("webhook: status %d", rec.Code)
	}

	changeState := func(c domain.Change) domain.ChangeState {
		changes, err := st.Changes().List(ctx, acc.ID, store.ChangeFilter{})
		must(t, err)
		for _, got := range changes {
			if got.Number == c.Number {
				return got.State
			}
		}
		t.Fatalf("change %d is gone", c.Number)
		return ""
	}
	waitFor(t, "the poller to merge the change", func() bool { return changeState(polled) == domain.ChangeMerged })
	waitFor(t, "the webhook queue to merge the change", func() bool { return changeState(hooked) == domain.ChangeMerged })
	waitFor(t, "the reaper to requeue the job", func() bool {
		j, err := st.Jobs().Get(ctx, acc.ID, job.ID)
		must(t, err)
		return j.Status == domain.JobQueued && j.Attempts == 1
	})
}
