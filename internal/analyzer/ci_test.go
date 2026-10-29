package analyzer

import (
	"errors"
	"io/fs"
	"slices"
	"testing"
)

func TestCIRunners(t *testing.T) {
	tests := []struct {
		name string
		yml  string
		want []string
	}{
		{
			name: "string runs-on",
			yml:  "jobs:\n  a:\n    runs-on: ubuntu-latest\n",
			want: []string{"ubuntu-latest"},
		},
		{
			name: "list runs-on and expression",
			yml:  "jobs:\n  a:\n    runs-on: [self-hosted, macos-14]\n  b:\n    runs-on: ${{ matrix.os }}\n    strategy:\n      matrix:\n        os: [windows-latest, ubuntu-latest]\n",
			want: []string{"macos-14", "self-hosted", "ubuntu-latest", "windows-latest"},
		},
		{
			name: "matrix include",
			yml:  "jobs:\n  a:\n    runs-on: x\n    strategy:\n      matrix:\n        include:\n          - os: macos-latest\n          - name: no-os\n          - os: ${{ matrix.x }}\n",
			want: []string{"macos-latest", "x"},
		},
		{
			name: "matrix with a non-list os",
			yml:  "jobs:\n  a:\n    strategy:\n      matrix:\n        os: windows-latest\n",
			want: []string{"windows-latest"},
		},
		{
			name: "unparsable workflow",
			yml:  "jobs: [\n",
			want: []string{},
		},
		{
			name: "no jobs",
			yml:  "name: empty\n",
			want: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFiles(t, root, map[string]string{".github/workflows/ci.yml": tt.yml})
			got, err := ciRunners(root, []string{".github/workflows/ci.yml", "README.md"})
			if err != nil {
				t.Fatalf("ciRunners: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("ciRunners = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCIRunnersMissingFile(t *testing.T) {
	_, err := ciRunners(t.TempDir(), []string{".github/workflows/gone.yml"})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestStringsOf(t *testing.T) {
	if got := stringsOf(42); got != nil {
		t.Errorf("stringsOf(42) = %v, want nil", got)
	}
	if got := stringsOf([]any{"a", 1, "b"}); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("stringsOf(list) = %v, want [a b]", got)
	}
}
