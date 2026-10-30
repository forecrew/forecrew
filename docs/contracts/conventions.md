# Conventions

Rules every Go package follows. AGENTS.md has the engineering rules; this file
has the coding conventions they refer to.

## Errors

- Wrap with context and `%w`: `fmt.Errorf("lease job %s: %w", ref, err)`.
  Messages are lowercase, without trailing punctuation, and say what was
  being done.
- Each package exposes typed sentinel errors for the conditions callers act
  on (`store.ErrNotFound`, `store.ErrLeaseLost`, `domain.ErrInvalidTransition`,
  `config.ErrInvalid`, ...). Callers match with `errors.Is`; never compare
  strings.
- Return errors; do not log and return the same error. The layer that handles
  an error (HTTP handler, worker loop, reaper) logs it once.
- HTTP APIs (`/worker/v1`, `/api/v1`) answer errors as
  `{"error": {"code": "<snake_case>", "message": "<human text>"}}` and map
  sentinels to status codes: not found 404, conflict / lease lost /
  invalid transition 409, invalid input 400, unauthorized 401.
- `panic` only for programmer errors at startup (for example
  `regexp.MustCompile` on a constant).

## Logging

- `log/slog` with the JSON handler, writing to stderr. The logger is created
  in `main`/`run` and passed down (struct field or argument); never use
  `slog.Default()` in library code.
- Use stable snake_case keys. Always add the IDs in scope: `account_id`,
  `project_id`, `goal_id`, `task_id`, `job_id`, `gate_id`, `worker_id`,
  `run_id`, and the human `ref` (`JOB-41`) when known. Prefer
  `logger.With(...)` once per job or request.
- Levels: `Debug` for detail, `Info` for state changes (job leased, gate
  resolved), `Warn` for handled anomalies (lease expired, webhook
  redelivered), `Error` for failures that need a human.
- Never log secrets: tokens, private keys, webhook secrets, `JobSpec.Env`,
  `JobSpec.RepoToken`, database URLs with passwords. Types holding secrets
  implement `slog.LogValuer` to redact (see `domain.JobSpec`).

## Configuration

- Config comes only from `forecrew.yaml` / `worker.yaml` plus environment
  variables for secrets (config.md), loaded once in `run` by
  `internal/config` and passed down as values. Packages do not read files or
  environment variables themselves.
- Behavior that differs between self-hosted and SaaS is config, never a code
  path keyed on "edition".

## Context

- Every function that does I/O or may block takes `ctx context.Context` as
  its first parameter and honors cancellation.
- Never store a context in a struct. Use `context.WithTimeout` around
  external calls (GitHub, runtime CLIs); the job spec `timeout` bounds a run.
- Request-scoped values (the authenticated actor) are passed as explicit
  arguments (`domain.Actor`), not hidden in the context.

## No globals

- No mutable package-level variables and no `init` side effects. Exceptions:
  sentinel errors, constant-like tables (`domain` transition tables, compiled
  regexps) and `main.version` set by `-ldflags`.
- Dependencies (store, clock, logger, HTTP client) are passed to
  constructors. Code that reads the time takes a `func() time.Time` (or uses
  the database clock) so tests can control it; see `testkit.Clock`.

## IDs and refs

- Every entity ID is a UUIDv7 (`domain.NewID`, stdlib `uuid.NewV7`), stored
  as Postgres `uuid`, sent as the canonical string.
- Goals, specs, tasks, jobs and gates also get a per-account number and a
  human ref `GOAL-3`, `SPEC-4`, `TASK-12`, `JOB-41`, `GATE-7`
  (`domain.Ref`, `domain.ParseRef`; parsing is case-insensitive). Humans,
  MCP tools, GitHub comments and logs use refs; APIs accept a ref or a UUID.
- Agent branches are `agent/<lowercase ref>[-<slug>]`, e.g. `agent/task-12`
  or `agent/goal-3-spec`. Workers push only branches starting with `agent/`.
- Tokens are random 32 bytes, base64url without padding, with a prefix:
  `fcw_` worker tokens, `fcm_` MCP/API tokens. Only their SHA-256 is stored;
  they are shown once. Compare hashes, not tokens.

## Time

- All times are UTC: `time.Now().UTC()`, `timestamptz` columns, RFC 3339 in
  JSON. Durations in JSON are Go duration strings (`"45m0s"`,
  `domain.Duration`).

## Labels and names

