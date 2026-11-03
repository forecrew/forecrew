-- +goose Up
-- Councils, registers, knowledge and improvement proposals
-- (docs/contracts/data-model.md). Existing goals become feature goals.
ALTER TABLE ref_counters DROP CONSTRAINT ref_counters_kind_check;
ALTER TABLE ref_counters ADD CONSTRAINT ref_counters_kind_check
  CHECK (kind IN ('GOAL','SPEC','TASK','JOB','GATE','COUNCIL','A','DEC','KB','PROP'));

ALTER TABLE gates DROP CONSTRAINT gates_kind_check;
ALTER TABLE gates ADD CONSTRAINT gates_kind_check
  CHECK (kind IN ('profile_approval','spec_approval','merge_approval','test_plan_approval','needs_you',
                  'assumption_review','council_decision','knowledge_proposal','access_request','improvement_proposal'));

ALTER TABLE signals DROP CONSTRAINT signals_kind_check;
ALTER TABLE signals ADD CONSTRAINT signals_kind_check
  CHECK (kind IN ('job.completed','gate.resolved','pr.merged','pr.closed','checks.completed','council.decided'));

-- ask_human questions are needs_you gates of their job, told apart from
-- playbook gates by origin.
ALTER TABLE gates ADD COLUMN origin text NOT NULL DEFAULT 'playbook' CHECK (origin IN ('playbook','ask_human'));
ALTER TABLE gates ADD CONSTRAINT gates_ask_human_check
  CHECK (origin <> 'ask_human' OR (kind = 'needs_you' AND job_id IS NOT NULL));

ALTER TABLE goals ADD COLUMN kind text NOT NULL DEFAULT 'feature' CHECK (kind IN ('feature','bug'));
ALTER TABLE goals ADD COLUMN size jsonb;
ALTER TABLE jobs ADD COLUMN context_manifest jsonb;

-- The SHA-256 of the job MCP token of the current lease (job-mcp.md); the
-- token itself is never stored. The trigger clears it whenever the lease
-- ends (complete, cancel, release, requeue, expiry) or a new lease starts
-- without a new hash, so no other code path can keep a dead token alive.
ALTER TABLE jobs ADD COLUMN job_mcp_token_hash bytea
  CHECK (job_mcp_token_hash IS NULL OR (status = 'running' AND octet_length(job_mcp_token_hash) = 32));
CREATE UNIQUE INDEX jobs_job_mcp_token ON jobs (job_mcp_token_hash) WHERE job_mcp_token_hash IS NOT NULL;
-- +goose StatementBegin
CREATE FUNCTION jobs_clear_job_mcp_token() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.status <> 'running'
     OR (NEW.job_mcp_token_hash IS NOT DISTINCT FROM OLD.job_mcp_token_hash
         AND (NEW.worker_id IS DISTINCT FROM OLD.worker_id OR NEW.started_at IS DISTINCT FROM OLD.started_at)) THEN
    NEW.job_mcp_token_hash := NULL;
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER jobs_clear_job_mcp_token BEFORE UPDATE ON jobs
  FOR EACH ROW EXECUTE FUNCTION jobs_clear_job_mcp_token();

CREATE TABLE councils (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid NOT NULL REFERENCES accounts(id),
  project_id  uuid NOT NULL REFERENCES projects(id),
  goal_id     uuid REFERENCES goals(id),
  number      bigint NOT NULL,
  kind        text NOT NULL CHECK (kind IN ('onboarding','product','technical','test_design','incident','adhoc')),
  question    text NOT NULL,
  constraints text[] NOT NULL DEFAULT '{}',
  status      text NOT NULL CHECK (status IN ('running','decided','failed','canceled')),
  brief       jsonb,
  gate_id     uuid REFERENCES gates(id),
  run_id      uuid REFERENCES workflow_runs(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, number),
  CHECK (status <> 'decided' OR brief IS NOT NULL)
);
CREATE INDEX councils_project ON councils (account_id, project_id, created_at);
CREATE INDEX councils_goal    ON councils (account_id, goal_id) WHERE goal_id IS NOT NULL;

