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
both `darwin`, the `GOOS` value of conventions.md, and `macos`, the name repo profiles use) and `runtime:claude-code` / `runtime:codex` / `runtime:opencode` for each of `claude`,
`codex` and `opencode` found on `PATH`, and `docker` when the Docker engine answers
`docker version` (see Environments).

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
- `job_status` says why a job waits, for example `No machine in the Reasoning
  zone is online`, or `Nothing here runs a machine with gpu` when workers of its
  pool are online and none has its labels ([mcp.md](mcp.md#why-a-job-waits)).
- The read-only roles of the packs, `architect` and `product-owner`, run in
  `control`. Until a worker of that pool is online their jobs stay queued and
  `job_status` says why. The Compose stack ships one (`worker-control`, see
  install.md); other installs add one with `forecrew worker add <name> --pool control`.
- Pools other than `build`, `control` and `actions` are defined under `pools`
  in `forecrew.yaml`; `forecrew worker add` and `move` read it (`--config`,
  default `forecrew.yaml`) only for those. Pool `actions` takes no workers:
  forecrewd runs its jobs on GitHub-hosted runners (see [actions.md](actions.md),
  with its costs and limits).

## Machines

The workers above are static: an admin adds them and they stay. A worker on a
machine of the fleet (docs/contracts/fleet.md) is **ephemeral**: it is made
when the machine boots, from a one-time token, takes its pool and labels from
the machine's shape, and goes with the machine. Nothing changes for static
workers.

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
- A changed path that matches the spec's `locked_paths` fails the job, like
  one outside `write_paths`: the error names the paths and nothing is pushed.
  A `locked_paths` pattern that is not valid doublestar syntax fails the job
  before anything runs.
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
- Every agent job gets a scratch directory for evidence (a screenshot, a log)
  next to its checkout, outside it (`<work_dir>/jobs/<job id>.evidence`), in
  `FORECREW_EVIDENCE_DIR`. It is removed with the job's work dir, and since
  it is not part of the checkout, a read-only role writing there does not
  trip the "changed the checkout" check. Mark its files with
  `evidence_attach` as `evidence:<relative path>`.
- Files the agent marks with `evidence_attach` arrive in heartbeat answers
  (`attach`) until the worker acknowledges them in its next heartbeat. After
  the runtime exits it sends one more heartbeat for the last ones, then uploads each as an artifact of the job
  (`PUT .../artifacts/{name}`) before checking and pushing the work tree. A
  file must be a regular file inside the work tree or the scratch directory, reached without
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
runs saying so. A job of an `actions` environment is never leased by a worker.
Services need provider `docker`; an environment with another provider and
`services` is refused when it is validated.

A command is not isolated from the worker (see below): it runs as the
worker's user and can read what that user can.

## Environments

A command job whose environment has provider `docker` (environment.schema.json)
runs in a container on a worker with the `docker` label. The worker adds the
label when `docker version` answers at start, and keeps working without it
(`forecrew-worker check` says which). The daemon must run on the worker's
machine: the work tree is bind-mounted. The worker uses the `docker` command
line client through `os/exec`, not the Engine API client: it needs a dozen
calls, the operator already configures the CLI (contexts, `DOCKER_HOST`,
registry logins), and it adds no dependency.

**Image.** `image` is pulled; `dockerfile` (with `context`, default the repo
root) and `devcontainer` are built from the job's checkout. Of a
`devcontainer.json` (comments and trailing commas are allowed) the worker uses
only `image`, `build.dockerfile`, `build.context` (both relative to the file),
`containerEnv` and `postCreateCommand`; mounts, `runArgs`, `privileged`,
features and every other key are ignored. The result is tagged
`forecrew-env:<hash>`; the hash covers the image reference, the `setup`
commands, the Dockerfile's path and contents, and, for a build, the path, type,
mode and contents of every file of the build context (`.git` excluded; there
is no `.dockerignore` handling, so keep `context` small). An existing tag is
reused: the second job with the same inputs neither pulls nor builds. `setup`
commands run as `RUN` steps in a layer on top of the image (as root; they see
no environment variables and no secrets). A build is not a place for secrets:
the worker passes no build arguments, no SSH agent and no credentials to it.
Image names come from repos, so by default every Docker call of the worker
(pull, build, service images) runs with an empty Docker config
(`DOCKER_CONFIG=<work_dir>/docker-config`; the worker's `DOCKER_CONFIG`,
`DOCKER_AUTH_CONFIG` and `DOCKER_CONTEXT` are dropped, the daemon of the
current context is kept) and so cannot use the operator's registry logins:
private images fail to pull. Set `registry_auth: true` in `worker.yaml` to use
the worker user's logins, only on a worker whose repos you trust with every
registry that user is logged in to.

**Pruning.** The worker records each image it builds (`forecrew-env:*`) or
pulls (the `image`, the images the FROM lines of a Dockerfile name, service
images) in `<work_dir>/env-images`, with the time of its last use. At start
and then daily it removes the images unused for `image_prune_days`
(worker.yaml, default 14). What stays: images that were on the daemon before
the worker needed them (they are the operator's), images a container still
uses (tried again next time), layers shared with an image that is kept, the
build cache (`docker builder prune` is the operator's), and images a
Dockerfile names through an `ARG` or only inside a multi-stage `COPY --from`.

**Network.** Every docker job gets its own bridge network
`forecrew-<job id>`, whether it has services or not, so concurrent jobs cannot
see each other on the default bridge. It is removed with the containers as
soon as the command ends. Docker allocates a subnet per network from its
address pools (about 30 /16 networks by default): a worker with a high
`max_jobs` should configure smaller pools in the daemon (`default-address-pools`
with `size: 24` in daemon.json). Build steps run on the default bridge.

**Services.** For an environment with `services` the worker pulls each
service image (unless the daemon has it; the output is in the job log) and
starts it on the network with its `env` and the network alias equal to its key (the container itself is named
`forecrew-<job id>-<key>`, so jobs on one daemon do not clash). It then waits
for `health` (`interval` and `retries` as in the schema), or, without
`health` and with `ports`, until every port accepts a connection (probed in
the service container with `nc`, or bash's `/dev/tcp`; an image with neither
needs a `health` command). A service that exits, or is not ready in time, or
the job's timeout, fails the job, with the end of the service's log in the job
log. Ports are never published. The job container gets `<NAME>_HOST` (the
key) and, with `ports`, `<NAME>_PORT` (the first port) for each service; NAME is
the key in upper case with every other character turned into `_`; two keys
that give the same NAME (`my-db` and `my_db`) are refused. Building the
image, pulling, starting and waiting for the services all count against the
job's `timeout`.

**Job container.** The work tree is mounted read-write at `/work`, which is
also the working directory (`command.workdir` below it). The command runs as
the worker user's uid and gid (the job fails if that is 0: run the worker as
a dedicated user) with no entrypoint of the image, `--init`, all capabilities
dropped, `no-new-privileges` and at most 4096 processes (`--pids-limit`), so a
fork bomb stays in the container. There is no memory limit: the environment
spec has no field for it. Its environment is exactly:
`LANG`, `LC_*` and `TZ` of the worker, `HOME=/tmp`, the devcontainer's
`containerEnv`, the service variables, the environment's `env` and the
command's `env`. `PATH`, the user's name and every other variable of the worker
stay out. Reports and artifacts are read from the work tree on the host
afterwards, exactly as for the host (same limits, symlinks refused). `setup`
and `timeout` share one budget: building the image, starting the services and
the command all count.

`git` does not work in the container: the work tree's `.git` file points to the
worker's repo cache on the host. A job that needs the history reads it from
files, or runs on a `machine` environment. If `docker run` itself fails (exit
code 125: the daemon could not start the container) the job is given back
(`retry`) instead of reporting 125 as the command's exit code.

**Versions.** Besides the output of the `versions` commands (run in the job
container), the test run records `image` (the registry digest, or the image
id for a build) and `os-release` (`PRETTY_NAME` of `/etc/os-release`).

**Cleanup.** Every container and network carries the labels
`forecrew.job=<job id>` and `forecrew.worker=<id of the work dir>`. They are
removed (`docker rm -f -v`, `docker network rm`) before the results are read,
and again whatever the outcome: success, failure, timeout, cancel, lease lost.
When the worker starts it removes the leftovers of its own earlier run, by the
worker label, before it takes a job.

**Security notes.**

- The job container has no Docker socket, is not privileged, does not use the
  host network or PID namespace, publishes no port and mounts nothing but the
  work tree. A Dockerfile, an image or a service of the repo is therefore code
  that runs in a container of the daemon, as the repo's code does on a
  `machine`; it cannot ask the daemon for anything.
- Build steps use the default bridge network and job containers their own
  network; both can reach the internet, the machine's LAN (and, through the host's address,
  services bound to it). Workers on sensitive networks should not take
  untrusted repos. Network isolation comes with the fleet microVM provider
  (T61).
- A repo can run any image already on the daemon (and any it can pull from a
  registry the worker may reach), including images the operator built or
  pulled for other things. Keep nothing in that daemon's image store that
  must stay private from the repos this worker takes.
- Whoever can start containers is root on the machine: give the worker user
  access to the daemon only on a machine you dedicate to this, or use rootless
  Docker. A rootless daemon maps the container's uids differently; the
  work tree must stay writable by the job's user.
- A bind mount needs the work dir to be visible to the daemon (Docker Desktop
  shares `/Users` and `/home` only; SELinux hosts may need the volume to be
  relabeled).
- Mutable image tags are pulled once per hash: pin tags or digests, as the
  schema asks.
- Environment variables of the spec are visible in the process list of the
  host while a container starts (`docker run -e`), which is why they are
  non-secret by contract.
- An `image` or service image that starts with `-` or contains white space is
  refused (it would be read as a flag), and the worker passes `--` before
  image names where the Docker client supports it.
- A `devcontainer` or `dockerfile` path that leaves the repo or crosses a
  symlink fails the job, and so does a name that looks like a secret in
  `containerEnv`.

## Toolboxes

A role whose pack names a `toolbox` (`packs/toolboxes/<name>.yaml`,
contracts/toolbox.schema.json) runs its agent jobs inside that toolbox's
container, with the toolbox's CLIs and MCP servers. The job needs a worker
with the `docker` label (plus the toolbox's `needs`): forecrewd adds them to the job.
The `security-reviewer` role uses `security` (semgrep, gitleaks,
osv-scanner and trivy in filesystem mode, versions pinned, downloads checked
against their checksums). `browser` (Playwright's MCP server and Chromium) is
for the QA tester of a later task.

Only the agent CLI runs in the container. The worker still clones, snapshots
the work tree after the run, checks `write_paths` and `locked_paths`, builds
its own commit and pushes it, outside the container, exactly as for a host job.

**Image.** A toolbox with `image` is pulled. One with `dockerfile` is built from
the toolbox files `forecrew-worker` ships (an embedded copy of `packs/toolboxes`,
used as the build context, so a worker needs no checkout; a worker older than
forecrewd may lack a file and fails the job saying so). The result is tagged
`forecrew-toolbox:<name>-<hash>`, the hash covering every file of the context, so
an existing tag is reused: the second job neither pulls nor builds, and
changing a file builds a new image. Tags are recorded like environment images
and pruned after `image_prune_days` without use.

**Agent CLIs.** Toolbox Dockerfiles start from the Forecrew base image,
`packs/toolboxes/base`, which has Node, git and both runtime CLIs at the exact
versions of its `package-lock.json`:

```dockerfile
ARG FORECREW_BASE
FROM ${FORECREW_BASE}
```

A Dockerfile that names `FORECREW_BASE` gets the base built first
(`forecrew-toolbox:base-<hash>`) and passed as a build argument. The
alternative, adding the CLI as a layer on top of any image, would have to
guess the distribution, its package manager and whether Node exists; the
base makes the CLIs one pinned, tested piece, at the price that an arbitrary
`image:` toolbox must already contain the CLI of the runtime its role uses
(`claude` or `codex` on its `PATH`), or the job fails.

**Container.** Each `docker run` of the job:

- runs as the worker's uid:gid (a worker running as root refuses), with
  `--cap-drop ALL`, `no-new-privileges`, a process limit and `--init`; no
  Docker socket, not privileged, no host network or PID namespace, no published
  port;
- mounts the work tree and the evidence directory read-write and the repo's
  bare cache read-only, all at their host paths (so `git diff` and `git log`
  work in the container; nothing can write the cache), and nothing else;
- has an empty in-memory `HOME` (`/home/agent`, which is also the runtime's
  config directory) and `/tmp` of 1 GiB each, and 512 MiB of shared memory for
  Chromium. They die with the container, so every run starts with an isolated
  home and the worker account's own `~/.claude` or `~/.codex` never reaches it;
- gets exactly the job's environment: the runtime key (`env` of the spec),
  `FORECREW_JOB_MCP_TOKEN`, `FORECREW_EVIDENCE_DIR`, the locale and time zone.
  The values of credentials travel in the environment of the docker client and
  the command line only names them (`-e NAME`), so they are not in the process
  list. They are part of the container's configuration, though: anyone who can
  use the Docker daemon sees them in `docker inspect` while the container
  exists (it is removed when the agent ends, before evidence is uploaded).
  The runtime's key may also come from the worker's own environment
  (`credential_env` empty, config.md): the worker forwards that one variable
  (`ANTHROPIC_API_KEY` or `CODEX_API_KEY`) into the container too. A credential whose name the docker client or the loader reads
  (`DOCKER_*`, `LD_*`, `LC_*`, `*_PROXY`, `PATH`, `HOME`, `TMPDIR`, `XDG_RUNTIME_DIR` and the
  other variables of the worker's allowlist) fails the job, set or not. The repo token and the worker's own
  environment never enter the container;
- is on a network of its own for the job (created and removed with the
  container, which is also removed on cancel or a crash like environment
  containers).

**Network.** The container can reach the internet and, like the containers of
environments, the machine's network. Agents need their model API, and the
scanners download their rules and databases; there is no egress filtering
before the microVM provider (T61). Do not give a toolbox role a repo you do not
trust with the machine's LAN.

**Reaching forecrewd.** The agent talks to the job MCP gateway at the worker's
`server_url`. If that is on the worker's own loopback (`localhost`,
`127.0.0.1`, `[::1]`), the container is told `host.docker.internal` instead,
which the worker maps to the host's gateway with `--add-host`
`host.docker.internal:host-gateway`; forecrewd must then listen on an address
the Docker bridge reaches (not only `127.0.0.1`), and anything else listening
on the host's non-loopback addresses is reachable too, so use a real
`server_url` outside of development. A `server_url` that is a name or address
the container can resolve is used as is.

Whether or not that rewrite applies, a container can always reach the host's
ports that are bound to all addresses (`0.0.0.0`, `[::]`) through the bridge
gateway. Do not bind an unauthenticated service on the worker's machine to all
addresses while it runs toolbox jobs. The worker does not rewrite https
certificates: a private CA is not trusted in the container.

**Custom endpoints.** A runtime's `base_url` is handed to the CLI as is. A
loopback one (`localhost`, `127.0.0.1`) reaches the container's own loopback,
not the host's, so a model server on the worker's machine must be given by an
address the container can reach. The endpoint's `key_env` is a credential like
the others: by name, and subject to the same refused names. The `opencode`
runtime does not run in a toolbox container yet (the base image has no
opencode): such a job fails with a message saying so.

**MCP servers.** The toolbox's `mcp_servers` are stdio servers the agent CLI
starts inside the container, from the same MCP configuration as the gateway
(`--mcp-config` with `--strict-mcp-config` for claude-code, `-c mcp_servers.*`
for codex; docs/runtimes.md). The job's allowed tools get `mcp__<server>`, every
tool of the server. A secret reaches a server only through its
`credential_env`, by variable name.

**Versions.** At the start of a job the worker runs each CLI's `version` command
once in the container and writes the first line of each to the job log
(`toolbox <name> cli <cli>: ...`).

## Secrets

The repo token is used only by the worker's own `git ls-remote`, `fetch` and
`push`, passed through the environment of that process (`GIT_CONFIG_*`
`http.extraHeader`). It is never in the agent's environment, a git config,
a remote URL or a file, and it is redacted from events and logs. The agent
does not inherit `FORECREW_*` variables, so it never sees the worker token.
The agent does not inherit `GIT_*` variables either. The only `FORECREW_*`
variables an agent sees are `FORECREW_JOB_MCP_TOKEN`, its own job's gateway
token (also redacted from events and logs), and `FORECREW_EVIDENCE_DIR`, the
path of its scratch directory.

A command job gets an allowlist instead: `PATH`, `HOME`, `USER`, `LOGNAME`,
`LANG`, `LC_*`, `TZ`, `TMPDIR`, `TEMP`, `TMP` and, for Windows, `SystemRoot`,
`WINDIR`, `COMSPEC`, `PATHEXT`, `USERPROFILE`, `APPDATA`, `LOCALAPPDATA` and
`PROGRAMDATA`, then the variables of the spec's environment and command. The
worker's other variables, such as `ANTHROPIC_API_KEY` or `GITHUB_TOKEN`, never
reach a command, and command jobs get no `env` and a read-only repo token.

## Agents are not isolated from the worker

Agents run as the worker's user. That is **not a security boundary**: an agent
(except in a toolbox container, above) can read the worker's files and, on systems without the protections below,
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
