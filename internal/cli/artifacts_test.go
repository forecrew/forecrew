package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/mcp"
)

func TestJobArtifacts(t *testing.T) {
	e := newRemoteEnv(t)
	id := domain.NewID()
	e.svc.ListArtifactsFunc = func(context.Context, domain.Actor, mcp.JobRefInput) ([]domain.Artifact, error) {
		return []domain.Artifact{
			{ID: id, Name: "reports/unit.xml", ContentType: "text/xml", Size: 4821, SHA256: strings.Repeat("ab", 32)},
			{Name: "raw", Size: 0, SHA256: "ab"},
		}, nil
	}
	// Both spellings of the group work.
	for _, group := range []string{"job", "jobs"} {
		out, errOut, code := e.run(t, group, "artifacts", "JOB-41")
		if code != 0 || errOut != "" {
			t.Fatalf("%s: code %d, stderr %q", group, code, errOut)
		}
		for _, want := range []string{"ID", "NAME", "SHA256", id.String() + "  reports/unit.xml  text/xml  4821  abababababab", "raw               -         0     ab"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: output lacks %q:\n%s", group, want, out)
			}
		}
	}
	if calls := e.svc.Calls(); len(calls) != 2 || calls[0].Tool != mcp.ToolListArtifacts || calls[0].Input != (mcp.JobRefInput{Job: "JOB-41"}) {
		t.Errorf("calls = %+v", calls)
	}
	e.svc.ListArtifactsFunc = nil
	if out, _, _ := e.run(t, "job", "artifacts", "JOB-1"); strings.Count(out, "\n") != 1 {
		t.Errorf("empty list prints only the header:\n%s", out)
	}
	e.svc.ListArtifactsFunc = func(context.Context, domain.Actor, mcp.JobRefInput) ([]domain.Artifact, error) {
		return nil, fmt.Errorf("job JOB-9: %w", mcp.ErrNotFound)
	}
	if _, errOut, code := e.run(t, "job", "artifacts", "JOB-9"); code != 1 || !strings.Contains(errOut, "JOB-9: not found") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
}

func TestArtifactGet(t *testing.T) {
	e := newRemoteEnv(t)
	id := domain.NewID()
	content := "<testsuite/>\n"
	e.svc.OpenArtifactFunc = func(_ context.Context, _ domain.Actor, got string) (domain.Artifact, io.ReadCloser, error) {
		if got != id.String() {
			return domain.Artifact{}, nil, fmt.Errorf("artifact %s: %w", got, mcp.ErrNotFound)
		}
		return domain.Artifact{ID: id, Name: "unit.xml", ContentType: "text/xml", Size: int64(len(content))}, io.NopCloser(strings.NewReader(content)), nil
	}

	file := filepath.Join(t.TempDir(), "out.xml")
	if out, errOut, code := e.run(t, "artifact", "get", id.String(), "-o", file); code != 0 || out != "" || errOut != "" {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != content {
		t.Fatalf("file = %q, %v", got, err)
	}
	for _, args := range [][]string{{"artifact", "get", id.String()}, {"artifact", "get", id.String(), "-o", "-"}} {
		if out, _, code := e.run(t, args...); code != 0 || out != content {
			t.Errorf("%v: code %d, stdout %q", args, code, out)
		}
	}

	// A failed request must not create the file or destroy an existing one.
	existing := filepath.Join(t.TempDir(), "keep.txt")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := e.run(t, "artifact", "get", domain.NewID().String(), "-o", existing); code != 1 || !strings.Contains(errOut, "not found") {
		t.Errorf("unknown id: code %d, stderr %q", code, errOut)
	}
	if got, _ := os.ReadFile(existing); string(got) != "keep" {
		t.Errorf("existing file = %q", got)
	}
	missingDir := filepath.Join(t.TempDir(), "no", "such", "dir", "f")
	if _, _, code := e.run(t, "artifact", "get", id.String(), "-o", missingDir); code != 1 {
		t.Errorf("unwritable output: code %d", code)
	}
}

func TestArtifactGetCutShort(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.OpenArtifactFunc = func(context.Context, domain.Actor, string) (domain.Artifact, io.ReadCloser, error) {
		return domain.Artifact{Name: "a", Size: 100}, io.NopCloser(io.MultiReader(strings.NewReader("part"), iotest.ErrReader(errors.New("boom")))), nil
	}
	file := filepath.Join(t.TempDir(), "partial")
	if _, _, code := e.run(t, "artifact", "get", domain.NewID().String(), "-o", file); code != 1 {
		t.Fatalf("code %d", code)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("a partial file was left behind: %v", err)
	}
}
