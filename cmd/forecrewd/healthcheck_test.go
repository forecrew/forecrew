package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/config"
)

func TestHealthTarget(t *testing.T) {
	tests := []struct {
		name       string
		server     config.Server
		wantURL    string
		wantServer string
	}{
		{name: "plain", server: config.Server{Listen: ":80"}, wantURL: "http://127.0.0.1:80/healthz"},
		{name: "plain with host", server: config.Server{Listen: "0.0.0.0:8080"}, wantURL: "http://127.0.0.1:8080/healthz"},
		{name: "tls", server: config.Server{Listen: ":443", TLSDomain: "forecrew.example.com"}, wantURL: "https://127.0.0.1:443/healthz", wantServer: "forecrew.example.com"},
		{name: "unparseable", server: config.Server{Listen: "nope"}, wantURL: "http://127.0.0.1:80/healthz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotURL, gotServer := healthTarget(&config.Config{Server: tt.server})
			if gotURL != tt.wantURL || gotServer != tt.wantServer {
				t.Errorf("healthTarget = %q, %q; want %q, %q", gotURL, gotServer, tt.wantURL, tt.wantServer)
			}
		})
	}
}

func TestCheckHealth(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	defer srv.Close()

	if err := checkHealth(t.Context(), srv.Client(), srv.URL); err != nil {
		t.Errorf("healthy: %v", err)
	}
	status = http.StatusServiceUnavailable
	if err := checkHealth(t.Context(), srv.Client(), srv.URL); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("unhealthy: %v", err)
	}
	if err := checkHealth(t.Context(), srv.Client(), "http://127.0.0.1:1/healthz"); err == nil {
		t.Error("connection refused was not reported")
	}
	if err := checkHealth(t.Context(), srv.Client(), "://bad"); err == nil {
		t.Error("a bad URL was accepted")
	}
}

func TestRunHealthcheck(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	cfg := filepath.Join(t.TempDir(), "forecrew.yaml")
	yaml := `
server: {public_base_url: "http://localhost", listen: ":` + port + `"}
github: {app_id: 1}
runtimes: {claude: {adapter: claude-code, vendor: anthropic}}
`
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		config.EnvDatabaseURL: "postgres://x", config.EnvGitHubPrivateKey: "k", config.EnvGitHubWebhookSecret: "s",
	}
	getenv := func(k string) string { return env[k] }

	tests := []struct {
		name     string
		args     []string
		status   int
		wantCode int
		wantErr  string
	}{
		{name: "healthy", args: []string{"--config", cfg}, status: 200, wantCode: 0},
		{name: "unhealthy", args: []string{"--config", cfg}, status: 503, wantCode: 1, wantErr: "503"},
		{name: "missing config", args: []string{"--config", filepath.Join(t.TempDir(), "x.yaml")}, status: 200, wantCode: 1, wantErr: "healthcheck:"},
		{name: "bad flag", args: []string{"--nope"}, status: 200, wantCode: 2, wantErr: "not defined"},
		{name: "help", args: []string{"-h"}, status: 200, wantCode: 0, wantErr: "-config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status = tt.status
			var stderr bytes.Buffer
			code := run(t.Context(), append([]string{"healthcheck"}, tt.args...), &bytes.Buffer{}, &stderr, getenv, "v1")
			if code != tt.wantCode || !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("code = %d, stderr = %q; want %d, %q", code, stderr.String(), tt.wantCode, tt.wantErr)
			}
		})
	}
}
