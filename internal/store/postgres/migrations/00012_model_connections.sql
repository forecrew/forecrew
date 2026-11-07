-- +goose Up
-- Model connections and assignments per organization
-- (docs/contracts/config-resources.md).
-- +goose StatementBegin
DO $$
BEGIN
  ALTER TABLE roles DISABLE TRIGGER roles_built_in_fixed;
  UPDATE roles SET permissions = (SELECT array_agg(p ORDER BY p COLLATE "C") FROM unnest(permissions || '{models.assign,models.manage,models.read}'::text[]) AS p), updated_at = now()
    WHERE built_in AND scope = 'organization' AND name IN ('owner', 'admin');
  UPDATE roles SET permissions = (SELECT array_agg(p ORDER BY p COLLATE "C") FROM unnest(permissions || '{models.read}'::text[]) AS p), updated_at = now()
    WHERE built_in AND scope = 'organization' AND name IN ('member', 'viewer');
  UPDATE roles SET permissions = (SELECT array_agg(p ORDER BY p COLLATE "C") FROM unnest(permissions || '{models.assign}'::text[]) AS p), updated_at = now()
    WHERE built_in AND scope = 'project' AND name = 'project_admin';
  ALTER TABLE roles ENABLE TRIGGER roles_built_in_fixed;
END $$;
-- +goose StatementEnd

CREATE TABLE model_connections (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id uuid NOT NULL REFERENCES accounts(id),
  name       text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9-]*$'),
  vendor     text NOT NULL CHECK (vendor <> ''),
  adapter    text NOT NULL CHECK (adapter IN ('claude-code','codex','opencode')),
  base_url   text NOT NULL DEFAULT '',
  key_env    text NOT NULL DEFAULT '',
  secret_ref text NOT NULL CHECK (secret_ref ~ '^store:models/.'),
  enabled    boolean NOT NULL DEFAULT true,
  updated_by text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, name)
);

-- project_id NULL: the organization's row, the only one with the policy.
CREATE TABLE model_assignments (
  id                uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id        uuid NOT NULL REFERENCES accounts(id),
  project_id        uuid REFERENCES projects(id),
  tiers             jsonb NOT NULL DEFAULT '{}',
  roles             jsonb NOT NULL DEFAULT '{}',
  project_overrides boolean,
  updated_by        text NOT NULL,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (account_id, project_id),
  CHECK (project_id IS NULL OR project_overrides IS NULL)
);

-- +goose Down
DROP TABLE model_assignments;
DROP TABLE model_connections;
-- +goose StatementBegin
DO $$
BEGIN
  ALTER TABLE roles DISABLE TRIGGER roles_built_in_fixed;
  UPDATE roles SET permissions = array_remove(array_remove(array_remove(permissions, 'models.read'), 'models.manage'), 'models.assign'), updated_at = now() WHERE built_in;
  ALTER TABLE roles ENABLE TRIGGER roles_built_in_fixed;
END $$;
-- +goose StatementEnd
