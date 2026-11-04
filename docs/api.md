# REST API

forecrewd serves a JSON API under `/api/v1/` on its public URL. It is the
same service as the [MCP tools](mcp.md): one endpoint per tool, with the same
inputs, outputs and rules. The web UI and the `forecrew` CLI use it.
Implemented by `internal/api`.

Bodies are JSON (`Content-Type: application/json`), times are RFC 3339 in
UTC, IDs are UUID strings. Everything is scoped to the account of the token.
Goals, jobs, tasks and gates are named by human ref (`GOAL-3`, `JOB-41`,
`TASK-12`, `GATE-7`) or UUID; projects by name or `owner/name`.

## Authentication

Send an MCP/API token (`fcm_...`, see [mcp.md](mcp.md)) as
`Authorization: Bearer <token>`, or, in a browser, log in once and let the
session cookie carry it.

`POST /api/v1/session` takes `{"token": "fcm_..."}` as `application/json` (else `415`), checks it like any other
call and sets the cookie `forecrew_session` (`HttpOnly`, `Secure`,
`SameSite=Strict`, `Path=/api/`, 30 days). `DELETE /api/v1/session` clears it
and needs no token. `GET /api/v1/me` shows who the caller is. These answers carry
`Cache-Control: no-store`. A cookie request that gets `401` (the token was
revoked) also clears the cookie.

A request with the `Authorization` header never uses the cookie. With only
the cookie, every request that is not `GET` or `HEAD` must be sent as
`Content-Type: application/json` (also a `DELETE` or a `POST` without a body),
else it is refused with `415`. A cross-site form cannot send that type.

## Errors

Non-2xx responses have this body:

```json
{"error": {"code": "not_found", "message": "job JOB-9: not found"}}
```

| Status | `code` | When |
|--------|--------|------|
| 400 | `bad_request` | Malformed JSON, unknown or mistyped fields, bad query parameter, missing `token` on login. |
| 400 | `invalid_input` | The tool refused the input: a field is missing or malformed, a bad ref, a decision the gate does not allow (`request-changes` on a `needs_you` gate, or without `feedback`). |
| 401 | `unauthorized` | Missing, unknown or revoked token. |
| 404 | `not_found` | Unknown project, goal, job, gate or active token, or one of another account; unknown endpoint. |
| 409 | `conflict` | The state does not allow it, for example a gate that is already decided or a job that is not queued or running (`cancel`). |
| 413 | `too_large` | Body over 1 MiB. |
| 415 | `unsupported_media_type` | A cookie session sent a state-changing request that is not `application/json`. |
| 500 | `internal` | Server error; the cause is only in the server log. |
| 503 | `unavailable` | The feature is not configured: downloading an artifact while no `evidence` storage is configured. |

## Endpoints

Lists are JSON arrays (`[]` when empty). Field names are those of the
structs in `internal/domain` and `internal/mcp`.

