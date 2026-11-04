package roles

import (
	"slices"
	"testing"
)

func TestReadOnly(t *testing.T) {
	if !(Role{}).ReadOnly() {
		t.Error("role without write paths is not read-only")
	}
	if (Role{WritePaths: []string{"**", "!.github/**"}}).ReadOnly() {
		t.Error("role with write paths is read-only")
	}
}

func TestWithoutWrites(t *testing.T) {
	allowed := Definition{Tools: []string{"repo.read", "repo.write", "git.read", "git.commit", "run.tests"}}.AllowedTools()
	r := Role{Name: "developer", WritePaths: []string{"**"}, AllowedTools: allowed}
	got := r.WithoutWrites()
	want := Definition{Tools: []string{"repo.read", "git.read", "run.tests"}}.AllowedTools()
	if !got.ReadOnly() || !slices.Equal(got.AllowedTools, want) || got.Name != "developer" {
		t.Errorf("WithoutWrites = %+v, want read-only with %v", got, want)
	}
	if !slices.Equal(r.AllowedTools, allowed) {
		t.Errorf("WithoutWrites changed the original: %v", r.AllowedTools)
	}
}

func TestPathAllowed(t *testing.T) {
	dev := []string{"**", "!.github/**"}
	for _, tt := range []struct {
		patterns []string
		file     string
		want     bool
	}{
		{dev, "a/b.go", true},
		{dev, ".github/ci.yml", false},
		{nil, "a.go", false},
		{[]string{"!a.go"}, "b.go", false},
		{[]string{"[a"}, "a", false},
	} {
		if got := PathAllowed(tt.patterns, tt.file); got != tt.want {
			t.Errorf("PathAllowed(%v, %q) = %v, want %v", tt.patterns, tt.file, got, tt.want)
		}
	}
}
