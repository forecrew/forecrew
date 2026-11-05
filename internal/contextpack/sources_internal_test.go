package contextpack

import (
	"slices"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/knowledge"
)

func TestPathsIn(t *testing.T) {
	text := "Edit internal/store/jobs.go and ./web/src/App.tsx, then .github/workflows/ci.yml.\n" +
		"See `docs/adr/0003-tokens.md`. Not and/or, e.g. this, i.e. that, w/o it, https://example.com/a/b, ../secret/x.go, /etc/passwd.\n" +
		"Also Makefile, README.md, v1.2 and go1.26 and cmd/.\n" +
		"internal/store/jobs.go again, with <script>/x.go and a\\b.go."
	want := []string{"internal/store/jobs.go", "web/src/App.tsx", ".github/workflows/ci.yml", "docs/adr/0003-tokens.md", "README.md", "cmd"}
	got := pathsIn(text, 30)
	if !slices.Equal(got, want) {
		t.Errorf("pathsIn = %q\nwant      %q", got, want)
	}
	if got := pathsIn(text, 2); len(got) != 2 {
		t.Errorf("pathsIn with a limit of 2 = %q", got)
	}
	if got := pathsIn(strings.Repeat("a", 300)+"/b.go", 5); got != nil {
		t.Errorf("a path of 300 characters = %q", got)
	}
}

func TestOrQuery(t *testing.T) {
	got := orQuery("Add the token store: tokens, token and Store; with a TTL of 1 hour. Hash hash hash", 4)
	if want := "token or store or tokens or hour"; got != want {
		t.Errorf("orQuery = %q, want %q", got, want)
	}
	if got := orQuery("Do it. Add a b c", 5); got != "" {
		t.Errorf("a text of short and common words = %q", got)
	}
}

func TestReviewOutcome(t *testing.T) {
	job := func(role, out string) domain.Job { return domain.Job{Role: role, Output: []byte(out)} }
	count, last := reviewOutcome([]domain.Job{
		job("developer", `{"verdict":"approve"}`), job("reviewer", `{"verdict":"nonsense"}`), job("reviewer", `not json`),
		job("reviewer", `{"verdict":"request_changes"}`), job("reviewer", `{"verdict":"approve"}`),
	})
	if count != 2 || last != "request_changes" {
		t.Errorf("reviewOutcome = %d, %q; want 2 reviews, the newest valid verdict", count, last)
	}
	if count, _ := reviewOutcome(nil); count != 0 {
		t.Errorf("count = %d", count)
	}
}

func TestSafeURL(t *testing.T) {
	for u, want := range map[string]bool{
		"https://github.com/acme/app/pull/1": true, "http://github.com/x": false, "https://x.com/a b": false,
		"https://x.com/`a": false, "https://x.com/a\n# Heading": false, "": false,
	} {
		if got := safeURL(u); got != want {
			t.Errorf("safeURL(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestClip(t *testing.T) {
	if got := clip("short"); got != "short" {
		t.Errorf("clip = %q", got)
	}
	long := strings.Repeat("é", excerpt+10)
	if got := clip(long); len([]rune(got)) != excerpt || !strings.HasSuffix(got, "…") {
		t.Errorf("clip cut to %d runes: %q", len([]rune(got)), got[len(got)-6:])
	}
}

func TestKnowledgeItemLabels(t *testing.T) {
	it := knowledgeItem(testResult("Bad Section\n# x", true), true)
	if it.Label != "entry" || !strings.Contains(it.Body, "May be out of date: moved") {
		t.Errorf("item = %+v", it)
	}
	if it := knowledgeItem(testResult("architecture", false), false); it.Label != "entry in architecture" || strings.Contains(it.Body, "snippet") || it.Untrusted {
		t.Errorf("item = %+v", it)
	}
}

func testResult(section string, reverify bool) knowledge.Result {
	r := knowledge.Result{Kind: domain.KnowledgeEntryKind, Provider: "repo", Section: section, Path: "docs/a.md", Title: "A", Snippet: "snippet", Untrusted: section == "Bad Section\n# x"}
	if reverify {
		r.NeedsReverify, r.ReverifyReason = true, "moved\nin abc"
	}
	return r
}
