-- +goose Up
-- Private code policy, key checks and suitability tests of model connections
-- (docs/contracts/config-resources.md, Private code).
-- +goose StatementBegin
DO $$
BEGIN
  ALTER TABLE roles DISABLE TRIGGER roles_built_in_fixed;
  UPDATE roles SET permissions = (SELECT array_agg(p ORDER BY p COLLATE "C") FROM unnest(permissions || '{models.authorize}'::text[]) AS p), updated_at = now()
    WHERE built_in AND scope = 'organization' AND name IN ('owner', 'admin');
  ALTER TABLE roles ENABLE TRIGGER roles_built_in_fixed;
END $$;
-- +goose StatementEnd

ALTER TABLE jobs DROP CONSTRAINT jobs_kind_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_kind_check CHECK (kind IN ('agent','command','model_check'));

-- What people entered of models the catalog does not know.
ALTER TABLE model_connections ADD COLUMN models jsonb NOT NULL DEFAULT '{}';

-- project_id NULL: every project of the organization.
CREATE TABLE model_authorizations (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id    uuid NOT NULL REFERENCES accounts(id),
  project_id    uuid REFERENCES projects(id),
  connection_id uuid NOT NULL REFERENCES model_connections(id) ON DELETE CASCADE,
  connection    text NOT NULL,
  adapter       text NOT NULL,
  base_url      text NOT NULL DEFAULT '',
  vendor        text NOT NULL,
  key_env       text NOT NULL DEFAULT '',
  authorized_by text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (account_id, connection_id, project_id)
);

-- The last key check of a connection (model '') and the last suitability
-- test of each of its models.
CREATE TABLE model_checks (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id    uuid NOT NULL REFERENCES accounts(id),
  connection_id uuid NOT NULL REFERENCES model_connections(id) ON DELETE CASCADE,
  connection    text NOT NULL,
  model         text NOT NULL DEFAULT '',
  job_id        uuid NOT NULL REFERENCES jobs(id),
  status        text NOT NULL,
  result        jsonb,
  stale         boolean NOT NULL DEFAULT false,
  created_by    text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, connection_id, model)
);
CREATE INDEX model_checks_job ON model_checks (account_id, job_id);

-- +goose Down
DROP TABLE model_checks;
DELETE FROM jobs WHERE kind = 'model_check';
ALTER TABLE jobs DROP CONSTRAINT jobs_kind_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_kind_check CHECK (kind IN ('agent','command'));
DROP TABLE model_authorizations;
ALTER TABLE model_connections DROP COLUMN models;
-- +goose StatementBegin
DO $$
BEGIN
  ALTER TABLE roles DISABLE TRIGGER roles_built_in_fixed;
  UPDATE roles SET permissions = array_remove(permissions, 'models.authorize'), updated_at = now() WHERE built_in;
  ALTER TABLE roles ENABLE TRIGGER roles_built_in_fixed;
END $$;
-- +goose StatementEnd
