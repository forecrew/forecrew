# Data model

Postgres 18 schema created by T02 (`internal/store`) and extended by later migrations (00002 goal platforms, 00003 testing system, 00004 team, 00005 identity and access, 00007 the fleet, 00008 invitation GitHub ID, 00009 knowledge note hits and promotion). Each table maps to a
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
- Foreign keys use the default `ON DELETE NO ACTION`; nothing is deleted
  except expired `webhook_deliveries`, by retention `artifacts`, expired
  `sessions` and `oauth_grants`, unused `oauth_clients`, the
  membership rows that removing a member, a team or a binding deletes
  (identity.md), and `images` by retention.
- `accounts` are organizations (identity.md): storage keeps `account` and
  `account_id`, the API, UI, CLI and docs say organization. Users,
  identities, OAuth clients and grants and sessions are global; every other
  identity table has `account_id`.
- `jobs.pool` and `workers.pool` hold pool names from config.md `pools`
  (no CHECK: pools are configuration); empty is stored as `build`.
- `account_id` NULL on `capacity_providers`, `shapes` and `images` means
  instance-wide, and on `machines` an unassigned warm machine (fleet.md).
  Triggers keep these rows in one organization, because a composite key
  cannot check a NULL one.
- `audit_log` is append-only: triggers reject `UPDATE`, `DELETE` and `TRUNCATE`.
- pgvector is optional. Migration 00004 creates the extension and
  `knowledge_entries.embedding` only when the extension is installed or
  available and the database role may create it; otherwise it logs a notice
  and goes on. Knowledge search never needs it: full-text search over
  `knowledge_entries.search` always works, and embeddings only add to the
  ranking where the column exists (decision 10, T36). Migration 00006
  gives the column its dimension, 1536 (OpenAI `text-embedding-3-small`), and
  a cosine index. To add it later, a superuser runs `CREATE EXTENSION vector;
  ALTER TABLE knowledge_entries ADD COLUMN embedding vector(1536); CREATE INDEX
  knowledge_entries_embedding ON knowledge_entries USING hnsw (embedding
  vector_cosine_ops);`. Entries stored before that have no embedding; forecrewd
  embeds them on its next sync.

## Tables

