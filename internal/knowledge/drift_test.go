package knowledge

import (
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/github"
)

func TestDriftOf(t *testing.T) {
	cmp := github.Comparison{Files: []github.FileChange{
		{Path: "internal/new.go", PreviousPath: "internal/old.go", Status: github.FileRenamed},
		{Path: "internal/queue/queue.go", Status: github.FileModified, Patch: "@@\n-func (q *Queue) Lease(ctx int) {}\n+func (q *Queue) LeaseNext(ctx int) {}\n func helper() {}\n+type Options struct{}\n+func New() {}\n+const MaxLeases = 3"},
		{Path: "web/src/api.ts", Status: github.FileModified, Patch: "+export async function fetchJobs() {}\n-export const Limit = 1\n+export interface JobView {}"},
		{Path: "README.md", Status: github.FileModified, Patch: "+func Docs() {}"},
		{Path: "gone.go", Status: github.FileRemoved, Patch: "-func Vanished() {}"},
		{Path: "fresh.go", Status: github.FileAdded, Patch: "+func Fresh() {}"},
	}}
	d := DriftOf(cmp)
	wantPaths := []string{"README.md", "gone.go", "internal/new.go", "internal/old.go", "internal/queue/queue.go", "web/src/api.ts"}
	wantSymbols := []string{"JobView", "Lease", "LeaseNext", "Limit", "MaxLeases", "Options", "Vanished", "fetchJobs"}
	wantSymbols = slices.DeleteFunc(wantSymbols, func(s string) bool { return len(s) < minSymbol })
	if !slices.Equal(d.Paths, wantPaths) {
		t.Errorf("paths = %v, want %v", d.Paths, wantPaths)
	}
	if !slices.Equal(d.Symbols, wantSymbols) {
		t.Errorf("symbols = %v, want %v", d.Symbols, wantSymbols)
	}
}

func TestDriftMentions(t *testing.T) {
	d := Drift{Paths: []string{"internal/old.go", "a.go"}, Symbols: []string{"LeaseNext"}}
	tests := []struct {
		body        string
		paths, syms int
	}{
		{"The queue lives in `internal/old.go`.", 1, 0},
		{"See internal/old.go, which leases.", 1, 0},
		{"See internal/old.go.", 1, 0},
		{"Look at myinternal/old.go or internal/old.gone", 0, 0},
		{"vendor/a.go and sub/a.go are other files", 0, 0},
		{"Call LeaseNext to get a job; LeaseNextAll is another", 0, 1},
		{"LeaseNextAll only", 0, 0},
		{"a.go", 1, 0},
	}
	for _, tt := range tests {
		p, s := d.Mentions(tt.body)
		if len(p) != tt.paths || len(s) != tt.syms {
			t.Errorf("Mentions(%q) = %v, %v", tt.body, p, s)
		}
	}
}

func TestDriftReason(t *testing.T) {
	if got := DriftReason("abcdef0123456789", []string{"a.go"}, []string{"Queue"}); got != "commit abcdef012345 changed a.go, Queue" {
		t.Errorf("reason = %q", got)
	}
	got := DriftReason("abc", []string{"1", "2", "3", "4", "5", "6", "7"}, nil)
	if !strings.HasSuffix(got, "and 2 more") || !strings.HasPrefix(got, "commit abc changed") {
		t.Errorf("long reason = %q", got)
	}
}
