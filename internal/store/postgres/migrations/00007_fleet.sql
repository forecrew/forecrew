-- +goose Up
-- The fleet (docs/contracts/fleet.md): capacity providers, shapes, machines
-- and what they need. Existing workers stay static (ephemeral false, no
-- machine row) and existing jobs ask for no resources.

ALTER TABLE workers ADD COLUMN ephemeral boolean NOT NULL DEFAULT false;
ALTER TABLE jobs ADD COLUMN resources jsonb NOT NULL DEFAULT '{}';

-- Composite keys so a machine can only point at a worker or job of its own
-- organization.
CREATE UNIQUE INDEX workers_account_id ON workers (account_id, id);
CREATE UNIQUE INDEX jobs_account_id ON jobs (account_id, id);

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

-- A shape and its provider have the same organization, or are both
-- instance-wide.
-- +goose StatementBegin
CREATE FUNCTION shapes_check() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM capacity_providers p WHERE p.id = NEW.provider_id AND p.account_id IS NOT DISTINCT FROM NEW.account_id) THEN
    RAISE EXCEPTION 'a shape and its provider have the same organization' USING ERRCODE = 'check_violation', CONSTRAINT = 'shapes_check';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER shapes_check BEFORE INSERT OR UPDATE ON shapes
  FOR EACH ROW EXECUTE FUNCTION shapes_check();

-- A provider or shape in use never changes organization, which would carry
-- its machines along.
-- +goose StatementBegin
CREATE FUNCTION fleet_account_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.account_id IS DISTINCT FROM OLD.account_id THEN
    RAISE EXCEPTION '% keep their organization', TG_TABLE_NAME USING ERRCODE = 'check_violation', CONSTRAINT = 'fleet_account_fixed';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
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
-- A worker only comes from the fleet: ephemeral.
-- +goose StatementBegin
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
  IF NEW.worker_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM workers w WHERE w.id = NEW.worker_id AND w.ephemeral) THEN
    RAISE EXCEPTION 'the worker of a machine is ephemeral' USING ERRCODE = 'check_violation', CONSTRAINT = 'machines_check';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
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

-- A bootstrap token has its machine's organization.
-- +goose StatementBegin
CREATE FUNCTION bootstrap_tokens_check() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM machines m WHERE m.id = NEW.machine_id AND (NEW.account_id IS NULL OR m.account_id = NEW.account_id)) THEN
    RAISE EXCEPTION 'a bootstrap token has its machine''s organization' USING ERRCODE = 'check_violation', CONSTRAINT = 'bootstrap_tokens_check';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
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
  machine_id uuid NOT NULL UNIQUE REFERENCES machines(id),
  shape_id   uuid NOT NULL REFERENCES shapes(id),
  ownership  text NOT NULL CHECK (ownership IN ('own','hosted')),
  minutes    numeric(12,3) NOT NULL CHECK (minutes >= 0),
  cost       numeric(14,6) NOT NULL CHECK (cost >= 0),
  started_at timestamptz NOT NULL,
  ended_at   timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (ended_at >= started_at)
);
CREATE INDEX usage_records_account ON usage_records (account_id, ended_at);

-- +goose Down
DROP TABLE usage_records;
DROP TABLE images;
DROP TABLE fleet_policy;
DROP TABLE bootstrap_tokens;
DROP FUNCTION bootstrap_tokens_check();
DROP TABLE machines;
DROP FUNCTION machines_check();
DROP TABLE shapes;
DROP FUNCTION shapes_check();
DROP TABLE provider_join_codes;
DROP TABLE capacity_providers;
DROP FUNCTION fleet_account_fixed();
DROP INDEX jobs_account_id;
DROP INDEX workers_account_id;
ALTER TABLE jobs DROP COLUMN resources;
ALTER TABLE workers DROP COLUMN ephemeral;
