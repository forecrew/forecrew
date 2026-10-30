# Live smoke test

`make smoke-live` runs one real onboarding and one small goal on a sandbox
repo, against a deployed instance with a real worker and real models. It
proves what the in-process suite (`make test-e2e`, `internal/e2e`) cannot:
the real GitHub App, the real runtime CLIs and a real deployment. It spends
money and is never run in CI.

## What you need

- An instance installed as in [install.md](install.md), reachable from your
  machine, with the GitHub App installed on the sandbox repo.
- The cheapest model tier for every role: in the instance's `forecrew.yaml`
  point each role's `tier` at the lowest tier you have and map that tier to
  the vendors' cheapest models, then restart forecrewd. Keep the reviewer on
  a different vendor than the developer, as in production.
- One worker ([add-worker.md](add-worker.md)) with the runtime CLIs for both
  vendors and the labels the sandbox repo's tasks need (for example `linux`
  and `go`). `forecrew-worker check` must pass on it.
- A sandbox repo you own and do not mind being changed: small, with tests and
  a CI workflow that runs them, a default branch without branch protection
  that blocks the App, and no `CONTRIBUTING.md` section on tests yet.
- An API token (`forecrew token create smoke`, see [mcp-token.md](mcp-token.md)).
- A fine-grained GitHub token with read access to the sandbox repo's pull
  requests. It is required: the test checks on GitHub that the PRs are merged
  and that the task PR has a review.

## Run

```sh
export FORECREW_SMOKE_URL=https://forecrew.example.com
export FORECREW_SMOKE_TOKEN=fcm_...            # read it without echo, as in add-worker.md
export FORECREW_SMOKE_REPO=you/forecrew-sandbox
export FORECREW_SMOKE_GITHUB_TOKEN=github_pat_...
export FORECREW_SMOKE_REPORT=smoke-report.md        # relative to the repo root; default as shown (git-ignored)
make smoke-live
```

`FORECREW_SMOKE_PROJECT` names the project when it is not the repo name.

The test acts as the human: it checks that a worker is online, onboards the
repo (approving the profile PR; when the project is already onboarded it
looks for the merged PR from `agent/onboarding` on GitHub and fails with "onboard a
fresh sandbox repo" if there is none), creates the goal "Add a CONTRIBUTING.md section on running tests"
and approves every spec and merge gate of that goal until it is done. A
`needs_you` gate stops the run: someone has to look. It stops after 45
minutes.

## What it asserts

Every check must hold; none is skipped.

- a worker is online;
- the profile PR exists (from the profile gate, or found on GitHub for an
  already onboarded project) and is merged on GitHub;
- the goal reaches `done`;
- every task PR is merged on forecrewd and on GitHub, and at least one task PR
  exists;
- every task PR has at least one review on GitHub;
- a reviewer job succeeded.

## The report

`smoke-report.md` holds the verdict, each check, the duration of each phase
(worker check, onboarding, goal), every job with its duration and cost, and
the total cost. Attach it to the PR or release it validates. The report is
written even when the run fails; the exit status says whether it passed.

## What a real run must also check

A passing run does not cover these; look at them with your own eyes during
the first runs and note the result next to the report:

1. **Codex.** The Codex adapter has never run against a real `codex` binary.
   Check the flags, the event shapes in `job logs` of the reviewer job, the
   `declined` status of a refused command and the `cache_write_input_tokens`
   field in the usage of a job.
2. **Claude Code denial.** Each run gets an empty `CLAUDE_CONFIG_DIR`. With a
   real API key, watch a command outside the allow list being denied (for
   example have a role prompt ask for `curl`; the `tool_result` event shows
   the denial) and check that the job still ends cleanly.
3. **Runtime drain.** A child process that leaves the agent's process group
   and keeps writing to stdout keeps the run open (see
   [runtimes.md](../runtimes.md)). Check that a job whose agent starts a
   background process still ends, and note how long it took.
