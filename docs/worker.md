# forecrew-worker

The worker daemon runs Forecrew's agent jobs on any machine. It only dials out
to forecrewd (`docs/contracts/worker-api.md`); config is `worker.yaml`
(`docs/contracts/config.md`) and the token comes from `FORECREW_WORKER_TOKEN`.

```
forecrew-worker run --config worker.yaml     # poll for jobs and run them
forecrew-worker check --config worker.yaml   # labels, git, runtime CLIs, server and token
forecrew-worker version
```

## Labels

The configured labels, the os (`linux`, `windows`; a macOS worker advertises
both `darwin`, the `GOOS` value of conventions.md, and `macos`, the name repo profiles use) and `runtime:claude-code` / `runtime:codex` for each of `claude`
and `codex` found on `PATH`.

## Pools

Every worker belongs to one pool, set by an admin on the server, never by
the worker: `forecrew worker add <name> --pool <pool>` (default `build`) and
`forecrew worker move <name> --pool <pool>`. A worker leases only jobs of its
pool; labels then route jobs inside the pool. Nothing in `worker.yaml` or a
poll changes the pool: a `pool:control` label is just a label. A move takes
effect at the worker's next poll; the jobs it is running finish.

A pool is also a trust zone. `forecrew.yaml` `pools` (config.md) says which
roles and job kinds each pool runs and which credentials its jobs receive:

| Pool | Runs | Repo token | Runtime keys |
|------|------|------------|--------------|
| `build` | every role, agent and command jobs | `write` for writing roles, `read` otherwise | every runtime |
| `control` | every read-only role, agent jobs only | `read` | every runtime |
| `actions` | command jobs on GitHub-hosted runners; no workers | `read` | none |

- A job's pool is its role pack's `pool` (default `build`), or the `pool`
  given to `dispatch_job`; a command job's pool comes from its environment
  (`actions` for provider `actions`, otherwise `build`). forecrewd refuses to
  create a job its pool does not allow, for example a writing role in
  `control`, with an error that says which rule refused it.
- At lease time the job gets only what its pool allows: a repo token no wider
  than the pool's `repo_token` (none for `none`), and the runtime keys
  (`credential_env`) only of the runtimes the pool lists. A `control` job
  never gets a write token, even if it had write paths. Command jobs never
  get runtime keys.
- `job_status` says when no online worker of the job's pool has its labels,
  for example `no online worker in pool control with labels runtime:codex`.
- The read-only roles of the packs, `architect` and `product-owner`, run in
  `control`. Until a worker of that pool is online their jobs stay queued and
  `job_status` says why. The Compose stack ships one (`worker-control`, see
  install.md); other installs add one with `forecrew worker add <name> --pool control`.
- Pools other than `build`, `control` and `actions` are defined under `pools`
  in `forecrew.yaml`; `forecrew worker add` and `move` read it (`--config`,
  default `forecrew.yaml`) only for those. Pool `actions` takes no workers.

## Jobs

- Each repo has a bare cache under `work_dir/repos/<owner>/<name>.git`; every
  job gets a fresh git worktree under `work_dir/jobs/<job id>`, removed
  afterwards. An existing remote `branch` is continued, otherwise it is
  created from `base_ref`.
- Up to `max_jobs` jobs run at once. A heartbeat answer of `cancel` (or
  `409 lease_lost`) stops the agent; nothing is pushed or reported.
- `head_sha` in `complete` is the head of the job's branch on the remote, also
  when this attempt pushed nothing new; empty only when there is no branch.
- The worker does all pushing itself, only to `agent/*` branches. It never
  pushes the agent's commits: after the run it snapshots the files of the
  worktree (in a private index, not looking at the worktree's `.git`, which the
  agent may have deleted or rewritten) and creates one commit by
  `forecrew-bot` on top of the starting commit. Every changed path must match
  the role's `write_paths`; symlinks and submodules are refused. Read-only
  roles never push and fail if the snapshot differs from the starting commit.
  The snapshot honors the repo's own `.gitignore`, so paths it ignores are
  invisible to the read-only check (they are not published either).
