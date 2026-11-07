-- +goose Up
-- Trackers (docs/contracts/tracker.md): connections, routes, who is who, how
-- far polling got, the tickets Forecrew was handed and the writes on their
-- way back. Also the permissions and the gate kind of tickets, and the way to
-- cancel a run from outside it.

CREATE TABLE tracker_connections (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id uuid NOT NULL REFERENCES accounts(id),
  name       text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
  kind       text NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_]{0,63}$'),  -- an open list: a new tracker is one package
  config     jsonb NOT NULL DEFAULT '{}',   -- the adapter's settings, never a secret
  triggers   jsonb NOT NULL DEFAULT '{}',   -- domain.TrackerTriggers
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, name),
  UNIQUE (account_id, id)
);
CREATE INDEX tracker_connections_kind ON tracker_connections (kind);

CREATE TABLE tracker_routes (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id    uuid NOT NULL REFERENCES accounts(id),
  connection_id uuid NOT NULL,
  position      integer NOT NULL CHECK (position > 0),
  match         jsonb NOT NULL DEFAULT '{}',   -- domain.TrackerMatch
  project_id    uuid NOT NULL,
  goal_kind     text CHECK (goal_kind IN ('feature','bug')),   -- NULL goes by the ticket's type
  playbook      text,                                          -- NULL is the project's goal playbook
  states        jsonb NOT NULL DEFAULT '{}',   -- logical state -> the tracker's state
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (account_id, connection_id) REFERENCES tracker_connections (account_id, id),
  FOREIGN KEY (account_id, project_id) REFERENCES projects (account_id, id)
);
CREATE INDEX tracker_routes_connection ON tracker_routes (account_id, connection_id, position);

CREATE TABLE tracker_identities (
  id            uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id    uuid NOT NULL REFERENCES accounts(id),
  connection_id uuid NOT NULL,
  external_id   text NOT NULL CHECK (external_id <> ''),   -- the tracker's stable user ID
  external_name text NOT NULL DEFAULT '',
  user_id       uuid NOT NULL REFERENCES users(id),
  created_by    text NOT NULL,                             -- domain.Actor.String()
  created_at    timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (account_id, connection_id) REFERENCES tracker_connections (account_id, id),
  UNIQUE (connection_id, external_id)
);

CREATE TABLE tracker_cursors (
  connection_id uuid PRIMARY KEY,
  account_id    uuid NOT NULL REFERENCES accounts(id),
  cursor        text NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (account_id, connection_id) REFERENCES tracker_connections (account_id, id)
);

CREATE TABLE ticket_links (
  id             uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id     uuid NOT NULL REFERENCES accounts(id),
  connection_id  uuid NOT NULL,
  key            text NOT NULL CHECK (key <> ''),
  title          text NOT NULL DEFAULT '',      -- ticket text: untrusted
  url            text NOT NULL DEFAULT '',
  status         text NOT NULL CHECK (status IN ('waiting','queued','active','done','canceled','dismissed')),
  reason         text CHECK (reason IN ('no_route','many_projects','unknown_person','not_permitted')),
  project_id     uuid,                          -- NULL: no route gave one project
  route_id       uuid REFERENCES tracker_routes(id) ON DELETE SET NULL,
  goal_id        uuid UNIQUE REFERENCES goals(id),
  handed_by      text NOT NULL DEFAULT '',
  user_id        uuid REFERENCES users(id),     -- the goal starts as this user
  gate_id        uuid REFERENCES gates(id),
  change_gate_id uuid REFERENCES gates(id),
  state          text CHECK (state IN ('queued','in_progress','in_review','done','canceled')),
  content_hash   text NOT NULL DEFAULT '',
  comment_cursor text NOT NULL DEFAULT '',
  issue_number   integer,                       -- the ticket is this goal issue
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (account_id, connection_id) REFERENCES tracker_connections (account_id, id),
  FOREIGN KEY (account_id, project_id) REFERENCES projects (account_id, id),
  CHECK (status <> 'active' OR goal_id IS NOT NULL),
  CHECK (status <> 'queued' OR project_id IS NOT NULL)
);
-- One live link per ticket: one goal at work, and a redelivery finds it.
CREATE UNIQUE INDEX ticket_links_live ON ticket_links (connection_id, key) WHERE status IN ('waiting','queued','active');
CREATE INDEX ticket_links_ticket ON ticket_links (account_id, connection_id, key);
CREATE INDEX ticket_links_project ON ticket_links (account_id, project_id, status);

