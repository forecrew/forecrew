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
`fcw_<43 base64url chars>` (see conventions.md). The worker on a machine gets
its token from `POST /worker/v1/bootstrap` instead.

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
| 411 | `length_required` | Artifact upload without a `Content-Length` (a chunked body). |
| 413 | `too_large` | Body over 1 MiB; for `complete`, over `max_test_results_size`; for an artifact upload, over `evidence.max_artifact_size`. |
| 500 | `internal` | Server error; the worker retries with backoff. |
| 503 | `unavailable` | Artifact upload while no `evidence` storage is configured. The worker completes the job without the artifact and says so in a `log` event. |

The worker retries 5xx and network errors with exponential backoff (1 s up to
30 s). It never retries 4xx except 401 after a config reload.

## POST /worker/v1/bootstrap

The first call of the worker on a machine (fleet.md, T58), the only one with
another credential: `Authorization: Bearer <bootstrap token>`, a one-time
`fcb_` token that reached the guest through its provider. Request:
`{"version": "v0.1.0"}`.

What the call gets depends on the token and its machine:

| Machine | Token | Answer |
|---------|-------|--------|
| `requested`, or `booting` and unassigned | unused | `202`, machine stays in its state |
| `booting`, assigned | usable | `200` |
| `booting`, assigned | expired | `401` |
| `ready` | used | `401`: a token works once |
| `busy`, `draining`, `destroyed`, `failed` | any | `401` |

Response `200`, once, only from `booting`: forecrewd marks the token used,
creates the worker with `ephemeral` true and a new token, links it to the machine
and moves the machine to `ready`, all in one transaction. The worker keeps the token in memory and goes on as any worker;
`labels` are its configured labels, on top of which it adds its own. A worker
that restarts cannot come back, and its machine is replaced.

```json
{"worker_token": "fcw_...", "name": "machine-7c1e9a0b42d6", "labels": ["amd64", "docker", "linux"], "max_jobs": 1}
```

Response `202`: wait. Either the machine has no organization yet, or the guest
was faster than its provider's report. The token stays unused and the worker asks
again after `retry_after_seconds`. This path never moves a machine out of
`requested` or `booting`: it only sets `ready_at` on a `booting` machine, once
(fleet.md, Warm).

```json
{"retry_after_seconds": 5}
```

`401 unauthorized`: an unknown, used or expired token, or a machine that can no
longer take a worker, as the table says. The worker exits and the machine is
replaced. Neither the token nor the answer is ever logged.

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
  account **in the worker's pool** whose `needs` (plus `runtime:<runtime>`
  for agent jobs) are all among `labels` (case-insensitive),
  `Jobs.LeaseNext`. If none is available it waits up to
  30 s for one and then answers `204`. The worker polls again right away; its
  HTTP client timeout must be at least 45 s.
- After leasing, forecrewd completes the spec with secrets (`env` from the
  runtime's `credential_env`, `repo_token` from a fresh GitHub App
  installation token with contents `write` for writing roles and `read` for
  read-only roles), within what the job's pool allows (config.md `pools`,
  `credentials`). Command jobs get no `env` and a `read` token. From T47,
  agent jobs also get `job_mcp` (job-mcp.md): the gateway's public URL, for
  information (the worker uses `<server_url>/job-mcp`), and a token bound to
  this lease. If that fails it releases the lease
  (`Jobs.Release`) and answers `500`.

### Pools

Every worker belongs to exactly one pool (`workers.pool`, default `build`),
set by an admin when the worker is created (`forecrew worker add --pool`)
and changed only by an admin. It is **never** taken from `poll`: nothing a
worker sends, labels included (a `pool:control` label is just a label),
changes which pool it leases from. A worker only ever leases jobs whose
`jobs.pool` equals its pool; labels route jobs inside a pool. A job's pool
is fixed when it is created: from its role pack's `pool` (roles.md) for
agent jobs, and from its environment for command jobs (provider `actions`
→ `actions`, otherwise `build`). forecrewd refuses to create a job whose
role or kind its pool's policy does not allow. Pool `actions` has no
workers: forecrewd itself runs its jobs on GitHub-hosted runners. The worker
on a machine gets its shape's pool (fleet.md).

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

Request: `{}` (an empty body is accepted). From T47 it may carry
`attach_received`, below.

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
| `cancel` | Stop now and discard the workspace without pushing. Sent when the job was canceled or finished, or when its lease expired and it is queued again. Do not call `complete`. |

From T47, a `continue` answer may also carry `attach`: the files the agent
marked with the job MCP tool `evidence_attach` (job-mcp.md) that the worker
has not acknowledged, as `[{"path": "screenshots/home.png", "name":
"evidence/screenshots/home.png"}]`. A later mark of a name replaces the
earlier one. The worker acknowledges what it holds by sending the same
entries as `attach_received` in its next heartbeat; until then they are sent
again, so a lost answer loses nothing, and the worker keeps one entry per
name. It uploads each through `PUT .../artifacts/{name}` with the rules of
`command.artifacts` (inside the work tree or the scratch directory, no symlinks), and sends one more
heartbeat after the runtime exits to collect the last ones before `complete`.
`complete` and a `cancel` answer drop what is left.

