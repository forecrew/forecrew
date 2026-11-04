package councils

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/testkit"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/golden/councils")

func TestBriefFixtures(t *testing.T) {
	testkit.Fixtures(t, filepath.Join("..", "..", "testdata", "councils", "brief"),
		filepath.Join("..", "..", "testdata", "golden", "councils", "brief"), ErrInvalid, *update,
		func(_ string, data []byte) (any, error) { return ParseBrief(data) })
}

// TestBriefSchemaCopies: the role pack copy equals the contract, and the
// shared definitions equal those of packs/schemas/common.json.
func TestBriefSchemaCopies(t *testing.T) {
	contract := read(t, "docs", "contracts", "council-brief.schema.json")
	if pack := read(t, "packs", "schemas", "council-brief.json"); string(pack) != string(contract) {
		t.Error("packs/schemas/council-brief.json differs from docs/contracts/council-brief.schema.json; copy the contract over it")
	}
	var brief, common struct {
		Defs map[string]any `json:"$defs"`
	}
	if err := json.Unmarshal(contract, &brief); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(read(t, "packs", "schemas", "common.json"), &common); err != nil {
		t.Fatal(err)
	}
	for _, def := range []string{"text", "level", "assumption"} {
		if !reflect.DeepEqual(brief.Defs[def], common.Defs[def]) {
			t.Errorf("$defs/%s of the brief differs from packs/schemas/common.json", def)
		}
	}
}

func read(t *testing.T, path ...string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, path...)...))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestUndisputed(t *testing.T) {
	advocate := domain.BriefDissent{Member: "devils-advocate@codex", Position: DevilsAdvocateDissent + "No."}
	tests := []struct {
		name string
		b    domain.CouncilBrief
		want bool
	}{
		{"nobody dissents", domain.CouncilBrief{Recommendation: "pg", Votes: []domain.BriefVote{{Member: "ux@codex", Option: "pg"}}}, true},
		{"the mandated dissent", domain.CouncilBrief{Recommendation: "pg", Dissent: []domain.BriefDissent{advocate},
			Votes: []domain.BriefVote{{Member: "devils-advocate@codex", Option: "nats"}}}, true},
		{"a real dissent", domain.CouncilBrief{Recommendation: "pg", Dissent: []domain.BriefDissent{advocate, {Member: "ux@codex", Position: "No."}}}, false},
		{"a vote against without a dissent", domain.CouncilBrief{Recommendation: "pg", Votes: []domain.BriefVote{{Member: "ux@codex", Option: "nats"}}}, false},
	}
	for _, tt := range tests {
		if got := Undisputed(tt.b); got != tt.want {
			t.Errorf("%s: Undisputed = %v", tt.name, got)
		}
	}
}
