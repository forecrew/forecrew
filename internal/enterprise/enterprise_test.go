package enterprise_test

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/enterprise"
)

func TestLoadWithoutARegistration(t *testing.T) {
	if f := enterprise.Load(enterprise.Deps{}); f.Tools != nil || f.SignIn != nil || f.SCIM != nil || f.Run != nil {
		t.Fatalf("features = %+v", f)
	}
}

func TestLoadRegistered(t *testing.T) {
	t.Cleanup(func() { enterprise.Register(nil) })
	enterprise.Register(func(enterprise.Deps) enterprise.Features { return enterprise.Features{SCIM: http.NotFoundHandler()} })
	if f := enterprise.Load(enterprise.Deps{}); f.SCIM == nil {
		t.Fatalf("features = %+v", f)
	}
}

// taggedEnterprise says whether a file only builds with the enterprise tag.
func taggedEnterprise(t *testing.T, path string) bool {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(src)) {
		if strings.HasPrefix(line, "package ") {
			return false
		}
		if !constraint.IsGoBuild(strings.TrimSpace(line)) {
			continue
		}
		expr, err := constraint.Parse(strings.TrimSpace(line))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return !expr.Eval(func(tag string) bool { return tag != "enterprise" })
	}
	return false
}

// importsEE says whether a Go file imports ee/.
func importsEE(t *testing.T, path string) bool {
	t.Helper()
	const ee = "github.com/forecrew/forecrew/ee"
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(file.Imports, func(imp *ast.ImportSpec) bool {
		name, err := strconv.Unquote(imp.Path.Value)
		return err == nil && (name == ee || strings.HasPrefix(name, ee+"/"))
	})
}

// TestOpenCoreDoesNotImportEE walks every Go file outside ee/. Only a file
// behind the build tag, in cmd/, may import ee/: that is how a binary of the
// enterprise build links it in.
func TestOpenCoreDoesNotImportEE(t *testing.T) {
	root := filepath.Join("..", "..")
	seen := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && slices.Contains([]string{"ee", "node_modules", ".git", "web"}, d.Name()) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		seen++
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if importsEE(t, path) && (!strings.HasPrefix(rel, "cmd"+string(filepath.Separator)) || !taggedEnterprise(t, path)) {
			t.Errorf("%s imports ee/", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 100 {
		t.Fatalf("walked only %d files", seen)
	}
}

// TestEEIsBehindTheTag keeps every Go file of ee/ out of the open-core build.
func TestEEIsBehindTheTag(t *testing.T) {
	err := filepath.WalkDir(filepath.Join("..", "..", "ee"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") && !taggedEnterprise(t, path) {
			t.Errorf("%s builds without the enterprise tag", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