- Files git refuses to stage (unreadable files, reserved names such as
  `.GIT/config`) fail the job; they are not retried. A `write_paths` pattern
  that is not valid doublestar syntax fails the job before anything runs.
- Git is run with no `GIT_*` variable, no user or system config
  (`GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`), no replace
  objects, `--git-dir`/`--work-tree` named explicitly and a ceiling above the
  work dir. The repo cache's config, `info/attributes`, `info/grafts`,
  `shallow` and `refs/replace` are reset before any worker git call that
  could run agent code or carry the token.
- The lease ends 5 minutes after the worker sent the last successful poll or
  heartbeat, or at the server's `lease_expires_at` if that is earlier, so a
  skewed server clock cannot extend it. If it ends without a successful
  heartbeat the job is stopped and nothing is pushed; a push also needs 30
  seconds of lease left.
- Agent jobs with `job_mcp` in their spec get the job MCP gateway
  (docs/contracts/job-mcp.md, docs/runtimes.md): `<server_url>/job-mcp` in
  the runtime's MCP configuration and its token in `FORECREW_JOB_MCP_TOKEN`, never on a
  command line or in a file. The token dies with the lease; a heartbeat
  answered `cancel` or `409 lease_lost` stops the agent, which ends any call
  in flight, `ask_human` included (the worker's heartbeats keep the lease
  while it waits).
- Files the agent marks with `evidence_attach` arrive in heartbeat answers
  (`attach`) until the worker acknowledges them in its next heartbeat. After
  the runtime exits it sends one more heartbeat for the last ones, then uploads each as an artifact of the job
  (`PUT .../artifacts/{name}`) before checking and pushing the work tree. A
  file must be a regular file inside the work tree, reached without
  symlinks, not under `.git`; one that is not, or fails to upload, is
  reported as an `error` event and skipped. forecrewd enforces the size cap.
- `SIGTERM` stops polling and lets running jobs finish for the grace period
  (10 minutes), then cancels them; their leases expire and forecrewd queues
  them again.

## Command jobs

A job of `kind: command` runs a command instead of an agent
(contracts/job-spec.md, "Worker rules: command jobs"). It needs no runtime
label. The worker clones like for an agent job with the read token, checks
out `overlay.paths` from `overlay.ref` on top of the work tree, runs
the `setup` commands of a `machine` environment, and runs `argv` (or `shell`
with `sh -c`, `cmd /c` on Windows) in `command.workdir`. The command is code
of the repo, so it gets only a short list of the worker's own variables
(see Secrets) plus the environment's and the command's variables.

The process runs in its own process group. `timeout` is one budget for the
setup commands and the command together; when it is used up the job fails.
Stdout and stderr stream as `log` events (stderr lines start with `stderr: `;
a line is cut at 8 KiB). A job logs at most 8 MiB of command output: after
that one `[output truncated ...]` event says so and the rest is dropped while
the command keeps running. When the command ends, what it left running is
killed before any file is looked at, so nothing can change them while they
are uploaded. A heartbeat answer of `cancel` kills the command the same way.

Then it uploads the files matching `reports` and `artifacts` and parses the
reports as JUnit XML into one test run for the suite. Globs do not follow
symlinks, symlinks and the files behind them are refused, the search stops
after 200000 directory entries or 30 seconds (the job then fails), at most
200 files are uploaded, and names that are not valid artifact names are
skipped with a log line. A report over 32 MiB is not read; names are cut to 512 bytes and
messages to 4 KiB, and a run may hold 100000 test cases and 16 MiB of names
and messages; beyond that the run is `errored`. NUL bytes and invalid UTF-8
are removed from everything the worker sends.

The job succeeds when the command ran and its results were reported, whatever
the tests did. A test run is `errored`, with one `(report)` result holding
the reason, when no report matched, none could be parsed, or the reports hold
no test case at all (an empty report is never green). A command that cannot
start, and a timeout, fail the job and report the `errored` run with exit
code -1. A run whose command exited non-zero is `failed` even when the
reports show no failure, with an errored `(command)` result saying so. A
failed upload to forecrewd is retried, then the job is given back (`retry`);
a file forecrewd refuses as too large, or an evidence store that is not
configured, only costs the artifact: the results are still reported. No test
case is left out of the report; if forecrewd refuses the results as larger
than its `max_test_results_size`, the worker completes again with errored
runs saying so. Docker environments and services are not run by this worker
yet: such a job fails.

