-- +goose Up
-- Organizations, people and access (docs/contracts/identity.md and
-- permissions.md). accounts stay the storage of organizations. Existing rows
-- keep working and nothing is deleted: the organization gets a slug, and
-- existing tokens stay ownerless (scope admin) until they are assigned to
-- the first Owner.

ALTER TABLE accounts ADD COLUMN slug text;
ALTER TABLE accounts ADD COLUMN created_by text;               -- domain.Actor.String(); NULL for the one created at install
ALTER TABLE accounts ADD COLUMN max_token_ttl interval CHECK (max_token_ttl > interval '0');  -- NULL: no cap
-- domain.Slugify(name), oldest account first; a slug already taken gets the
-- first free -2, -3, ..., so this cannot clash whatever the names are.
-- +goose StatementBegin
DO $$
DECLARE
  a record;
  base text;
  candidate text;
  n int;
BEGIN
  FOR a IN SELECT id, name FROM accounts ORDER BY created_at, id LOOP
    base := coalesce(nullif(rtrim(left(trim(both '-' from regexp_replace(lower(a.name), '[^a-z0-9]+', '-', 'g')), 63), '-'), ''), 'org');
    candidate := base;
    n := 1;
    WHILE EXISTS (SELECT 1 FROM accounts WHERE slug = candidate) LOOP
      n := n + 1;
      candidate := rtrim(left(base, 62 - length(n::text)), '-') || '-' || n;
    END LOOP;
    UPDATE accounts SET slug = candidate WHERE id = a.id;
  END LOOP;
