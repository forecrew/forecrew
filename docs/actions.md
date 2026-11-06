# Jobs on GitHub Actions

A command job whose environment has `provider: actions` (environment.schema.json)
runs on a GitHub-hosted runner. The job's pool is `actions`; no worker serves
it. forecrewd does: the **Actions provider** (`internal/actionsprovider`)
leases those jobs like a worker would, dispatches a workflow, follows its run
and reports the results, so the matrix, the test runs and `job_status` look the
same as for jobs on workers.

## How a job runs

1. The provider keeps a worker row named `forecrewd-actions` in pool
   `actions` for each account, to hold the lease, and marks it seen so
   `job_status` shows no missing-worker reason for queued jobs of the pool. It
   has no token and cannot call the worker API. The name is reserved:
   `forecrew worker add` and `move` refuse it, and the provider refuses to
   serve a row of that name it did not create, or one that is revoked or no
   longer in pool `actions`. It also checks the pool of every job it leases.
   When several accounts have queued jobs they take turns for the in-flight
   slots, one job each. Jobs in pool `actions` have no `needs`: they are
   dropped when the job is created, since no label could satisfy them. The lease and heartbeat rules are the worker API's:
   the lease is extended on every poll of the run (15 s), a canceled job ends
   the lease and the provider cancels the run, and a lease that expires (for
   example forecrewd was down) queues the job again.
2. A job that is leased again after an earlier attempt (forecrewd restarted,
   or an attempt was released) first looks for the run it dispatched, by the
   job id in the run's title (`forecrew-matrix <job_id>`), and follows that
   run instead of starting another. The timeout counts from the dispatch. A
   run that already ended without a usable result (no artifact, no result
   file) is not resumed: the job gets a fresh run, so a run that died before
   the command is retried once. A shutdown leaves the runs going for this
   reason; only a canceled job or a timeout cancels one. A run is the job's
   only if it was started by `workflow_dispatch` of `forecrew-matrix.yml`,
   triggered by the App's bot, on the branch the job is dispatched on, and its
   title carries the job id; the title alone proves nothing, any workflow can
   set it. A run that fails any check, or lacks a field, is not adopted.
   The runs are looked up on that branch, up to 500 of them.
3. Otherwise it dispatches `.github/workflows/forecrew-matrix.yml` with
   `workflow_dispatch` on the job's branch (or its `base_ref`; for a commit
   SHA, on the project's default branch, because a dispatch takes a branch or
   tag). Inputs: `job_id`, `ref` (the full SHA to test; empty for a branch,
   which is tested at the head the run was dispatched on), `overlay_ref` and
   `overlay_paths`, `runs_on`, `timeout_minutes`, `payload` (JSON: setup
   commands, env, argv or shell, workdir, versions), `reports` and
   `artifacts` (globs, one per line). A short SHA is refused: it cannot be
   told from a branch name. The id of the run comes from the dispatch answer;
   when GitHub gives none (GitHub Enterprise Server) the run is found by its
   title.
4. It reads the run every 15 s, and at once when GitHub sends a `workflow_run`
   webhook. The job log gets the run's URL; the command's own output stays in
   the run's log on GitHub.
5. When the run ends it downloads the artifact `forecrew-<job_id>`, unpacks it
   in memory with the limits below, stores the reports and artifacts as the
   job's artifacts (same limits and storage as a worker's uploads), parses the
   JUnit reports with the same rules as a worker (`internal/testreport`) and
   completes the job with the test run.

The commit a test run is recorded on is the one the provider expects: the
requested SHA for a job that names a commit, the head the run was dispatched
on for a branch. The workflow reports the commit it checked out
(`head_sha`, from the checkout step, before any repo code runs); the report is
only compared with the expectation. A mismatch is an `errored` test run on the
expected commit, never a run on the commit the repo claims. Errored and
timed-out runs land on the expected commit too.

`runner_image`, `runner_image_version` and `runner_os` (from the runner's
`ImageOS`, `ImageVersion`, `RUNNER_OS` and `RUNNER_ARCH`) are added to the test
run's `versions`, next to the environment's own `versions` commands.

## Failures

