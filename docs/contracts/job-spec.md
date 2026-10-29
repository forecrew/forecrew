# Job spec

The job spec is everything a worker receives to run one job. It is
`domain.JobSpec`, returned in the `job` field of a successful
`POST /worker/v1/poll` (worker-api.md).

forecrewd builds the spec when the job is created (`workflow.JobCreator`):
the role is resolved (`roles.Resolver`: runtime adapter, model, write paths,
tools, schema, limits) and the prompt is built (`contextpack.Builder`). The
spec is stored in `jobs.spec` **without** `env` and `repo_token`
(`JobSpec.Redacted`). Those two secrets are added when the job is leased and
exist only in memory on both sides; they are never stored, logged or written
to disk (`JobSpec.LogValue` omits them).

## Example

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
  "env": {"ANTHROPIC_API_KEY": "sk-ant-..."},
  "repo_token": "ghs_..."
}
```

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
| `repo_token` | string | Short-lived GitHub App installation token scoped to `repo`: contents `write` when `write_paths` is non-empty, `read` otherwise. Used for clone and push over HTTPS through a credential helper or `GIT_CONFIG_*` env vars, never written into `.git/config`. Expires after about an hour. |

## Worker rules

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
   paths in `error` and push nothing. Read-only roles never commit or push, whether or not `branch` is
   set; since their tools may include a shell, the worker also checks after
   the run that the working tree is unchanged and otherwise completes the job
   as `failed` with the changed paths in `error`.
5. Push `branch` only if it starts with `agent/`, and report the pushed SHA
   as `head_sha`. If nothing changed, push nothing; `head_sha` is then the head of `branch` on the remote when the branch exists there (a retry whose earlier attempt already pushed), and empty only when there is no branch.
6. Complete the job (worker-api.md). forecrewd opens or updates the PR.
