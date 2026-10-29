package analyzer

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"testing"
)

func TestComponentsInTempDirs(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []Component
	}{
		{
			name: "pnpm package with scripts",
			files: map[string]string{
				"package.json":   `{"scripts":{"build":"tsc","test":"vitest"}}`,
				"pnpm-lock.yaml": "x",
			},
			want: []Component{{Path: ".", Stack: "node", Manifests: []string{"package.json"}, Build: "pnpm run build", Test: "pnpm test"}},
		},
		{
			name: "yarn package with scripts",
			files: map[string]string{
				"app/package.json": `{"scripts":{"build":"tsc","test":"vitest"}}`,
				"app/yarn.lock":    "x",
			},
			want: []Component{{Path: "app", Stack: "node", Manifests: []string{"app/package.json"}, Build: "yarn run build", Test: "yarn test"}},
		},
		{
			name: "invalid package.json has no scripts",
			files: map[string]string{
				"package.json": `{"scripts":`,
			},
			want: []Component{{Path: ".", Stack: "node", Manifests: []string{"package.json"}}},
		},
		{
			name: "makefile targets with a go module",
			files: map[string]string{
				"go.mod":   "module x\n",
				"Makefile": "VERSION := 1\n.PHONY: build\nbuild: deps\n\techo b\n\ntest:\n\techo t\n\nname != echo\n",
			},
			want: []Component{{Path: ".", Stack: "go", Manifests: []string{"Makefile", "go.mod"}, Build: "make build", Test: "make test"}},
		},
		{
			name: "makefile only",
			files: map[string]string{
				"tools/Makefile": "lint:\n\techo l\n",
			},
			want: []Component{{Path: "tools", Stack: "make", Manifests: []string{"tools/Makefile"}}},
		},
		{
			name: "docker only",
			files: map[string]string{
				"deploy/Dockerfile": "FROM scratch\n",
			},
			want: []Component{{Path: "deploy", Stack: "docker", Manifests: []string{"deploy/Dockerfile"}, Build: "docker build ."}},
		},
		{
			name: "maven, gradle and cmake",
			files: map[string]string{
				"java/pom.xml":            "<project/>",
				"gradle/build.gradle.kts": "plugins {}",
				"native/CMakeLists.txt":   "project(x)",
			},
			want: []Component{
				{Path: "gradle", Stack: "java", Manifests: []string{"gradle/build.gradle.kts"}, Build: "./gradlew build", Test: "./gradlew test"},
				{Path: "java", Stack: "java", Manifests: []string{"java/pom.xml"}, Build: "mvn -B package", Test: "mvn -B test"},
				{Path: "native", Stack: "cmake", Manifests: []string{"native/CMakeLists.txt"}, Build: "cmake -S . -B build && cmake --build build", Test: "ctest --test-dir build"},
			},
		},
		{
			name: "python requirements only",
			files: map[string]string{
				"requirements.txt": "requests\n",
			},
			want: []Component{{Path: ".", Stack: "python", Manifests: []string{"requirements.txt"}, Build: "python -m build", Test: "python -m pytest"}},
		},
		{
			name:  "no build file",
			files: map[string]string{"notes/todo.txt": "x", "README.md": "x"},
			want:  []Component{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFiles(t, root, tt.files)
			facts, err := Local{}.Analyze(context.Background(), root)
			if err != nil {
				t.Fatalf("Analyze: %v", err)
			}
			if !slices.EqualFunc(facts.Components, tt.want, func(a, b Component) bool {
				return a.Path == b.Path && a.Stack == b.Stack && a.Build == b.Build && a.Test == b.Test &&
					slices.Equal(a.Manifests, b.Manifests)
			}) {
				t.Errorf("components = %+v, want %+v", facts.Components, tt.want)
			}
		})
	}
}

func TestComponentsErrors(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"Makefile": "build:\n"})
	// The Makefile is listed but its directory is gone, so reading it fails.
	if _, err := components(root, []string{"gone/Makefile"}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("components with missing Makefile: err = %v, want fs.ErrNotExist", err)
	}
	if _, _, err := componentOf(root, "missing", []string{"package.json"}); err == nil {
		t.Error("componentOf with missing package.json: want error")
	}
	if _, _, err := commands(root, "missing", "go", []string{"Makefile"}); err == nil {
		t.Error("commands with missing Makefile: want error")
	}
	if _, err := makeTargets(filepath.Join(root, "nope")); err == nil {
		t.Error("makeTargets on missing file: want error")
	}
	if _, err := packageScripts(filepath.Join(root, "nope")); err == nil {
		t.Error("packageScripts on missing file: want error")
	}
}

func TestComponentOfWithoutBuildFile(t *testing.T) {
	_, ok, err := componentOf(t.TempDir(), "docs", []string{"README.md"})
	if err != nil || ok {
		t.Errorf("componentOf without build file = (ok %v, err %v), want (false, nil)", ok, err)
	}
}

func TestStackDefaultsUnknownStack(t *testing.T) {
	if b, test := stackDefaults("cobol", nil); b != "" || test != "" {
		t.Errorf("stackDefaults(cobol) = %q, %q, want empty", b, test)
	}
}
