-- +goose Up
ALTER TABLE playground_sessions
    ADD COLUMN desired text NOT NULL DEFAULT 'RUNNING'
        CHECK (desired IN ('RUNNING', 'DELETED'));

-- "Live" sessions count toward a user's or client's quota. Queries must repeat
-- this predicate exactly for PostgreSQL to use these partial indexes.
CREATE INDEX sessions_live_by_owner ON playground_sessions (owner_id)
    WHERE desired = 'RUNNING' AND status IN ('CREATED','PROVISIONING','STARTING','RUNNING');
CREATE INDEX sessions_live_by_client ON playground_sessions (client_key)
    WHERE desired = 'RUNNING' AND status IN ('CREATED','PROVISIONING','STARTING','RUNNING');

-- +goose Down
DROP INDEX IF EXISTS sessions_live_by_client;
DROP INDEX IF EXISTS sessions_live_by_owner;
ALTER TABLE playground_sessions DROP COLUMN desired;
