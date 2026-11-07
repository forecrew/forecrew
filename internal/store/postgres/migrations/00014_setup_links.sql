-- +goose Up
-- One-time links that make the first Owner of an organization: printed by
-- forecrew setup-link on the server, single use and short lived. Only the
-- SHA-256 of the token is stored.
CREATE TABLE setup_links (
  id          uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id  uuid NOT NULL REFERENCES accounts(id),
  token_hash  bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  expires_at  timestamptz NOT NULL,
  used_by     uuid REFERENCES users(id),
  used_at     timestamptz,
  created_by  text NOT NULL,                              -- domain.Actor.String()
  created_at  timestamptz NOT NULL DEFAULT now(),
  CHECK ((used_by IS NULL) = (used_at IS NULL))
);
CREATE INDEX setup_links_account ON setup_links (account_id, created_at);

-- +goose Down
DROP TABLE setup_links;
