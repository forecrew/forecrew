# Data model

Postgres 18 schema created by T02 (`internal/store`). Each table maps to a
type in `internal/domain` and a repository in `store.Store`; JSON names in
`domain` equal the column names. The DDL below is the contract; T02 may add
indexes but not change columns, types or constraints without a contract
change.

## Rules

- IDs are UUIDv7, generated in Go (`domain.NewID`) or by the column default
  `uuidv7()` (built into Postgres 18).
- Every project-scoped table has `account_id uuid NOT NULL REFERENCES
  accounts(id)`, and every repository call filters on it.
- Timestamps are `timestamptz`, written in UTC. `created_at` and `updated_at`
  default to `now()`; the store sets `updated_at = now()` on every update.
- Optional references and optional times are `NULL` when unset; Go uses the
  zero UUID / zero `time.Time` for them.
- Status and kind columns are `text` with a `CHECK` listing the values of the
  matching `domain` enum.
- Repos (`owner/name`) are stored as given and compared with `lower()`.
  Labels (`jobs.needs`, `workers.labels`) are stored lowercased and sorted
  (`domain.NormalizeLabels`).
- Human refs: entities with a `number` get it from `ref_counters` in the same
  transaction as the insert, per account and kind, starting at 1.
- Foreign keys use the default `ON DELETE NO ACTION`; nothing is deleted in
  the MVP except expired `webhook_deliveries`.
- `audit_log` is append-only: triggers reject `UPDATE`, `DELETE` and `TRUNCATE`.

## Tables