CREATE TABLE council_contributions (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id uuid NOT NULL REFERENCES accounts(id),
  council_id uuid NOT NULL REFERENCES councils(id),
  role       text NOT NULL,
  runtime    text NOT NULL DEFAULT '',
  stage      text NOT NULL CHECK (stage IN ('proposal','critique','revision','synthesis')),
  round      integer NOT NULL CHECK (round >= 1 AND round <= CASE stage WHEN 'revision' THEN 3 WHEN 'synthesis' THEN 2 ELSE 1 END),
  job_id     uuid UNIQUE REFERENCES jobs(id),
  content    jsonb NOT NULL,
  evidence_check jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (council_id, role, runtime, stage, round)
);
CREATE INDEX council_contributions_council ON council_contributions (council_id, created_at);

CREATE TABLE assumptions (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid NOT NULL REFERENCES accounts(id),
  project_id  uuid NOT NULL REFERENCES projects(id),
  number      bigint NOT NULL,
  goal_id     uuid REFERENCES goals(id),
  statement   text NOT NULL,
  made_by     text NOT NULL,
  job_id      uuid REFERENCES jobs(id),
  spec_id     uuid REFERENCES specs(id),
  council_id  uuid REFERENCES councils(id),
  confidence  text NOT NULL CHECK (confidence IN ('low','medium','high')),
  impact      text NOT NULL CHECK (impact IN ('low','medium','high')),
  status      text NOT NULL CHECK (status IN ('unverified','verified','invalidated','accepted')),
  verify_by   text NOT NULL CHECK (verify_by IN ('test','ask-human','research')),
  evidence    text NOT NULL DEFAULT '',
  dependents  jsonb NOT NULL DEFAULT '[]',
  resolved_by text,
  resolved_at timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, number),
  CHECK ((status = 'unverified') = (resolved_at IS NULL))
);
CREATE INDEX assumptions_project ON assumptions (account_id, project_id, status, impact);
CREATE INDEX assumptions_goal    ON assumptions (account_id, goal_id) WHERE goal_id IS NOT NULL;

CREATE TABLE decisions (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid NOT NULL REFERENCES accounts(id),
  project_id  uuid NOT NULL REFERENCES projects(id),
  number      bigint NOT NULL,
  goal_id     uuid REFERENCES goals(id),
  question    text NOT NULL,
  options     jsonb NOT NULL DEFAULT '[]',
  choice      text NOT NULL,
  reasons     text NOT NULL DEFAULT '',
  dissent     text NOT NULL DEFAULT '',
  decided_by  text NOT NULL,
  council_id  uuid REFERENCES councils(id),
  gate_id     uuid REFERENCES gates(id),
  assumptions uuid[] NOT NULL DEFAULT '{}',
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, number),
  CHECK (decided_by <> 'policy' OR council_id IS NOT NULL)
);
CREATE INDEX decisions_project ON decisions (account_id, project_id, created_at);
CREATE INDEX decisions_goal    ON decisions (account_id, goal_id) WHERE goal_id IS NOT NULL;

