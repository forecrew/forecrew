-- +goose Up
-- Job kinds and worker pools (docs/contracts/data-model.md). Existing jobs
-- are agent jobs; existing jobs and workers are in pool build.
ALTER TABLE jobs ADD COLUMN kind text NOT NULL DEFAULT 'agent' CHECK (kind IN ('agent','command'));
ALTER TABLE jobs ADD COLUMN pool text NOT NULL DEFAULT 'build';
ALTER TABLE workers ADD COLUMN pool text NOT NULL DEFAULT 'build';
DROP INDEX jobs_queued;
CREATE INDEX jobs_queued ON jobs (account_id, pool, created_at, id) WHERE status = 'queued';

ALTER TABLE gates DROP CONSTRAINT gates_kind_check;
ALTER TABLE gates ADD CONSTRAINT gates_kind_check
  CHECK (kind IN ('profile_approval','spec_approval','merge_approval','test_plan_approval','needs_you'));

CREATE TABLE artifacts (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id   uuid NOT NULL REFERENCES accounts(id),
  project_id   uuid NOT NULL REFERENCES projects(id),
  job_id       uuid REFERENCES jobs(id),       -- NULL for artifacts forecrewd makes
  name         text NOT NULL,
  content_type text NOT NULL,
  size         bigint NOT NULL CHECK (size >= 0),
  sha256       text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
  storage_key  text NOT NULL UNIQUE,
  created_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (job_id, name)
);
CREATE INDEX artifacts_created ON artifacts (account_id, created_at);

CREATE TABLE test_runs (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id       uuid NOT NULL REFERENCES accounts(id),
  project_id       uuid NOT NULL REFERENCES projects(id),
  job_id           uuid NOT NULL REFERENCES jobs(id),
  change_id        uuid REFERENCES changes(id),
  sha              text NOT NULL,
  suite            text NOT NULL,
  environment      text NOT NULL DEFAULT '',
  cell             jsonb NOT NULL DEFAULT '{}',
  status           text NOT NULL CHECK (status IN ('passed','failed','errored')),
  counts           jsonb NOT NULL DEFAULT '{}',
  duration_ms      bigint NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
  exit_code        integer NOT NULL,
  versions         jsonb NOT NULL DEFAULT '{}',
  report_artifacts text[] NOT NULL DEFAULT '{}',
  attempt          integer NOT NULL CHECK (attempt >= 1),
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

CREATE TABLE verification_reports (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid NOT NULL REFERENCES accounts(id),
  project_id  uuid NOT NULL REFERENCES projects(id),
  change_id   uuid NOT NULL REFERENCES changes(id),
  sha         text NOT NULL,
  status      text NOT NULL CHECK (status IN ('passed','failed','incomplete')),
  report      jsonb NOT NULL,
  artifact_id uuid REFERENCES artifacts(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (change_id, sha)
);

-- +goose Down
DROP TABLE verification_reports;
DROP TABLE test_results;
DROP TABLE test_runs;
DROP TABLE artifacts;
ALTER TABLE gates DROP CONSTRAINT gates_kind_check;
ALTER TABLE gates ADD CONSTRAINT gates_kind_check
  CHECK (kind IN ('profile_approval','spec_approval','merge_approval','needs_you'));
DROP INDEX jobs_queued;
CREATE INDEX jobs_queued ON jobs (account_id, created_at, id) WHERE status = 'queued';
ALTER TABLE workers DROP COLUMN pool;
ALTER TABLE jobs DROP COLUMN pool;
ALTER TABLE jobs DROP COLUMN kind;
