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
- `SIGTERM` stops polling and lets running jobs finish for the grace period
  (10 minutes), then cancels them; their leases expire and forecrewd queues
  them again.

## Secrets

The repo token is used only by the worker's own `git ls-remote`, `fetch` and
`push`, passed through the environment of that process (`GIT_CONFIG_*`
`http.extraHeader`). It is never in the agent's environment, a git config,
a remote URL or a file, and it is redacted from events and logs. The agent
does not inherit `FORECREW_*` variables, so it never sees the worker token.
The agent does not inherit `GIT_*` variables either.

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
