-- Retention (ADR 0010, section 12): DELETED sessions are purged after
-- PLAYGROUND_SESSION_RETENTION, audit events after PLAYGROUND_AUDIT_RETENTION.
--
-- The append-only trigger refused every DELETE, so not even the retention job
-- could purge audit rows. Two group roles fix that:
--
--   playground_app        what the API connects as: no DELETE on audit_events.
--   playground_retention  what the retention job switches to (SET LOCAL ROLE):
--                         SELECT and DELETE on audit_events only.
--
-- The trigger now allows a DELETE only when current_user is
-- playground_retention, so a DELETE granted to the app by mistake is still
-- refused. UPDATE and TRUNCATE stay refused for everyone.
--
-- Both roles are NOLOGIN groups: an operator creates login users and grants
-- them membership, for example
--   CREATE ROLE api_prod LOGIN PASSWORD '...' IN ROLE playground_app;
--   CREATE ROLE retention_prod LOGIN PASSWORD '...' IN ROLE playground_retention;
-- Roles belong to the whole cluster, not this database, so they are created
-- only if missing and are never dropped here.

-- +goose Up

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'playground_app') THEN
        CREATE ROLE playground_app NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'playground_retention') THEN
        CREATE ROLE playground_retention NOLOGIN;
    END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO playground_app, playground_retention;

-- The app deletes DELETED session rows itself; they are not append-only.
GRANT SELECT, INSERT, UPDATE, DELETE ON playground_sessions TO playground_app;
GRANT SELECT, INSERT, UPDATE ON users TO playground_app;
GRANT SELECT, INSERT ON audit_events TO playground_app;

GRANT SELECT, DELETE ON audit_events TO playground_retention;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION audit_events_are_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND current_user = 'playground_retention' THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit_events is append-only';
END;
$$;
-- +goose StatementEnd

-- The session purge's work list: DELETED rows, oldest ending first.
CREATE INDEX sessions_deleted_by_terminated ON playground_sessions (terminated_at)
    WHERE status = 'DELETED';

-- +goose Down
-- For resetting a local database only. The roles stay: other databases in
-- the cluster may use them.

DROP INDEX IF EXISTS sessions_deleted_by_terminated;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION audit_events_are_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only';
END;
$$;
-- +goose StatementEnd

REVOKE ALL ON audit_events FROM playground_app, playground_retention;
REVOKE ALL ON users FROM playground_app;
REVOKE ALL ON playground_sessions FROM playground_app;
REVOKE USAGE ON SCHEMA public FROM playground_app, playground_retention;