- Worker labels and job `needs` are compared case-insensitively: normalize
  with `domain.NormalizeLabels` (trim, lowercase, sort, dedupe) before storing
  or matching, and match with `domain.MatchLabels`.
- Runtime labels are `runtime:<adapter>` (`domain.RuntimeLabel`), OS labels
  are Go `GOOS` values (`linux`, `darwin`, `windows`); a macOS worker advertises both `darwin` and `macos`.
- Repos are `owner/name`, compared case-insensitively; signal keys use the
  lowercased repo.
- GitHub logins (allowlist, comment authors) are compared case-insensitively.

## Signals

Signals are facts the workflow engine waits on (`domain.Signal`). Each has a
correlation `key` (what a waiting step listens for) and a unique semantic
`dedupe_key`, so the same fact from a webhook and from a poll is stored once.

| Kind | Payload fields | Key | Dedupe key |
|------|----------------|-----|------------|
| `job.completed` | `job_id`, `status` (succeeded \| failed \| canceled) | `job:<job_id>` | `job.completed:<job_id>` |
| `gate.resolved` | `gate_id`, `decision` (approved \| changes_requested \| rejected), `feedback` | `gate:<gate_id>` | `gate.resolved:<gate_id>` |
| `pr.merged` | `repo`, `number`, `sha` (merge commit) | `pr:<repo>#<number>` | `pr.merged:<repo>#<number>` |
| `pr.closed` | `repo`, `number` | `pr:<repo>#<number>` | `pr.closed:<repo>#<number>` |
| `checks.completed` | `repo`, `number`, `sha`, `conclusion` (success \| failure \| none) | `checks:<repo>@<sha>` | `checks.completed:<repo>@<sha>` |

In `key` and `dedupe_key`, `<repo>` and `<sha>` are always lowercased
(`domain.PRKey`, `domain.ChecksKey`), e.g. `checks:owner/repo@4f1c2b9e`, even
when GitHub reports them in mixed case. Never build keys by hand; use the
`domain` helpers so webhooks, polls and the API produce the same strings.

The first `checks.completed` for a SHA is final for that SHA; a new push is a
new SHA. Build signals with `domain.NewSignal`, which validates the payload
and computes both keys.

## Gate decisions

Humans resolve a pending gate with one of three decisions, through the web
UI, `/api/v1`, MCP or `@forecrew` on GitHub. The MCP tools (`mcp.ToolService`)
are:

| Tool | Decision | Gate kinds | Notes |
|------|----------|------------|-------|
| `approve` | `approved` | all | On `needs_you`: retry. |
| `request_changes` | `changes_requested` | `profile_approval`, `spec_approval`, `merge_approval` | `feedback` required. On `needs_you` it fails with `mcp.ErrInvalidInput`. |
| `reject` | `rejected` | all | On `needs_you`: stop. |

`mcp.GateDecision` implements these rules (on top of
`domain.GateKind.AllowsDecision`); every entry point uses it. A resolved gate
is final and publishes `gate.resolved`; a new review round opens a new gate.

The 16 MCP tools are listed by `mcp.Tools()`: `onboard_project`,
`create_goal`, `job_status`, `job_logs`, `list_workers`,
`list_pending_gates`, `approve`, `request_changes`, `reject`,
`dispatch_job`, `list_projects`, `get_project`, `list_goals`, `get_goal`,
`list_jobs`, `cancel_job`.

## Canceling jobs

`cancel_job` takes a job ref or UUID and calls `jobs.Service.Cancel` as the
caller: only a queued or running job can be canceled, any other state fails
with `domain.ErrInvalidTransition` (`conflict`, 409). The job becomes
`canceled`, `job.canceled` is audited and `job.completed` is published. The
worker running it learns on its next heartbeat (`action: cancel`, at most 30 s
later), kills the agent, removes the checkout and reports nothing; a late
`complete` or `events` call gets `409 lease_lost`. The playbook step waiting
for the job fails with `JOB-n canceled`, which follows the step's `on_failure`
(a `needs_you` gate when it escalates, otherwise the run fails), and the
`finally` steps run when the run ends.

## Tests

- Table-driven tests; `httptest` for HTTP; fakes from `internal/testkit`
  (extend them, never fork them); no network in unit tests.
- Store tests run against real Postgres behind the `integration` build tag,
  reading `FORECREW_TEST_DATABASE_URL` (`make test-integration`).
- Every package under `internal/` keeps at least 80% statement coverage
  (`make cover`). `cmd/*` is excluded, so `main` only calls a tested `run`.
