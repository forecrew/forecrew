package fleetagent_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/fleet"
	"github.com/forecrew/forecrew/internal/fleetagent"
	"github.com/forecrew/forecrew/internal/testkit"
)

func (w *world) joinCode() (string, domain.ProviderJoinCode) {
	w.t.Helper()
	code, hash := fleet.NewJoinCode()
	c := domain.ProviderJoinCode{AccountID: w.provider.AccountID, CodeHash: hash, ExpiresAt: time.Now().Add(time.Hour)}
	must(w.t, w.st.ProviderJoinCodes().Create(w.t.Context(), &c))
	return code, c
}

// confirm is the person: it waits for the report and makes the provider.
func (w *world) confirm(c domain.ProviderJoinCode, name string) domain.CapacityProvider {
	w.t.Helper()
	ctx := context.Background()
	eventually(w.t, "the agent's report", func() bool {
		got, err := w.st.ProviderJoinCodes().Get(ctx, c.ID)
		must(w.t, err)
		c = got
		return len(got.ClaimHash) != 0
	})
	p := domain.CapacityProvider{AccountID: c.AccountID, Name: name, Kind: c.Kind, Mode: domain.ProviderModeAgent, Status: domain.ProviderPending, Capabilities: c.Facts}
	must(w.t, w.st.CapacityProviders().Create(ctx, &p))
	must(w.t, w.st.ProviderJoinCodes().Confirm(ctx, c.ID, p.ID))
	return p
}

func TestJoinGetsATokenOverItsOwnConnection(t *testing.T) {
	w := newWorld(t)
	code, c := w.joinCode()
	done := make(chan string, 1)
	go func() {
		token, err := fleetagent.Join(context.Background(), fleetagent.NewClient(w.url, "", nil), code, domain.ProviderMicroVM, domain.ProviderCapabilities{Host: "nas"}, w.log, time.Millisecond)
		if err != nil {
			t.Errorf("join: %v", err)
		}
		done <- token
	}()
	p := w.confirm(c, "nas")
	if p.Capabilities.Host != "nas" || p.Kind != domain.ProviderMicroVM {
		t.Fatalf("provider = %+v", p)
	}
	token := <-done // forecrewd says to wait 5 s between calls
	if !strings.HasPrefix(token, "fcp_") {
		t.Fatalf("token of %d characters", len(token))
	}
	// The token works, and the code gives nothing any more.
	if _, err := fleetagent.NewClient(w.url, token, nil).Register(t.Context(), domain.ProviderCapabilities{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if strings.Contains(w.logs.String(), token) || strings.Contains(w.logs.String(), code) {
		t.Fatal("a secret is in the logs")
	}
	_, err := fleetagent.Join(t.Context(), fleetagent.NewClient(w.url, "", nil), code, domain.ProviderMicroVM, domain.ProviderCapabilities{}, w.log, time.Millisecond)
	if !errors.Is(err, fleetagent.ErrRejected) {
		t.Fatalf("second use: %v", err)
	}
}

func TestJoinRetriesThroughFailures(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			rw.WriteHeader(http.StatusTooManyRequests)
		case 2:
			rw.WriteHeader(http.StatusInternalServerError)
		case 3:
			_, _ = io.WriteString(rw, `{"status":"waiting"}`)
		default:
			_, _ = io.WriteString(rw, `{"status":"confirmed","token":"fcp_x"}`)
		}
	}))
	defer srv.Close()
	token, err := fleetagent.Join(t.Context(), fleetagent.NewClient(srv.URL, "", nil), "C", domain.ProviderAWS, domain.ProviderCapabilities{}, slog.New(slog.DiscardHandler), time.Millisecond)
	if err != nil || token != "fcp_x" || calls.Load() != 4 {
		t.Fatalf("%q, %v after %d calls", token, err, calls.Load())
	}
}