CREATE TABLE knowledge_entries (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id      uuid NOT NULL REFERENCES accounts(id),
  project_id      uuid NOT NULL REFERENCES projects(id),
  provider        text NOT NULL,
  path            text NOT NULL,
  source_url      text NOT NULL DEFAULT '',
  version         text NOT NULL DEFAULT '',
  section         text NOT NULL,
  title           text NOT NULL,
  body            text NOT NULL DEFAULT '',
  body_hash       text NOT NULL CHECK (body_hash ~ '^[0-9a-f]{64}$'),
  provenance      jsonb NOT NULL DEFAULT '{}',
  generated       boolean NOT NULL DEFAULT false,
  needs_reverify  boolean NOT NULL DEFAULT false,
  reverify_reason text NOT NULL DEFAULT '',
  scope           text CHECK (scope IN ('goal','project')),
  goal_id         uuid REFERENCES goals(id),
  expires_at      timestamptz,
  tags            text[] NOT NULL DEFAULT '{}',
  synced_at       timestamptz NOT NULL DEFAULT now(),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  search          tsvector GENERATED ALWAYS AS
                    (setweight(to_tsvector('english', title), 'A') || setweight(to_tsvector('english', body), 'B')) STORED,
  UNIQUE (account_id, project_id, provider, path),
  CHECK ((provider = 'internal') = (section = 'notes')),
  CHECK ((provider = 'internal') = (scope IS NOT NULL)),
  CHECK ((coalesce(scope, '') = 'goal') = (goal_id IS NOT NULL)),
  CHECK (provider = 'internal' OR (expires_at IS NULL AND tags = '{}')),
  CHECK (generated = (section = 'generated'))
);
CREATE INDEX knowledge_entries_search   ON knowledge_entries USING gin (search);
CREATE INDEX knowledge_entries_section  ON knowledge_entries (account_id, project_id, section);
CREATE INDEX knowledge_entries_goal     ON knowledge_entries (account_id, goal_id) WHERE goal_id IS NOT NULL;
CREATE INDEX knowledge_entries_expires  ON knowledge_entries (expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX knowledge_entries_reverify ON knowledge_entries (account_id, project_id) WHERE needs_reverify;

-- Embeddings are optional: the column exists only where pgvector is (or
-- can be) installed. Search works without it.
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') THEN
    IF NOT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector') THEN
      RAISE NOTICE 'pgvector is not available: knowledge search uses full-text search only';
      RETURN;
    END IF;
    BEGIN
      CREATE EXTENSION vector;
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE NOTICE 'no privilege to create pgvector: knowledge search uses full-text search only';
      RETURN;
    END;
  END IF;
  ALTER TABLE knowledge_entries ADD COLUMN embedding vector;
END $$;
-- +goose StatementEnd

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
  proposed_by  text NOT NULL,
  job_id       uuid REFERENCES jobs(id),
  goal_id      uuid REFERENCES goals(id),
  batch        text NOT NULL CHECK (batch IN ('per_goal','standalone')),
  external_ref text NOT NULL DEFAULT '',
  change_id    uuid REFERENCES changes(id),
  gate_id      uuid REFERENCES gates(id),
  status       text NOT NULL CHECK (status IN ('queued','open','accepted','rejected')),
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, number),
  CHECK (section NOT IN ('generated','notes') AND provider <> 'internal'),
  CHECK (batch <> 'per_goal' OR goal_id IS NOT NULL)
);
CREATE INDEX knowledge_proposals_queued ON knowledge_proposals (account_id, goal_id) WHERE status = 'queued';
CREATE INDEX knowledge_proposals_open   ON knowledge_proposals (account_id, project_id) WHERE status = 'open';

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
  decided_by      text,
  reason          text,
  outcome         jsonb,
  gate_id         uuid REFERENCES gates(id),
  goal_id         uuid REFERENCES goals(id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, number),
  CHECK ((status = 'snoozed') = (snoozed_until IS NOT NULL))
);
CREATE UNIQUE INDEX proposals_dedupe ON proposals (account_id, project_id, dedupe_key)
  WHERE status IN ('proposed','accepted','snoozed');
CREATE INDEX proposals_project ON proposals (account_id, project_id, status);

-- +goose Down
DROP TABLE proposals;
DROP TABLE knowledge_proposals;
DROP TABLE knowledge_entries;
DROP TABLE decisions;
DROP TABLE assumptions;
DROP TABLE council_contributions;
DROP TABLE councils;
DROP TRIGGER jobs_clear_job_mcp_token ON jobs;
DROP FUNCTION jobs_clear_job_mcp_token();
ALTER TABLE jobs DROP COLUMN job_mcp_token_hash;
ALTER TABLE jobs DROP COLUMN context_manifest;
ALTER TABLE goals DROP COLUMN size;
ALTER TABLE goals DROP COLUMN kind;
ALTER TABLE signals DROP CONSTRAINT signals_kind_check;
ALTER TABLE signals ADD CONSTRAINT signals_kind_check
  CHECK (kind IN ('job.completed','gate.resolved','pr.merged','pr.closed','checks.completed'));
ALTER TABLE gates DROP COLUMN origin;
ALTER TABLE gates DROP CONSTRAINT gates_kind_check;
ALTER TABLE gates ADD CONSTRAINT gates_kind_check
  CHECK (kind IN ('profile_approval','spec_approval','merge_approval','test_plan_approval','needs_you'));
DELETE FROM ref_counters WHERE kind IN ('COUNCIL','A','DEC','KB','PROP');
ALTER TABLE ref_counters DROP CONSTRAINT ref_counters_kind_check;
ALTER TABLE ref_counters ADD CONSTRAINT ref_counters_kind_check
  CHECK (kind IN ('GOAL','SPEC','TASK','JOB','GATE'));
