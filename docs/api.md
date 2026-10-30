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
| 404 | `not_found` | Unknown project, goal, job or gate, or one of another account; unknown endpoint. |
| 409 | `conflict` | The state does not allow it, for example a gate that is already decided or a job that is not queued or running (`cancel`). |
| 413 | `too_large` | Body over 1 MiB. |
| 415 | `unsupported_media_type` | A cookie session sent a state-changing request that is not `application/json`. |
| 500 | `internal` | Server error; the cause is only in the server log. |

## Endpoints

Lists are JSON arrays (`[]` when empty). Field names are those of the
structs in `internal/domain` and `internal/mcp`.

| Method and path | Tool | Request | Response |
|-----------------|------|---------|----------|
| `POST /api/v1/projects` | `onboard_project` | `{"repo": "owner/name", "name": "optional", "default_branch": "optional, default main"}` | `202` `{"project": {...}, "run_id": "uuid"}` |
| `GET /api/v1/projects` | `list_projects` | | `200` projects |
| `GET /api/v1/projects/{project}` | `get_project` | `{project}` is the name or `owner/name` | `200` project with status and approved `profile` |
| `POST /api/v1/goals` | `create_goal` | `{"project", "title", "body", "platforms": ["web"]}` | `201` goal (draft) |
| `GET /api/v1/goals?project=&status=` | `list_goals` | both filters optional | `200` goals |
| `GET /api/v1/goals/{goal}` | `get_goal` | | `200` goal detail, see below |
| `POST /api/v1/jobs` | `dispatch_job` | `{"project", "role", "base_ref", "branch", "needs": [], "include_repo_facts", "goal", "task", "feedback", "prompt"}`; `project` and `role` required, the role must be read-only | `201` job (queued) |
| `GET /api/v1/jobs?project=&goal=&status=&limit=` | `list_jobs` | all optional; `limit` defaults to 50, at most 500 | `200` jobs, newest first |
| `GET /api/v1/jobs/{job}` | `job_status` | | `200` job with `ref`, `reason` (why a queued job waits, why a failed job failed) and `worker_name` |
| `GET /api/v1/jobs/{job}/logs?after_seq=&limit=` | `job_logs` | `limit` defaults to 200, at most 1000 | `200` `{"events": [...], "next_seq": 12}`; pass `next_seq` as `after_seq` to continue |
| `POST /api/v1/jobs/{job}/cancel` | `cancel_job` | body optional | `200` job (now `canceled`) with `ref`; `409` when the job is not queued or running |
| `GET /api/v1/workers` | `list_workers` | | `200` workers with `labels`, `version`, `last_seen_at`, `online` and `running_jobs` (refs) |
| `GET /api/v1/gates?project=` | `list_pending_gates` | `project` optional | `200` pending gates with `ref`, `kind`, `project`, `goal_ref`, `task_ref`, `summary`, `url` (GitHub issue or PR) and `created_at` |
| `POST /api/v1/gates/{gate}/approve` | `approve` | `{"feedback": "optional"}` (body optional) | `200` the gate, now `approved` |
| `POST /api/v1/gates/{gate}/request-changes` | `request_changes` | `{"feedback": "required"}` | `200` the gate, now `changes_requested` |
| `POST /api/v1/gates/{gate}/reject` | `reject` | `{"feedback": "optional"}` (body optional) | `200` the gate, now `rejected` |
| `POST /api/v1/session` | | `{"token": "fcm_..."}` | `200` session, sets the cookie |
| `DELETE /api/v1/session` | | | `204`, clears the cookie |
| `GET /api/v1/me` | | | `200` session |

On a `needs_you` gate, `approve` retries the stuck work and `reject` stops
it. Gate decisions follow the rules in
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
