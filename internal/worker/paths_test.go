package worker

import (
	"context"
	"slices"
	"testing"
	"uuid"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/config"
	"github.com/forecrew/forecrew/internal/roles"
)

func TestPathAllowed(t *testing.T) {
	dev := []string{"**", "!.github/**", "!.forecrew/**"}
	docs := []string{"docs/**", "*.md"}
	tests := []struct {
		name     string
		patterns []string
		file     string
		want     bool
	}{
		{"developer source", dev, "internal/a/b.go", true},
		{"developer root file", dev, "main.go", true},
		{"developer workflow excluded", dev, ".github/workflows/ci.yml", false},
		{"developer forecrew dir excluded", dev, ".forecrew/profile.yaml", false},
		{"docs include", docs, "docs/a/b.md", true},
		{"docs root md", docs, "README.md", true},
		{"docs outside", docs, "src/main.go", false},
		{"read only", nil, "a.go", false},
		{"only excludes", []string{"!a.go"}, "b.go", false},
		{"bad pattern matches nothing", []string{"[a"}, "a", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := roles.PathAllowed(tt.patterns, tt.file); got != tt.want {
				t.Errorf("roles.PathAllowed(%v, %q) = %v, want %v", tt.patterns, tt.file, got, tt.want)
			}
		})
	}
}

func TestOutsideWritePaths(t *testing.T) {
	got := outsideWritePaths([]string{"src/**"}, []string{"src/a.go", "main.go", "docs/x.md"})
	if want := []string{"main.go", "docs/x.md"}; !slices.Equal(got, want) {
		t.Errorf("outsideWritePaths() = %v, want %v", got, want)
	}
	if got := outsideWritePaths([]string{"**"}, []string{"a"}); len(got) != 0 {
		t.Errorf("outsideWritePaths() = %v, want none", got)
	}
}

// TestTestRoleWritePaths checks the write paths of the test roles, as the
// resolver hands them to jobs, with the check the worker applies.
func TestTestRoleWritePaths(t *testing.T) {
	cfg := &config.Config{
		Runtimes:   map[string]config.Runtime{"claude": {Adapter: config.AdapterClaudeCode, Vendor: "anthropic"}},
		ModelTiers: map[config.Tier]map[string]string{config.TierStrong: {"anthropic": "m"}, config.TierBest: {"anthropic": "m"}},
		Roles: map[string]config.Role{
			"test-architect":       {Runtime: "claude", Tier: config.TierBest},
			"environment-engineer": {Runtime: "claude", Tier: config.TierStrong},
			"test-engineer":        {Runtime: "claude", Tier: config.TierStrong},
		},
	}
	c, err := roles.New(forecrew.Files, cfg)
	if err != nil {
		t.Fatalf("roles.New: %v", err)
	}
	tests := map[string]struct{ allowed, denied []string }{
		"test-engineer": {
			allowed: []string{
				"internal/store/reset_test.go", "web/src/reset.test.tsx", "web/src/reset.spec.ts", "spec/reset_spec.rb", "web/src/reset.spec.tsx", "test/conftest.py", "tests/fixtures/users.json",
				"tests/test_reset.py", "pkg/tests/helpers.py", "src/test/java/ResetTest.java", "web/src/__tests__/reset.tsx",
				"internal/store/testdata/tokens.json", "test_reset.py",
			},
			denied: []string{
				"internal/store/reset.go", "web/src/reset.tsx", "main.go", "go.mod", "package.json", "Makefile",
				".forecrew/matrix.yaml", ".forecrew/environments/linux.yaml", ".forecrew/tests/x_test.go",
				".github/workflows/ci.yml", ".github/tests/x.yml", "vendor/x/y_test.go", "web/node_modules/a/a.test.js",
				// Dependency, build and CI files inside test directories.
				"tests/go.mod", "tests/go.sum", "test/go.work", "tests/package.json", "tests/pnpm-lock.yaml",
				"test/package-lock.json", "tests/Cargo.lock", "tests/yarn.lock", "tests/bun.lockb", "tests/Cargo.toml",
				"tests/requirements-dev.txt", "tests/pyproject.toml", "test/Makefile", "tests/rules.mk",
				"tests/Dockerfile", "tests/Dockerfile.ci", "tests/docker-compose.yml", "test/run.sh", "tests/setup.ps1",
				"tests/.github/workflows/ci.yml", "tests/.gitlab-ci.yml", "test/Jenkinsfile", "test/pom.xml",
				"tests/build.gradle.kts",
				// Spec-like documents are not test code.
				"docs/openapi.spec.json", "docs/product_spec.md", "api/v1.spec.yaml",
			},
		},
		"environment-engineer": {
			allowed: []string{".forecrew/environments/linux.yaml", ".forecrew/environments/windows.yaml"},
			denied: []string{
				".forecrew/matrix.yaml", ".forecrew/test-strategy.md", ".forecrew/profile.yaml", "Dockerfile",
				".devcontainer/devcontainer.json", ".github/workflows/ci.yml", "internal/a_test.go", "main.go",
			},
		},
		"test-architect": {
			allowed: []string{".forecrew/matrix.yaml", ".forecrew/test-strategy.md"},
			denied: []string{
				".forecrew/environments/linux.yaml", ".forecrew/profile.yaml", "internal/a_test.go", "main.go",
				".github/workflows/ci.yml",
			},
		},
	}
	for name, tt := range tests {
		r, err := c.Resolve(context.Background(), uuid.Nil(), uuid.Nil(), name)
		if err != nil {
			t.Fatalf("Resolve %s: %v", name, err)
		}
		for _, f := range tt.allowed {
			if !roles.PathAllowed(r.WritePaths, f) {
				t.Errorf("%s may not write %s", name, f)
			}
		}
		for _, f := range tt.denied {
			if roles.PathAllowed(r.WritePaths, f) {
				t.Errorf("%s may write %s", name, f)
			}
		}
	}
}

func TestLockedPaths(t *testing.T) {
	got := lockedPaths([]string{"src/*_test.go", "testdata/**"}, []string{"src/a.go", "src/a_test.go", "testdata/x/y.json"})
	if want := []string{"src/a_test.go", "testdata/x/y.json"}; !slices.Equal(got, want) {
		t.Errorf("lockedPaths() = %v, want %v", got, want)
	}
	if got := lockedPaths(nil, []string{"a"}); len(got) != 0 {
		t.Errorf("lockedPaths(nil) = %v, want none", got)
	}
}
