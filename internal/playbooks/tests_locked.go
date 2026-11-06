package playbooks

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/store"
	"github.com/forecrew/forecrew/internal/workflow"
)

// testerRole is the role whose output holds the files that get locked.
const testerRole = "test-engineer"

// newTest is one entry of the tests of a test map (packs/schemas/test-map.json):
// a test, the suite that runs it and the file that holds it.
type newTest struct {
	Suite string `json:"suite"`
	ID    string `json:"id"`
	File  string `json:"file"`
}

// testMapDoc is the part of the test engineer's output (packs/schemas/test-map.json)
// that the locked tests, the base check, the verification and tests.map use.
type testMapDoc struct {
	Files []string `json:"files"`
	Tests []struct {
		Criterion string `json:"criterion"`
		newTest
	} `json:"tests"`
	Untestable []struct {
		Criterion string `json:"criterion"`
		Reason    string `json:"reason"`
	} `json:"untestable"`
	Escalation *string `json:"escalation"`
}

// testerOutputs returns the outputs of the successful test-engineer jobs of
// the task, oldest first. A retry reports only what it changed itself, so
// every caller reads all of them.
func (a *actions) testerOutputs(ctx context.Context, t domain.Task) ([]testMapDoc, error) {
	jobs, err := a.Store.Jobs().List(ctx, t.AccountID, store.JobFilter{TaskID: t.ID, Status: domain.JobSucceeded})
	if err != nil {
		return nil, fmt.Errorf("list jobs of %s: %w", t.Ref(), err)
	}
	slices.SortFunc(jobs, func(x, y domain.Job) int {
		return cmp.Or(x.CreatedAt.Compare(y.CreatedAt), cmp.Compare(x.Number, y.Number))
	})
	var docs []testMapDoc
	for _, j := range jobs {
		if j.Role != testerRole || len(j.Output) == 0 {
			continue
		}
		var doc testMapDoc
		if err := parseOutput(j.Output, &doc); err != nil {
			return nil, fmt.Errorf("output of the %s job %s of %s: %w", testerRole, j.Ref(), t.Ref(), err)
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

// globEscaper makes a file name a glob that matches only that name, using
// character classes: a backslash escape would not be a relative path
// (domain.RelativePath), and a "]" alone is literal.
var globEscaper = strings.NewReplacer("[", "[[]", "*", "[*]", "?", "[?]", "{", "[{]", "}", "[}]")

// literalGlobs turns file paths into sorted, distinct globs that match
// exactly them. It refuses the names no glob can match exactly: with a
// backslash, or starting with "!" (an exclude).
func literalGlobs(files []string) ([]string, error) {
	out := make([]string, 0, len(files))
	for _, f := range files {
		if strings.Contains(f, `\`) || strings.HasPrefix(f, "!") {
			return nil, fmt.Errorf("file %q cannot be locked: it contains a backslash or starts with an exclamation mark", f)
		}
		out = append(out, globEscaper.Replace(f))
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// lockedPathsOutput is the output of tests.locked_paths.
type lockedPathsOutput struct {
	LockedPaths []string `json:"locked_paths"`
	// Feedback is the human's note of the gate that lifted the lock, for the
	// round it was lifted for; empty otherwise.
	Feedback string `json:"feedback"`
}

// testsLockedPaths is the action tests.locked_paths. It returns, as
// locked_paths, every file the successful test-engineer jobs of the task
// created or changed or put a test in (a retry reports only what it changed
// itself, so all jobs count), or [] when there is none.
//
// with.iteration is the round of the loop around the developer's job, and
// with.decision and with.feedback the decision and the note of the gate the
// round before opened when the developer escalated. Only a later round after
// an approved gate runs without the lock: locked_paths is [] and feedback is
// the human's note. Any other decision, and the first round, keep the lock
// (the note still reaches the developer in a later round).
func (a *actions) testsLockedPaths(ctx context.Context, call workflow.ActionCall) (any, error) {
	var with struct {
		TaskID    uuid.UUID         `json:"task_id"`
		Iteration int               `json:"iteration"`
		Decision  domain.GateStatus `json:"decision"`
		Feedback  string            `json:"feedback"`
	}
	if err := workflow.DecodeWith(call.With, &with); err != nil {
		return nil, err
	}
	t, err := a.loadTask(ctx, call.Run, with.TaskID)
	if err != nil {
		return nil, err
	}
	note := ""
	if with.Iteration > 1 {
		note = strings.TrimSpace(with.Feedback)
		if with.Decision == domain.GateApproved {
			return lockedPathsOutput{LockedPaths: []string{}, Feedback: note}, nil
		}
	}
	docs, err := a.testerOutputs(ctx, t)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, doc := range docs {
		files = append(files, doc.Files...)
		for _, x := range doc.Tests {
			files = append(files, x.File)
		}
	}
	globs, err := literalGlobs(files)
	if err != nil {
		return nil, err
	}
	return lockedPathsOutput{LockedPaths: globs, Feedback: note}, nil
}
