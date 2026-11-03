package testkit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var errBad = errors.New("bad")

func TestFixtures(t *testing.T) {
	dir, goldens := t.TempDir(), t.TempDir()
	for _, f := range []string{"valid/a.txt", "invalid/b.txt"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	parse := func(_ string, data []byte) (any, error) {
		if strings.HasPrefix(string(data), "invalid") {
			return nil, errBad
		}
		return map[string]string{"got": string(data)}, nil
	}
	Fixtures(t, dir, goldens, errBad, true, parse) // writes the goldens
	Fixtures(t, dir, goldens, errBad, false, parse)
	got, err := os.ReadFile(filepath.Join(goldens, "valid", "a.txt.golden"))
	if err != nil || string(got) != "{\n  \"got\": \"valid/a.txt\"\n}\n" {
		t.Fatalf("golden = %q, %v", got, err)
	}
	if got, _ := os.ReadFile(filepath.Join(goldens, "invalid", "b.txt.golden")); string(got) != "bad\n" {
		t.Fatalf("invalid golden = %q", got)
	}
}