```sql
-- An organization (identity.md).
CREATE TABLE accounts (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  name       text NOT NULL,
  slug       text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),  -- domain.ValidSlug
  created_by text,                          -- domain.Actor.String(); NULL for the one created at install
  max_token_ttl interval CHECK (max_token_ttl > interval '0'),  -- token cap under SSO enforcement (identity.md); NULL: no cap
  created_at timestamptz NOT NULL DEFAULT now()
);
-- Migration 00005 gave existing accounts domain.Slugify(name), oldest first,
-- with the first free -2, -3, ... on a clash.

-- Next human ref number per account and kind (domain.RefKinds).
CREATE TABLE ref_counters (
  account_id uuid NOT NULL REFERENCES accounts(id),
  kind       text NOT NULL CHECK (kind IN ('GOAL','SPEC','TASK','JOB','GATE','COUNCIL','A','DEC','KB','PROP')),
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
  kind         text NOT NULL DEFAULT 'feature' CHECK (kind IN ('feature','bug')),  -- every goal before 00004 is a feature
  title        text NOT NULL,
  body         text NOT NULL DEFAULT '',
  platforms    text[] NOT NULL DEFAULT '{}',   -- target platforms (worker labels), normalized
  status       text NOT NULL CHECK (status IN ('draft','spec_review','planned','in_progress','done','canceled')),
  size         jsonb,                          -- domain.GoalSize; NULL until sized, then set once. Impact and reversibility are both empty when a person set the councils (no triage)
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
  kind             text NOT NULL DEFAULT 'agent' CHECK (kind IN ('agent','command')),
  pool             text NOT NULL DEFAULT 'build',  -- worker pool; only its workers lease the job
  role             text NOT NULL,          -- may be '' for command jobs
  runtime          text NOT NULL,          -- adapter: claude-code | codex | opencode; '' for command jobs
  needs            text[] NOT NULL DEFAULT '{}',
  resources        jsonb NOT NULL DEFAULT '{}',   -- domain.Resources: what it needs of a machine (fleet.md)
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
  context_manifest jsonb,                         -- domain.ContextManifest: what the context pack held (T38)
  job_mcp_token_hash bytea                        -- SHA-256 of the job MCP token of the current lease (job-mcp.md)
                   CHECK (job_mcp_token_hash IS NULL OR (status = 'running' AND octet_length(job_mcp_token_hash) = 32)),
  usage            jsonb NOT NULL DEFAULT '{}',   -- domain.Usage
  cost_usd         numeric(12,6) NOT NULL DEFAULT 0,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, number),
  CHECK ((status = 'running') = (worker_id IS NOT NULL AND lease_expires_at IS NOT NULL))
);
CREATE INDEX jobs_queued   ON jobs (account_id, pool, created_at, id) WHERE status = 'queued';
CREATE INDEX jobs_leases   ON jobs (account_id, lease_expires_at) WHERE status = 'running';
CREATE INDEX jobs_project  ON jobs (account_id, project_id, created_at DESC);
CREATE INDEX jobs_task     ON jobs (account_id, task_id) WHERE task_id IS NOT NULL;
CREATE INDEX jobs_goal     ON jobs (account_id, goal_id) WHERE goal_id IS NOT NULL;
CREATE INDEX jobs_worker   ON jobs (worker_id) WHERE status = 'running';
CREATE UNIQUE INDEX jobs_job_mcp_token ON jobs (job_mcp_token_hash) WHERE job_mcp_token_hash IS NOT NULL;
-- Trigger jobs_clear_job_mcp_token (BEFORE UPDATE, migration 00004) sets
-- job_mcp_token_hash to NULL when status leaves running (complete, cancel,
-- release, requeue, lease expiry) and when a new lease starts (worker_id or
-- started_at change) without a new hash. The token itself is never stored.

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
  pool         text NOT NULL DEFAULT 'build', -- set by an admin, never by the worker
  ephemeral    boolean NOT NULL DEFAULT false, -- the worker of a machine (fleet.md); false on every static worker
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
  kind           text NOT NULL CHECK (kind IN ('profile_approval','spec_approval','merge_approval','test_plan_approval','needs_you',
                                               'assumption_review','council_decision','knowledge_proposal','access_request','improvement_proposal')),
  origin         text NOT NULL DEFAULT 'playbook' CHECK (origin IN ('playbook','ask_human')),  -- domain.GateOrigin
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
  CHECK ((status = 'pending') = (resolved_at IS NULL)),
  CHECK (origin <> 'ask_human' OR (kind = 'needs_you' AND job_id IS NOT NULL))
);
-- origin ask_human: a question the job MCP tool ask_human asked (job-mcp.md):
-- summary holds the question, the approval's feedback the answer. No
-- playbook step waits on it; escalations are origin playbook.
CREATE INDEX gates_pending ON gates (account_id, project_id) WHERE status = 'pending';
CREATE INDEX gates_subject ON gates (account_id, lower(subject_repo), subject_number) WHERE status = 'pending';

CREATE TABLE signals (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id uuid NOT NULL REFERENCES accounts(id),
  project_id uuid NOT NULL REFERENCES projects(id),
  kind       text NOT NULL CHECK (kind IN ('job.completed','gate.resolved','pr.merged','pr.closed','checks.completed','council.decided')),
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

-- Personal and service account tokens (identity.md). Tokens from before
-- migration 00005 have no owner and scope admin until T52 assigns them to
-- the first Owner.
CREATE TABLE mcp_tokens (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id   uuid NOT NULL REFERENCES accounts(id),   -- the organization
  name         text NOT NULL,
  token_hash   bytea NOT NULL UNIQUE,       -- SHA-256 of the token
  owner_kind   text CHECK (owner_kind IN ('user','service_account')),
  owner_id     uuid,                        -- users.id or service_accounts.id
  scopes       text[] NOT NULL CHECK (cardinality(scopes) > 0 AND scopes <@ ARRAY['read','write','admin']),
  expires_at   timestamptz,                 -- NULL only on tokens from before 00005
  last_used_at timestamptz,
  revoked_at   timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),
  CHECK ((owner_kind IS NULL) = (owner_id IS NULL))
);
CREATE UNIQUE INDEX mcp_tokens_owner_name ON mcp_tokens (account_id, owner_kind, owner_id, name) NULLS NOT DISTINCT;
CREATE INDEX mcp_tokens_owner ON mcp_tokens (owner_kind, owner_id) WHERE owner_id IS NOT NULL;
-- Trigger mcp_tokens_check_owner (migration 00005): an owner, when set, is a
-- member or service account of the token's organization (foreign key
-- violation otherwise, store.ErrNotFound). A later change to the membership
-- leaves the row alone.

CREATE TABLE audit_log (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id   uuid NOT NULL REFERENCES accounts(id),
  project_id   uuid REFERENCES projects(id),
  actor        text NOT NULL,               -- domain.Actor.String(), e.g. github:octocat, user:<uuid>
  credential   text,                        -- domain.Credential.String(): session:<id>, token:<id>, oauth_grant:<id>
  action       text NOT NULL,               -- <subject>.<verb>, e.g. gate.resolved
  subject_type text NOT NULL,               -- job, gate, goal, task, project, worker, token, ...
  subject_id   uuid,
  data         jsonb,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_account ON audit_log (account_id, created_at DESC);
CREATE INDEX audit_log_subject ON audit_log (account_id, subject_id, created_at DESC) WHERE subject_id IS NOT NULL;
CREATE INDEX audit_log_actor ON audit_log (account_id, actor, created_at DESC);

CREATE FUNCTION audit_log_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'audit_log is append-only';
END $$;
CREATE TRIGGER audit_log_append_only BEFORE UPDATE OR DELETE ON audit_log
  FOR EACH STATEMENT EXECUTE FUNCTION audit_log_append_only();
CREATE TRIGGER audit_log_no_truncate BEFORE TRUNCATE ON audit_log
  FOR EACH STATEMENT EXECUTE FUNCTION audit_log_append_only();

-- Testing system (migration 00003). Artifacts are deleted by retention
-- (evidence.retention_days), the only other deletion besides webhook
-- deliveries.

-- Evidence files; the content is in object storage under storage_key.
CREATE TABLE artifacts (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id   uuid NOT NULL REFERENCES accounts(id),
  project_id   uuid NOT NULL REFERENCES projects(id),
  job_id       uuid REFERENCES jobs(id),       -- NULL for artifacts forecrewd makes (verification reports)
  name         text NOT NULL,                  -- domain.ValidArtifactName, e.g. reports/unit.xml
  content_type text NOT NULL,
  size         bigint NOT NULL CHECK (size >= 0),
  sha256       text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
  storage_key  text NOT NULL UNIQUE,
  created_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (job_id, name)                        -- a re-upload replaces the row
);
CREATE INDEX artifacts_created ON artifacts (account_id, created_at);  -- retention

-- One suite in one matrix cell, reported by a command job's complete.
CREATE TABLE test_runs (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id       uuid NOT NULL REFERENCES accounts(id),
  project_id       uuid NOT NULL REFERENCES projects(id),
  job_id           uuid NOT NULL REFERENCES jobs(id),
  change_id        uuid REFERENCES changes(id),   -- the PR whose head is sha, if any
  sha              text NOT NULL,                 -- commit tested, lowercase
  suite            text NOT NULL,
  environment      text NOT NULL DEFAULT '',      -- environment name; '' for the worker's host
  cell             jsonb NOT NULL DEFAULT '{}',   -- axis -> value
  status           text NOT NULL CHECK (status IN ('passed','failed','errored')),
  counts           jsonb NOT NULL DEFAULT '{}',   -- domain.TestCounts
  duration_ms      bigint NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
  exit_code        integer NOT NULL,              -- -1: did not exit by itself
  versions         jsonb NOT NULL DEFAULT '{}',   -- tool -> version output
  report_artifacts text[] NOT NULL DEFAULT '{}',  -- artifact names of the JUnit reports
  attempt          integer NOT NULL CHECK (attempt >= 1),  -- jobs.attempts + 1 when reported
  created_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (job_id, attempt, suite)
);
CREATE INDEX test_runs_sha    ON test_runs (account_id, project_id, sha);
CREATE INDEX test_runs_change ON test_runs (account_id, change_id) WHERE change_id IS NOT NULL;

CREATE TABLE test_results (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid NOT NULL REFERENCES accounts(id),
  run_id      uuid NOT NULL REFERENCES test_runs(id),
  name        text NOT NULL,
  classname   text NOT NULL DEFAULT '',
  status      text NOT NULL CHECK (status IN ('passed','failed','skipped','errored')),
  duration_ms bigint NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
  message     text NOT NULL DEFAULT ''
);
CREATE INDEX test_results_run ON test_results (run_id);

-- One per PR head; report follows verification.schema.json.
CREATE TABLE verification_reports (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid NOT NULL REFERENCES accounts(id),
  project_id  uuid NOT NULL REFERENCES projects(id),
  change_id   uuid NOT NULL REFERENCES changes(id),
  sha         text NOT NULL,                  -- lowercase
  status      text NOT NULL CHECK (status IN ('passed','failed','incomplete')),
  report      jsonb NOT NULL,
  artifact_id uuid REFERENCES artifacts(id),  -- rendered Markdown, when stored
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (change_id, sha)                     -- a rebuild updates the row
);

-- Team (migration 00004): councils, the assumption and decision
-- registers, the knowledge index and improvement proposals.

-- A council answers one question with a decision brief
-- (council-brief.schema.json, set at synthesis). Ref COUNCIL-n.
CREATE TABLE councils (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid NOT NULL REFERENCES accounts(id),
  project_id  uuid NOT NULL REFERENCES projects(id),
  goal_id     uuid REFERENCES goals(id),          -- NULL for a council of its own (start_council)
  number      bigint NOT NULL,
  kind        text NOT NULL CHECK (kind IN ('onboarding','product','technical','test_design','incident','adhoc')),
  question    text NOT NULL,
  constraints text[] NOT NULL DEFAULT '{}',       -- given at start or added mid-debate
  status      text NOT NULL CHECK (status IN ('running','decided','failed','canceled')),
  brief       jsonb,                              -- domain.CouncilBrief
  gate_id     uuid REFERENCES gates(id),          -- the council_decision gate, if any
  run_id      uuid REFERENCES workflow_runs(id),  -- the council playbook's run
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, number),
  CHECK (status <> 'decided' OR brief IS NOT NULL)
);
CREATE INDEX councils_project ON councils (account_id, project_id, created_at);
CREATE INDEX councils_goal    ON councils (account_id, goal_id) WHERE goal_id IS NOT NULL;

-- One member's output at one stage. Not ref-numbered. A council has one
-- member per role and runtime (the same role may sit on several runtimes).
-- Rounds: proposal and critique 1; revision 1..3, where the council runs at most 2 of its own and request_changes on its
-- council_decision gate adds one human round (domain.MaxRevisionRounds),
-- after which the gate can only be approved or rejected; synthesis 1..2, the
-- second after the human round. The facilitator's opening brief and its
-- "do positions still move" checks are jobs of the council's run, not
-- contributions.
CREATE TABLE council_contributions (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id uuid NOT NULL REFERENCES accounts(id),
  council_id uuid NOT NULL REFERENCES councils(id),
  role       text NOT NULL,                       -- member role
  runtime    text NOT NULL DEFAULT '',            -- runtime adapter it ran on (vendor diversity)
  stage      text NOT NULL CHECK (stage IN ('proposal','critique','revision','synthesis')),
  round      integer NOT NULL CHECK (round >= 1 AND round <= CASE stage WHEN 'revision' THEN 3 WHEN 'synthesis' THEN 2 ELSE 1 END),
  job_id     uuid UNIQUE REFERENCES jobs(id),     -- one contribution per job: a resumed council adds none twice
  content    jsonb NOT NULL,                      -- the job's output, valid against its stage's schema
  evidence_check jsonb,                           -- [domain.SourceCheck] of the cited sources; NULL until checked (T33)
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (council_id, role, runtime, stage, round) -- a retried job (new id) cannot add a second one
);
CREATE INDEX council_contributions_council ON council_contributions (council_id, created_at);

-- Register of assumptions. Ref A-n. Sources are the job, spec or council it
-- came from (any may be NULL); dependents is [{kind, id}] (domain.Dependent).
CREATE TABLE assumptions (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid NOT NULL REFERENCES accounts(id),
  project_id  uuid NOT NULL REFERENCES projects(id),
  number      bigint NOT NULL,
  goal_id     uuid REFERENCES goals(id),           -- duplicates merge per goal (T32)
  statement   text NOT NULL,
  made_by     text NOT NULL,                       -- role
  job_id      uuid REFERENCES jobs(id),
  spec_id     uuid REFERENCES specs(id),
  council_id  uuid REFERENCES councils(id),
  confidence  text NOT NULL CHECK (confidence IN ('low','medium','high')),
  impact      text NOT NULL CHECK (impact IN ('low','medium','high')),
  status      text NOT NULL CHECK (status IN ('unverified','verified','invalidated','accepted')),
  verify_by   text NOT NULL CHECK (verify_by IN ('test','ask-human','research')),
  evidence    text NOT NULL DEFAULT '',
  dependents  jsonb NOT NULL DEFAULT '[]',
  resolved_by text,                                -- domain.Actor.String()
  resolved_at timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, number),
  CHECK ((status = 'unverified') = (resolved_at IS NULL))
);
CREATE INDEX assumptions_project ON assumptions (account_id, project_id, status, impact);
CREATE INDEX assumptions_goal    ON assumptions (account_id, goal_id) WHERE goal_id IS NOT NULL;

-- Register of decisions. Ref DEC-n. Never updated.
CREATE TABLE decisions (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid NOT NULL REFERENCES accounts(id),
  project_id  uuid NOT NULL REFERENCES projects(id),
  number      bigint NOT NULL,
  goal_id     uuid REFERENCES goals(id),
  question    text NOT NULL,
  options     jsonb NOT NULL DEFAULT '[]',        -- [{name, summary}]
  choice      text NOT NULL,
  reasons     text NOT NULL DEFAULT '',
  dissent     text NOT NULL DEFAULT '',
  decided_by  text NOT NULL,                      -- domain.Actor.String() or 'policy'
  council_id  uuid REFERENCES councils(id),
  gate_id     uuid REFERENCES gates(id),
  assumptions uuid[] NOT NULL DEFAULT '{}',       -- assumptions.id it rests on
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, number),
  CHECK (decided_by <> 'policy' OR council_id IS NOT NULL)
);
CREATE INDEX decisions_project ON decisions (account_id, project_id, created_at);
CREATE INDEX decisions_goal    ON decisions (account_id, goal_id) WHERE goal_id IS NOT NULL;

-- The knowledge index (knowledge.md): one row per entry of a provider, per
-- generated entry and per agent note (provider 'internal', section 'notes').
-- Not ref-numbered.
CREATE TABLE knowledge_entries (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id      uuid NOT NULL REFERENCES accounts(id),
  project_id      uuid NOT NULL REFERENCES projects(id),
  provider        text NOT NULL,                  -- provider name from the policy, or 'internal'
  path            text NOT NULL,                  -- repo path, or the provider's stable id
  source_url      text NOT NULL DEFAULT '',
  version         text NOT NULL DEFAULT '',       -- blob SHA, page version
  section         text NOT NULL,
  title           text NOT NULL,
  body            text NOT NULL DEFAULT '',       -- Markdown without frontmatter
  body_hash       text NOT NULL CHECK (body_hash ~ '^[0-9a-f]{64}$'),
  provenance      jsonb NOT NULL DEFAULT '{}',    -- domain.Provenance {author_role, source, created, last_verified}
  generated       boolean NOT NULL DEFAULT false,
  needs_reverify  boolean NOT NULL DEFAULT false,
  reverify_reason text NOT NULL DEFAULT '',       -- commit and paths that changed
  hits            integer NOT NULL DEFAULT 0 CHECK (hits >= 0),  -- searches that returned the entry
  scope           text CHECK (scope IN ('goal','project')),  -- notes only
  goal_id         uuid REFERENCES goals(id),      -- goal notes only
  expires_at      timestamptz,                    -- notes only
  tags            text[] NOT NULL DEFAULT '{}',   -- notes only
  promoted_at     timestamptz,                    -- notes only: proposed for a curated section; never expires
  synced_at       timestamptz NOT NULL DEFAULT now(),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  search          tsvector GENERATED ALWAYS AS
                    (setweight(to_tsvector('english', title), 'A') || setweight(to_tsvector('english', body), 'B')) STORED,
  -- embedding vector(1536),                    -- only with pgvector (Rules)
  UNIQUE (account_id, project_id, provider, path),
  CHECK ((provider = 'internal') = (section = 'notes')),
  CHECK ((provider = 'internal') = (scope IS NOT NULL)),
  CHECK ((coalesce(scope, '') = 'goal') = (goal_id IS NOT NULL)),
  CHECK (provider = 'internal' OR (expires_at IS NULL AND tags = '{}')),
  CHECK (generated = (section = 'generated')),
  CHECK (promoted_at IS NULL OR provider = 'internal')
);
-- Query with: search @@ websearch_to_tsquery('english', $q), ranked by ts_rank(search, query).
CREATE INDEX knowledge_entries_search   ON knowledge_entries USING gin (search);
CREATE INDEX knowledge_entries_section  ON knowledge_entries (account_id, project_id, section);
CREATE INDEX knowledge_entries_goal     ON knowledge_entries (account_id, goal_id) WHERE goal_id IS NOT NULL;
CREATE INDEX knowledge_entries_expires  ON knowledge_entries (expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX knowledge_entries_reverify ON knowledge_entries (account_id, project_id) WHERE needs_reverify;
-- Only with pgvector (Rules): CREATE INDEX knowledge_entries_embedding ON knowledge_entries USING hnsw (embedding vector_cosine_ops);

-- A proposed change to a curated section. Ref KB-n. Never to 'generated'
-- or to notes.
CREATE TABLE knowledge_proposals (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id   uuid NOT NULL REFERENCES accounts(id),
  project_id   uuid NOT NULL REFERENCES projects(id),
  number       bigint NOT NULL,
  provider     text NOT NULL,
  section      text NOT NULL,
  path         text NOT NULL,
  action       text NOT NULL CHECK (action IN ('add','edit','retire')),
  body         text NOT NULL DEFAULT '',
  reason       text NOT NULL,
  proposed_by  text NOT NULL,                     -- role, or domain.Actor.String() of a human
  job_id       uuid REFERENCES jobs(id),
  goal_id      uuid REFERENCES goals(id),
  batch        text NOT NULL CHECK (batch IN ('per_goal','standalone')),
  external_ref text NOT NULL DEFAULT '',          -- PR number, page id: what the review is followed by
  change_id    uuid REFERENCES changes(id),       -- the PR, for the repo provider
  gate_id      uuid REFERENCES gates(id),         -- knowledge_proposal gate, if a human decides here; of a human proposal saved as a lesson, the gate whose feedback it is (the source, never decided on it)
  status       text NOT NULL CHECK (status IN ('queued','open','accepted','rejected')),
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, number),
  CHECK (section NOT IN ('generated','notes') AND provider <> 'internal'),
  CHECK (batch <> 'per_goal' OR goal_id IS NOT NULL)
);
CREATE INDEX knowledge_proposals_queued ON knowledge_proposals (account_id, goal_id) WHERE status = 'queued';
CREATE INDEX knowledge_proposals_open   ON knowledge_proposals (account_id, project_id) WHERE status = 'open';

-- Improvement proposals. Ref PROP-n. action is exactly one of
-- {goal: {title, body, platforms}}, {config_pr: {files: [{path, content}]}}
-- (paths under .forecrew/) and {setup: {instructions, issue}}
-- (domain.ProposalAction); measure is {detector, value}, outcome
-- {before, after, measured_at}, evidence [{kind, ref, detail}].
CREATE TABLE proposals (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id      uuid NOT NULL REFERENCES accounts(id),
  project_id      uuid NOT NULL REFERENCES projects(id),
  number          bigint NOT NULL,
  scope           text NOT NULL CHECK (scope IN ('project','platform')),
  category        text NOT NULL CHECK (category IN ('observability','testing','ci','security','docs','knowledge','tooling','workers','roles','cost')),
  title           text NOT NULL,
  rationale       text NOT NULL,
  evidence        jsonb NOT NULL DEFAULT '[]',
  benefit         text NOT NULL,
  effort          text NOT NULL CHECK (effort IN ('s','m','l')),
  risk            text NOT NULL CHECK (risk IN ('low','medium','high')),
  action          jsonb NOT NULL,
  measure         jsonb,
  status          text NOT NULL CHECK (status IN ('proposed','accepted','rejected','snoozed','done','superseded')),
  dedupe_key      text NOT NULL,
  evidence_weight double precision NOT NULL DEFAULT 0 CHECK (evidence_weight >= 0),
  snoozed_until   timestamptz,
  decided_by      text,                           -- domain.Actor.String()
  reason          text,
  outcome         jsonb,
  gate_id         uuid REFERENCES gates(id),      -- the improvement_proposal gate
  goal_id         uuid REFERENCES goals(id),      -- the goal a goal action created
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, number),
  CHECK ((status = 'snoozed') = (snoozed_until IS NOT NULL))
);
-- rejected, done and superseded are final; a dedupe key is unique among the others.
CREATE UNIQUE INDEX proposals_dedupe ON proposals (account_id, project_id, dedupe_key)
  WHERE status IN ('proposed','accepted','snoozed');
CREATE INDEX proposals_project ON proposals (account_id, project_id, status);

-- Identity and access (migration 00005, identity.md, permissions.md).
-- projects gets the key the composite foreign keys below need:
CREATE UNIQUE INDEX projects_account_id ON projects (account_id, id);

CREATE TABLE users (
  id             uuid PRIMARY KEY DEFAULT uuidv7(),
  name           text NOT NULL DEFAULT '',
  email          text,                        -- primary email
  email_verified boolean NOT NULL DEFAULT false,
  instance_admin boolean NOT NULL DEFAULT false,
  disabled_at    timestamptz,
  last_seen_at   timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  CHECK (email IS NOT NULL OR NOT email_verified)
);
CREATE UNIQUE INDEX users_verified_email ON users (lower(email)) WHERE email_verified;
CREATE INDEX users_email ON users (lower(email));

-- account_id NULL: an instance-level connection from forecrew.yaml
-- (auth.connections), upserted by name at startup. The client secret is
-- never stored, only where to read it.
CREATE TABLE connections (
  id                uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id        uuid REFERENCES accounts(id),
  kind              text NOT NULL CHECK (kind IN ('github','google','microsoft','oidc')),
  name              text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
  issuer            text NOT NULL DEFAULT '',
  client_id         text NOT NULL CHECK (client_id <> ''),
  client_secret_ref text NOT NULL,            -- connections_secret_ref below
  jit               jsonb NOT NULL DEFAULT '{"enabled": false, "default_roles": []}'
                      CHECK (jsonb_typeof(jit->'enabled') = 'boolean' AND jsonb_typeof(jit->'default_roles') = 'array'),
  group_claim       text NOT NULL DEFAULT '',
  enabled           boolean NOT NULL DEFAULT true,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (account_id, name),
  -- an organization's connection only reaches the reserved namespace;
  -- store: is the secret store's form (identity.md, Connections)
  CONSTRAINT connections_secret_ref CHECK (client_secret_ref !~ '\.\.' AND client_secret_ref ~ CASE WHEN account_id IS NULL
    THEN '^(env:[A-Z_][A-Z0-9_]{0,127}|secret:[a-z0-9][a-z0-9._/-]{0,127}|store:[a-z0-9][a-z0-9._/-]{0,127})$'
    ELSE '^(env:FORECREW_CONN_[A-Z0-9_]{1,114}|secret:conn-[a-z0-9._/-]{1,123}|store:[a-z0-9][a-z0-9._/-]{0,127})$' END),
  CHECK (CASE kind WHEN 'github' THEN issuer = ''
                   WHEN 'google' THEN issuer IN ('', 'https://accounts.google.com')
                   ELSE issuer ~ '^https://[^/?#]+' END),
  CHECK (account_id IS NOT NULL OR (jit->>'enabled' = 'false' AND jit->'default_roles' = '[]'))
);

CREATE TABLE identities (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id       uuid NOT NULL REFERENCES users(id),
  connection_id uuid NOT NULL REFERENCES connections(id),
  subject       text NOT NULL CHECK (subject <> ''),   -- the provider's stable user ID
  login         text NOT NULL DEFAULT '',              -- GitHub login or preferred username
  email         text NOT NULL DEFAULT '',
  last_used_at  timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (connection_id, subject)
);
CREATE INDEX identities_user ON identities (user_id);
CREATE INDEX identities_login ON identities (connection_id, lower(login)) WHERE login <> '';

CREATE TABLE memberships (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid NOT NULL REFERENCES accounts(id),
  user_id     uuid NOT NULL REFERENCES users(id),
  status      text NOT NULL CHECK (status IN ('active','invited','suspended')),
  source      text NOT NULL CHECK (source IN ('manual','invitation','jit','scim')),
  external_id text,                                    -- SCIM externalId
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, user_id)
);
CREATE UNIQUE INDEX memberships_external ON memberships (account_id, external_id) WHERE external_id IS NOT NULL;
CREATE INDEX memberships_user ON memberships (user_id);

CREATE TABLE invitations (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id   uuid NOT NULL REFERENCES accounts(id),
  email        text,
  github_login text,
  github_user_id bigint CHECK (github_user_id > 0),    -- migration 00008; the stable ID of github_login, resolved at creation
  roles        jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(roles) = 'array'),  -- [domain.Grant]
  token_hash   bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  expires_at   timestamptz NOT NULL,
  invited_by   text NOT NULL,                          -- domain.Actor.String()
  accepted_by  uuid REFERENCES users(id),
  accepted_at  timestamptz,
  revoked_at   timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),
  CHECK ((email IS NULL) <> (github_login IS NULL)),
  CHECK ((accepted_by IS NULL) = (accepted_at IS NULL))
);
CREATE INDEX invitations_account ON invitations (account_id, created_at);

CREATE TABLE teams (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid NOT NULL REFERENCES accounts(id),
  name        text NOT NULL,
  slug        text NOT NULL CHECK (slug ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
  source      text NOT NULL CHECK (source IN ('manual','idp','scim')),
  external_id text,                                    -- SCIM group externalId
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, slug),
  UNIQUE (account_id, id)
);
CREATE UNIQUE INDEX teams_external ON teams (account_id, external_id) WHERE external_id IS NOT NULL;

-- A team member must be a member of the team's organization.
CREATE TABLE team_members (
  team_id    uuid NOT NULL,
  user_id    uuid NOT NULL,
  account_id uuid NOT NULL,
  source     text NOT NULL CHECK (source IN ('manual','idp','scim')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (team_id, user_id),
  FOREIGN KEY (account_id, team_id) REFERENCES teams (account_id, id),
  FOREIGN KEY (account_id, user_id) REFERENCES memberships (account_id, user_id)
);
CREATE INDEX team_members_user ON team_members (account_id, user_id);

CREATE TABLE service_accounts (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid NOT NULL REFERENCES accounts(id),
  name        text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
  description text NOT NULL DEFAULT '',
  created_by  text NOT NULL,
  disabled_at timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, name)
);

-- account_id NULL: a built-in role, the same in every organization and
-- equal to domain.BuiltInRoles(). Built-in rows change only by migration,
-- which disables the trigger roles_built_in_fixed around the change.
CREATE TABLE roles (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid REFERENCES accounts(id),
  name        text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
  description text NOT NULL DEFAULT '',
  scope       text NOT NULL CHECK (scope IN ('organization','project')),
  permissions text[] NOT NULL CHECK (cardinality(permissions) > 0),  -- domain.Permissions(), sorted
  built_in    boolean NOT NULL,
  custom      boolean NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (account_id, scope, name),
  CHECK (built_in <> custom),
  CHECK (built_in = (account_id IS NULL))
);
-- Migration 00005 seeds the built-in roles of domain.BuiltInRoles()
-- (permissions.md, Built-in roles): owner, admin, billing, member, viewer
-- (scope organization); project_admin, maintainer, contributor, viewer
-- (scope project).
CREATE FUNCTION roles_built_in_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (TG_OP = 'INSERT' AND NEW.built_in) OR (TG_OP <> 'INSERT' AND OLD.built_in) THEN
    RAISE EXCEPTION 'built-in roles change only by migration' USING ERRCODE = 'check_violation', CONSTRAINT = 'roles_built_in_fixed';
  END IF;
  RETURN CASE TG_OP WHEN 'DELETE' THEN OLD ELSE NEW END;
END $$;
CREATE TRIGGER roles_built_in_fixed BEFORE INSERT OR UPDATE OR DELETE ON roles
  FOR EACH ROW EXECUTE FUNCTION roles_built_in_fixed();

-- project_id NULL: organization scope.
CREATE TABLE role_bindings (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id   uuid NOT NULL REFERENCES accounts(id),
  subject_kind text NOT NULL CHECK (subject_kind IN ('user','team','service_account')),
  subject_id   uuid NOT NULL,
  role_id      uuid NOT NULL REFERENCES roles(id),
  project_id   uuid,
  created_by   text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (account_id, project_id) REFERENCES projects (account_id, id),
  UNIQUE NULLS NOT DISTINCT (account_id, subject_kind, subject_id, role_id, project_id)
);
CREATE INDEX role_bindings_subject ON role_bindings (account_id, subject_kind, subject_id);
CREATE INDEX role_bindings_role ON role_bindings (role_id);

-- The role of a binding is built in or of the binding's organization, an
-- organization role is bound to the organization only, the Owner role only
-- to users, and the subject exists in the organization.
CREATE FUNCTION role_bindings_check() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  r roles%ROWTYPE;
  found_subject boolean;
BEGIN
  SELECT * INTO r FROM roles WHERE id = NEW.role_id;
  IF NOT r.built_in AND r.account_id <> NEW.account_id THEN
    RAISE EXCEPTION 'role % belongs to another organization', r.name USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'role_bindings_role';
  END IF;
  IF r.scope = 'organization' AND NEW.project_id IS NOT NULL THEN
    RAISE EXCEPTION 'organization role % cannot be bound to a project', r.name USING ERRCODE = 'check_violation', CONSTRAINT = 'role_bindings_scope';
  END IF;
  IF r.built_in AND r.scope = 'organization' AND r.name = 'owner' AND NEW.subject_kind <> 'user' THEN
    RAISE EXCEPTION 'the owner role is only for users' USING ERRCODE = 'check_violation', CONSTRAINT = 'role_bindings_owner';
  END IF;
  found_subject := CASE NEW.subject_kind
      WHEN 'user' THEN EXISTS (SELECT 1 FROM memberships WHERE account_id = NEW.account_id AND user_id = NEW.subject_id)
      WHEN 'team' THEN EXISTS (SELECT 1 FROM teams WHERE account_id = NEW.account_id AND id = NEW.subject_id)
      ELSE EXISTS (SELECT 1 FROM service_accounts WHERE account_id = NEW.account_id AND id = NEW.subject_id) END;
  IF NOT found_subject THEN
    RAISE EXCEPTION '% % is not in the organization', NEW.subject_kind, NEW.subject_id USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'role_bindings_subject';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER role_bindings_check BEFORE INSERT OR UPDATE ON role_bindings
  FOR EACH ROW EXECUTE FUNCTION role_bindings_check();

-- An organization that has an active Owner keeps one: a user with an
-- active membership, not disabled, bound to the built-in owner role on the
-- organization. Removing or demoting the last one, suspending or removing
-- their membership, or disabling the user fails with check_violation on
-- constraint last_owner (store.ErrLastOwner). The organization row is
-- locked (FOR NO KEY UPDATE) so concurrent changes are checked one after
-- the other; this relies on READ COMMITTED, the store's isolation level.
CREATE FUNCTION is_active_owner(acc uuid, usr uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
  SELECT EXISTS (
    SELECT 1 FROM role_bindings b
    JOIN roles r ON r.id = b.role_id AND r.built_in AND r.scope = 'organization' AND r.name = 'owner'
    JOIN memberships m ON m.account_id = b.account_id AND m.user_id = b.subject_id AND m.status = 'active'
    JOIN users u ON u.id = b.subject_id AND u.disabled_at IS NULL
    WHERE b.account_id = acc AND b.subject_kind = 'user' AND b.project_id IS NULL
      AND (usr IS NULL OR b.subject_id = usr))
$$;
CREATE FUNCTION keep_an_owner(acc uuid) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  PERFORM 1 FROM accounts WHERE id = acc FOR NO KEY UPDATE;
  IF NOT is_active_owner(acc, NULL) THEN
    RAISE EXCEPTION 'organization % must keep an active owner', acc USING ERRCODE = 'check_violation', CONSTRAINT = 'last_owner';
  END IF;
END $$;
CREATE FUNCTION role_bindings_keep_owner() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.subject_kind = 'user' AND OLD.project_id IS NULL
     AND EXISTS (SELECT 1 FROM roles WHERE id = OLD.role_id AND built_in AND scope = 'organization' AND name = 'owner')
     AND EXISTS (SELECT 1 FROM memberships WHERE account_id = OLD.account_id AND user_id = OLD.subject_id AND status = 'active')
     AND EXISTS (SELECT 1 FROM users WHERE id = OLD.subject_id AND disabled_at IS NULL) THEN
    PERFORM keep_an_owner(OLD.account_id);
  END IF;
  RETURN NULL;
END $$;
CREATE TRIGGER role_bindings_keep_owner AFTER UPDATE OR DELETE ON role_bindings
  FOR EACH ROW EXECUTE FUNCTION role_bindings_keep_owner();
CREATE FUNCTION memberships_keep_owner() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'active' AND (TG_OP = 'DELETE' OR NEW.status <> 'active' OR NEW.user_id <> OLD.user_id OR NEW.account_id <> OLD.account_id)
     AND EXISTS (SELECT 1 FROM role_bindings b JOIN roles r ON r.id = b.role_id AND r.built_in AND r.scope = 'organization' AND r.name = 'owner'
                 WHERE b.account_id = OLD.account_id AND b.subject_kind = 'user' AND b.subject_id = OLD.user_id AND b.project_id IS NULL)
     AND EXISTS (SELECT 1 FROM users WHERE id = OLD.user_id AND disabled_at IS NULL) THEN
    PERFORM keep_an_owner(OLD.account_id);
  END IF;
  RETURN NULL;
END $$;
CREATE TRIGGER memberships_keep_owner AFTER UPDATE OR DELETE ON memberships
  FOR EACH ROW EXECUTE FUNCTION memberships_keep_owner();
CREATE FUNCTION users_keep_owner() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  acc uuid;
BEGIN
  IF OLD.disabled_at IS NULL AND NEW.disabled_at IS NOT NULL THEN
    FOR acc IN SELECT m.account_id FROM memberships m
               JOIN role_bindings b ON b.account_id = m.account_id AND b.subject_kind = 'user' AND b.subject_id = m.user_id AND b.project_id IS NULL
               JOIN roles r ON r.id = b.role_id AND r.built_in AND r.scope = 'organization' AND r.name = 'owner'
               WHERE m.user_id = OLD.id AND m.status = 'active' LOOP
      PERFORM keep_an_owner(acc);
    END LOOP;
  END IF;
  RETURN NULL;
END $$;
CREATE TRIGGER users_keep_owner AFTER UPDATE OF disabled_at ON users
  FOR EACH ROW EXECUTE FUNCTION users_keep_owner();

CREATE TABLE group_mappings (
  id             uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id     uuid NOT NULL REFERENCES accounts(id),
  connection_id  uuid NOT NULL REFERENCES connections(id),
  external_group text NOT NULL CHECK (external_group <> ''),
  team_id        uuid NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (account_id, team_id) REFERENCES teams (account_id, id),
  UNIQUE (account_id, connection_id, external_group, team_id)
);

-- Trigger group_mappings_check: the connection is instance-level or of the
-- mapping's organization (store.ErrNotFound otherwise).

-- A domain can be pending in several organizations but verified in one.
CREATE TABLE verified_domains (
  id                 uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id         uuid NOT NULL REFERENCES accounts(id),
  domain             text NOT NULL CHECK (domain ~ '^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$'),
  verification_token text NOT NULL CHECK (verification_token <> ''),
  verified_at        timestamptz,
  enforce_sso        boolean NOT NULL DEFAULT false,
  connection_id      uuid REFERENCES connections(id),
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, domain),
  CHECK (NOT enforce_sso OR (verified_at IS NOT NULL AND connection_id IS NOT NULL))
);
CREATE UNIQUE INDEX verified_domains_domain ON verified_domains (domain) WHERE verified_at IS NOT NULL;

-- Server-side browser sessions; the cookie holds the token, the row its hash.
CREATE TABLE sessions (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  token_hash      bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  user_id         uuid NOT NULL REFERENCES users(id),
  account_id      uuid REFERENCES accounts(id),        -- current organization
  connection_id   uuid REFERENCES connections(id),     -- NULL: break-glass sign-in
  created_at      timestamptz NOT NULL DEFAULT now(),
  last_seen_at    timestamptz NOT NULL DEFAULT now(),
  idle_expires_at timestamptz NOT NULL,
  expires_at      timestamptz NOT NULL,
  user_agent      text NOT NULL DEFAULT '',
  ip              inet,
  revoked_at      timestamptz,
  CHECK (idle_expires_at <= expires_at)
);
CREATE INDEX sessions_user ON sessions (user_id) WHERE revoked_at IS NULL;

-- OAuth for MCP clients. Clients are instance-wide; every code, access and
-- refresh token is a grant row with its hash, in one organization.
CREATE TABLE oauth_clients (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  client_id     text NOT NULL UNIQUE CHECK (client_id <> ''),
  name          text NOT NULL,
  redirect_uris text[] NOT NULL CHECK (cardinality(redirect_uris) > 0),
  secret_hash   bytea CHECK (octet_length(secret_hash) = 32),
  last_used_at  timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE oauth_grants (
  id             uuid PRIMARY KEY DEFAULT uuidv7(),
  kind           text NOT NULL CHECK (kind IN ('code','access','refresh')),
  token_hash     bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  family_id      uuid NOT NULL,
  client_id      uuid NOT NULL REFERENCES oauth_clients(id),
  user_id        uuid NOT NULL REFERENCES users(id),
  account_id     uuid NOT NULL REFERENCES accounts(id),
  scopes         text[] NOT NULL CHECK (cardinality(scopes) > 0 AND scopes <@ ARRAY['read','write','admin']),
  resource       text NOT NULL CHECK (resource <> ''),
  redirect_uri   text NOT NULL DEFAULT '',
  code_challenge text NOT NULL DEFAULT '',
  expires_at     timestamptz NOT NULL,
  used_at        timestamptz,
  revoked_at     timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now(),
  CHECK (kind <> 'code' OR (redirect_uri <> '' AND code_challenge <> '')),
  CHECK (kind <> 'access' OR used_at IS NULL)
);
CREATE INDEX oauth_grants_family ON oauth_grants (family_id);
CREATE INDEX oauth_grants_user ON oauth_grants (user_id, account_id) WHERE revoked_at IS NULL;

CREATE TABLE scim_tokens (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id   uuid NOT NULL REFERENCES accounts(id),
  name         text NOT NULL,
  token_hash   bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  created_by   text NOT NULL,
  last_used_at timestamptz,
  revoked_at   timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, name)
);

-- The fleet (fleet.md). Migration 00007 also adds workers.ephemeral,
-- jobs.resources and the unique indexes workers (account_id, id) and
-- jobs (account_id, id) that machines' composite keys point at.
CREATE TABLE capacity_providers (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id    uuid REFERENCES accounts(id),
  name          text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
  kind          text NOT NULL CHECK (kind IN ('microvm','proxmox','hyperv','tart','aws','fake')),
  mode          text NOT NULL DEFAULT 'agent' CHECK (mode IN ('agent','direct')),
  status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','active','paused','revoked')),
  token_hash    bytea UNIQUE CHECK (octet_length(token_hash) = 32),
  last_seen_at  timestamptz,
  capabilities  jsonb NOT NULL DEFAULT '{}',
  limits        jsonb NOT NULL DEFAULT '{}',
  network_allow text[] NOT NULL DEFAULT '{}',
  tags          jsonb NOT NULL DEFAULT '{}',
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (account_id, name),
  CHECK (mode = 'agent' OR token_hash IS NULL),
  CHECK (mode = 'direct' OR status <> 'active' OR token_hash IS NOT NULL)
);

CREATE TABLE provider_join_codes (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid REFERENCES accounts(id),
  code_hash   bytea NOT NULL UNIQUE CHECK (octet_length(code_hash) = 32),
  claim_hash  bytea CHECK (octet_length(claim_hash) = 32),
  kind        text CHECK (kind IN ('microvm','proxmox','hyperv','tart','aws','fake')),
  facts       jsonb NOT NULL DEFAULT '{}',
  provider_id uuid REFERENCES capacity_providers(id),
  expires_at  timestamptz NOT NULL,
  used_at     timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  CHECK (used_at IS NULL OR provider_id IS NOT NULL),
  CHECK (provider_id IS NULL OR claim_hash IS NOT NULL)
);

CREATE TABLE shapes (
  id                   uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id           uuid REFERENCES accounts(id),
  provider_id          uuid NOT NULL REFERENCES capacity_providers(id),
  name                 text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
  os                   text NOT NULL CHECK (os IN ('linux','windows','macos')),
  arch                 text NOT NULL CHECK (arch IN ('amd64','arm64')),
  cpu                  integer NOT NULL CHECK (cpu >= 1),
  memory_mb            integer NOT NULL CHECK (memory_mb >= 1),
  disk_gb              integer NOT NULL CHECK (disk_gb >= 1),
  gpu                  integer NOT NULL DEFAULT 0 CHECK (gpu >= 0),
  pool                 text NOT NULL DEFAULT 'build' CHECK (pool <> 'actions'),
  labels               text[] NOT NULL DEFAULT '{}',
  provider_config      jsonb NOT NULL DEFAULT '{}',
  image                text NOT NULL,
  price_per_minute     numeric(12,6) NOT NULL DEFAULT 0 CHECK (price_per_minute >= 0),
  monthly_budget       numeric(14,2) CHECK (monthly_budget >= 0),
  warm_min             integer NOT NULL DEFAULT 0 CHECK (warm_min >= 0),
  max                  integer NOT NULL DEFAULT 1 CHECK (max >= 1),
  idle_seconds         integer NOT NULL DEFAULT 0 CHECK (idle_seconds >= 0),
  boot_timeout_seconds integer NOT NULL DEFAULT 300 CHECK (boot_timeout_seconds >= 1),
  max_jobs             integer NOT NULL DEFAULT 1 CHECK (max_jobs >= 1),
  note                 text NOT NULL DEFAULT '',
  enabled              boolean NOT NULL DEFAULT true,
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (account_id, name),
  CHECK (warm_min <= max)
);
CREATE INDEX shapes_provider ON shapes (provider_id);

-- A shape or join code and its provider have the same organization, or are
-- both instance-wide. A composite key would not check a NULL organization.
CREATE FUNCTION provider_scope_check() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.provider_id IS NOT NULL AND NOT EXISTS (
       SELECT 1 FROM capacity_providers p WHERE p.id = NEW.provider_id AND p.account_id IS NOT DISTINCT FROM NEW.account_id) THEN
    RAISE EXCEPTION '% and its provider have the same organization', TG_TABLE_NAME USING ERRCODE = 'check_violation', CONSTRAINT = TG_NAME;
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER shapes_check BEFORE INSERT OR UPDATE ON shapes
  FOR EACH ROW EXECUTE FUNCTION provider_scope_check();
CREATE TRIGGER provider_join_codes_check BEFORE INSERT OR UPDATE ON provider_join_codes
  FOR EACH ROW EXECUTE FUNCTION provider_scope_check();

-- A provider or shape in use never changes organization, which would carry
-- its machines along.
CREATE FUNCTION fleet_account_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.account_id IS DISTINCT FROM OLD.account_id THEN
    RAISE EXCEPTION '% keep their organization', TG_TABLE_NAME USING ERRCODE = 'check_violation', CONSTRAINT = 'fleet_account_fixed';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER capacity_providers_account_fixed BEFORE UPDATE ON capacity_providers
  FOR EACH ROW EXECUTE FUNCTION fleet_account_fixed();
CREATE TRIGGER shapes_account_fixed BEFORE UPDATE ON shapes
  FOR EACH ROW EXECUTE FUNCTION fleet_account_fixed();

-- account_id NULL: an unassigned warm machine of an instance-wide shape.
CREATE TABLE machines (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id   uuid REFERENCES accounts(id),
  provider_id  uuid NOT NULL REFERENCES capacity_providers(id),
  shape_id     uuid NOT NULL REFERENCES shapes(id),
  external_id  text,
  state        text NOT NULL DEFAULT 'requested'
               CHECK (state IN ('requested','booting','ready','busy','draining','destroyed','failed')),
  worker_id    uuid,
  job_id       uuid,
  jobs_taken   integer NOT NULL DEFAULT 0 CHECK (jobs_taken >= 0),
  error        text NOT NULL DEFAULT '',
  assigned_at  timestamptz,
  booting_at   timestamptz,
  ready_at     timestamptz,
  deadline_at  timestamptz,
  destroyed_at timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider_id, external_id),
  UNIQUE (worker_id),
  UNIQUE (id, account_id, shape_id),             -- what usage_records point at
  FOREIGN KEY (account_id, worker_id) REFERENCES workers (account_id, id),
  FOREIGN KEY (account_id, job_id) REFERENCES jobs (account_id, id),
  CHECK (account_id IS NOT NULL OR (worker_id IS NULL AND job_id IS NULL)),
  CHECK ((account_id IS NULL) = (assigned_at IS NULL)),
  CHECK ((state = 'destroyed') = (destroyed_at IS NOT NULL))
);
CREATE INDEX machines_active ON machines (provider_id, shape_id) WHERE state NOT IN ('destroyed','failed');
CREATE INDEX machines_account ON machines (account_id, created_at);

-- A machine runs a shape of its provider. A machine of an organization's
-- shape belongs to that organization from the start; a machine of an
-- instance-wide shape to one organization at most, and that never changes.
-- A worker only comes from the fleet: ephemeral. jobs_taken never rises past
-- the shape's max_jobs; a machine already past a max_jobs lowered later can
-- still drain and be destroyed.
CREATE FUNCTION machines_check() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  s shapes%ROWTYPE;
BEGIN
  SELECT * INTO s FROM shapes WHERE id = NEW.shape_id;
  IF s.provider_id IS DISTINCT FROM NEW.provider_id THEN
    RAISE EXCEPTION 'a machine runs a shape of its provider' USING ERRCODE = 'check_violation', CONSTRAINT = 'machines_check';
  END IF;
  IF s.account_id IS NOT NULL AND s.account_id IS DISTINCT FROM NEW.account_id THEN
    RAISE EXCEPTION 'a machine of an organization''s shape belongs to that organization' USING ERRCODE = 'check_violation', CONSTRAINT = 'machines_check';
  END IF;
  IF TG_OP = 'UPDATE' AND OLD.account_id IS NOT NULL AND OLD.account_id IS DISTINCT FROM NEW.account_id THEN
    RAISE EXCEPTION 'a machine keeps its organization' USING ERRCODE = 'check_violation', CONSTRAINT = 'machines_check';
  END IF;
  IF NEW.jobs_taken > s.max_jobs AND (TG_OP = 'INSERT' OR NEW.jobs_taken > OLD.jobs_taken) THEN
    RAISE EXCEPTION 'a machine takes at most its shape''s max_jobs' USING ERRCODE = 'check_violation', CONSTRAINT = 'machines_check';
  END IF;
  IF NEW.worker_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM workers w WHERE w.id = NEW.worker_id AND w.ephemeral) THEN
    RAISE EXCEPTION 'the worker of a machine is ephemeral' USING ERRCODE = 'check_violation', CONSTRAINT = 'machines_check';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER machines_check BEFORE INSERT OR UPDATE ON machines
  FOR EACH ROW EXECUTE FUNCTION machines_check();

-- account_id and expires_at NULL: the machine is unassigned and the token
-- can only be told to wait.
CREATE TABLE bootstrap_tokens (
  machine_id uuid PRIMARY KEY REFERENCES machines(id),
  account_id uuid REFERENCES accounts(id),
  token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  expires_at timestamptz,
  used_at    timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((account_id IS NULL) = (expires_at IS NULL)),
  CHECK (used_at IS NULL OR account_id IS NOT NULL)
);

-- A bootstrap token has exactly its machine's organization, none on an
-- unassigned machine. Assigning updates the machine first, then the token.
CREATE FUNCTION bootstrap_tokens_check() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM machines m WHERE m.id = NEW.machine_id AND m.account_id IS NOT DISTINCT FROM NEW.account_id) THEN
    RAISE EXCEPTION 'a bootstrap token has its machine''s organization' USING ERRCODE = 'check_violation', CONSTRAINT = 'bootstrap_tokens_check';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER bootstrap_tokens_check BEFORE INSERT OR UPDATE ON bootstrap_tokens
  FOR EACH ROW EXECUTE FUNCTION bootstrap_tokens_check();

CREATE TABLE fleet_policy (
  account_id     uuid PRIMARY KEY REFERENCES accounts(id),
  routing        text NOT NULL DEFAULT 'own_only' CHECK (routing IN ('own_only','own_first','hosted')),
  allowed_shapes text[] NOT NULL DEFAULT '{}',
  quotas         jsonb NOT NULL DEFAULT '{}',
  egress         text[] NOT NULL DEFAULT '{}',
  plan           text NOT NULL DEFAULT 'free' CHECK (plan IN ('free','paid')),
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE images (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id uuid REFERENCES accounts(id),
  name       text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
  version    text NOT NULL CHECK (version ~ '^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$'),
  os         text NOT NULL CHECK (os IN ('linux','windows','macos')),
  arch       text NOT NULL CHECK (arch IN ('amd64','arm64')),
  format     text NOT NULL CHECK (format IN ('rootfs','qcow2','vhdx','ami','tart')),
  uri        text NOT NULL,
  digest     text NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
  signature  text NOT NULL,
  contents   jsonb NOT NULL DEFAULT '{}',
  built_at   timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (account_id, name, version, arch, format)
);

CREATE TABLE usage_records (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id uuid NOT NULL REFERENCES accounts(id),
  machine_id uuid NOT NULL UNIQUE,
  shape_id   uuid NOT NULL,
  ownership  text NOT NULL CHECK (ownership IN ('own','hosted')),
  minutes    numeric(12,3) NOT NULL CHECK (minutes >= 0),
  cost       numeric(14,6) NOT NULL CHECK (cost >= 0),
  started_at timestamptz NOT NULL,
  ended_at   timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (machine_id, account_id, shape_id) REFERENCES machines (id, account_id, shape_id),
  CHECK (ended_at >= started_at)
);
CREATE INDEX usage_records_account ON usage_records (account_id, ended_at);
```

