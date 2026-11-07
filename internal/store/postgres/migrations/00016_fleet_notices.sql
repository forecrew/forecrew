-- +goose Up
-- What the instance's operator must look at: a machine the abuse checks
-- stopped. One per machine and kind, so a report sent again opens none.
CREATE TABLE fleet_notices (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid REFERENCES accounts(id),
  machine_id  uuid NOT NULL REFERENCES machines(id),
  provider_id uuid NOT NULL REFERENCES capacity_providers(id),
  job_id      uuid,
  kind        text NOT NULL CHECK (kind IN ('cpu','mining')),
  detail      text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now(),
  resolved_at timestamptz,
  resolved_by text NOT NULL DEFAULT '',
  UNIQUE (machine_id, kind),
  CHECK ((resolved_at IS NULL) = (resolved_by = ''))
);
CREATE INDEX fleet_notices_open ON fleet_notices (created_at) WHERE resolved_at IS NULL;

-- +goose Down
DROP TABLE fleet_notices;
