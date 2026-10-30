package playbooks_test

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/github"
)

// openPR opens a PR for the env's project and returns its number and head SHA.
func (v *env) openPR(t *testing.T) (int, string) {
	t.Helper()
	out, err := v.call(t, "github.open_or_update_pr", prWith(nil))
	must(t, err)
	m := toMap(t, out)
	return int(m["number"].(float64)), m["head_sha"].(string)
}

func TestMergePR(t *testing.T) {
	tests := []struct {
		name  string
		state github.CheckState // none leaves the commit without checks
		total int
	}{
		{"checks passed", github.ChecksSuccess, 2},
		{"no checks", github.ChecksNone, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newEnv(t)
			n, sha := v.openPR(t)
			must(t, v.tracker.SetCheckStatus("acme/app", sha, tt.state, tt.total))
			out, err := v.call(t, "github.merge_pr", map[string]any{"repo": "acme/app", "number": n})
			must(t, err)
			m := toMap(t, out)
			if m["merged"] != true || m["already_merged"] != false || m["sha"] != "merge-1" {
				t.Errorf("output = %v", m)
			}
			pr, _ := v.tracker.GetPR(t.Context(), "acme/app", n)
			if !pr.Merged {
				t.Error("pull request not merged")
			}
			c, err := v.st.Changes().GetByPR(t.Context(), v.account, "acme/app", n)
			must(t, err)
			if c.State != domain.ChangeMerged {
				t.Errorf("change state = %s, want merged", c.State)
			}
			sigs, err := v.bus.Lookup(t.Context(), v.account, domain.PRKey("acme/app", n))
			must(t, err)
			if len(sigs) != 1 || sigs[0].Kind != domain.SignalPRMerged {
				t.Errorf("signals = %+v", sigs)
			}
		})
	}
}

func TestMergePRSkipsMergedPR(t *testing.T) {
	v := newEnv(t)
	n, _ := v.openPR(t)
	_, err := v.tracker.MergePR(t.Context(), "acme/app", n, github.MergeInput{})
	must(t, err)
	out, err := v.call(t, "github.merge_pr", map[string]any{"repo": "acme/app", "number": n})
	must(t, err)
	if m := toMap(t, out); m["merged"] != true || m["already_merged"] != true || m["sha"] != "merge-1" {
		t.Errorf("output = %v", m)
	}
	sigs, _ := v.bus.Lookup(t.Context(), v.account, domain.PRKey("acme/app", n))
	if len(sigs) != 0 {
		t.Errorf("an already merged PR published %d signals", len(sigs))
	}
}

func TestMergePRRefuses(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, v *env, n int, sha string)
		with  map[string]any
		want  string
	}{
		{"checks failed", func(t *testing.T, v *env, _ int, sha string) {
			must(t, v.tracker.SetCheckStatus("acme/app", sha, github.ChecksFailure, 2))
		}, nil, "checks failed"},
		{"checks running", func(t *testing.T, v *env, _ int, sha string) {
			must(t, v.tracker.SetCheckStatus("acme/app", sha, github.ChecksPending, 2))
		}, nil, "still running"},
		{"closed without merging", func(t *testing.T, v *env, n int, _ string) {
			must(t, v.tracker.ClosePR("acme/app", n))
		}, nil, "closed without merging"},
		{"unknown PR", func(*testing.T, *env, int, string) {}, map[string]any{"number": 99}, "get pull request"},
		{"unknown key", func(*testing.T, *env, int, string) {}, map[string]any{"method": "squash"}, "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newEnv(t)
			n, sha := v.openPR(t)
			tt.setup(t, v, n, sha)
			with := map[string]any{"repo": "acme/app", "number": n}
			maps.Copy(with, tt.with)
			_, err := v.call(t, "github.merge_pr", with)
			wantErr(t, err, tt.want)
			if pr, _ := v.tracker.GetPR(t.Context(), "acme/app", n); pr.Merged {
				t.Error("pull request merged")
			}
		})
	}
}

func TestMergePRChecksTheProfileFile(t *testing.T) {
	tests := []struct {
		name    string
		file    string // empty means no file
		wantErr string
	}{
		{"valid", `{"version": 1, "name": "shop", "summary": "x"}`, ""},
		{"edited to something invalid", `{"version": 7, "name": "shop", "summary": "x"}`, "not merging acme/app#1: .forecrew/profile.yaml at agent/onboarding-1"},
		{"missing", "", "does not exist at agent/onboarding-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newEnv(t)
			n, sha := v.openPR(t)
			if tt.file != "" {
				must(t, v.tracker.SetFile("acme/app", sha, ".forecrew/profile.yaml", []byte(tt.file)))
			}
			_, err := v.call(t, "github.merge_pr", map[string]any{"repo": "acme/app", "number": n, "check_profile": true})
			pr, _ := v.tracker.GetPR(t.Context(), "acme/app", n)
			if tt.wantErr == "" {
				must(t, err)
			} else {
				wantErr(t, err, tt.wantErr)
			}
			if pr.Merged != (tt.wantErr == "") {
				t.Errorf("merged = %v, error %v", pr.Merged, err)
			}
		})
	}
	// Without check_profile the file is not read.
	v := newEnv(t)
	n, _ := v.openPR(t)
	_, err := v.call(t, "github.merge_pr", map[string]any{"repo": "acme/app", "number": n})
	must(t, err)
}

func TestMergePRFileReadError(t *testing.T) {
	v := newEnv(t)
	v.wrapped = &failingFiles{Tracker: v.tracker, n: 1}
	v.register()
	n, _ := v.openPR(t)
	_, err := v.call(t, "github.merge_pr", map[string]any{"repo": "acme/app", "number": n, "check_profile": true})
	wantErr(t, err, "connection reset")
}

// failingFiles is a tracker whose GetFile fails the first n times.
type failingFiles struct {
	github.Tracker
	n int
}

func (f *failingFiles) GetFile(ctx context.Context, repo, ref, path string) ([]byte, error) {
	if f.n > 0 {
		f.n--
		return nil, errors.New("connection reset")
	}
	return f.Tracker.GetFile(ctx, repo, ref, path)
}
