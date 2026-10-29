package worker

import (
	"slices"
	"testing"
)

func TestWritable(t *testing.T) {
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
			if got := writable(tt.patterns, tt.file); got != tt.want {
				t.Errorf("writable(%v, %q) = %v, want %v", tt.patterns, tt.file, got, tt.want)
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
