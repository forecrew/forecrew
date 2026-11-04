package actionsprovider

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/forecrew/forecrew/internal/domain"
)

// maxInputBytes keeps the inputs of a dispatch below GitHub's limit of
// 65535 characters for all of them.
const maxInputBytes = 60_000

// payload is the JSON input of the workflow: what to set up and run.
type payload struct {
	Setup    []string          `json:"setup,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Argv     []string          `json:"argv,omitempty"`
	Shell    string            `json:"shell,omitempty"`
	Workdir  string            `json:"workdir,omitempty"`
	Versions map[string]string `json:"versions,omitempty"`
}

var (
	fullSHA  = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	shortSHA = regexp.MustCompile(`^[0-9a-fA-F]{7,39}$`)
)

// dispatchPlan is how a job is dispatched.
type dispatchPlan struct {
	// Ref is the branch the workflow is dispatched on (workflow_dispatch
	// takes a branch or tag, never a commit).
	Ref    string
	Inputs map[string]string
	// Commit is the full SHA to test when the job names a commit, else "":
	// a branch is tested at the head the run is dispatched on.
	Commit string
}

// errNotRunnable means the job cannot run on Actions; the job fails with
// the error's text.
var errNotRunnable = errors.New("job cannot run on GitHub Actions")

// plan builds the dispatch of a command job. defaultBranch is where the
// workflow is dispatched when the job names only a commit.
func plan(spec domain.JobSpec, defaultBranch string) (dispatchPlan, error) {
	if err := spec.Validate(); err != nil {
		return dispatchPlan{}, fmt.Errorf("%w: %w", errNotRunnable, err)
	}
	env := spec.Environment
	if !spec.IsCommand() || env == nil || env.Provider != domain.ProviderActions || env.RunsOn == "" {
		return dispatchPlan{}, fmt.Errorf("%w: it needs an environment with provider actions and runs_on", errNotRunnable)
	}
	if len(env.Services) > 0 {
		return dispatchPlan{}, fmt.Errorf("%w: services are not supported on GitHub-hosted runners", errNotRunnable)
	}
	c := spec.Command
	p := payload{Setup: env.Setup, Env: map[string]string{}, Argv: c.Argv, Shell: c.Shell, Workdir: c.Workdir, Versions: env.Versions}
	for _, m := range []map[string]string{env.Env, c.Env} {
		maps.Copy(p.Env, m)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return dispatchPlan{}, fmt.Errorf("%w: %w", errNotRunnable, err)
	}
	checkout := spec.Branch
	if checkout == "" {
		checkout = spec.BaseRef
	}
	if shortSHA.MatchString(checkout) {
		return dispatchPlan{}, fmt.Errorf("%w: %q looks like a short commit SHA, which cannot be told from a branch; use the full 40 characters", errNotRunnable, checkout)
	}
	pl := dispatchPlan{Ref: checkout}
	in := map[string]string{
		"job_id":          spec.JobID.String(),
		"runs_on":         env.RunsOn,
		"payload":         string(raw),
		"reports":         strings.Join(c.Reports, "\n"),
		"artifacts":       strings.Join(c.Artifacts, "\n"),
		"timeout_minutes": strconv.Itoa(timeoutMinutes(time.Duration(spec.Timeout))),
	}
	if fullSHA.MatchString(checkout) {
		// workflow_dispatch takes a branch, not a commit: the default
		// branch's workflow checks the commit out.
		pl.Ref, pl.Commit = defaultBranch, strings.ToLower(checkout)
		in["ref"] = pl.Commit
	}
	// A branch is checked out at the head it was dispatched on: the
	// workflow's checkout takes it from the event when ref is empty, so the
	// branch cannot move between the dispatch and the checkout.
	if o := spec.Overlay; o != nil {
		in["overlay_ref"], in["overlay_paths"] = o.Ref, strings.Join(o.Paths, "\n")
	}
	size := 0
	for k, v := range in {
		if strings.Contains(v, "\x00") {
			return dispatchPlan{}, fmt.Errorf("%w: input %s holds a NUL byte", errNotRunnable, k)
		}
		size += len(k) + len(v)
	}
	if size > maxInputBytes {
		return dispatchPlan{}, fmt.Errorf("%w: the command and its environment are %d bytes, over the %d GitHub accepts as workflow inputs", errNotRunnable, size, maxInputBytes)
	}
	pl.Inputs = in
	return pl, nil
}

// timeoutMinutes is the timeout-minutes of the workflow's job: the job's
// timeout rounded up plus a minute of slack, at most GitHub's 360. It
// bounds the run when forecrewd is not there to cancel it.
func timeoutMinutes(d time.Duration) int {
	if d <= 0 {
		return maxRunMinutes
	}
	return min(int((d+time.Minute-1)/time.Minute)+1, maxRunMinutes)
}

// maxRunMinutes is GitHub's limit for a job on a hosted runner.
const maxRunMinutes = 360
