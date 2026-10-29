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
