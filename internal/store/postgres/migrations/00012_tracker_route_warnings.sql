-- +goose Up
-- A route says when the tracker would not take a state Forecrew wrote: no
-- transition reaches it, or one needs fields Forecrew does not have. Routes
-- of a connection get one position each, so a route has a name: the
-- connection and its position.
ALTER TABLE tracker_routes ADD COLUMN warning text NOT NULL DEFAULT '';
UPDATE tracker_routes r SET position = n.rn
  FROM (SELECT id, row_number() OVER (PARTITION BY connection_id ORDER BY position, created_at, id) AS rn FROM tracker_routes) n
  WHERE r.id = n.id AND r.position <> n.rn;
ALTER TABLE tracker_routes ADD CONSTRAINT tracker_routes_position UNIQUE (connection_id, position);

-- +goose Down
ALTER TABLE tracker_routes DROP CONSTRAINT tracker_routes_position;
ALTER TABLE tracker_routes DROP COLUMN warning;