`404` if the job is not in the worker's account. `409 lease_lost` if the job
now runs under another worker's lease (it expired and was leased again); the
worker stops as for `cancel`.

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
the worker redacts the values of `env`, `repo_token` and `job_mcp.token` from any text it sends.

Response `204`. `409 lease_lost` when the job is not running under this
worker (the worker stops sending).

## PUT /worker/v1/jobs/{job_id}/artifacts/{name}

Uploads one artifact of a running job: a JUnit report, a log, a screenshot,
coverage output. `{name}` is the rest of the path, a relative path such as
`reports/unit.xml` (`domain.ValidArtifactName`: at most 255 bytes, segments
of letters, digits, `.`, `_` and `-` not starting with `.`); the worker
uses the file's path relative to the repo root. The body is the raw file
(not JSON), with its `Content-Type` (`application/octet-stream` when
unknown) and `Content-Length`.

- Only while the worker holds the job's lease, like `heartbeat`: otherwise
  `409 lease_lost` (or `404` outside the account).
- The request must carry `Content-Length`: without it, `411 length_required`
  and nothing is read.
- The body may be at most `evidence.max_artifact_size` bytes (config.md):
  above it, `413 too_large` and nothing is stored. forecrewd streams the
  body to evidence storage and computes its SHA-256 while streaming.
- Uploading the same name again replaces the artifact (a retry).
- Workers never receive storage credentials; this endpoint is their only
  way to store evidence.

Response `201`:

```json
{"id": "01960d3e-8b20-7c1a-9f3e-2a7c4b1d9e12", "name": "reports/unit.xml", "size": 48213, "sha256": "9f86d08..."}
```

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
| `head_sha` | string | Head of the spec `branch` on the remote after the run (the commit pushed, or the existing head when nothing new was pushed); empty if there is no branch. For command jobs: the commit checked out and tested (`branch`, or `base_ref` when there is none), before any `overlay`.
| `error` | string | Required for `failed` and `retry`. |
| `usage` | object | Total `domain.Usage`. |
| `cost_usd` | number | Total cost reported by the runtime. |
| `test_runs` | array | Command jobs only, optional: one entry per suite and cell the job ran, stored in `test_runs` and `test_results` (data-model.md). |
| `test_runs[].suite` | string | Suite name (the spec's `suite`). |
| `test_runs[].status` | `passed` \| `failed` \| `errored` | `passed`: every test passed or was skipped and the command exited 0. `failed`: a test failed or errored, or the command exited non-zero although the reports show no failure (the run then holds an errored `(command)` result saying so). `errored`: no usable result (no report, a report that cannot be parsed or holds no test case, the command could not start). |
| `test_runs[].counts` | object | `passed`, `failed`, `skipped`, `errored` (integers, `domain.TestCounts`). |
| `test_runs[].duration_ms` | integer | Wall time of the command. |
| `test_runs[].results` | array | One per test case: `name`, `classname`, `status` (`passed`, `failed`, `skipped`, `errored`), `duration_ms`, `message` (failure, error or skip text, may be empty). |
| `test_runs[].report_artifacts` | string[] | Names of the uploaded artifacts holding the JUnit reports. |
| `test_runs[].versions` | object string → string | Tool → output of the environment's `versions` command. |
| `test_runs[].exit_code` | integer | Exit code of the command; `-1` when it did not exit by itself (killed on timeout or cancel, or never started). |

The body of `complete` may be up to `max_test_results_size` (config.md); no
test case is ever left out. A worker whose results are refused with `413`
completes again with each run `errored` and one `(report)` result saying the
results were too large.

A command job's `complete` example:

```json
{
  "status": "succeeded",
  "test_runs": [{
    "suite": "unit",
    "status": "failed",
    "counts": {"passed": 41, "failed": 1, "skipped": 2, "errored": 0},
    "duration_ms": 48211,
    "results": [
      {"name": "TestLogin", "classname": "app/internal/auth", "status": "passed", "duration_ms": 12, "message": ""},
      {"name": "TestLogout", "classname": "app/internal/auth", "status": "failed", "duration_ms": 30, "message": "logout_test.go:21: want 204, got 500"}
    ],
    "report_artifacts": ["reports/unit.xml"],
    "versions": {"go": "go version go1.27.1 linux/amd64"},
    "exit_code": 1
  }]
}
```

forecrewd stores each entry with the job's project, the SHA tested (the
job's `head_sha`), the
environment and cell of the spec, the job's attempt, and the change whose
head is that SHA when there is one. `test_runs` on an agent job is a `400`.

Server behavior:

- `succeeded` / `failed`: `Jobs.Complete`. For `succeeded` forecrewd validates
  `output` against the spec's `output_schema`; if it does not match, the job
  is stored as `failed` with error `output does not match schema`.
- `retry`: `Jobs.Release` with `error` as reason: back to `queued` with
  `attempts + 1` and held back by a backoff (`run_after`: 30 s doubling per
  attempt, at most 10 min, see `domain.RetryBackoff`), or `failed` when
  attempts are exhausted.
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
expired gets `cancel` on its next heartbeat, or `409 lease_lost` if another
worker has leased the job since.

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
