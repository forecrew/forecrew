package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRun(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	invalid := filepath.Join(t.TempDir(), "forecrew.yaml")
	if err := os.WriteFile(invalid, []byte("server: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "version", args: []string{"--version"}, wantCode: 0, wantStdout: "forecrewd v1.2.3\n"},
		{name: "help", args: []string{"-h"}, wantCode: 0, wantStderr: "-config"},
		{name: "bad flag", args: []string{"--nope"}, wantCode: 2, wantStderr: "flag provided but not defined"},
		{name: "missing config", args: []string{"--config", missing}, wantCode: 1, wantStderr: "load config"},
		{name: "invalid config", args: []string{"--config", invalid}, wantCode: 1, wantStderr: "invalid config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), tt.args, &stdout, &stderr, func(string) string { return "" }, "v1.2.3")
			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d", code, tt.wantCode)
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

type fakeDB struct{ err error }

func (d fakeDB) Ping(context.Context) error { return d.err }

func TestHealthz(t *testing.T) {
	workers := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	for _, tt := range []struct {
		db   fakeDB
		path string
		want int
	}{
		{fakeDB{}, "/healthz", http.StatusOK},
		{fakeDB{errors.New("down")}, "/healthz", http.StatusServiceUnavailable},
		{fakeDB{}, "/worker/v1/poll", http.StatusTeapot},
		{fakeDB{}, "/other", http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		newMux(tt.db, workers).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil))
		if rec.Code != tt.want {
			t.Errorf("%s: status %d, want %d", tt.path, rec.Code, tt.want)
		}
	}
}

func TestListen(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	srv := &http.Server{Addr: "127.0.0.1:0", Handler: newMux(fakeDB{}, http.NotFoundHandler()), ReadHeaderTimeout: time.Second}
	var logs bytes.Buffer
	loopDone := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		errc <- listen(ctx, srv, slog.New(slog.NewJSONHandler(&logs, nil)), "v1", func(ctx context.Context) {
			<-ctx.Done()
			close(loopDone)
		})
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	select {
	case <-loopDone:
	default:
		t.Fatal("loop still running after listen returned")
	}
	if !strings.Contains(logs.String(), "forecrewd started") {
		t.Fatalf("logs = %s", logs.String())
	}
}

func TestListenBusyPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	srv := &http.Server{Addr: ln.Addr().String(), ReadHeaderTimeout: time.Second}
	if err := listen(t.Context(), srv, slog.New(slog.NewJSONHandler(io.Discard, nil)), "v1"); err == nil {
		t.Fatal("want listen error")
	}
}
