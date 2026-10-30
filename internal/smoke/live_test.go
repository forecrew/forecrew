//go:build smoke

package smoke

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLive runs the smoke test against the instance named by the
// environment (see docs/runbooks/smoke-live.md) and writes the report.
func TestLive(t *testing.T) {
	// Checked first: the run spends model budget and must not fail afterwards.
	path := os.Getenv("FORECREW_SMOKE_REPORT") // make smoke-live sets it to an absolute path
	if !filepath.IsAbs(path) {
		t.Fatalf("FORECREW_SMOKE_REPORT must be an absolute path, got %q (use make smoke-live)", path)
	}
	cfg := Config{
		ServerURL: os.Getenv("FORECREW_SMOKE_URL"), Token: os.Getenv("FORECREW_SMOKE_TOKEN"),
		Repo: os.Getenv("FORECREW_SMOKE_REPO"), Project: os.Getenv("FORECREW_SMOKE_PROJECT"),
		GitHubToken: os.Getenv("FORECREW_SMOKE_GITHUB_TOKEN"),
	}
	rep, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(rep.Markdown()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("report written to %s", path)
	if !rep.Passed() {
		t.Fatalf("smoke test failed:\n%s", rep.Markdown())
	}
}
