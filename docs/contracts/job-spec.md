# Job spec

The job spec is everything a worker receives to run one job. It is
`domain.JobSpec`, returned in the `job` field of a successful
`POST /worker/v1/poll` (worker-api.md).

A job has a **kind** (`jobs.kind`, `domain.JobKind`):

- `agent` (the default; specs without `kind` are agent jobs): an agent role
  run through a runtime adapter, with a prompt and a model.
- `command`: a command run without a model, e.g. one test suite in one
  matrix cell. It has no prompt, model, runtime adapter, write paths or
  toolbox, never commits or pushes, and is routed by `needs` and its pool
  like any job (worker-api.md). `domain.JobSpec.Validate` checks these
  rules.

forecrewd builds the spec when the job is created (`workflow.JobCreator`):
the role is resolved (`roles.Resolver`: runtime adapter, model, write paths,
tools, schema, limits) and the prompt is built (`contextpack.Builder`). The
spec is stored in `jobs.spec` **without** `env`, `repo_token` and `job_mcp`
(`JobSpec.Redacted`). These secrets are added when the job is leased and
exist only in memory on both sides; they are never stored, logged or written
to disk (`JobSpec.LogValue` and `JobMCP.LogValue` omit them).

## Example: agent job

```json
{
  "job_id": "01960d3e-6d7b-7c1a-9f3e-2a7c4b1d9e10",
  "account_id": "01960d3e-1111-7000-8000-000000000001",
  "repo": "acme/app",
  "base_ref": "main",
  "branch": "agent/task-12",
  "role": "developer",
  "write_paths": ["**", "!.github/**", "!.forecrew/**"],
  "runtime": "claude-code",
  "model": "claude-sonnet-4-5",
  "prompt": "You are the developer...\n\n# Task TASK-12\n...",
  "output_schema": {"type": "object", "required": ["summary"], "properties": {"summary": {"type": "string"}}},
  "allowed_tools": ["Read", "Edit", "Write", "Bash"],
  "max_turns": 80,
  "timeout": "45m0s",
  "include_repo_facts": false,
  "locked_paths": ["internal/auth/*_test.go"],
  "env": {"ANTHROPIC_API_KEY": "sk-ant-..."},
  "repo_token": "ghs_...",
  "job_mcp": {"url": "https://forecrew.example.com/job-mcp", "token": "fcj_..."}
}
```

## Example: command job

```json
{
  "job_id": "01960d3e-7a10-7c1a-9f3e-2a7c4b1d9e11",
  "account_id": "01960d3e-1111-7000-8000-000000000001",
  "kind": "command",
  "repo": "acme/app",
  "base_ref": "main",
  "branch": "agent/task-12",
  "role": "",
  "write_paths": [],
  "runtime": "",
  "model": "",
  "prompt": "",
  "allowed_tools": [],
  "max_turns": 0,
  "timeout": "20m0s",
  "include_repo_facts": false,
  "command": {
    "argv": ["gotestsum", "--junitfile", "reports/unit.xml", "--", "./..."],
    "env": {"CGO_ENABLED": "0"},
    "reports": ["reports/unit.xml"],
    "artifacts": ["coverage.out"]
  },
  "environment": {
    "name": "linux-go",
    "provider": "docker",
    "image": "golang:1.27-bookworm",
    "needs": ["linux"],
    "versions": {"go": "go version"},
    "services": {"postgres": {"image": "postgres:18", "ports": [5432], "health": {"command": "pg_isready -U postgres", "interval": "2s"}}}
  },
  "cell": {"os": "linux", "go": "1.27"},
  "suite": "unit",
  "repo_token": "ghs_..."
}
```

A run of new tests against the base branch (the fail-on-base check) is the
same with `"branch": ""`, `"base_ref": "main"` and
`"overlay": {"ref": "agent/task-12", "paths": ["internal/auth/login_test.go"]}`.

## Fields