| Method and path | Tool | Request | Response |
|-----------------|------|---------|----------|
| `POST /api/v1/projects` | `onboard_project` | `{"repo": "owner/name", "name": "optional", "default_branch": "optional, default main"}` | `202` `{"project": {...}, "run_id": "uuid"}` |
| `GET /api/v1/projects` | `list_projects` | | `200` projects |
| `GET /api/v1/projects/{project}` | `get_project` | `{project}` is the name or `owner/name` | `200` project with status and approved `profile` |
| `POST /api/v1/goals` | `create_goal` | `{"project", "title", "body", "platforms": ["web"], "councils": ["product"]}`; `councils` is optional, `[]` means none (no triage) | `201` goal (draft) |
| `GET /api/v1/goals?project=&status=` | `list_goals` | both filters optional | `200` goals |
| `GET /api/v1/goals/{goal}` | `get_goal` | | `200` goal detail, see below |
| `POST /api/v1/jobs` | `dispatch_job` | `{"project", "role", "base_ref", "branch", "needs": [], "pool", "include_repo_facts", "goal", "task", "feedback", "prompt"}`; `project` and `role` required, the role must be read-only; `pool` defaults to the role's pool and its policy must allow the role (`400 invalid_input` otherwise) | `201` job (queued, with its `pool`) |
| `GET /api/v1/jobs?project=&goal=&status=&limit=` | `list_jobs` | all optional; `limit` defaults to 50, at most 500 | `200` jobs, newest first |
| `GET /api/v1/jobs/{job}` | `job_status` | | `200` job with `ref`, `reason` (why a queued job waits, e.g. `no online worker in pool build with labels go, runtime:claude-code`; why a failed job failed) and `worker_name`; for a command job also `tests`, the test runs of its latest attempt added up: `runs`, `status`, `passed`, `failed`, `skipped`, `errored` |
| `GET /api/v1/jobs/{job}/logs?after_seq=&limit=` | `job_logs` | `limit` defaults to 200, at most 1000 | `200` `{"events": [...], "next_seq": 12}`; pass `next_seq` as `after_seq` to continue |
| `POST /api/v1/jobs/{job}/cancel` | `cancel_job` | body optional | `200` job (now `canceled`) with `ref`; `409` when the job is not queued or running |
| `GET /api/v1/jobs/{job}/artifacts` | `list_artifacts` | | `200` artifacts of the job by name: `id`, `name`, `content_type`, `size`, `sha256`, `created_at` |
| `GET /api/v1/test-runs?job=&project=&change=&sha=&limit=` | `list_test_runs` | at least one of `job`, `change` (a PR number, needs `project`) and `sha`; `limit` defaults to 100, at most 500 | `200` test runs of command jobs, oldest first: `id`, `job_id`, `job_ref`, `change_id`, `sha`, `suite`, `environment`, `cell`, `status`, `counts`, `duration_ms`, `exit_code`, `versions`, `report_artifacts`, `attempt`, `created_at` and `failures` (the cases that failed or errored, at most 100 per run: `name`, `classname`, `status`, `message`, written by the repo's tests and so untrusted data) and `failures_truncated` |
| `GET /api/v1/artifacts/{id}` | | `{id}` is an artifact UUID | `200` the file, streamed; see [Artifact downloads](#artifact-downloads). `404` when it is unknown, of another account, or deleted by retention; `503` without evidence storage |
| `GET /api/v1/workers` | `list_workers` | | `200` workers with `pool`, `labels`, `version`, `last_seen_at`, `online` and `running_jobs` (refs) |
| `GET /api/v1/gates?project=` | `list_pending_gates` | `project` optional | `200` pending gates with `ref`, `kind`, `project`, `goal_ref`, `task_ref`, `summary`, `url` (GitHub issue or PR) and `created_at` |
| `POST /api/v1/gates/{gate}/approve` | `approve` | `{"feedback": "optional"}` (body optional) | `200` the gate, now `approved` |
| `POST /api/v1/gates/{gate}/request-changes` | `request_changes` | `{"feedback": "required"}` | `200` the gate, now `changes_requested` |
| `POST /api/v1/gates/{gate}/reject` | `reject` | `{"feedback": "optional"}` (body optional) | `200` the gate, now `rejected` |
| `GET /api/v1/tokens` | `list_tokens` | | `200` tokens with `id`, `name`, `created_at`, `last_used_at`, `revoked_at` and `current` (the token of this request); no hash or secret |
| `POST /api/v1/tokens/{token}/revoke` | `revoke_token` | `{token}` is a name or ID; body optional | `200` `{"token": {...}, "self": false, "message": "..."}`; `404` when no active token has that name or ID. `self` is true when the caller revoked the token it used: the next request, with the header or the session cookie, gets `401` |
| `POST /api/v1/session` | | `{"token": "fcm_..."}` | `200` session, sets the cookie |
| `DELETE /api/v1/session` | | | `204`, clears the cookie |
| `GET /api/v1/me` | | | `200` session |

### Artifact downloads

Artifacts are files jobs produced (test reports, logs, screenshots,
verification reports); workers upload them through the worker API and
forecrewd keeps them in S3-compatible storage (`evidence` in
[config.md](contracts/config.md)). `GET /api/v1/artifacts/{id}` streams the
file with `Content-Length` and these headers:

- `Content-Type` and `Content-Disposition: inline` only for `text/plain`
  (served as UTF-8), `image/png`, `image/jpeg` and `application/json`.
  Everything else, HTML and SVG included, is sent as
  `Content-Disposition: attachment; filename=<last path segment>` with
  `Content-Type: application/octet-stream`, so an uploaded page can never run
  on forecrewd's origin.
- `X-Content-Type-Options: nosniff`, `Content-Security-Policy: sandbox;
  default-src 'none'` and `Cache-Control: private, no-cache`.

If the storage fails halfway, the connection is cut: a short body is never a
complete download.

Retention: a daily sweep deletes artifacts older than `evidence.retention_days`
(`0` keeps them forever) from storage and the database, and records each
deletion in the audit log (`artifact.deleted` by `system:forecrewd`). A
verification report that points at a deleted artifact keeps its data and loses
only the link to the rendered copy (`artifact_id` becomes null).

The same daily pass removes orphans, with or without retention: objects under
`jobs/` that no artifact row points at (left by forecrewd crashing between
storing an object and writing its row, or by a failed delete of a replaced
object) and that are older than 24 hours, so an upload in flight is never
touched. Each is recorded as `artifact.orphan_deleted` (account from the key;
data: `key`, `size`, `last_modified`). A pass looks at up to 20000 objects
and continues where it stopped on the next one. Objects outside `jobs/` are
never touched.

### Registers

| Method and path | Tool | Request | Response |
|-----------------|------|---------|----------|
| `GET /api/v1/assumptions?project=&goal=&task=&status=&impact=&verify_by=` | `list_assumptions` | `project` required | `200` assumptions, by number |
| `POST /api/v1/assumptions/{assumption}/resolve` | `resolve_assumption` | `{"status", "evidence"}` | `200` assumption |
| `GET /api/v1/decisions?project=&goal=&council=` | `list_decisions` | `project` required | `200` decisions, by number |

Assumptions and decisions are named by ref (`A-42`, `DEC-9`) or UUID; see
[mcp.md](mcp.md#assumption-and-decision-registers) for what they hold.
`GET /api/v1/goals/{goal}` has the goal's assumptions as `assumptions`.

### Planned endpoints

Contract only (T31), with the planned MCP tools of [mcp.md](mcp.md#planned-tools),
same inputs and outputs; each is served when its tool is built, together with
the CLI command listed there.

| Method and path | Tool | Request | Response |
|-----------------|------|---------|----------|
| `POST /api/v1/councils` | `start_council` | `{"project", "question", "kind", "roles": []}` | `202` council |
| `GET /api/v1/councils/{council}` | `get_council` | | `200` council with brief, contributions, gate, decision |
| `POST /api/v1/councils/{council}/constraints` | | `{"constraint": "..."}` | `200` council; the next round's prompts include it (T33) |
| `GET /api/v1/knowledge?project=&q=&section=&kind=&provider=` | `search_knowledge` | `project` and `q` required | `200` results |
| `POST /api/v1/knowledge/proposals` | `propose_knowledge` | `{"project", "section", "path", "body", "reason", "action"}` | `201` knowledge proposal |
| `POST /api/v1/bugs` | `report_bug` | `{"project", "title", "body", "platforms": []}` | `201` goal of kind `bug` |
| `GET /api/v1/proposals?project=&status=&category=` | `list_proposals` | `project` required | `200` proposals |
| `POST /api/v1/proposals/{proposal}/decide` | `decide_proposal` | `{"decision", "reason", "until"}` | `200` proposal |

Councils, knowledge proposals and proposals are named by ref
(`COUNCIL-2`, `KB-5`, `PROP-3`) or UUID.

On a `needs_you` gate, `approve` retries the stuck work and `reject` stops
it. A gate with `"origin": "ask_human"` is instead a question a running job
asked through the job MCP gateway ([contracts/job-mcp.md](contracts/job-mcp.md)):
`approve` with the answer as `feedback` answers it, `reject` declines, and
the job goes on either way. Gate decisions follow the rules in
[conventions.md](contracts/conventions.md#gate-decisions).

The session, returned by login and `me`:

```json
{"token": "laptop", "account": {"id": "uuid", "name": "default", "created_at": "..."}}
```

`GET /api/v1/goals/{goal}` returns:

```json
{
  "goal": {"id": "uuid", "number": 3, "title": "...", "status": "in_progress", "platforms": ["web"], "...": "..."},
  "ref": "GOAL-3",
  "project": "app",
  "url": "https://github.com/owner/app/issues/5",
  "spec": {"revision": 2, "body": "..."},
  "tasks": [{
    "ref": "TASK-12", "title": "...", "status": "in_review", "url": "https://github.com/owner/app/issues/6",
    "needs": ["GATE-9"],
    "changes": [{"number": 9, "url": "https://github.com/owner/app/pull/9", "state": "open", "checks": "failure"}],
    "iterations": 2, "cost_usd": 0.75, "...": "..."
  }],
  "gates": [{"ref": "GATE-9", "kind": "merge_approval", "status": "pending", "task_ref": "TASK-12", "url": "...", "...": "..."}],
  "assumptions": [{"ref": "A-4", "statement": "...", "impact": "high", "status": "unverified", "source": "JOB-41", "...": "..."}],
  "cost_usd": 1.75
}
```

- `spec` is the latest revision and is absent before the first one.
- `needs` lists the pending gates waiting for a human on the task.
- `checks` is the aggregate check status of an open PR's head commit:
  `failure` wins over `pending`, which wins over `success`; `none` when there
  are no checks; `unknown` when GitHub could not be read. It is absent for
  merged or closed PRs.
- `iterations` is the number of jobs run for the task and `cost_usd` their cost;
  the goal's `cost_usd` adds up every job of the goal.
- `gates` holds every gate of the goal with its decision (`status`,
  `resolved_by`, `feedback`).