### Configuration (migration 00010)

See [config-resources.md](config-resources.md).

```sql
CREATE TABLE assistant_conversations (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id uuid NOT NULL REFERENCES accounts(id),
  project_id uuid NOT NULL REFERENCES projects(id),
  owner      text NOT NULL,                 -- domain.Actor.String()
  credential text NOT NULL DEFAULT '',      -- domain.Credential.String()
  title      text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX assistant_conversations_owner ON assistant_conversations (account_id, owner, created_at DESC);

CREATE TABLE assistant_messages (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id      uuid NOT NULL REFERENCES accounts(id),
  conversation_id uuid NOT NULL REFERENCES assistant_conversations(id) ON DELETE CASCADE,
  author          text NOT NULL CHECK (author IN ('user','assistant')),
  body            text NOT NULL,
  job_id          uuid REFERENCES jobs(id),
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX assistant_messages_conversation ON assistant_messages (conversation_id, created_at, id);
CREATE UNIQUE INDEX assistant_messages_turn ON assistant_messages (job_id, author) WHERE job_id IS NOT NULL;

CREATE TABLE config_plans (
  id                uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id        uuid NOT NULL REFERENCES accounts(id),
  status            text NOT NULL CHECK (status IN ('pending','awaiting_approval','applied','canceled','reverted','expired')),
  source            text NOT NULL CHECK (source IN ('form','assistant','yaml','cli','mcp')),
  client            text NOT NULL DEFAULT '',
  title             text NOT NULL,
  request           text NOT NULL DEFAULT '',
  applies           text NOT NULL,          -- now, or pr:owner/name
  changes           jsonb NOT NULL,         -- []domain.ConfigChange, with the previous values
  steps             jsonb NOT NULL DEFAULT '[]',
  needs             text[] NOT NULL DEFAULT '{}',
  affects           text NOT NULL DEFAULT '',
  cost              text NOT NULL DEFAULT '',
  undo              text NOT NULL DEFAULT '',
  reversible        boolean NOT NULL,
  lowers_protection text[] NOT NULL DEFAULT '{}',
  confirm_word      text NOT NULL DEFAULT '',
  base_digest       text NOT NULL,
  untrusted         text NOT NULL DEFAULT '',
  created_by        text NOT NULL,          -- domain.Actor.String()
  conversation_id   uuid REFERENCES assistant_conversations(id) ON DELETE SET NULL,
  job_id            uuid REFERENCES jobs(id),
  reverts           uuid REFERENCES config_plans(id),
  expires_at        timestamptz NOT NULL,
  applied_by        text,
  applied_at        timestamptz,
  pr_url            text,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  CHECK ((status IN ('applied','reverted')) = (applied_at IS NOT NULL)),
  CHECK ((cardinality(lowers_protection) > 0) <= (confirm_word <> ''))
);
CREATE INDEX config_plans_account ON config_plans (account_id, created_at DESC);
CREATE INDEX config_plans_open ON config_plans (account_id, expires_at) WHERE status IN ('pending','awaiting_approval');

-- account_id NULL: the scope of the whole server.
CREATE TABLE config_settings (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id uuid REFERENCES accounts(id),
  kind       text NOT NULL,
  name       text NOT NULL,
  spec       jsonb NOT NULL,
  updated_by text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (account_id, kind, name)
);

-- account_id NULL: the scope of the whole server. AES-256-GCM under
-- FORECREW_SECRET_KEY, with the scope and the name as additional data.
CREATE TABLE secrets (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id uuid REFERENCES accounts(id),
  name       text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9._/-]{0,127}$' AND name !~ '\.\.'),
  key_id     text NOT NULL,                 -- which key encrypted it (secrets.KeyID)
  nonce      bytea NOT NULL,
  ciphertext bytea NOT NULL,
  updated_by text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (account_id, name)
);
```

