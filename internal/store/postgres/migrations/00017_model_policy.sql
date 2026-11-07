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

-- project_id NULL: every project of the organization.
CREATE TABLE model_authorizations (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id    uuid NOT NULL REFERENCES accounts(id),
  project_id    uuid REFERENCES projects(id),
  connection_id uuid NOT NULL REFERENCES model_connections(id) ON DELETE CASCADE,
  connection    text NOT NULL,
  adapter       text NOT NULL,
  base_url      text NOT NULL DEFAULT '',
  authorized_by text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (account_id, connection_id, project_id)
);

-- +goose Down
DROP TABLE model_authorizations;
-- +goose StatementBegin
DO $$
BEGIN
  ALTER TABLE roles DISABLE TRIGGER roles_built_in_fixed;
  UPDATE roles SET permissions = array_remove(permissions, 'models.authorize'), updated_at = now() WHERE built_in;
  ALTER TABLE roles ENABLE TRIGGER roles_built_in_fixed;
END $$;
-- +goose StatementEnd
