package analyzer

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
)

// buildFiles lists the build files that make their directory a component. The
// order of buildFiles picks a component's stack: the first matching entry wins.
var buildFiles = []struct {
	pattern string // base name glob
	stack   string
}{
	{"go.mod", "go"},
	{"package.json", "node"},
	{"Cargo.toml", "rust"},
	{"pyproject.toml", "python"},
	{"requirements*.txt", "python"},
	{"pom.xml", "java"},
	{"build.gradle*", "java"},
	{"CMakeLists.txt", "cmake"},
	{"Makefile", "make"},
	{"Dockerfile", "docker"},
}

// makeTargetRe matches a target line such as "build: deps" in a Makefile.
var makeTargetRe = regexp.MustCompile(`(?m)^([A-Za-z0-9_][A-Za-z0-9_.-]*)[ \t]*:($|[^=])`)

// buildFileIndex returns the index in buildFiles of base, or -1.
func buildFileIndex(base string) int {
	return slices.IndexFunc(buildFiles, func(b struct{ pattern, stack string }) bool {
		return matchGlob(b.pattern, base)
	})
}

// components finds every directory that holds a build file, sorted by path.
func components(root string, files []string) ([]Component, error) {
	byDir := map[string][]string{} // directory -> base names of its files
	for _, rel := range files {
		dir := path.Dir(rel)
		byDir[dir] = append(byDir[dir], path.Base(rel))
	}
	var out []Component
	for _, dir := range slices.Sorted(maps.Keys(byDir)) {
		c, ok, err := componentOf(root, dir, byDir[dir])
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, c)
		}
	}
	return nonNil(out), nil
}

// componentOf returns the component of dir, whose files have the base names
// names, and false when dir holds no build file.
func componentOf(root, dir string, names []string) (Component, bool, error) {
	best := -1
	var manifests []string
	for _, n := range names {
		i := buildFileIndex(n)
		if i < 0 {
			continue
		}
		if best < 0 || i < best {
			best = i
		}
		manifests = append(manifests, path.Join(dir, n))
	}
	if best < 0 {
		return Component{}, false, nil
	}
	build, test, err := commands(root, dir, buildFiles[best].stack, names)
	if err != nil {
		return Component{}, false, fmt.Errorf("component %s: %w", dir, err)
	}
	slices.Sort(manifests)
	return Component{
		Path:      dir,
		Stack:     buildFiles[best].stack,
		Manifests: manifests,
		Build:     build,
		Test:      test,
	}, true, nil
}

// commands returns the likely build and test commands of a component run in
// its directory. package.json scripts win, then Makefile targets, then the
// defaults of the stack.
func commands(root, dir, stack string, names []string) (string, string, error) {
	var build, test string
	if slices.Contains(names, "package.json") {
		scripts, err := packageScripts(filepath.Join(root, filepath.FromSlash(dir), "package.json"))
		if err != nil {
			return "", "", err
		}
		pm := packageManager(names)
		if _, ok := scripts["build"]; ok {
			build = pm + " run build"
		}
		if _, ok := scripts["test"]; ok {
			test = pm + " test"
		}
	}
	if slices.Contains(names, "Makefile") {
		targets, err := makeTargets(filepath.Join(root, filepath.FromSlash(dir), "Makefile"))
		if err != nil {
			return "", "", err
		}
		if build == "" && targets["build"] {
			build = "make build"
		}
		if test == "" && targets["test"] {
			test = "make test"
		}
	}
	defBuild, defTest := stackDefaults(stack, names)
	return cmp.Or(build, defBuild), cmp.Or(test, defTest), nil
}

// packageScripts returns the scripts of a package.json. An unparsable file
// has no usable scripts, so it yields none rather than an error.
func packageScripts(p string) (map[string]string, error) {
	data, err := os.ReadFile(p) // #nosec G304 -- p is inside the checkout
	if err != nil {
		return nil, err
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, nil
	}
	return pkg.Scripts, nil
}

// packageManager returns the package manager a lockfile in names points to.
func packageManager(names []string) string {
	switch {
	case slices.Contains(names, "pnpm-lock.yaml"):
		return "pnpm"
	case slices.Contains(names, "yarn.lock"):
		return "yarn"
	default:
		return "npm"
	}
}

// makeTargets returns the names of the targets a Makefile defines.
func makeTargets(p string) (map[string]bool, error) {
	data, err := os.ReadFile(p) // #nosec G304 -- p is inside the checkout
	if err != nil {
		return nil, err
	}
	targets := map[string]bool{}
	for _, m := range makeTargetRe.FindAllSubmatch(data, -1) {
		targets[string(m[1])] = true
	}
	return targets, nil
}

// stackDefaults returns the usual build and test commands of a stack.
func stackDefaults(stack string, names []string) (string, string) {
	switch stack {
	case "go":
		return "go build ./...", "go test ./..."
	case "rust":
		return "cargo build", "cargo test"
	case "python":
		return "python -m build", "python -m pytest"
	case "java":
		if slices.Contains(names, "pom.xml") {
			return "mvn -B package", "mvn -B test"
		}
		return "./gradlew build", "./gradlew test"
	case "cmake":
		return "cmake -S . -B build && cmake --build build", "ctest --test-dir build"
	case "docker":
		return "docker build .", ""
	}
	return "", ""
}
