# Worker API

The worker API is how forecrew-worker talks to forecrewd. Execution is pull
based: the worker dials out over HTTPS and forecrewd never connects to a
worker. All paths are under `/worker/v1/`, bodies are JSON
(`Content-Type: application/json`), times are RFC 3339 in UTC, and IDs are
UUID strings.

Implemented by `internal/workerapi` (server, T08) and `internal/worker`
(client, T07).

## Constants

| Name | Value | Go |
|------|-------|----|
| Lease duration | 5 min | `domain.LeaseDuration` |
| Heartbeat interval | 30 s | returned by poll as `heartbeat_interval_seconds` |
| Long-poll wait | up to 30 s | |
| Online window | 60 s | `domain.OnlineWindow` |
| Max attempts per job | 3 (default) | `domain.DefaultMaxAttempts` |

A worker is **online** if it polled or heartbeated in the last 60 s.

## Authentication

Every request carries `Authorization: Bearer <worker token>`. forecrewd hashes
the token (SHA-256), looks it up with `Workers.GetByTokenHash` and rejects
unknown or revoked workers with `401`. The token also identifies the account.
Tokens are printed once by `forecrew worker add` and look like
`fcw_<43 base64url chars>` (see conventions.md).

## Errors

Non-2xx responses have this body:

```json
{"error": {"code": "lease_lost", "message": "job JOB-41 is not leased to this worker"}}
```

| Status | `code` | When |
|--------|--------|------|
| 400 | `bad_request` | Malformed JSON, unknown field values, missing required fields. |
| 401 | `unauthorized` | Missing, unknown or revoked token. |
| 404 | `not_found` | `job_id` is not a job of the worker's account. |
| 409 | `lease_lost` | The job is not running under this worker's lease. |
| 413 | `too_large` | Body over 1 MiB. |
| 500 | `internal` | Server error; the worker retries with backoff. |

The worker retries 5xx and network errors with exponential backoff (1 s up to
30 s). It never retries 4xx except 401 after a config reload.

## POST /worker/v1/poll

Asks for work. Also records the worker as seen (`Workers.Seen`: last_seen_at,
labels, version).

Request:

```json
{
  "labels": ["go", "node", "linux", "runtime:claude-code"],
  "free_slots": 1,
  "version": "v0.1.0"
}
```

| Field | Type | Meaning |
|-------|------|---------|
| `labels` | string[] | Every label of the worker, including the os and runtime labels it adds. Normalized (trimmed, lowercased, deduplicated) by the server. |
| `free_slots` | int ≥ 0 | `max_jobs` minus running jobs. |
| `version` | string | forecrew-worker version. |

Behavior:

- `free_slots == 0`: answer `204` immediately (presence only). A worker with
  no free slot stays online through its heartbeats and does not need to poll.
- Otherwise forecrewd leases at most one job: the oldest queued job of the
  account whose `needs` and `runtime:<runtime>` labels are all among `labels`
  (case-insensitive), `Jobs.LeaseNext`. If none is available it waits up to
  30 s for one and then answers `204`. The worker polls again right away; its
  HTTP client timeout must be at least 45 s.
- After leasing, forecrewd completes the spec with secrets (`env` from the
  runtime's `credential_env`, `repo_token` from a fresh GitHub App
  installation token with contents `write` for writing roles and `read` for
  read-only roles). If that fails it releases the lease (`Jobs.Release`) and
  answers `500`.

Response `200`:

```json
{
  "job": { "...": "job spec, see job-spec.md" },
  "lease_expires_at": "2026-03-01T12:05:00Z",
  "heartbeat_interval_seconds": 30
}
```

Response `204`: no body, no job.

## POST /worker/v1/jobs/{job_id}/heartbeat

Extends the lease of a running job (`Jobs.ExtendLease`, now + 5 min) and
records the worker as seen. Sent every `heartbeat_interval_seconds` while the
job runs.

Request: `{}` (an empty body is accepted).

Response `200`:

```json
{"action": "continue", "lease_expires_at": "2026-03-01T12:05:30Z"}
```

```json
{"action": "cancel"}
```

| `action` | Meaning |
|----------|---------|
| `continue` | Keep working; the lease now ends at `lease_expires_at`. |
| `cancel` | Stop now and discard the workspace without pushing. Sent when the job was canceled, or when the lease is no longer this worker's (expired and requeued, or finished). Do not call `complete`. |

`404` if the job is not in the worker's account.

## POST /worker/v1/jobs/{job_id}/events

Appends normalized events to the job log (`JobEvents.Append`). The worker
batches events and sends them at least every 2 s while there are any.

Request:

