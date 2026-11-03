package testkit

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Fixtures runs parse on every file of dir/valid and dir/invalid: valid
// files must parse, invalid ones must fail with an error wrapping invalid.
// The result (indented JSON) or the error message must equal the file's
// golden file goldens/<valid|invalid>/<name>.golden; with update the golden
// files are rewritten first.
func Fixtures(t *testing.T, dir, goldens string, invalid error, update bool, parse func(file string, data []byte) (any, error)) {
	t.Helper()
	for _, outcome := range []string{"valid", "invalid"} {
		files, err := filepath.Glob(filepath.Join(dir, outcome, "*"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s/%s fixtures: %v, %v", dir, outcome, files, err)
		}
		for _, file := range files {
			name := outcome + "/" + filepath.Base(file)
			t.Run(name, func(t *testing.T) {
				out := parseFixture(t, file, outcome == "valid", invalid, parse)
				CheckGolden(t, filepath.Join(goldens, name+".golden"), out, update)
			})
		}
	}
}

// parseFixture parses one file and returns what its golden file holds.
func parseFixture(t *testing.T, file string, valid bool, invalid error, parse func(string, []byte) (any, error)) []byte {
	t.Helper()
	data, err := os.ReadFile(file) //nolint:gosec // a fixture path from the test
	if err != nil {
		t.Fatal(err)
	}
	got, err := parse(filepath.Base(file), data)
	switch {
	case valid && err != nil:
		t.Fatalf("valid fixture refused: %v", err)
	case !valid && !errors.Is(err, invalid):
		t.Fatalf("invalid fixture: err = %v, want %v", err, invalid)
	case err != nil:
		return []byte(err.Error() + "\n")
	}
	out, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

// CheckGolden fails t unless got equals the golden file; with update it
// writes got there first.
func CheckGolden(t testing.TB, golden string, got []byte, update bool) {
	t.Helper()
	if update {
		if err := os.MkdirAll(filepath.Dir(golden), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil { //nolint:gosec // a fixture, not a secret
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden) //nolint:gosec // a golden path from the test
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s; rerun the test with -update\n%s", golden, got)
	}
}