func TestJoinStopsOnConflictAndCancel(t *testing.T) {
	status := atomic.Int64{}
	status.Store(http.StatusConflict)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(rw, `{"status":"waiting","retry_after_seconds":60}`)
	}))
	defer srv.Close()
	c := fleetagent.NewClient(srv.URL, "", nil)
	log := slog.New(slog.DiscardHandler)
	_, err := fleetagent.Join(t.Context(), c, "C", domain.ProviderAWS, domain.ProviderCapabilities{}, log, 0)
	var api *fleetagent.APIError
	if !errors.As(err, &api) || api.Status != http.StatusConflict || !strings.Contains(api.Error(), "409") {
		t.Fatalf("conflict: %v", err)
	}
	status.Store(http.StatusOK)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := fleetagent.Join(ctx, c, "C", domain.ProviderAWS, domain.ProviderCapabilities{}, log, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "token")
	if tok, err := fleetagent.LoadToken(path); err != nil || tok != "" {
		t.Fatalf("no file: %q, %v", tok, err)
	}
	must(t, fleetagent.SaveToken(path, "fcp_x"))
	info, err := os.Stat(path)
	must(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	if tok, err := fleetagent.LoadToken(path); err != nil || tok != "fcp_x" {
		t.Fatalf("read: %q, %v", tok, err)
	}
	if _, err := fleetagent.LoadToken(filepath.Dir(path)); err == nil {
		t.Error("a directory reads as a token")
	}
	blocked := filepath.Join(path, "below")
	if err := fleetagent.SaveToken(blocked, "x"); err == nil {
		t.Error("saved below a file")
	}
}

// serveWorld registers the fake as a kind and runs the real entry point.
func TestServeJoinsThenKeepsItsToken(t *testing.T) {
	w := newWorld(t)
	fleetagent.Register(domain.ProviderFake, func() (fleet.Provider, error) { return w.fake, nil })
	code, c := w.joinCode()
	path := filepath.Join(t.TempDir(), "token")
	cfg := &config.FleetAgent{ServerURL: w.url, Kind: domain.ProviderFake, TokenFile: path, JoinCode: code}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fleetagent.Serve(ctx, cfg, "test", w.log) }()
	p := w.confirm(c, "joined")
	eventually(t, "the token on disk", func() bool {
		tok, err := fleetagent.LoadToken(path)
		return err == nil && tok != ""
	})
	eventually(t, "the agent online", func() bool {
		got, err := w.st.CapacityProviders().Get(context.Background(), p.ID)
		must(t, err)
		return got.Status == domain.ProviderActive && got.Capabilities.AgentVersion == "test"
	})
	cancel()
	must(t, <-done)

	// The next start needs no code; the code in the environment is ignored.
	cfg.JoinCode = "used"
	must(t, w.st.CapacityProviders().SetStatus(t.Context(), p.ID, domain.ProviderRevoked))
	if err := fleetagent.Serve(t.Context(), cfg, "test", w.log); !errors.Is(err, fleetagent.ErrRejected) {
		t.Fatalf("revoked: %v", err)
	}
}

func TestServeNeedsCredentialsAndAProvider(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	cfg := &config.FleetAgent{ServerURL: "http://127.0.0.1:1", Kind: domain.ProviderTart, TokenFile: filepath.Join(t.TempDir(), "token")}
	if err := fleetagent.Serve(t.Context(), cfg, "test", log); err == nil || !strings.Contains(err.Error(), "no provider of kind tart") {
		t.Errorf("no provider: %v", err)
	}
	fleetagent.Register(domain.ProviderHyperV, func() (fleet.Provider, error) { return testkit.NewFakeProvider(), nil })
	cfg.Kind = domain.ProviderHyperV
	if err := fleetagent.Serve(t.Context(), cfg, "test", log); err == nil || !strings.Contains(err.Error(), config.EnvJoinCode) {
		t.Errorf("no credentials: %v", err)
	}
}

func TestProviderTokenFromTheEnvironmentWins(t *testing.T) {
	w := newWorld(t)
	fleetagent.Register(domain.ProviderFake, func() (fleet.Provider, error) { return w.fake, nil })
	cfg := &config.FleetAgent{ServerURL: w.url, Kind: domain.ProviderFake, TokenFile: filepath.Join(t.TempDir(), "token"), Token: w.token}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fleetagent.Serve(ctx, cfg, "test", w.log) }()
	m := w.request()
	w.waitState(m.ID, domain.MachineBooting)
	cancel()
	must(t, <-done)
	if _, err := os.Stat(cfg.TokenFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a token from the environment went to disk: %v", err)
	}
}
