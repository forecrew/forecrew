package analyzer_test

import (
	"bytes"
	"flag"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/forecrew/forecrew/internal/analyzer"
	"github.com/forecrew/forecrew/internal/testkit"
)

// TestProfileFixtures: profiles with and without integrations validate
// and decode into Profile; bad integrations are refused. It runs outside
// package analyzer because testkit imports it; -update is the flag of the
// package's own tests.
func TestProfileFixtures(t *testing.T) {
	update := flag.Lookup("update").Value.(flag.Getter).Get().(bool)
	testkit.Fixtures(t, filepath.Join("..", "..", "testdata", "profiles"), filepath.Join("..", "..", "testdata", "golden", "profiles"),
		analyzer.ErrInvalidProfile, update, func(_ string, data []byte) (any, error) {
			if err := analyzer.Validate(data); err != nil {
				return nil, err
			}
			var p analyzer.Profile
			dec := yaml.NewDecoder(bytes.NewReader(data))
			dec.KnownFields(true)
			return p, dec.Decode(&p)
		})
}