| What happens | Job |
|--------------|-----|
| `forecrew-matrix.yml` is not on the ref (HTTP 404) | `failed`: "forecrew-matrix.yml is not installed on ... : onboarding installs it". Install it with the playbook action `actions.install_workflow`. |
| GitHub refuses the dispatch (422), or the App may not run workflows (403) | `failed`, with GitHub's message. |
| Dispatch, run read or artifact download fail for another reason (5xx, rate limit) | released: queued again (attempts as for workers). |
| The run ends without the command having run (checkout failed, runner lost, run cancelled on GitHub) | released once (`retry`), `failed` on the second attempt. |
| A setup command fails | `failed`: "environment setup failed: ..." (as on a worker). |
| The run ends fine but the artifact is missing, expired, unusable, or has no usable report | `succeeded` with an `errored` test run saying why (as for a worker whose command left no report). |
| The command exits non-zero | `succeeded` with a `failed` test run, as on a worker. |
| The job's `timeout` (default 6 h, GitHub's own limit) passes | the run is cancelled; `failed` with an `errored` test run. |
| The workflow reports another commit than the expected one | `succeeded` with an `errored` test run on the expected commit. |
| forecrewd shuts down | the runs keep going; the leases expire, the jobs are queued again and resume their runs. The workflow's `timeout-minutes` (the job's timeout plus a minute) bounds a run forecrewd never comes back to. |

A job with `services` is refused (`failed`): the workflow cannot start service
containers. Setup commands and `command.shell` run in `bash`, also on Windows
runners (Git Bash), not in `cmd`.

## The workflow

`packs/workflows/forecrew-matrix.yml` is installed by the action
`actions.install_workflow` (`repo`, `branch`; playbooks.md; the `onboarding-tests` playbook runs it on its branch): it writes the file
and leaves an identical one alone. It checks out `ref`, applies the overlay,
runs setup, versions and the command, and uploads the result file, the reports
and the artifacts as the artifact `forecrew-<job_id>` (kept for one day).

- **Least privilege.** The workflow has `permissions: contents: read` and uses
  no secret: only the default `GITHUB_TOKEN`, read only. The checkout does not
  keep the token.
- **Pinned.** Every action is pinned by commit SHA.
- **No script injection.** The inputs are untrusted. They reach the steps only
  through `env:` or `with:`, never through `${{ inputs.* }}` inside a `run:`
  script (a test enforces it), and the variables of the environment are
  exported only when their names are valid identifiers. Setup, version and
  command processes read `/dev/null`, so none can eat the lines of the next.
- **A deterministic artifact.** `forecrew-root.txt`, an empty file at the
  workspace root, is uploaded with the result so that the artifact is rooted
  at the workspace whatever the report and artifact globs are (a multi-path
  upload is rooted at the paths' common ancestor); results are always at
  `forecrew-out/result.json`. The provider drops the marker.
- The template is linted with `actionlint` by `make lint` (skipped where it is not installed), by the lint job of CI, and by a test that runs it (the test fails instead of skipping when `CI` is set).

## Limits of the artifact

The artifact is data the repo controls, so the provider bounds it before it
trusts it:

- download and unpacked size together: 128 MiB (`actionsprovider.MaxUnpacked`);
  more fails with an `errored` test run;
- at most 200 files are taken; each file at most `evidence.max_artifact_size`;
- entries that are symlinks, whose name is not a valid artifact name (this
  refuses absolute paths, `..` and `\`) or that are duplicates are skipped and
  named in the job log. Nothing is written to disk.
- reports are parsed with the limits of `internal/testreport` (cases, names,
  messages).

`workflow_dispatch` accepts 25 inputs and 65535 characters in all; the provider
uses 9 inputs and fails a job whose command and environment need more than
60 000 bytes.

## Costs and limits

Every job is one workflow run, billed by GitHub per runner minute (macOS and
Windows minutes cost more than Linux ones; private repos beyond the free quota
only). Forecrew adds:

- `actions.max_in_flight` (forecrew.yaml, config.md; default 10): the most runs
  followed at once. Further jobs stay queued, so a large matrix cannot start
  hundreds of runs. Lower it to bound the spend.
- A job that sets no `timeout` is cancelled after 6 hours (and GitHub stops it
  then by itself).
- A failed run is retried once, which doubles its cost.
- GitHub's own limits apply: concurrent jobs per plan, the rate limits of the
  REST API (the provider makes about four calls per run and 15 s), and
  artifact storage (one day of retention here).

## Setup checklist

The GitHub App needs, beyond the permissions of the other features:

| Permission | Level | For |
|------------|-------|-----|
| Actions | Read and write | dispatch, read and cancel runs, download artifacts |
| Contents | Read and write | `actions.install_workflow` writes the file |
| Workflows | Read and write | writing under `.github/workflows` |
| Metadata | Read | always |

Subscribe the App to the **Workflow runs** event so runs are read as soon as
they end; without it the provider polls every 15 s. Existing installations
must accept the new permissions. Then, for each project that uses an `actions`
environment: run `actions.install_workflow` (onboarding does), and check that
Actions are enabled for the repo.

Not supported: self-hosted runners and other CI systems.
