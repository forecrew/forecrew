package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/forecrew/forecrew"
	"github.com/forecrew/forecrew/internal/domain"
	"github.com/forecrew/forecrew/internal/models"
	"github.com/forecrew/forecrew/internal/runtime"
)

// FixtureDir is the work tree of a suitability test in the packs.
const FixtureDir = "packs/fixtures/model-test"

// What a suitability test edits and leaves behind, and what it should read.
const (
	fixtureGreeting = "greeting.txt"
	fixtureNotes    = "notes.txt"
	fixtureToken    = "token.txt"
	fixtureRan      = ".check-ran"
	wantGreeting    = "hello, forecrew"
)

// executeModelCheck lists the models of a connection with its key, from
// this worker, which may reach a model the server cannot.
func (r *jobRun) executeModelCheck(ctx context.Context) Completion {
	m := r.spec.ModelCheck
	if m == nil {
		return r.failed("model check without a connection")
	}
	res := domain.ModelCheckResult{Status: domain.ModelCheckUnsupported, Detail: "this provider has no list of models: enter its models by hand"}
	if m.URL != "" {
		r.ev.text(domain.EventStatus, "checking")
		res = models.Probe(ctx, models.ProbeRequest{URL: m.URL, Auth: m.Auth, Format: m.Format, Key: r.spec.Env[m.KeyEnv], Local: r.w.cfg.LocalModelEndpoints}, nil)
	}
	res.AgentVersion = r.agentVersion(ctx, m.Adapter)
	out, err := json.Marshal(res)
	if err != nil {
		return r.failed("encode the check: %v", err)
	}
	return Completion{Status: StatusSucceeded, Output: out}
}

// agentVersion is the version of the adapter's CLI here, or "".
func (r *jobRun) agentVersion(ctx context.Context, adapter string) string {
	a, err := r.w.cfg.Runtimes.Get(adapter)
	if err != nil {
		return ""
	}
	v, err := a.Detect(ctx)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

// executeSuitability runs a suitability test: the agent works on a copy of
// the fixture, never on a repo, and the worker reports what it did there.
func (r *jobRun) executeSuitability(ctx context.Context) Completion {
	s := r.spec.Suitability
	if r.dir == "" {
		return r.failed("suitability test without a work dir")
	}
	defer func() { _ = os.RemoveAll(r.dir) }()
	defer r.removeScratch()
	if err := writeFixture(r.dir, s.Nonce); err != nil {
		return r.retry("prepare the fixture: %v", err)
	}
	if err := r.makeScratch(); err != nil {
		return r.retry("%v", err)
	}
	r.ev.text(domain.EventStatus, "running")
	r.spec.OutputSchema = s.Schema
	res, err := r.runAgent(ctx, r.spec.Prompt, nil)
	if errors.Is(err, runtime.ErrNotInstalled) || errors.Is(err, runtime.ErrUnknownAdapter) {
		return r.retry("%v", err)
	}
	rep := domain.SuitabilityReport{Output: res.Output, Turns: res.Turns, AgentVersion: r.agentVersion(ctx, r.spec.Runtime)}
	if err != nil {
		rep.Output, rep.Error = nil, cleanText(r.red.String(err.Error()))
	}
	rep.Read = strings.Contains(res.Text, s.Nonce)
	rep.Edited = fileIs(filepath.Join(r.dir, fixtureGreeting), wantGreeting)
	rep.Ran = fileIs(filepath.Join(r.dir, fixtureRan), s.Nonce+"-ran")
	out, err := json.Marshal(rep)
	if err != nil {
		return r.failed("encode the report: %v", err)
	}
	return Completion{Status: StatusSucceeded, Output: out, Usage: res.Usage, CostUSD: res.CostUSD}
}

// writeFixture copies the fixture into dir with the test's code word and
// token.
func writeFixture(dir, nonce string) error {
	src, err := fs.Sub(forecrew.Files, FixtureDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if err := os.CopyFS(dir, src); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	files := map[string]string{
		fixtureNotes: "Notes of the test.\ncode word: " + nonce + "\n",
		fixtureToken: nonce + "-ran\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// fileIs reports whether the file at path holds want, give or take spaces
// around it.
func fileIs(path, want string) bool {
	b, err := os.ReadFile(path) // #nosec G304 -- a file of the job's own work tree
	return err == nil && strings.TrimSpace(string(b)) == want
}