END $$;
-- +goose StatementEnd
ALTER TABLE accounts ALTER COLUMN slug SET NOT NULL;
ALTER TABLE accounts ADD CONSTRAINT accounts_slug_check CHECK (slug ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$');
ALTER TABLE accounts ADD CONSTRAINT accounts_slug_key UNIQUE (slug);

-- Composite keys so rows of one organization can only point at rows of
-- the same organization.
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
-- never stored, only where to read it. An organization's connection is
-- limited to a reserved namespace, because whoever manages it controls the
-- issuer the secret is sent to. store: is the secret store's form (T66),
-- accepted here so it needs no change to this CHECK and refused by
-- domain.ValidSecretRef until the store exists.
CREATE TABLE connections (
  id                uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id        uuid REFERENCES accounts(id),
  kind              text NOT NULL CHECK (kind IN ('github','google','microsoft','oidc')),
  name              text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
  issuer            text NOT NULL DEFAULT '',
  client_id         text NOT NULL CHECK (client_id <> ''),
  client_secret_ref text NOT NULL,
  jit               jsonb NOT NULL DEFAULT '{"enabled": false, "default_roles": []}'
                      CHECK (jsonb_typeof(jit->'enabled') = 'boolean' AND jsonb_typeof(jit->'default_roles') = 'array'),
  group_claim       text NOT NULL DEFAULT '',
  enabled           boolean NOT NULL DEFAULT true,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (account_id, name),
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
INSERT INTO roles (name, scope, description, permissions, built_in, custom)
SELECT name, scope, description, permissions::text[], true, false FROM (VALUES
  ('owner', 'organization', 'Everything, including ownership and billing',
   '{assumption.resolve,audit.read,billing.manage,connection.manage,council.start,gate.approve.access_request,gate.approve.assumption,gate.approve.council,gate.approve.improvement,gate.approve.knowledge,gate.approve.knowledge_protected,gate.approve.merge,gate.approve.needs_you,gate.approve.profile,gate.approve.spec,gate.approve.test_plan,gate.read,goal.create,goal.read,job.cancel,job.dispatch,job.read,knowledge.propose,member.manage,org.manage,org.read,pool.manage,project.create,project.manage,project.read,proposal.decide,role.manage,service_account.manage,team.manage,token.manage,worker.manage,worker.read}'),
  ('admin', 'organization', 'Everything but ownership and billing',
   '{assumption.resolve,audit.read,connection.manage,council.start,gate.approve.access_request,gate.approve.assumption,gate.approve.council,gate.approve.improvement,gate.approve.knowledge,gate.approve.knowledge_protected,gate.approve.merge,gate.approve.needs_you,gate.approve.profile,gate.approve.spec,gate.approve.test_plan,gate.read,goal.create,goal.read,job.cancel,job.dispatch,job.read,knowledge.propose,member.manage,org.read,pool.manage,project.create,project.manage,project.read,proposal.decide,role.manage,service_account.manage,team.manage,token.manage,worker.manage,worker.read}'),
  ('billing', 'organization', 'Billing only',
   '{billing.manage,org.read}'),
  ('member', 'organization', 'Reads every project, creates goals and proposes knowledge',
   '{gate.read,goal.create,goal.read,job.read,knowledge.propose,org.read,project.read,worker.read}'),
  ('viewer', 'organization', 'Reads every project',
   '{gate.read,goal.read,job.read,org.read,project.read,worker.read}'),
  ('project_admin', 'project', 'Everything on the project, its access included',
   '{assumption.resolve,council.start,gate.approve.access_request,gate.approve.assumption,gate.approve.council,gate.approve.improvement,gate.approve.knowledge,gate.approve.knowledge_protected,gate.approve.merge,gate.approve.needs_you,gate.approve.profile,gate.approve.spec,gate.approve.test_plan,gate.read,goal.create,goal.read,job.cancel,job.dispatch,job.read,knowledge.propose,project.manage,project.read,proposal.decide}'),
  ('maintainer', 'project', 'Runs the project and resolves every gate',
   '{assumption.resolve,council.start,gate.approve.access_request,gate.approve.assumption,gate.approve.council,gate.approve.improvement,gate.approve.knowledge,gate.approve.knowledge_protected,gate.approve.merge,gate.approve.needs_you,gate.approve.profile,gate.approve.spec,gate.approve.test_plan,gate.read,goal.create,goal.read,job.cancel,job.dispatch,job.read,knowledge.propose,project.read,proposal.decide}'),
  ('contributor', 'project', 'Creates goals and jobs, approves specs, councils and questions, not merges',
   '{council.start,gate.approve.assumption,gate.approve.council,gate.approve.knowledge,gate.approve.needs_you,gate.approve.spec,gate.read,goal.create,goal.read,job.cancel,job.dispatch,job.read,knowledge.propose,project.read}'),
  ('viewer', 'project', 'Reads the project',
   '{gate.read,goal.read,job.read,project.read}')
) AS r (name, scope, description, permissions);
-- +goose StatementBegin
CREATE FUNCTION roles_built_in_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (TG_OP = 'INSERT' AND NEW.built_in) OR (TG_OP <> 'INSERT' AND OLD.built_in) THEN
    RAISE EXCEPTION 'built-in roles change only by migration' USING ERRCODE = 'check_violation', CONSTRAINT = 'roles_built_in_fixed';
  END IF;
  RETURN CASE TG_OP WHEN 'DELETE' THEN OLD ELSE NEW END;
END $$;
-- +goose StatementEnd
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
-- +goose StatementBegin
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
-- +goose StatementEnd
CREATE TRIGGER role_bindings_check BEFORE INSERT OR UPDATE ON role_bindings
  FOR EACH ROW EXECUTE FUNCTION role_bindings_check();

-- An organization that has an active Owner keeps one: a user with an
-- active membership, not disabled, bound to the built-in owner role on the
-- organization. Removing or demoting the last one, suspending or removing
-- their membership, or disabling the user fails with check_violation on
-- constraint last_owner (store.ErrLastOwner). The organization row is
-- locked (FOR NO KEY UPDATE) so concurrent changes are checked one after
-- the other; this relies on READ COMMITTED, the store's isolation level.
-- +goose StatementBegin
CREATE FUNCTION is_active_owner(acc uuid, usr uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
  SELECT EXISTS (
    SELECT 1 FROM role_bindings b
    JOIN roles r ON r.id = b.role_id AND r.built_in AND r.scope = 'organization' AND r.name = 'owner'
    JOIN memberships m ON m.account_id = b.account_id AND m.user_id = b.subject_id AND m.status = 'active'
    JOIN users u ON u.id = b.subject_id AND u.disabled_at IS NULL
    WHERE b.account_id = acc AND b.subject_kind = 'user' AND b.project_id IS NULL
      AND (usr IS NULL OR b.subject_id = usr))
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION keep_an_owner(acc uuid) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  PERFORM 1 FROM accounts WHERE id = acc FOR NO KEY UPDATE;
  IF NOT is_active_owner(acc, NULL) THEN
    RAISE EXCEPTION 'organization % must keep an active owner', acc USING ERRCODE = 'check_violation', CONSTRAINT = 'last_owner';
  END IF;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
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
-- +goose StatementEnd
CREATE TRIGGER role_bindings_keep_owner AFTER UPDATE OR DELETE ON role_bindings
  FOR EACH ROW EXECUTE FUNCTION role_bindings_keep_owner();
-- +goose StatementBegin
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
-- +goose StatementEnd
CREATE TRIGGER memberships_keep_owner AFTER UPDATE OR DELETE ON memberships
  FOR EACH ROW EXECUTE FUNCTION memberships_keep_owner();
-- +goose StatementBegin
CREATE FUNCTION users_keep_owner() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  acc uuid;
BEGIN
  IF OLD.disabled_at IS NULL AND NEW.disabled_at IS NOT NULL THEN
    FOR acc IN SELECT m.account_id FROM memberships m
               JOIN role_bindings b ON b.account_id = m.account_id AND b.subject_kind = 'user' AND b.subject_id = m.user_id AND b.project_id IS NULL
               JOIN roles r ON r.id = b.role_id AND r.built_in AND r.scope = 'organization' AND r.name = 'owner'
               WHERE m.user_id = OLD.id AND m.status = 'active'
               ORDER BY m.account_id LOOP
      PERFORM keep_an_owner(acc);
    END LOOP;
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
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

-- A mapping's connection is instance-level or of the mapping's organization.
-- +goose StatementBegin
CREATE FUNCTION group_mappings_check() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM connections WHERE id = NEW.connection_id AND (account_id IS NULL OR account_id = NEW.account_id)) THEN
    RAISE EXCEPTION 'connection % is not usable by the organization', NEW.connection_id USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'group_mappings_connection';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER group_mappings_check BEFORE INSERT OR UPDATE ON group_mappings
  FOR EACH ROW EXECUTE FUNCTION group_mappings_check();

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

-- Existing tokens become personal or service account tokens. Rows from
-- before this migration have no owner and scope admin until T52 assigns
-- them to the first Owner. Names are unique per organization and owner.
ALTER TABLE mcp_tokens ADD COLUMN owner_kind text CHECK (owner_kind IN ('user','service_account'));
ALTER TABLE mcp_tokens ADD COLUMN owner_id uuid;
ALTER TABLE mcp_tokens ADD COLUMN scopes text[] NOT NULL DEFAULT '{admin}'
  CHECK (cardinality(scopes) > 0 AND scopes <@ ARRAY['read','write','admin']);
ALTER TABLE mcp_tokens ALTER COLUMN scopes DROP DEFAULT;
ALTER TABLE mcp_tokens ADD COLUMN expires_at timestamptz;
ALTER TABLE mcp_tokens ADD CONSTRAINT mcp_tokens_owner_check CHECK ((owner_kind IS NULL) = (owner_id IS NULL));
ALTER TABLE mcp_tokens DROP CONSTRAINT mcp_tokens_account_id_name_key;
CREATE UNIQUE INDEX mcp_tokens_owner_name ON mcp_tokens (account_id, owner_kind, owner_id, name) NULLS NOT DISTINCT;
CREATE INDEX mcp_tokens_owner ON mcp_tokens (owner_kind, owner_id) WHERE owner_id IS NOT NULL;

-- A token's owner is a member or service account of its organization when
-- it is set; a later change to the membership leaves the token row alone, so a
-- removed member's token can still be listed and revoked.
-- +goose StatementBegin
CREATE FUNCTION mcp_tokens_check_owner() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  found_owner boolean;
BEGIN
  found_owner := CASE NEW.owner_kind
      WHEN 'user' THEN EXISTS (SELECT 1 FROM memberships WHERE account_id = NEW.account_id AND user_id = NEW.owner_id)
      ELSE EXISTS (SELECT 1 FROM service_accounts WHERE account_id = NEW.account_id AND id = NEW.owner_id) END;
  IF NOT found_owner THEN
    RAISE EXCEPTION '% % is not in the organization', NEW.owner_kind, NEW.owner_id USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'mcp_tokens_owner';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER mcp_tokens_check_owner BEFORE INSERT OR UPDATE OF owner_kind, owner_id, account_id ON mcp_tokens
  FOR EACH ROW WHEN (NEW.owner_id IS NOT NULL) EXECUTE FUNCTION mcp_tokens_check_owner();

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

-- The credential (session:<id>, token:<id>, oauth_grant:<id>) an actor used.
ALTER TABLE audit_log ADD COLUMN credential text;
CREATE INDEX audit_log_actor ON audit_log (account_id, actor, created_at DESC);

-- +goose Down
DROP INDEX audit_log_actor;
ALTER TABLE audit_log DROP COLUMN credential;
DROP TABLE scim_tokens;
DROP TABLE oauth_grants;
DROP TABLE oauth_clients;
DROP TRIGGER mcp_tokens_check_owner ON mcp_tokens;
DROP FUNCTION mcp_tokens_check_owner();
DROP INDEX mcp_tokens_owner;
DROP INDEX mcp_tokens_owner_name;
ALTER TABLE mcp_tokens ADD CONSTRAINT mcp_tokens_account_id_name_key UNIQUE (account_id, name);
ALTER TABLE mcp_tokens DROP CONSTRAINT mcp_tokens_owner_check;
ALTER TABLE mcp_tokens DROP COLUMN expires_at;
ALTER TABLE mcp_tokens DROP COLUMN scopes;
ALTER TABLE mcp_tokens DROP COLUMN owner_id;
ALTER TABLE mcp_tokens DROP COLUMN owner_kind;
DROP TABLE sessions;
DROP TABLE verified_domains;
DROP TABLE group_mappings;
DROP FUNCTION group_mappings_check();
DROP TRIGGER users_keep_owner ON users;
DROP TRIGGER memberships_keep_owner ON memberships;
DROP TABLE role_bindings;
DROP FUNCTION users_keep_owner();
DROP FUNCTION memberships_keep_owner();
DROP FUNCTION role_bindings_keep_owner();
DROP FUNCTION keep_an_owner(uuid);
DROP FUNCTION is_active_owner(uuid, uuid);
DROP FUNCTION role_bindings_check();
DROP TABLE roles;
DROP FUNCTION roles_built_in_fixed();
DROP TABLE service_accounts;
DROP TABLE team_members;
DROP TABLE teams;
DROP TABLE invitations;
DROP TABLE memberships;
DROP TABLE identities;
DROP TABLE connections;
DROP TABLE users;
DROP INDEX projects_account_id;
ALTER TABLE accounts DROP COLUMN max_token_ttl;
ALTER TABLE accounts DROP COLUMN created_by;
ALTER TABLE accounts DROP COLUMN slug;