A command is not isolated from the worker (see below): it runs as the
worker's user and can read what that user can.

## Secrets

The repo token is used only by the worker's own `git ls-remote`, `fetch` and
`push`, passed through the environment of that process (`GIT_CONFIG_*`
`http.extraHeader`). It is never in the agent's environment, a git config,
a remote URL or a file, and it is redacted from events and logs. The agent
does not inherit `FORECREW_*` variables, so it never sees the worker token.
The agent does not inherit `GIT_*` variables either. The only `FORECREW_*`
variable an agent sees is `FORECREW_JOB_MCP_TOKEN`, its own job's gateway
token, also redacted from events and logs.

A command job gets an allowlist instead: `PATH`, `HOME`, `USER`, `LOGNAME`,
`LANG`, `LC_*`, `TZ`, `TMPDIR`, `TEMP`, `TMP` and, for Windows, `SystemRoot`,
`WINDIR`, `COMSPEC`, `PATHEXT`, `USERPROFILE`, `APPDATA`, `LOCALAPPDATA` and
`PROGRAMDATA`, then the variables of the spec's environment and command. The
worker's other variables, such as `ANTHROPIC_API_KEY` or `GITHUB_TOKEN`, never
reach a command, and command jobs get no `env` and a read-only repo token.

## Agents are not isolated from the worker

Agents run as the worker's user. That is **not a security boundary**: an agent
can read the worker's files and, on systems without the protections below,
its environment. Run the worker under a dedicated user that owns nothing else,
and treat everything on the machine that user can read as available to
agents. Isolation beyond that (containers, VMs) is not part of the worker.

**With `max_jobs` above 1 the agents of concurrent jobs, and anything they
leave running, can read each other's repo tokens** (from the environment of
the worker's git processes, or by interfering with them), and either token
can push to any branch. `max_jobs` is 1 by default; raise it only for
repos and roles you trust equally; the worker logs a warning at startup
when you do.
A production worker that needs isolation between jobs runs one job per worker
user or per machine.

What the worker does on Linux to narrow the exposure of the repo token:

- At startup it calls `prctl(PR_SET_DUMPABLE, 0)`, so its children cannot read
  `/proc/<pid>/environ` or the memory of the worker.
- It is a child subreaper. After each agent run, before any git call that
  carries the token, it kills what the agent left running: the processes
  marked with the job id (`FORECREW_JOB_MARK`) and their descendants, and,
  when no other agent is running, every other child of the worker except its
  own git processes (including `setsid` escapees and processes that scrubbed
  their environment). It stops every candidate with `SIGSTOP`, chases their
  children and rescans until nothing new appears, and only then kills them,
  so a chain of processes that respawn each other cannot outrun it. Agent
  starts and git starts wait while a sweep decides. With concurrent jobs it
  cannot tell an unmarked leftover from another job's agent and leaves those.

On other systems there is no such protection: the worker's environment may be
readable by agents and leftover agent processes are not killed.

## Running as a service

`deploy/worker/install.sh` (Linux with systemd, macOS with launchd) and
`install.ps1` (Windows) install the worker as a service under a dedicated
unprivileged user; see `docs/runbooks/add-worker.md`.

Known limit on Windows: the worker runs from a scheduled task started at boot
under that user, not as a Windows service, because `forecrew-worker` does not
implement the service control protocol. Stopping the task ends the worker
without the 10-minute grace period, and `install.ps1` runs `check` as the
administrator rather than as the service user.

The install script keeps the worker token in `/etc/forecrew-worker/token.env`.
On Linux that file is `root:root` 0600, so agents (which run as the service
user) cannot read it. On macOS and Windows the service user must read it, so
agents can read the worker token there.
