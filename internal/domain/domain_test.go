package domain

import (
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"
)

var now = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func TestNewIDIsV7(t *testing.T) {
	id := NewID()
	if v := id[6] >> 4; v != 7 {
		t.Fatalf("version = %d, want 7", v)
	}
	if NewID() == id {
		t.Fatal("NewID returned the same ID twice")
	}
}

func TestRefs(t *testing.T) {
	tests := []struct {
		in      string
		kind    RefKind
		number  int64
		wantErr bool
	}{
		{in: "GOAL-3", kind: RefGoal, number: 3},
		{in: "task-12", kind: RefTask, number: 12},
		{in: "JOB-41", kind: RefJob, number: 41},
		{in: "GATE-7", kind: RefGate, number: 7},
		{in: "SPEC-1", kind: RefSpec, number: 1},
		{in: "GOAL", wantErr: true},
		{in: "GOAL-0", wantErr: true},
		{in: "GOAL-x", wantErr: true},
		{in: "FOO-1", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		kind, n, err := ParseRef(tt.in)
		if tt.wantErr {
			if !errors.Is(err, ErrInvalidRef) {
				t.Errorf("ParseRef(%q) err = %v, want ErrInvalidRef", tt.in, err)
			}
			continue
		}
		if err != nil || kind != tt.kind || n != tt.number {
			t.Errorf("ParseRef(%q) = %s, %d, %v", tt.in, kind, n, err)
		}
		if got := Ref(kind, n); got != strings.ToUpper(tt.in) {
			t.Errorf("Ref round trip = %q, want %q", got, strings.ToUpper(tt.in))
		}
	}
	if len(RefKinds()) != 5 {
		t.Errorf("RefKinds = %v", RefKinds())
	}
}

func TestEntityRefs(t *testing.T) {
	got := []string{
		Goal{Number: 3}.Ref(), Spec{Number: 4}.Ref(), Task{Number: 12}.Ref(),
		Job{Number: 41}.Ref(), Gate{Number: 7}.Ref(),
	}
	want := []string{"GOAL-3", "SPEC-4", "TASK-12", "JOB-41", "GATE-7"}
	if !slices.Equal(got, want) {
		t.Fatalf("refs = %v, want %v", got, want)
	}
}

func TestActorString(t *testing.T) {
	if got := (Actor{Kind: ActorGitHub, Name: "octocat"}).String(); got != "github:octocat" {
		t.Fatalf("String = %q", got)
	}
}

func TestLabels(t *testing.T) {
	if got := NormalizeLabels([]string{" Linux", "go", "", "GO", "runtime:Claude"}); !slices.Equal(got, []string{"go", "linux", "runtime:claude"}) {
		t.Fatalf("NormalizeLabels = %v", got)
	}
	have := []string{"linux", "Go", "runtime:claude"}
	tests := []struct {
		needs []string
		want  bool
	}{
		{needs: nil, want: true},
		{needs: []string{"GO", "runtime:CLAUDE"}, want: true},
		{needs: []string{"go", "node"}, want: false},
	}
	for _, tt := range tests {
		if got := MatchLabels(tt.needs, have); got != tt.want {
			t.Errorf("MatchLabels(%v) = %v, want %v", tt.needs, got, tt.want)
		}
	}
}

func TestWorkerOnline(t *testing.T) {
	tests := []struct {
		name string
		w    Worker
		want bool
	}{
		{name: "never seen", w: Worker{}, want: false},
		{name: "recent", w: Worker{LastSeenAt: now.Add(-59 * time.Second)}, want: true},
		{name: "at window", w: Worker{LastSeenAt: now.Add(-OnlineWindow)}, want: true},
		{name: "stale", w: Worker{LastSeenAt: now.Add(-61 * time.Second)}, want: false},
		{name: "revoked", w: Worker{LastSeenAt: now, RevokedAt: now}, want: false},
	}
	for _, tt := range tests {
		if got := tt.w.Online(now); got != tt.want {
			t.Errorf("%s: Online = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestJobRequiredLabels(t *testing.T) {
	j := Job{Needs: []string{"go"}, Runtime: "codex"}
	if got := j.RequiredLabels(); !slices.Equal(got, []string{"go", "runtime:codex"}) {
		t.Fatalf("RequiredLabels = %v", got)
	}
	if !slices.Equal(j.Needs, []string{"go"}) {
		t.Fatalf("Needs modified: %v", j.Needs)
	}
}

func TestJobLifecycle(t *testing.T) {
	worker := NewID()
	j := Job{Status: JobQueued, MaxAttempts: 2}
	if err := j.Lease(worker, now); err != nil {
		t.Fatal(err)
	}
	if j.Status != JobRunning || j.WorkerID != worker || !j.LeaseExpiresAt.Equal(now.Add(LeaseDuration)) || !j.StartedAt.Equal(now) {
		t.Fatalf("after lease: %+v", j)
	}
	if err := j.Lease(worker, now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("double lease err = %v", err)
	}
	if err := j.Requeue("lease expired", now); err != nil {
		t.Fatal(err)
	}
	if j.Status != JobQueued || j.Attempts != 1 || j.WorkerID != uuid.Nil() || !j.LeaseExpiresAt.IsZero() {
		t.Fatalf("after first requeue: %+v", j)
	}
	if err := j.Requeue("again", now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("requeue of queued job err = %v", err)
	}
	if err := j.Lease(worker, now); err != nil {
		t.Fatal(err)
	}
	if err := j.Requeue("lease expired", now); err != nil {
		t.Fatal(err)
	}
	if j.Status != JobFailed || j.Attempts != 2 || !j.FinishedAt.Equal(now) || !strings.Contains(j.Error, "attempts exhausted") {
		t.Fatalf("after exhausted: %+v", j)
	}
}

func TestJobFinish(t *testing.T) {
	tests := []struct {
		name    string
		from    JobStatus
		to      JobStatus
		wantErr bool
	}{
		{name: "succeed", from: JobRunning, to: JobSucceeded},
		{name: "fail", from: JobRunning, to: JobFailed},
		{name: "cancel queued", from: JobQueued, to: JobCanceled},
		{name: "cancel running", from: JobRunning, to: JobCanceled},
		{name: "not terminal", from: JobRunning, to: JobQueued, wantErr: true},
		{name: "succeed queued", from: JobQueued, to: JobSucceeded, wantErr: true},
		{name: "already done", from: JobSucceeded, to: JobFailed, wantErr: true},
	}
	for _, tt := range tests {
		j := Job{Status: tt.from, WorkerID: NewID()}
		err := j.Finish(tt.to, now)
		if tt.wantErr {
			if !errors.Is(err, ErrInvalidTransition) || j.Status != tt.from {
				t.Errorf("%s: err = %v, status = %s", tt.name, err, j.Status)
			}
			continue
		}
		if err != nil || j.Status != tt.to || j.WorkerID != uuid.Nil() || !j.FinishedAt.Equal(now) {
			t.Errorf("%s: err = %v, job = %+v", tt.name, err, j)
		}
	}
}

func TestJobSpecSecrets(t *testing.T) {
	s := JobSpec{JobID: NewID(), Repo: "o/r", Env: map[string]string{"KEY": "secret"}, RepoToken: "ghs_secret", Timeout: Duration(45 * time.Minute)}
	r := s.Redacted()
	if r.Env != nil || r.RepoToken != "" || s.RepoToken == "" {
		t.Fatalf("Redacted = %+v (original %+v)", r, s)
	}
	var buf strings.Builder
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("job", "spec", s)
	if strings.Contains(buf.String(), "secret") || !strings.Contains(buf.String(), "o/r") {
		t.Fatalf("log = %s", buf.String())
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"timeout":"45m0s"`) {
		t.Fatalf("json = %s", b)
	}
	var back JobSpec
	if err := json.Unmarshal(b, &back); err != nil || back.Timeout != s.Timeout {
		t.Fatalf("round trip: %v, %v", back.Timeout, err)
	}
	if err := json.Unmarshal([]byte(`{"timeout":"soon"}`), &back); err == nil {
		t.Fatal("bad duration accepted")
	}
}

func TestGateResolve(t *testing.T) {
	by := Actor{Kind: ActorUser, Name: "ana"}
	tests := []struct {
		kind     GateKind
		decision GateStatus
		wantErr  error
	}{
		{kind: GateSpecApproval, decision: GateApproved},
		{kind: GateSpecApproval, decision: GateChangesRequested},
		{kind: GateMergeApproval, decision: GateRejected},
		{kind: GateProfileApproval, decision: GateChangesRequested},
		{kind: GateNeedsYou, decision: GateApproved},
		{kind: GateNeedsYou, decision: GateRejected},
		{kind: GateNeedsYou, decision: GateChangesRequested, wantErr: ErrInvalidDecision},
		{kind: GateSpecApproval, decision: GatePending, wantErr: ErrInvalidDecision},
	}
	for _, tt := range tests {
		g := Gate{Kind: tt.kind, Status: GatePending}
		err := g.Resolve(by, tt.decision, "fix it", now)
		if tt.wantErr != nil {
			if !errors.Is(err, tt.wantErr) || g.Status != GatePending {
				t.Errorf("%s/%s: err = %v", tt.kind, tt.decision, err)
			}
			continue
		}
		if err != nil || g.Status != tt.decision || g.ResolvedBy != "user:ana" || g.Feedback != "fix it" || !g.ResolvedAt.Equal(now) {
			t.Errorf("%s/%s: err = %v, gate = %+v", tt.kind, tt.decision, err, g)
		}
		if err := g.Resolve(by, GateApproved, "", now); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("second resolve err = %v", err)
		}
	}
	if len(GateKinds()) != 4 {
		t.Errorf("GateKinds = %v", GateKinds())
	}
}

func TestJobEventTypes(t *testing.T) {
	if len(JobEventTypes()) != 7 {
		t.Fatalf("JobEventTypes = %v", JobEventTypes())
	}
}
