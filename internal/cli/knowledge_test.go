package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/knowledge"
	"github.com/forecrew/forecrew/internal/mcp"
)

func TestKnowledgeSearch(t *testing.T) {
	e := newRemoteEnv(t)
	id := domain.NewID()
	e.svc.SearchKnowledgeFunc = func(context.Context, domain.Actor, mcp.SearchKnowledgeInput) ([]knowledge.Result, error) {
		return []knowledge.Result{
			{ID: id, Kind: domain.KnowledgeNoteKind, Section: "notes", Path: "notes/1", Title: "Spool", Untrusted: true, NeedsReverify: true},
			{Kind: domain.KnowledgeEntryKind, Section: "vision", Path: "knowledge/vision/v.md", Title: "Why"},
		}, nil
	}
	out, errOut, code := e.run(t, "knowledge", "search", "--section", "notes", "--kind", "note", "--provider", "internal", "--limit", "4", "app", "queue", "--", "-slow")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"KIND", id.String() + "  note", "untrusted,stale", "knowledge/vision/v.md  Why    -"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	calls := e.svc.Calls()
	want := mcp.SearchKnowledgeInput{Project: "app", Query: "queue -slow", Section: "notes", Kind: "note", Provider: "internal", Limit: 4}
	if len(calls) != 1 || calls[0].Tool != mcp.ToolSearchKnowledge || fmt.Sprint(calls[0].Input) != fmt.Sprint(want) {
		t.Errorf("calls = %+v", calls)
	}
	if _, errOut, code := e.run(t, "kb", "search", "app"); code == 0 || !strings.Contains(errOut, "arg") {
		t.Errorf("a search without a project: code %d, stderr %q", code, errOut)
	}
}

func TestKnowledgeDeleteNote(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.DeleteNoteFunc = func(_ context.Context, _ domain.Actor, in mcp.DeleteNoteInput) (mcp.DeleteNoteOutput, error) {
		if in.Note == "gone" {
			return mcp.DeleteNoteOutput{}, fmt.Errorf("note gone: %w", mcp.ErrNotFound)
		}
		return mcp.DeleteNoteOutput{Message: "Note Spool deleted."}, nil
	}
	if out, _, code := e.run(t, "knowledge", "delete-note", "abc"); code != 0 || !strings.Contains(out, "Note Spool deleted.") {
		t.Errorf("code %d, output %q", code, out)
	}
	if _, errOut, code := e.run(t, "knowledge", "delete-note", "gone"); code != 1 || !strings.Contains(errOut, "not found") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
}

func TestKnowledgePropose(t *testing.T) {
	e := newRemoteEnv(t)
	e.svc.ProposeKnowledgeFunc = func(context.Context, domain.Actor, mcp.ProposeKnowledgeInput) (mcp.ProposeKnowledgeOutput, error) {
		return mcp.ProposeKnowledgeOutput{Proposal: "KB-5", Status: domain.KnowledgeOpen, Number: 12, URL: "https://github.com/acme/app/pull/12"}, nil
	}
	file := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(file, []byte("# Why\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := e.run(t, "knowledge", "propose", "app", "vision", "knowledge/vision/why.md", "--body-file", file, "--reason", "We decided.")
	if code != 0 || !strings.Contains(out, "KB-5: https://github.com/acme/app/pull/12") {
		t.Fatalf("code %d, out %q, stderr %q", code, out, errOut)
	}
	calls := e.svc.Calls()
	want := mcp.ProposeKnowledgeInput{Project: "app", Section: "vision", Path: "knowledge/vision/why.md", Body: "# Why\n", Reason: "We decided."}
	if len(calls) != 1 || calls[0].Tool != mcp.ToolProposeKnowledge || fmt.Sprint(calls[0].Input) != fmt.Sprint(want) {
		t.Errorf("calls = %+v", calls)
	}
	if _, _, code := e.run(t, "knowledge", "propose", "app", "vision", "a.md", "--action", "retire", "--reason", "Old."); code != 0 {
		t.Errorf("a retirement needs no body: code %d", code)
	}
	if _, errOut, code := e.run(t, "knowledge", "propose", "app", "vision", "a.md"); code == 0 || !strings.Contains(errOut, "reason") {
		t.Errorf("without a reason: code %d, stderr %q", code, errOut)
	}
	if _, _, code := e.run(t, "knowledge", "propose", "app", "vision", "a.md", "--reason", "r", "--body-file", "/nonexistent/x"); code == 0 {
		t.Error("a missing body file must fail")
	}
}
