package domain

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestSignals(t *testing.T) {
	job, gate, council, decision := NewID(), NewID(), NewID(), NewID()
	sha := "ABC123"
	tests := []struct {
		name      string
		payload   SignalPayload
		kind      SignalKind
		key       string
		dedupeKey string
	}{
		{
			name: "job completed", payload: &JobCompleted{JobID: job, Status: JobSucceeded},
			kind: SignalJobCompleted, key: "job:" + job.String(), dedupeKey: "job.completed:" + job.String(),
		},
		{
			name: "gate resolved", payload: &GateResolved{GateID: gate, Decision: GateChangesRequested, Feedback: "more tests"},
			kind: SignalGateResolved, key: "gate:" + gate.String(), dedupeKey: "gate.resolved:" + gate.String(),
		},
		{
			name: "pr merged", payload: &PRMerged{Repo: "Owner/Repo", Number: 12, SHA: sha},
			kind: SignalPRMerged, key: "pr:owner/repo#12", dedupeKey: "pr.merged:owner/repo#12",
		},
		{
			name: "pr closed", payload: &PRClosed{Repo: "owner/repo", Number: 12},
			kind: SignalPRClosed, key: "pr:owner/repo#12", dedupeKey: "pr.closed:owner/repo#12",
		},
		{
			name: "checks completed", payload: &ChecksCompleted{Repo: "owner/repo", Number: 12, SHA: sha, Conclusion: ChecksNone},
			kind: SignalChecksCompleted, key: "checks:owner/repo@abc123", dedupeKey: "checks.completed:owner/repo@abc123",
		},
		{
			name: "council decided", payload: &CouncilDecidedSignal{CouncilID: council, Status: CouncilDecided, DecisionID: decision},
			kind: SignalCouncilDecided, key: "council:" + council.String(), dedupeKey: "council.decided:" + council.String(),
		},
		{
			name: "council failed", payload: &CouncilDecidedSignal{CouncilID: council, Status: CouncilFailed},
			kind: SignalCouncilDecided, key: "council:" + council.String(), dedupeKey: "council.decided:" + council.String(),
		},
	}
	account, project := NewID(), NewID()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := NewSignal(account, project, tt.payload, now)
			if err != nil {
				t.Fatal(err)
			}
			if s.Kind != tt.kind || s.Key != tt.key || s.DedupeKey != tt.dedupeKey || s.AccountID != account || s.ProjectID != project || !s.CreatedAt.Equal(now) {
				t.Fatalf("signal = %+v", s)
			}
			got, err := s.Decode()
			if err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(tt.payload)
			gotJSON, _ := json.Marshal(got)
			if string(gotJSON) != string(want) {
				t.Fatalf("decoded %s, want %s", gotJSON, want)
			}
		})
	}
	if len(SignalKinds()) != 6 {
		t.Errorf("SignalKinds = %v", SignalKinds())
	}
}

func TestInvalidSignals(t *testing.T) {
	payloads := []SignalPayload{
		&JobCompleted{Status: JobSucceeded},
		&JobCompleted{JobID: NewID(), Status: JobRunning},
		&GateResolved{Decision: GateApproved},
		&GateResolved{GateID: NewID(), Decision: GatePending},
		&PRMerged{Number: 1, SHA: "a"},
		&PRMerged{Repo: "o/r", SHA: "a"},
		&PRMerged{Repo: "o/r", Number: 1},
		&PRClosed{Repo: "o/r"},
		&ChecksCompleted{Repo: "o/r", SHA: "a", Conclusion: ChecksSuccess},
		&ChecksCompleted{Repo: "o/r", Number: 1, Conclusion: ChecksSuccess},
		&ChecksCompleted{Repo: "o/r", Number: 1, SHA: "a", Conclusion: "pending"},
		&CouncilDecidedSignal{Status: CouncilCanceled},
		&CouncilDecidedSignal{CouncilID: NewID(), Status: CouncilRunning},
		&CouncilDecidedSignal{CouncilID: NewID(), Status: "gone"},
		&CouncilDecidedSignal{CouncilID: NewID(), Status: CouncilDecided},
		&CouncilDecidedSignal{CouncilID: NewID(), Status: CouncilFailed, DecisionID: NewID()},
	}
	for _, p := range payloads {
		if _, err := NewSignal(NewID(), NewID(), p, now); !errors.Is(err, ErrInvalidSignal) {
			t.Errorf("NewSignal(%+v) err = %v, want ErrInvalidSignal", p, err)
		}
	}
}

func TestDecodeErrors(t *testing.T) {
	tests := []Signal{
		{Kind: "nope", Payload: []byte(`{}`)},
		{Kind: SignalPRMerged, Payload: []byte(`{`)},
		{Kind: SignalPRClosed, Payload: []byte(`{"repo":"o/r"}`)},
	}
	for _, s := range tests {
		if _, err := s.Decode(); !errors.Is(err, ErrInvalidSignal) {
			t.Errorf("Decode(%s %s) err = %v", s.Kind, s.Payload, err)
		}
	}
}

func TestChecksConclusions(t *testing.T) {
	for _, c := range []CheckConclusion{ChecksSuccess, ChecksFailure, ChecksNone} {
		p := &ChecksCompleted{Repo: "o/r", Number: 1, SHA: "a", Conclusion: c}
		if _, err := NewSignal(NewID(), NewID(), p, now); err != nil {
			t.Errorf("%s: %v", c, err)
		}
	}
}

func TestSignalKeysIgnoreCase(t *testing.T) {
	tests := []struct {
		name         string
		lower, mixed SignalPayload
	}{
		{
			name:  "pr merged",
			lower: &PRMerged{Repo: "owner/repo", Number: 3, SHA: "abc"},
			mixed: &PRMerged{Repo: "Owner/Repo", Number: 3, SHA: "ABC"},
		},
		{
			name:  "pr closed",
			lower: &PRClosed{Repo: "owner/repo", Number: 3},
			mixed: &PRClosed{Repo: "OWNER/repo", Number: 3},
		},
		{
			name:  "checks completed",
			lower: &ChecksCompleted{Repo: "owner/repo", Number: 3, SHA: "abc123", Conclusion: ChecksSuccess},
			mixed: &ChecksCompleted{Repo: "Owner/REPO", Number: 3, SHA: "ABC123", Conclusion: ChecksSuccess},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lower, err := NewSignal(NewID(), NewID(), tt.lower, now)
			if err != nil {
				t.Fatal(err)
			}
			mixed, err := NewSignal(NewID(), NewID(), tt.mixed, now)
			if err != nil {
				t.Fatal(err)
			}
			if mixed.Key != lower.Key || mixed.DedupeKey != lower.DedupeKey {
				t.Fatalf("mixed case keys %q, %q; want %q, %q", mixed.Key, mixed.DedupeKey, lower.Key, lower.DedupeKey)
			}
		})
	}
}