```sql
CREATE TABLE accounts (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  name       text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- Next human ref number per account and kind (GOAL, SPEC, TASK, JOB, GATE).
CREATE TABLE ref_counters (
  account_id uuid NOT NULL REFERENCES accounts(id),
  kind       text NOT NULL CHECK (kind IN ('GOAL','SPEC','TASK','JOB','GATE')),
  last       bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (account_id, kind)
);
-- Allocation (returns 1 for the first row of a kind):
--   INSERT INTO ref_counters (account_id, kind, last) VALUES ($1, $2, 1)
--   ON CONFLICT (account_id, kind) DO UPDATE SET last = ref_counters.last + 1
--   RETURNING last

CREATE TABLE projects (
  id             uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id     uuid NOT NULL REFERENCES accounts(id),
  name           text NOT NULL,
  repo           text NOT NULL,
  default_branch text NOT NULL DEFAULT 'main',
  status         text NOT NULL CHECK (status IN ('registered','onboarding','onboarded')),
  profile        jsonb,                      -- approved .forecrew/profile.yaml as JSON
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, name)
);
CREATE UNIQUE INDEX projects_account_repo ON projects (account_id, lower(repo));
CREATE INDEX projects_repo ON projects (lower(repo));          -- ListByRepo (webhooks)

CREATE TABLE goals (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id   uuid NOT NULL REFERENCES accounts(id),
  project_id   uuid NOT NULL REFERENCES projects(id),
  number       bigint NOT NULL,
  title        text NOT NULL,
  body         text NOT NULL DEFAULT '',
  platforms    text[] NOT NULL DEFAULT '{}',   -- target platforms (worker labels), normalized
  status       text NOT NULL CHECK (status IN ('draft','spec_review','planned','in_progress','done','canceled')),
  issue_number integer,
  created_by   text NOT NULL,              -- domain.Actor.String()
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, number)
);
CREATE INDEX goals_project_status ON goals (account_id, project_id, status);

CREATE TABLE jobs (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id       uuid NOT NULL REFERENCES accounts(id),
  project_id       uuid NOT NULL REFERENCES projects(id),
  number           bigint NOT NULL,
  status           text NOT NULL CHECK (status IN ('queued','running','succeeded','failed','canceled')),
  role             text NOT NULL,
  runtime          text NOT NULL,          -- adapter: claude-code | codex
  needs            text[] NOT NULL DEFAULT '{}',
  goal_id          uuid REFERENCES goals(id),
  spec_id          uuid,                   -- FK added after specs
  task_id          uuid,                   -- FK added after tasks
  spec             jsonb NOT NULL,         -- domain.JobSpec without env and repo_token
  attempts         integer NOT NULL DEFAULT 0,
  max_attempts     integer NOT NULL DEFAULT 3 CHECK (max_attempts >= 1),
  run_after        timestamptz,            -- retry backoff: not leased before this (NULL = now)
  worker_id        uuid,                   -- FK added after workers; set while running
  lease_expires_at timestamptz,            -- set while running
  started_at       timestamptz,
  finished_at      timestamptz,
  output           jsonb,
  head_sha         text,
  error            text,
  usage            jsonb NOT NULL DEFAULT '{}',   -- domain.Usage
  cost_usd         numeric(12,6) NOT NULL DEFAULT 0,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, number),
  CHECK ((status = 'running') = (worker_id IS NOT NULL AND lease_expires_at IS NOT NULL))
);
CREATE INDEX jobs_queued   ON jobs (account_id, created_at, id) WHERE status = 'queued';
CREATE INDEX jobs_leases   ON jobs (account_id, lease_expires_at) WHERE status = 'running';
CREATE INDEX jobs_project  ON jobs (account_id, project_id, created_at DESC);
CREATE INDEX jobs_task     ON jobs (account_id, task_id) WHERE task_id IS NOT NULL;
CREATE INDEX jobs_goal     ON jobs (account_id, goal_id) WHERE goal_id IS NOT NULL;
CREATE INDEX jobs_worker   ON jobs (worker_id) WHERE status = 'running';

CREATE TABLE specs (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id uuid NOT NULL REFERENCES accounts(id),
  project_id uuid NOT NULL REFERENCES projects(id),
  goal_id    uuid NOT NULL REFERENCES goals(id),
  number     bigint NOT NULL,
  revision   integer NOT NULL CHECK (revision >= 1),
  body       text NOT NULL,
  job_id     uuid REFERENCES jobs(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, number),
  UNIQUE (goal_id, revision)
);

CREATE TABLE tasks (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id   uuid NOT NULL REFERENCES accounts(id),
  project_id   uuid NOT NULL REFERENCES projects(id),
  goal_id      uuid NOT NULL REFERENCES goals(id),
  spec_id      uuid NOT NULL REFERENCES specs(id),
  number       bigint NOT NULL,
  seq          integer NOT NULL,
  title        text NOT NULL,
  body         text NOT NULL DEFAULT '',
  status       text NOT NULL CHECK (status IN ('open','in_progress','in_review','merge_ready','merged','closed')),
  issue_number integer,
  branch       text,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, number)
);
CREATE INDEX tasks_goal ON tasks (account_id, goal_id, seq);
CREATE UNIQUE INDEX tasks_issue ON tasks (account_id, project_id, issue_number) WHERE issue_number IS NOT NULL;

ALTER TABLE jobs ADD FOREIGN KEY (spec_id) REFERENCES specs(id);
ALTER TABLE jobs ADD FOREIGN KEY (task_id) REFERENCES tasks(id);

CREATE TABLE changes (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id uuid NOT NULL REFERENCES accounts(id),
  project_id uuid NOT NULL REFERENCES projects(id),
  task_id    uuid REFERENCES tasks(id),
  repo       text NOT NULL,
  number     integer NOT NULL,             -- PR number
  branch     text NOT NULL,
  base_ref   text NOT NULL,
  head_sha   text NOT NULL,
  state      text NOT NULL CHECK (state IN ('open','merged','closed')),
  url        text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
-- Upsert key: INSERT ... ON CONFLICT (account_id, lower(repo), number) DO UPDATE
CREATE UNIQUE INDEX changes_pr ON changes (account_id, lower(repo), number);
CREATE INDEX changes_task  ON changes (account_id, task_id) WHERE task_id IS NOT NULL;
CREATE INDEX changes_state ON changes (account_id, state);

CREATE TABLE workers (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id   uuid NOT NULL REFERENCES accounts(id),
  name         text NOT NULL,
  token_hash   bytea NOT NULL UNIQUE,       -- SHA-256 of the worker token
  labels       text[] NOT NULL DEFAULT '{}',
  version      text NOT NULL DEFAULT '',
  max_jobs     integer NOT NULL DEFAULT 1 CHECK (max_jobs >= 1),
  last_seen_at timestamptz,
  revoked_at   timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, name)
);

ALTER TABLE jobs ADD FOREIGN KEY (worker_id) REFERENCES workers(id);

CREATE TABLE job_events (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id uuid NOT NULL REFERENCES accounts(id),
  job_id     uuid NOT NULL REFERENCES jobs(id),
  seq        bigint NOT NULL,               -- 1, 2, ... per job
  time       timestamptz NOT NULL,
  type       text NOT NULL CHECK (type IN ('status','log','message','tool_call','tool_result','usage','error')),
  text       text NOT NULL DEFAULT '',
  data       jsonb,
  UNIQUE (job_id, seq)
);
-- Append takes SELECT ... FOR UPDATE on the job row to serialize seq.

CREATE TABLE gates (
  id             uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id     uuid NOT NULL REFERENCES accounts(id),
  project_id     uuid NOT NULL REFERENCES projects(id),
  number         bigint NOT NULL,
  kind           text NOT NULL CHECK (kind IN ('profile_approval','spec_approval','merge_approval','needs_you')),
  status         text NOT NULL CHECK (status IN ('pending','approved','changes_requested','rejected')),
  title          text NOT NULL,
  summary        text NOT NULL DEFAULT '',
  subject_repo   text NOT NULL,
  subject_number integer,                   -- issue or PR number
  goal_id        uuid REFERENCES goals(id),
  spec_id        uuid REFERENCES specs(id),
  task_id        uuid REFERENCES tasks(id),
  job_id         uuid REFERENCES jobs(id),
  resolved_by    text,                      -- domain.Actor.String()
  feedback       text,
  resolved_at    timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, number),
  CHECK ((status = 'pending') = (resolved_at IS NULL))
);
CREATE INDEX gates_pending ON gates (account_id, project_id) WHERE status = 'pending';
CREATE INDEX gates_subject ON gates (account_id, lower(subject_repo), subject_number) WHERE status = 'pending';

CREATE TABLE signals (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id uuid NOT NULL REFERENCES accounts(id),
  project_id uuid NOT NULL REFERENCES projects(id),
  kind       text NOT NULL CHECK (kind IN ('job.completed','gate.resolved','pr.merged','pr.closed','checks.completed')),
  key        text NOT NULL,                 -- correlation key, e.g. job:<id>
  dedupe_key text NOT NULL,                 -- e.g. pr.merged:owner/repo#12
  payload    jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, dedupe_key)
);
-- Insert: INSERT ... ON CONFLICT (account_id, dedupe_key) DO NOTHING
CREATE INDEX signals_key ON signals (account_id, key, created_at);

CREATE TABLE workflow_runs (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid NOT NULL REFERENCES accounts(id),
  project_id  uuid NOT NULL REFERENCES projects(id),
  playbook    text NOT NULL,
  status      text NOT NULL CHECK (status IN ('running','waiting','succeeded','failed','canceled')),
  goal_id     uuid REFERENCES goals(id),
  task_id     uuid REFERENCES tasks(id),
  inputs      jsonb NOT NULL DEFAULT '{}',
  state       jsonb NOT NULL DEFAULT '{}',
  error       text,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz
);
CREATE INDEX workflow_runs_active ON workflow_runs (account_id, created_at) WHERE status IN ('running','waiting');
CREATE INDEX workflow_runs_goal   ON workflow_runs (account_id, goal_id) WHERE goal_id IS NOT NULL;

CREATE TABLE step_runs (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid NOT NULL REFERENCES accounts(id),
  run_id      uuid NOT NULL REFERENCES workflow_runs(id),
  step_id     text NOT NULL,                -- step id in the playbook
  attempt     integer NOT NULL DEFAULT 1,
  status      text NOT NULL CHECK (status IN ('pending','running','waiting','succeeded','failed','skipped')),
  wait_key    text,                         -- signal key awaited while waiting
  job_id      uuid REFERENCES jobs(id),
  gate_id     uuid REFERENCES gates(id),
  signal_id   uuid REFERENCES signals(id),
  output      jsonb,
  error       text,
  created_at  timestamptz NOT NULL DEFAULT now(),
  started_at  timestamptz,
  finished_at timestamptz,
  UNIQUE (run_id, step_id, attempt)
);
CREATE INDEX step_runs_waiting ON step_runs (account_id, wait_key) WHERE status = 'waiting';

-- Not account-scoped: a delivery is recorded before its account is known.
-- Rows older than 30 days are deleted.
CREATE TABLE webhook_deliveries (
  delivery_id text PRIMARY KEY,             -- X-GitHub-Delivery
  event       text NOT NULL,                -- X-GitHub-Event
  received_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX webhook_deliveries_received ON webhook_deliveries (received_at);

CREATE TABLE mcp_tokens (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id   uuid NOT NULL REFERENCES accounts(id),
  name         text NOT NULL,
  token_hash   bytea NOT NULL UNIQUE,       -- SHA-256 of the token
  last_used_at timestamptz,
  revoked_at   timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, name)
);

CREATE TABLE audit_log (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id   uuid NOT NULL REFERENCES accounts(id),
  project_id   uuid REFERENCES projects(id),
  actor        text NOT NULL,               -- domain.Actor.String(), e.g. github:octocat
  action       text NOT NULL,               -- <subject>.<verb>, e.g. gate.resolved
  subject_type text NOT NULL,               -- job, gate, goal, task, project, worker, token, ...
  subject_id   uuid,
  data         jsonb,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_account ON audit_log (account_id, created_at DESC);
CREATE INDEX audit_log_subject ON audit_log (account_id, subject_id, created_at DESC) WHERE subject_id IS NOT NULL;

CREATE FUNCTION audit_log_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'audit_log is append-only';
END $$;
CREATE TRIGGER audit_log_append_only BEFORE UPDATE OR DELETE ON audit_log
  FOR EACH STATEMENT EXECUTE FUNCTION audit_log_append_only();
CREATE TRIGGER audit_log_no_truncate BEFORE TRUNCATE ON audit_log
  FOR EACH STATEMENT EXECUTE FUNCTION audit_log_append_only();
```