```json
{
  "events": [
    {"time": "2026-03-01T12:00:01Z", "type": "status", "text": "cloning"},
    {"time": "2026-03-01T12:00:09Z", "type": "message", "text": "I will add the handler first."},
    {"time": "2026-03-01T12:00:10Z", "type": "tool_call", "data": {"name": "Edit", "input": {"file_path": "main.go"}}},
    {"time": "2026-03-01T12:00:11Z", "type": "tool_result", "data": {"name": "Edit", "output": "ok", "is_error": false}},
    {"time": "2026-03-01T12:04:00Z", "type": "usage", "data": {"input_tokens": 1200, "output_tokens": 300, "cache_read_tokens": 0, "cache_write_tokens": 0, "cost_usd": 0.02}}
  ]
}
```

| Field | Type | Meaning |
|-------|------|---------|
| `events[].time` | time | When it happened on the worker. |
| `events[].type` | string | One of `status`, `log`, `message`, `tool_call`, `tool_result`, `usage`, `error` (`domain.JobEventType`). |
| `events[].text` | string | Text for `status` (phase: `cloning`, `analyzing`, `running`, `checking`, `pushing`), `log`, `message`, `error`. |
| `events[].data` | object | Structured data for `tool_call` `{name, input}`, `tool_result` `{name, output, is_error}`, `usage` (`domain.Usage` fields plus `cost_usd`). |

At most 500 events and 1 MiB per request. Secrets must never appear in events;
the worker redacts the values of `env` and `repo_token` from any text it sends.

Response `204`. `409 lease_lost` when the job is not running under this
worker (the worker stops sending).

## POST /worker/v1/jobs/{job_id}/complete

Reports the end of a job. Sent once, after the last events batch and, for
writing roles, after pushing.

Request:

```json
{
  "status": "succeeded",
  "output": {"summary": "Added login handler"},
  "head_sha": "4f1c2b9e8d...",
  "error": "",
  "usage": {"input_tokens": 52000, "output_tokens": 8100, "cache_read_tokens": 40000, "cache_write_tokens": 3000},
  "cost_usd": 0.84
}
```

| Field | Type | Meaning |
|-------|------|---------|
| `status` | `succeeded` \| `failed` \| `retry` | `succeeded`: the agent finished. `failed`: the agent or a check of the worker failed (bad output, write outside `write_paths`, timeout); not retried. `retry`: the worker could not run the job for an infrastructure reason (clone failed, disk full, shutting down); the attempt does not count as a result. |
| `output` | object | Structured output; required for `succeeded` when the spec has an `output_schema`. |
| `head_sha` | string | Commit pushed to the spec `branch`; empty if nothing was pushed. |
| `error` | string | Required for `failed` and `retry`. |
| `usage` | object | Total `domain.Usage`. |
| `cost_usd` | number | Total cost reported by the runtime. |

Server behavior:

- `succeeded` / `failed`: `Jobs.Complete`. For `succeeded` forecrewd validates
  `output` against the spec's `output_schema`; if it does not match, the job
  is stored as `failed` with error `output does not match schema`.
- `retry`: `Jobs.Release` with `error` as reason: back to `queued` with
  `attempts + 1`, or `failed` when attempts are exhausted.
- When the job reaches a terminal state forecrewd publishes a
  `job.completed` signal (see conventions.md and `domain.JobCompleted`).
  forecrewd, not the worker, opens and updates pull requests.

Response `200`:

```json
{"status": "succeeded"}
```

`status` is the job's resulting state (`succeeded`, `failed` or `queued`).
`409 lease_lost` when the job is not running under this worker, e.g. it
expired, was canceled or was already completed; the result is discarded and
the worker drops it.

## Lease expiry

The lease reaper in forecrewd runs every 30 s and calls
`Jobs.RequeueExpired` per account: each running job whose lease expired goes
back to `queued` with `attempts + 1`, or to `failed` (error
`attempts exhausted: lease expired`) when `attempts` reaches `max_attempts`;
the latter publishes `job.completed`. A worker that comes back after its lease
expired gets `cancel` on its next heartbeat.

## Sequence

```
worker                                   forecrewd
  | POST /worker/v1/poll  ---------------->  |  lease oldest matching job
  | <---------------- 200 {job, lease...}    |
  | POST .../jobs/{id}/events  ----------->  |  every ~2 s
  | POST .../jobs/{id}/heartbeat  -------->  |  every 30 s
  | <---------------- 200 {action}           |
  | (push agent/* branch with repo_token)    |
  | POST .../jobs/{id}/complete  --------->  |  job.completed signal
  | <---------------- 200 {status}           |
```
