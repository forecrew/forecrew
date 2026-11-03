package knowledge

import (
	"errors"
	"flag"
	"path/filepath"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/testkit"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/golden/knowledge")

var (
	fixtures = filepath.Join("..", "..", "testdata", "knowledge")
	goldens  = filepath.Join("..", "..", "testdata", "golden", "knowledge")
)

func TestPolicyFixtures(t *testing.T) {
	testkit.Fixtures(t, filepath.Join(fixtures, "policy"), filepath.Join(goldens, "policy"), ErrInvalid, *update,
		func(_ string, data []byte) (any, error) { return ParsePolicy(data) })
}

func TestEntryFixtures(t *testing.T) {
	testkit.Fixtures(t, filepath.Join(fixtures, "entries"), filepath.Join(goldens, "entries"), ErrInvalid, *update,
		func(file string, data []byte) (any, error) { return ParseEntry("knowledge/"+file, data) })
}

func TestPolicyDefaults(t *testing.T) {
	p, err := ParsePolicy([]byte("version: 1\nsections:\n  lessons: {}\n  notes: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := p.Sections["lessons"]
	if s.Provider != RepoProvider || s.Paths[0] != "knowledge/lessons/**" || s.Owner != OwnerHuman ||
		s.Access.Read[0] != Everyone || s.Access.Propose[0] != Everyone || s.Access.Write == nil || len(s.Access.Write) != 0 {
		t.Fatalf("lessons = %+v", s)
	}
	if n := p.Sections["notes"]; n.Provider != "" || n.Access.Read[0] != Everyone || n.Access.Propose != nil {
		t.Fatalf("notes = %+v", n)
	}
	if !Reserved("generated") || !Reserved("notes") || Reserved("lessons") {
		t.Fatal("Reserved")
	}
	if _, err := ParsePolicy([]byte("version: [")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad yaml: %v", err)
	}
}

func TestSplitFrontmatter(t *testing.T) {
	tests := []struct {
		name, in, front, body string
		hasFront              bool
	}{
		{"none", "# T\n", "", "# T\n", false},
		{"empty", "---\n---\nbody\n", "", "body\n", true},
		{"crlf", "---\r\ntitle: x\r\n---\r\nbody\r\n", "title: x\n", "body\n", true},
		{"at end", "---\ntitle: x\n---", "title: x\n", "", true},
	}
	for _, tt := range tests {
		front, body, err := splitFrontmatter([]byte(tt.in))
		if err != nil || (front != nil) != tt.hasFront || string(front) != tt.front || string(body) != tt.body {
			t.Errorf("%s: front %q, body %q, %v", tt.name, front, body, err)
		}
	}
}

func TestParseEntryTitles(t *testing.T) {
	e, err := ParseEntry("docs/runbooks/restart-worker.md", []byte("No heading here.\n"))
	if err != nil || e.Title != "restart-worker" || e.HasProvenance || !e.Provenance.Created.IsZero() {
		t.Fatalf("entry = %+v, %v", e, err)
	}
	e, err = ParseEntry("x.md", []byte("---\ntitle: Given\nauthor_role: human\nsource: human\ncreated: 2026-01-02T03:04:05+01:00\n---\n# Heading\n"))
	if err != nil || e.Title != "Given" || !e.HasProvenance || !e.Provenance.Created.Equal(time.Date(2026, 1, 2, 2, 4, 5, 0, time.UTC)) {
		t.Fatalf("entry = %+v, %v", e, err)
	}
}
