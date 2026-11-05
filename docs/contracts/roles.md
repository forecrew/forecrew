# Role packs

A role is a YAML file `packs/roles/<role>.yaml` with its prompt next to it
(`packs/roles/<role>.md`) and its output schema under `packs/schemas/`. The
packs are embedded in forecrewd (`forecrew.Files`) and loaded by
`roles.Load`, which decodes strictly (unknown keys are errors) and reports
every problem of every file at once, wrapping `roles.ErrInvalidRole`.
forecrew.yaml (config.md) picks the runtime and model tier each role runs
with; `roles.Catalog` combines both into a `roles.Role`, which the
JobCreator turns into a job spec (job-spec.md).

## Example

```yaml
role: developer
model: strong
prompt: roles/developer.md
tools: [repo.read, repo.write, git.read, git.commit, run.tests]
write_paths: ["**", "!.github/workflows/**", "!.forecrew/**"]
produces: [code, tests]
output_schema: schemas/developer-result.json
definition_of_done:
  - every acceptance criterion of the task has at least one test
escalate_when:
  - a criterion is ambiguous
max_turns: 120
timeout: 60m
pool: build          # optional, default build
toolbox: security    # optional, packs/toolboxes/security.yaml
resources:           # optional, what its jobs need of a machine
  min_memory_mb: 8192
```

## Fields

| Key | Type | Required | Meaning |
|-----|------|----------|---------|
| `role` | string | yes | Role name, `^[a-z][a-z0-9-]*$`, equal to the file name without `.yaml`. |
| `model` | `fast` \| `strong` \| `best` | yes | Tier the role is written for. forecrew.yaml `roles.<role>.tier` decides the tier jobs run with. |
| `prompt` | path | yes | Role instructions, relative to `packs/`; must not be empty. Placed first in the job prompt. |
| `tools` | list | yes, ≥1 | Forecrew tool names, mapped to runtime tools: `repo.read` (Glob, Grep, Read), `repo.write` (Edit, Write), `git.read` (git diff, log, show, status), `git.commit` (git add, commit), `run.tests` (Bash). Unknown names are errors. |
| `write_paths` | list of globs | yes (`[]` for read-only) | Doublestar globs relative to the repo root; `!` excludes; no absolute paths or `..`. Non-empty needs `repo.write` and at least one include pattern; empty forbids `repo.write` and `git.commit`. The worker enforces it (job-spec.md). |
| `produces` | list of strings | no | What the role's work is, e.g. `code`, `tests`, `spec`; informational. |
| `output_schema` | path | yes | JSON Schema of the final output, relative to `packs/`, bundled inline (`roles.BundleSchema`) into the spec. |
| `definition_of_done` | list of strings | yes, ≥1 | What must be true when the job finishes; part of the prompt. |
| `escalate_when` | list of strings | no | When the agent must stop and ask a human; part of the prompt. |
| `max_turns` | integer ≥ 0 | no | Agent turns; 0 means the runtime default. |
| `timeout` | Go duration | yes | Positive, e.g. `30m`. |
| `pool` | string | no, default `build` | Worker pool the role's jobs run in (config.md `pools`, worker-api.md Pools). A `domain.ValidName`; whether the pool exists and allows the role is checked when a job is created. Read-only reasoning roles (councils, triage, architect, product-owner) belong in `control`; a read-only role that runs the tests, such as `reviewer`, stays in `build`. |
| `toolbox` | string | no | Name of a toolbox, `packs/toolboxes/<name>.yaml` (toolbox.schema.json); it must exist. The role's agent jobs run inside the toolbox image, with its CLIs and MCP servers, on a worker with the `docker` label (from T48). The resolved toolbox is the spec's `toolbox`. |
| `resources` | mapping | no | What the role's jobs need of a machine: `min_cpu`, `min_memory_mb`, `min_disk_gb`, `gpu`, integers ≥ 0. Copied to `jobs.resources`; it picks the shape (fleet.md). |
| `context` | mapping | no | Reserved for the context pack settings of T38 (which knowledge sections and registers a job sees). Must be absent or empty until then. |

## Job MCP tools

The job MCP gateway (job-mcp.md) adds role tools that expose its tools to
the role's agent jobs: `kb.search`, `kb.propose`, `request_access`,
`assumption.record`, `evidence.attach`, `notes.write`, `ask_human`,
`report_progress` and `council.read`. A job sees a gateway tool only when
its role lists the role tool. They become valid `tools` entries when the
gateway registers them (T47 and the tasks that add each tool); until then
`roles.Load` refuses them as unknown.

## Toolboxes

`packs/toolboxes/*.yaml` are loaded with the roles (`roles.LoadToolboxes`)
and validated by `testsys.ParseToolbox` against toolbox.schema.json and
`domain.Toolbox.Validate`: the name equals the file name, exactly one of
`image` and `dockerfile`, unique CLI and MCP server names, and no
secret-looking names in MCP server `env` (secrets reach a server only
through `credential_env`, which names variables of the job's credentials).
There may be no toolboxes.
