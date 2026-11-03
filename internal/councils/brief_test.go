package councils

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"

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
