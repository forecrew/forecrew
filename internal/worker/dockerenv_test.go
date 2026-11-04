package worker

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestImageHash(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		writeFile(t, root, rel, body)
	}
	write("ci/Dockerfile", "FROM alpine\nCOPY app /app\n")
	write("app/main.go", "package main\n")
	write("other/readme", "x")
	write(".git/HEAD", "ref: a")
	hash := func(src envSource) string {
		t.Helper()
		h, err := imageHash(context.Background(), root, src)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	build := envSource{Dockerfile: "ci/Dockerfile", Context: "."}
	base := hash(build)
	if hash(build) != base {
		t.Error("the hash is not stable")
	}

	same := map[string]func(){
		"a git change": func() { write(".git/HEAD", "ref: b") },
	}
	for name, change := range same {
		change()
		if hash(build) != base {
			t.Errorf("%s changed the hash", name)
		}
	}
	differs := map[string]func(){
		"a context file":     func() { write("app/main.go", "package main // v2\n") },
		"a new context file": func() { write("app/new.go", "x") },
		"the dockerfile":     func() { write("ci/Dockerfile", "FROM alpine\nCOPY app /app\nRUN true\n") },
	}
	for name, change := range differs {
		prev := hash(build)
		change()
		if hash(build) == prev {
			t.Errorf("%s did not change the hash", name)
		}
	}
	if hash(envSource{Dockerfile: "ci/Dockerfile", Context: "other"}) == hash(build) {
		t.Error("the context does not change the hash")
	}
	if hash(envSource{Dockerfile: "ci/Dockerfile", Context: ".", Setup: []string{"make"}}) == hash(build) {
		t.Error("setup does not change the hash")
	}
	if hash(envSource{Image: "alpine:3"}) == hash(envSource{Image: "alpine:4"}) {
		t.Error("the image does not change the hash")
	}
	// Executable bit and symlink targets are part of the context.
	prev := hash(build)
	if err := os.Chmod(filepath.Join(root, "app/main.go"), 0o700); err != nil { //nolint:gosec // test file
		t.Fatal(err)
	}
	if hash(build) == prev {
		t.Error("the file mode does not change the hash")
	}
	prev = hash(build)
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "app/link")); err != nil {
		t.Fatal(err)
	}
	if hash(build) == prev {
		t.Error("a symlink does not change the hash")
	}
}

func TestImageHashRefusals(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "ci/Dockerfile", "FROM x")
	writeFile(t, root, "d/inner", "x")
	if err := os.Symlink("/etc/hostname", filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("d", filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("ci/Dockerfile", filepath.Join(root, "Dockerfile.link")); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		src  envSource
		want string
	}{
		{"missing dockerfile", envSource{Dockerfile: "no/Dockerfile", Context: "."}, "dockerfile"},
		{"dockerfile is a directory", envSource{Dockerfile: "d", Context: "."}, "not a regular file"},
		{"dockerfile is a symlink", envSource{Dockerfile: "Dockerfile.link", Context: "."}, "symlink"},
		{"context is a symlink", envSource{Dockerfile: "ci/Dockerfile", Context: "dirlink"}, "symlink"},
		{"missing context", envSource{Dockerfile: "ci/Dockerfile", Context: "nope"}, "build context"},
	}
	for _, tt := range tests {
		if _, err := imageHash(context.Background(), root, tt.src); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}

	old := maxWalkEntries
	maxWalkEntries = 2
	defer func() { maxWalkEntries = old }()
	if _, err := imageHash(context.Background(), root, envSource{Dockerfile: "ci/Dockerfile", Context: "."}); err == nil || !strings.Contains(err.Error(), "too many files") {
		t.Errorf("many files: err = %v", err)
	}
	maxWalkEntries = old
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := imageHash(ctx, root, envSource{Dockerfile: "ci/Dockerfile", Context: "."}); err == nil {
		t.Error("a canceled hash succeeded")
	}
}

func TestImageHashIgnoresTheWorktreeGitFile(t *testing.T) {
	// In a job's checkout .git is a file naming a path of that job.
	root := t.TempDir()
	writeFile(t, root, "Dockerfile", "FROM alpine\n")
	writeFile(t, root, "src/main.go", "package main\n")
	src := envSource{Dockerfile: "Dockerfile", Context: "."}
	hash := func(gitfile string) string {
		writeFile(t, root, ".git", gitfile)
		h, err := imageHash(context.Background(), root, src)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if a, b := hash("gitdir: /work/jobs/one/.git\n"), hash("gitdir: /work/jobs/two/.git\n"); a != b {
		t.Error("the .git file of the worktree changes the hash")
	}
}

func TestFromRefs(t *testing.T) {
	df := `# syntax=docker/dockerfile:1
ARG V=1
FROM --platform=linux/amd64 golang:1.27 AS build
from node:22
FROM build AS test
FROM scratch
FROM base:$V
FROM golang:1.27
FROM
COPY --from=build /a /b
`
	if got, want := fromRefs(df), []string{"golang:1.27", "node:22"}; !slices.Equal(got, want) {
		t.Errorf("fromRefs = %v, want %v", got, want)
	}
}
