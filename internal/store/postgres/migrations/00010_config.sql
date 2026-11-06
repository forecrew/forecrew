-- +goose Up
-- Configuration resources, change plans, server settings over forecrew.yaml,
-- the secret store and the assistant's conversations
-- (docs/contracts/config-resources.md).
-- +goose StatementBegin
DO $$
BEGIN
  ALTER TABLE roles DISABLE TRIGGER roles_built_in_fixed;
  UPDATE roles SET permissions = (SELECT array_agg(p ORDER BY p COLLATE "C") FROM unnest(permissions || '{config.apply,config.read}'::text[]) AS p), updated_at = now()
    WHERE built_in AND scope = 'organization' AND name IN ('owner', 'admin', 'billing', 'member');
  UPDATE roles SET permissions = (SELECT array_agg(p ORDER BY p COLLATE "C") FROM unnest(permissions || '{config.read}'::text[]) AS p), updated_at = now()
    WHERE built_in AND scope = 'organization' AND name = 'viewer';
  ALTER TABLE roles ENABLE TRIGGER roles_built_in_fixed;
END $$;
-- +goose StatementEnd

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
  nonce      bytea NOT NULL,
  ciphertext bytea NOT NULL,
  updated_by text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (account_id, name)
);

-- +goose Down
DROP TABLE secrets;
DROP TABLE config_settings;
DROP TABLE config_plans;
DROP TABLE assistant_messages;
DROP TABLE assistant_conversations;
-- +goose StatementBegin
DO $$
BEGIN
  ALTER TABLE roles DISABLE TRIGGER roles_built_in_fixed;
  UPDATE roles SET permissions = array_remove(array_remove(permissions, 'config.read'), 'config.apply'), updated_at = now() WHERE built_in;
  ALTER TABLE roles ENABLE TRIGGER roles_built_in_fixed;
END $$;
-- +goose StatementEnd