## Leasing query

`Jobs.LeaseNext(account, worker, labels, leaseFor)`; `labels` is normalized:

```sql
UPDATE jobs SET status = 'running', worker_id = $2, started_at = now(),
       lease_expires_at = now() + $4, updated_at = now()
WHERE id = (
  SELECT id FROM jobs
  WHERE account_id = $1 AND status = 'queued'
    AND (run_after IS NULL OR run_after <= now())
    AND (needs || ARRAY[lower('runtime:' || runtime)]) <@ $3::text[]
    AND EXISTS (SELECT 1 FROM workers w WHERE w.id = $2 AND w.account_id = $1)
  ORDER BY created_at, id
  LIMIT 1
  FOR UPDATE SKIP LOCKED
)
RETURNING *;
```

`Jobs.Release` (Nack) requeues with `domain.Job.Release`: `Requeue` plus
`run_after = now() + domain.RetryBackoff(attempts)` (30 s base, doubling, 10 min
cap). `RequeueExpired` does not delay the job.

`Jobs.RequeueExpired(account)` applies `domain.Job.Requeue` in SQL to every
`running` job with `lease_expires_at <= now()`: `attempts + 1`, clear
`worker_id` and `lease_expires_at`, then `status = 'failed'`,
`finished_at = now()`, `error = 'attempts exhausted: lease expired'` when
`attempts + 1 >= max_attempts`, else `status = 'queued'`.

## Entity to table

| domain type | table | repository |
|-------------|-------|------------|
| Account | accounts | `Accounts()` |
| Project | projects | `Projects()` |
| Goal | goals | `Goals()` |
| Spec | specs | `Specs()` |
| Task | tasks | `Tasks()` |
| Change | changes | `Changes()` |
| Job, JobSpec, Usage | jobs | `Jobs()` |
| JobEvent | job_events | `JobEvents()` |
| Gate | gates | `Gates()` |
| Signal | signals | `Signals()` |
| Worker | workers | `Workers()` |
| WorkflowRun | workflow_runs | `WorkflowRuns()` |
| StepRun | step_runs | `StepRuns()` |
| WebhookDelivery | webhook_deliveries | `WebhookDeliveries()` |
| MCPToken | mcp_tokens | `MCPTokens()` |
| AuditEntry | audit_log | `Audit()` |
| (human ref numbers) | ref_counters | used by `Create` |