CREATE TABLE tracker_outbox (
  id              uuid PRIMARY KEY DEFAULT uuidv7(),
  account_id      uuid NOT NULL REFERENCES accounts(id),
  connection_id   uuid NOT NULL,
  link_id         uuid NOT NULL REFERENCES ticket_links(id),
  key             text NOT NULL,
  dedupe_key      text NOT NULL CHECK (dedupe_key <> ''),
  kind            text NOT NULL CHECK (kind IN ('comment','transition')),
  value           text NOT NULL,
  attempts        integer NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  sent_at         timestamptz,
  external_id     text NOT NULL DEFAULT '',
  last_error      text NOT NULL DEFAULT '',
  created_at      timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (account_id, connection_id) REFERENCES tracker_connections (account_id, id),
  UNIQUE (link_id, dedupe_key)
);
CREATE INDEX tracker_outbox_due ON tracker_outbox (account_id, next_attempt_at) WHERE sent_at IS NULL;
CREATE INDEX tracker_outbox_sent ON tracker_outbox (link_id, external_id) WHERE external_id <> '';

ALTER TABLE gates DROP CONSTRAINT gates_kind_check;
ALTER TABLE gates ADD CONSTRAINT gates_kind_check
  CHECK (kind IN ('profile_approval','spec_approval','merge_approval','test_plan_approval','needs_you',
                  'assumption_review','council_decision','knowledge_proposal','access_request','improvement_proposal',
                  'ticket_intake'));

ALTER TABLE workflow_runs ADD COLUMN cancel_reason text NOT NULL DEFAULT '';

ALTER TABLE roles DISABLE TRIGGER roles_built_in_fixed;
UPDATE roles SET permissions = (SELECT array_agg(p ORDER BY p COLLATE "C") FROM unnest(permissions || '{trackers.read}'::text[]) AS p)
  WHERE built_in AND name <> 'billing';
UPDATE roles SET permissions = (SELECT array_agg(p ORDER BY p COLLATE "C") FROM unnest(permissions || '{trackers.manage}'::text[]) AS p)
  WHERE built_in AND scope = 'organization' AND name IN ('owner','admin');
ALTER TABLE roles ENABLE TRIGGER roles_built_in_fixed;

-- +goose Down
ALTER TABLE roles DISABLE TRIGGER roles_built_in_fixed;
UPDATE roles SET permissions = array_remove(array_remove(permissions, 'trackers.read'), 'trackers.manage') WHERE built_in;
ALTER TABLE roles ENABLE TRIGGER roles_built_in_fixed;

ALTER TABLE workflow_runs DROP COLUMN cancel_reason;

DROP TABLE tracker_outbox;
DROP TABLE ticket_links;
DROP TABLE tracker_cursors;
DROP TABLE tracker_identities;
DROP TABLE tracker_routes;
DROP TABLE tracker_connections;

DELETE FROM gates WHERE kind = 'ticket_intake';
ALTER TABLE gates DROP CONSTRAINT gates_kind_check;
ALTER TABLE gates ADD CONSTRAINT gates_kind_check
  CHECK (kind IN ('profile_approval','spec_approval','merge_approval','test_plan_approval','needs_you',
                  'assumption_review','council_decision','knowledge_proposal','access_request','improvement_proposal'));