## Leasing query

`Jobs.LeaseNext(account, worker, labels, leaseFor)`; `labels` is normalized.
The worker's pool is read from its row, never from the request: a worker
only leases jobs of its pool, whatever labels it sends. Command jobs need no
runtime label:

```sql
UPDATE jobs SET status = 'running', worker_id = $2, started_at = now(),
       lease_expires_at = now() + $4, updated_at = now()
WHERE id = (
  SELECT id FROM jobs
  WHERE account_id = $1 AND status = 'queued'
    AND (run_after IS NULL OR run_after <= now())
    AND pool = (SELECT w.pool FROM workers w WHERE w.id = $2 AND w.account_id = $1)
    AND (needs || CASE WHEN kind = 'agent' THEN ARRAY[lower('runtime:' || runtime)]
                       ELSE '{}'::text[] END) <@ $3::text[]
  ORDER BY created_at, id
  LIMIT 1
  FOR UPDATE SKIP LOCKED
)
RETURNING *;
```

From T58 an ephemeral worker also leases only jobs whose `resources` fit its
machine's shape, and only while `machines.jobs_taken` is below the shape's
`max_jobs`; the lease adds one in the same transaction, with a conditional
update that gives `ErrConflict` to the loser of two concurrent polls
(`MachineRepo.TakeJob`; fleet.md, Machines). `resources` do not restrict a static worker.

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
| Goal, GoalSize | goals | `Goals()` |
| Spec | specs | `Specs()` |
| Task | tasks | `Tasks()` |
| Change | changes | `Changes()` |
| Job, JobSpec, Usage, ContextManifest, Resources | jobs | `Jobs()` |
| JobEvent | job_events | `JobEvents()` |
| Gate | gates | `Gates()` |
| Signal | signals | `Signals()` |
| Worker | workers | `Workers()` |
| WorkflowRun | workflow_runs | `WorkflowRuns()` |
| StepRun | step_runs | `StepRuns()` |
| WebhookDelivery | webhook_deliveries | `WebhookDeliveries()` |
| MCPToken | mcp_tokens | `MCPTokens()`; `store.PersonalTokenRepo` from T52 |
| AuditEntry | audit_log | `Audit()` |
| ConfigPlan | config_plans | `ConfigPlans()` |
| ConfigSetting | config_settings | `ConfigSettings()` |
| StoredSecret | secrets | `Secrets()` |
| AssistantConversation, AssistantMessage | assistant_conversations, assistant_messages | `Conversations()` |
| Artifact | artifacts | `store.ArtifactRepo` (added to `Store` by T21) |
| TestRun, TestResult | test_runs, test_results | `store.TestRunRepo` (added to `Store` by T22) |
| VerificationReport | verification_reports | `store.VerificationReportRepo` (added to `Store` by T27) |
| Council, CouncilBrief | councils | `store.CouncilRepo` (added to `Store` by T33) |
| CouncilContribution | council_contributions | `store.CouncilRepo` (T33) |
| Assumption, StatedAssumption, Dependent | assumptions | `store.AssumptionRepo` (added to `Store` by T32) |
| Decision | decisions | `store.DecisionRepo` (added to `Store` by T32) |
| KnowledgeEntry, Provenance | knowledge_entries | `store.KnowledgeRepo` (added to `Store` by T36) |
| KnowledgeProposal | knowledge_proposals | `store.KnowledgeProposalRepo` (added to `Store` by T37) |
| Proposal | proposals | `store.ProposalRepo` (added to `Store` by T50) |
| User | users | `store.UserRepo` (added to `Store` by T52) |
| Identity | identities | `store.IdentityRepo` (T52) |
| Membership | memberships | `store.MembershipRepo` (T52) |
| Invitation, Grant | invitations | `store.InvitationRepo` (T52) |
| Connection, JIT | connections | `store.ConnectionRepo` (T52) |
| GroupMapping | group_mappings | `store.ConnectionRepo` (T56) |
| Session | sessions | `store.SessionRepo` (T52) |
| Team, TeamMember | teams, team_members | `store.TeamRepo` (added to `Store` by T53) |
| ServiceAccount | service_accounts | `store.ServiceAccountRepo` (T53) |
| Role | roles | `store.RoleRepo` (T53) |
| RoleBinding | role_bindings | `store.RoleBindingRepo` (T53) |
| OAuthClient, OAuthGrant | oauth_clients, oauth_grants | `store.OAuthRepo` (added to `Store` by T55) |
| VerifiedDomain | verified_domains | `store.VerifiedDomainRepo` (added to `Store` by T56) |
| SCIMToken | scim_tokens | `store.SCIMTokenRepo` (T56) |
| CapacityProvider, ProviderLimits, ProviderCapabilities | capacity_providers | `store.CapacityProviderRepo` (added to `Store` by T58) |
| ProviderJoinCode | provider_join_codes | `store.ProviderJoinCodeRepo` (T59) |
| Shape | shapes | `store.ShapeRepo` (T58) |
| Machine | machines | `store.MachineRepo` (T58) |
| BootstrapToken | bootstrap_tokens | `store.BootstrapTokenRepo` (T58) |
| FleetPolicy, FleetQuotas | fleet_policy | `store.FleetPolicyRepo` (T58) |
| Image, ImageContents | images | `store.ImageRepo` (T60) |
| UsageRecord | usage_records | `store.UsageRepo` (T65) |
| (human ref numbers) | ref_counters | used by `Create` |
