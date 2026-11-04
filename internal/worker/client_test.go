package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL+"/", "fcw_secret-token", nil)
	c.BackoffMin, c.BackoffMax = time.Millisecond, 5*time.Millisecond
	return c
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func TestClientPoll(t *testing.T) {
	id := domain.NewID()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/worker/v1/poll" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer fcw_secret-token" {
			t.Errorf("Authorization = %q", got)
		}
		var req struct {
			Labels    []string `json:"labels"`
			FreeSlots int      `json:"free_slots"`
			Version   string   `json:"version"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		switch req.FreeSlots {
		case 0:
			w.WriteHeader(http.StatusNoContent)
		case 1:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"job": map[string]any{"job_id": id, "repo": "acme/app", "base_ref": "main"}, "heartbeat_interval_seconds": 30,
			})
		default:
			writeErr(w, http.StatusUnauthorized, "unauthorized", "revoked")
		}
	})
	if lease, err := c.Poll(t.Context(), []string{"go"}, 0, "v1"); err != nil || lease != nil {
		t.Fatalf("Poll(204) = %v, %v", lease, err)
	}
	lease, err := c.Poll(t.Context(), []string{"go"}, 1, "v1")
	if err != nil || lease == nil || lease.Job.JobID != id || lease.Job.Repo != "acme/app" || lease.HeartbeatInterval != 30 ||
		time.Since(lease.SentAt) > time.Minute || lease.SentAt.IsZero() {
		t.Fatalf("Poll(200) = %+v, %v", lease, err)
	}
	if _, err := c.Poll(t.Context(), nil, 2, "v1"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Poll(401) error = %v, want ErrUnauthorized", err)
	}
}

func TestClientJobCalls(t *testing.T) {
	id := domain.NewID()
	var gotPaths []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			_ = json.NewEncoder(w).Encode(map[string]string{"action": "cancel", "lease_expires_at": "2026-03-01T12:05:30Z"})
		case strings.HasSuffix(r.URL.Path, "/events"):
			var b struct {
				Events []domain.JobEvent `json:"events"`
			}
			if err := json.NewDecoder(r.Body).Decode(&b); err != nil || len(b.Events) != 1 || b.Events[0].Text != "hi" {
				t.Errorf("events body = %+v, %v", b, err)
			}
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/complete"):
			var b Completion
			if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Status != StatusSucceeded || b.HeadSHA != "abc" {
				t.Errorf("complete body = %+v, %v", b, err)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "succeeded"})
		}
	})
	if b, err := c.Heartbeat(t.Context(), id, nil); err != nil || b.Action != ActionCancel || b.LeaseExpiresAt.Year() != 2026 {
		t.Errorf("Heartbeat = %+v, %v", b, err)
	}
	if err := c.SendEvents(t.Context(), id, []domain.JobEvent{{Type: domain.EventLog, Text: "hi", Time: time.Now()}}); err != nil {
		t.Errorf("SendEvents: %v", err)
	}
	status, err := c.Complete(t.Context(), id, Completion{Status: StatusSucceeded, HeadSHA: "abc"})
	if err != nil || status != domain.JobSucceeded {
		t.Errorf("Complete = %q, %v", status, err)
	}
	if len(gotPaths) != 3 || gotPaths[0] != "/worker/v1/jobs/"+id.String()+"/heartbeat" {
		t.Errorf("paths = %v", gotPaths)
	}
}

func TestClientErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr error
		calls   int32
	}{
		{"lease lost", http.StatusConflict, ErrLeaseLost, 1},
		{"unauthorized", http.StatusUnauthorized, ErrUnauthorized, 1},
		{"not found", http.StatusNotFound, ErrJobNotFound, 1},
		{"bad request is not retried", http.StatusBadRequest, nil, 1},
		{"server error is retried", http.StatusInternalServerError, nil, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writeErr(w, tt.status, "x", "boom")
			})
			_, err := c.Heartbeat(t.Context(), uuid.New(), nil)
			if err == nil {
				t.Fatal("Heartbeat error = nil")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil {
				if apiErr, ok := errors.AsType[*APIError](err); !ok || apiErr.Status != tt.status || !strings.Contains(err.Error(), "boom") {
					t.Errorf("error = %v, want *APIError %d", err, tt.status)
				}
			}
			if calls.Load() != tt.calls {
				t.Errorf("calls = %d, want %d", calls.Load(), tt.calls)
			}
		})
	}
}

func TestClientRetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			writeErr(w, http.StatusBadGateway, "internal", "later")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "failed"})
	})
	status, err := c.Complete(t.Context(), uuid.New(), Completion{Status: StatusFailed, Error: "x"})
	if err != nil || status != domain.JobFailed || calls.Load() != 3 {
		t.Errorf("Complete = %q, %v after %d calls", status, err, calls.Load())
	}
}

func TestClientNetworkErrorDoesNotLeakToken(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := NewClient(url, "fcw_secret-token", nil)
	c.BackoffMin, c.BackoffMax = time.Millisecond, time.Millisecond
	_, err := c.Poll(t.Context(), nil, 1, "v")
	if err == nil || strings.Contains(err.Error(), "fcw_secret-token") {
		t.Errorf("Poll error = %v", err)
	}
	if err := c.SendEvents(t.Context(), uuid.New(), nil); err == nil {
		t.Error("SendEvents error = nil, want a network error")
	}
}

func TestClientRetryStopsWhenContextEnds(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, http.StatusInternalServerError, "internal", "boom")
	})
	c.BackoffMin, c.BackoffMax = time.Hour, time.Hour
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Complete(ctx, uuid.New(), Completion{Status: StatusRetry, Error: "x"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Complete error = %v, want deadline exceeded", err)
	}
}

func TestBackoff(t *testing.T) {
	c := &Client{BackoffMin: 100 * time.Millisecond, BackoffMax: time.Second}
	for n, hi := range map[int]time.Duration{1: 100 * time.Millisecond, 2: 200 * time.Millisecond, 5: time.Second, 20: time.Second} {
		for range 20 {
			if d := c.backoff(n); d < hi/2 || d > hi {
				t.Errorf("backoff(%d) = %v, want in [%v, %v]", n, d, hi/2, hi)
			}
		}
	}
	if d := (&Client{}).backoff(1); d < DefaultBackoffMin/2 || d > DefaultBackoffMin {
		t.Errorf("default backoff(1) = %v", d)
	}
}