| Field | JSON type | Meaning |
|-------|-----------|---------|
| `job_id` | string (UUID) | The job. Used in every worker API path. |
| `account_id` | string (UUID) | Account the job belongs to. |
| `repo` | string | GitHub repo `owner/name`. Clone URL: `https://github.com/<repo>.git`. |
| `base_ref` | string | Branch or commit SHA to check out and start from. |
| `branch` | string | Branch to check out, and for writing roles to commit to and push. Always starts with `agent/` (e.g. `agent/task-12`). Writing roles: the worker creates it from `base_ref` if it does not exist on the remote, otherwise checks it out (rework); it force-pushes nothing: pushes are fast-forward only. Read-only roles: may be empty (the worker checks out `base_ref`) or set to an existing branch to inspect, e.g. the branch a reviewer reviews; the worker checks it out but never commits or pushes it. |
| `role` | string | Role name, e.g. `developer`. Informational for the worker. |
| `write_paths` | string[] | Paths the job may change, as [doublestar](https://github.com/bmatcuk/doublestar) globs relative to the repo root with `/` separators. A pattern starting with `!` excludes. A file is writable if it matches at least one include pattern and no exclude pattern. Empty means read-only: nothing is committed or pushed. |
| `runtime` | string | Runtime adapter to run: `claude-code` or `codex`. The worker has the label `runtime:<runtime>`. |
| `model` | string | Model name passed to the runtime CLI. |
| `prompt` | string | Full prompt (role instructions plus context pack). |
| `output_schema` | object | JSON Schema (draft 2020-12) the final output must match, inline with all `$ref`s resolved. Absent means free text. |
| `allowed_tools` | string[] | Tools the agent may use, in the runtime's own syntax (Claude Code tool names and patterns such as `Bash(go test:*)`; Codex maps them to its sandbox and approval settings). Empty means the adapter's default for the access level (read-only roles get no editing tools). |
| `max_turns` | integer | Maximum agent turns; 0 means the runtime default. |
| `timeout` | string | Go duration (`"45m0s"`). The worker stops the runtime when it passes and completes the job as `failed`. |
| `include_repo_facts` | boolean | When true the worker runs the analyzer on the checkout and appends the facts to the prompt as a `## Repo facts` section holding the `analyzer.Facts` JSON in a fenced block. |
| `env` | object string → string | Secrets (runtime credentials) set in the runtime process environment only. Never written to disk or logs. May be absent. |
| `repo_token` | string | Short-lived GitHub App installation token scoped to `repo`: contents `write` when `write_paths` is non-empty, `read` otherwise (always `read` for command jobs, and never wider than the job's pool allows, config.md `pools`). Used for clone and push over HTTPS through a credential helper or `GIT_CONFIG_*` env vars, never written into `.git/config`. Expires after about an hour. |
| `job_mcp` | object | Agent jobs only, from T47. The job MCP gateway (job-mcp.md): `url` (`<public base>/job-mcp`, informational, may be empty; the worker connects to `<server_url>/job-mcp`) and `token` (`fcj_...`), a secret valid only while this worker holds this lease. Added at lease time, never stored; the worker passes the token to the runtime in an environment variable, never in argv or a file. Absent: the agent has no channel back to forecrewd. |
| `kind` | `agent` \| `command` | Job kind; absent means `agent`. |
| `locked_paths` | string[] | Agent jobs only. Globs like `write_paths` (no `!`) the job may not change even when `write_paths` allow it, e.g. the tests a test engineer wrote before the developer's job. |
| `toolbox` | object | Agent jobs only. The role's toolbox (`packs/toolboxes/<name>.yaml`, toolbox.schema.json), resolved: the image the agent runs in, its CLIs and MCP servers. Absent: the agent runs on the worker's host. A job with a toolbox needs the `docker` label. |
| `command` | object | Command jobs only; required. What to run, below. |
| `command.argv` | string[] | Program and arguments, run without a shell. Exactly one of `argv` and `shell`. |
| `command.shell` | string | Run with `sh -c`, or `cmd /c` on Windows. |
| `command.env` | object string → string | Non-secret variables, on top of the environment's `env`. Names that look like secrets (`domain.SecretLikeName`: `*_TOKEN`, `*_SECRET`, `*_PASSWORD`, `*_KEY`) are refused. |
| `command.workdir` | string | Directory to run in, relative to the repo root; empty is the root. |
| `command.reports` | string[] | JUnit XML globs relative to the repo root, parsed into `test_runs` (worker-api.md, `complete`). |
| `command.artifacts` | string[] | Globs of other files to upload as artifacts. |
| `environment` | object | Command jobs only. The environment (`.forecrew/environments/<name>.yaml`, environment.schema.json), resolved inline. Absent: the worker's host as it is. Its `needs` (plus `docker` for provider `docker`, or services) are part of the job's `needs`. |
| `cell` | object string → string | Command jobs only. The matrix cell (axis → value) the job runs. |
| `suite` | string | Command jobs only. The matrix suite the job runs. |
| `overlay` | object | Command jobs only. `ref` (branch or SHA) and `paths` (paths or globs): after checking out `base_ref`, the worker checks out these paths from `ref` on top of the work tree before running, e.g. new tests run against the base. |

## Worker rules: agent jobs

1. Create a fresh workspace under `work_dir` per job and delete it afterwards,
   whatever the outcome.
2. Clone `repo` at `base_ref` (shallow is fine), check out `branch` if set.
   When `branch` is set, fetch enough history that
   `git diff origin/<base_ref>...HEAD` (or `<base_ref>...HEAD` when
   `base_ref` is a SHA) shows the branch's changes; prompts rely on it.
3. Run the runtime adapter with `model`, `prompt` (plus repo facts),
   `output_schema`, `allowed_tools`, `max_turns`, `timeout`, `env`.
4. For writing roles, the worker builds the commit itself from the files in
   the work tree after the run: one commit by `forecrew-bot` whose parent is
   the starting commit. The agent's own commits and history are never
   pushed. Every path changed, added or deleted between the starting commit
   and that commit must be writable per `write_paths`, and none may be a
   symlink or a submodule; otherwise complete as `failed` with the offending
   paths in `error` and push nothing. A changed path matching
   `locked_paths` fails the job the same way. Read-only roles never commit or push, whether or not `branch` is
   set; since their tools may include a shell, the worker also checks after
   the run that the working tree is unchanged and otherwise completes the job
   as `failed` with the changed paths in `error`.
5. Push `branch` only if it starts with `agent/`, and report the pushed SHA
   as `head_sha`. If nothing changed, push nothing; `head_sha` is then the head of `branch` on the remote when the branch exists there (a retry whose earlier attempt already pushed), and empty only when there is no branch.
6. Complete the job (worker-api.md). forecrewd opens or updates the PR.
7. With a `toolbox` (from T48), the runtime runs inside the toolbox image with the work
   tree mounted; commit, push and the path checks stay on the worker,
   outside the container.

## Worker rules: command jobs

1. Create, and afterwards delete, a fresh workspace as for agent jobs.
2. Clone `repo` at `base_ref`, check out `branch` if set (read-only, with
   the read token), then apply `overlay`: check out `overlay.paths` from
   `overlay.ref` on top of the work tree. Paths must stay inside the repo.
3. Prepare the `environment`: `docker` builds or pulls the image (cached by
   a hash of the environment and the files it names), runs `setup` at build
   time and starts `services` as sibling containers on a per-job network
   after their `health` check passes; `machine` runs `setup` on the host;
   `actions` jobs are not leased by workers (pool `actions`, served by
   forecrewd). No Docker socket is mounted into containers.
4. Run `command` in `command.workdir` with the environment's `env` and
   `command.env` (no secrets: `env` of the spec is empty for command jobs),
   bounded by `timeout`; stream stdout and stderr as `log` events. Run each
   `versions` command and record its output.
5. Collect `command.reports` and `command.artifacts` (symlinks and paths
   outside the work tree refused), upload them
   (`PUT /worker/v1/jobs/{job_id}/artifacts/{name}`, worker-api.md) and
   parse the reports into `test_runs`.
6. Never commit or push. The read-only check of rule 4 above does not
   apply: tests may write build output and reports into the work tree,
   which is deleted anyway.
7. Complete with `status: succeeded` when the command ran and its results
   were reported, whatever the test outcome: failing tests make a `failed`
   test run, a missing or unparsable report an `errored` one, and the exit
   code is kept in the run. `failed` is for the job itself: the environment
   could not be built, a service never became healthy, or `timeout` passed.
   `retry` is for infrastructure problems, as for agent jobs.
