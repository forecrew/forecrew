package knowledge_test

import (
	"errors"
	"flag"
	"path/filepath"
	"testing"
	"time"

	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/testkit"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/golden/knowledge")

var (
	fixtures = filepath.Join("..", "..", "testdata", "knowledge")
	goldens  = filepath.Join("..", "..", "testdata", "golden", "knowledge")
)

func TestPolicyFixtures(t *testing.T) {
	testkit.Fixtures(t, filepath.Join(fixtures, "policy"), filepath.Join(goldens, "policy"), knowledge.ErrInvalid, *update,
		func(_ string, data []byte) (any, error) { return knowledge.ParsePolicy(data) })
}

func TestEntryFixtures(t *testing.T) {
	testkit.Fixtures(t, filepath.Join(fixtures, "entries"), filepath.Join(goldens, "entries"), knowledge.ErrInvalid, *update,
		func(file string, data []byte) (any, error) { return knowledge.ParseEntry("knowledge/"+file, data) })
}

func TestPolicyDefaults(t *testing.T) {
	p, err := knowledge.ParsePolicy([]byte("version: 1\nsections:\n  lessons: {}\n  notes: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := p.Sections["lessons"]
	if s.Provider != knowledge.RepoProvider || s.Paths[0] != "knowledge/lessons/**" || s.Owner != knowledge.OwnerHuman ||
		s.Access.Read[0] != knowledge.Everyone || s.Access.Propose[0] != knowledge.Everyone || s.Access.Write == nil || len(s.Access.Write) != 0 {
		t.Fatalf("lessons = %+v", s)
	}
	if n := p.Sections["notes"]; n.Provider != "" || n.Access.Read[0] != knowledge.Everyone || n.Access.Propose != nil {
		t.Fatalf("notes = %+v", n)
	}
	if !knowledge.Reserved("generated") || !knowledge.Reserved("notes") || knowledge.Reserved("lessons") {
		t.Fatal("knowledge.Reserved")
	}
	if _, err := knowledge.ParsePolicy([]byte("version: [")); !errors.Is(err, knowledge.ErrInvalid) {
		t.Fatalf("bad yaml: %v", err)
	}
}

func TestParseEntryTitles(t *testing.T) {
	e, err := knowledge.ParseEntry("docs/runbooks/restart-worker.md", []byte("No heading here.\n"))
	if err != nil || e.Title != "restart-worker" || e.HasProvenance || !e.Provenance.Created.IsZero() {
		t.Fatalf("entry = %+v, %v", e, err)
	}
	e, err = knowledge.ParseEntry("x.md", []byte("---\ntitle: Given\nauthor_role: human\nsource: human\ncreated: 2026-01-02T03:04:05+01:00\n---\n# Heading\n"))
	if err != nil || e.Title != "Given" || !e.HasProvenance || !e.Provenance.Created.Equal(time.Date(2026, 1, 2, 2, 4, 5, 0, time.UTC)) {
		t.Fatalf("entry = %+v, %v", e, err)
	}
}
