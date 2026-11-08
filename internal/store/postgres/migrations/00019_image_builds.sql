-- +goose Up
-- Builds of a Windows image on a location's host, asked for from the web, REST,
-- MCP or CLI and done by the location's fleet agent. A row holds references
-- (the ISO's path on the host and its checksum) and the progress the agent
-- reports: never the ISO, a credential or the image itself. A location builds one
-- image at a time.
CREATE TABLE image_builds (
  id                   uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id           uuid REFERENCES accounts(id),   -- the location's; NULL for an instance-wide one
  provider_id          uuid NOT NULL REFERENCES capacity_providers(id),
  state                text NOT NULL DEFAULT 'requested' CHECK (state IN ('requested','running','done','failed')),
  version              text NOT NULL CHECK (version ~ '^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$'),
  iso                  text NOT NULL CHECK (iso <> ''),
  iso_checksum         text NOT NULL CHECK (iso_checksum ~* '^sha256:[0-9a-f]{64}$'),
  chocolatey_sha256    text NOT NULL CHECK (chocolatey_sha256 ~* '^[0-9a-f]{64}$'),
  windows_image_name   text NOT NULL DEFAULT '',
  license_confirmed_by text NOT NULL CHECK (license_confirmed_by <> ''),  -- domain.Actor.String()
  step                 text NOT NULL DEFAULT '',
  percent              integer NOT NULL DEFAULT 0 CHECK (percent BETWEEN 0 AND 100),
  error                text NOT NULL DEFAULT '',
  manifest             text NOT NULL DEFAULT '',        -- path on the agent's host: unsigned
  digest               text NOT NULL DEFAULT '',
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  started_at           timestamptz,
  finished_at          timestamptz,
  CHECK ((state IN ('done','failed')) = (finished_at IS NOT NULL))
);
CREATE INDEX image_builds_provider ON image_builds (provider_id, created_at DESC);
CREATE UNIQUE INDEX image_builds_one_open ON image_builds (provider_id) WHERE state IN ('requested','running');
CREATE TRIGGER image_builds_check BEFORE INSERT OR UPDATE ON image_builds
  FOR EACH ROW EXECUTE FUNCTION provider_scope_check();
CREATE TRIGGER image_builds_account_fixed BEFORE UPDATE ON image_builds
  FOR EACH ROW EXECUTE FUNCTION fleet_account_fixed();

-- +goose Down
DROP TABLE image_builds;
