-- Retention:  DELETED sessions are purged after
-- PLAYGROUND_SESSION_RETENTION, audit events after PLAYGROUND_AUDIT_RETENTION.
--
-- The append-only trigger refused every DELETE, so not even the retention job
-- could purge audit rows. Two group roles fix that:
--
-- playground_app        what the API connects as: no DELETE on audit_events.
-- playground_retention  what the retention job switches to (SET LOCAL ROLE):
-- SELECT and DELETE on audit_events only.
--
-- The trigger now allows a DELETE only when current_user is
-- playground_retention, so a DELETE granted to the app by mistake is still
-- refused. UPDATE and TRUNCATE stay refused for everyone.
--
-- Both roles are NOLOGIN groups: an operator creates login users and grants
-- them membership, for example
-- CREATE ROLE api_prod LOGIN PASSWORD '...' IN ROLE playground_app;
-- CREATE ROLE retention_prod LOGIN PASSWORD '...' IN ROLE playground_retention;
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
grant usage
on schema public
to playground_app, playground_retention
;

-- The app deletes DELETED session rows itself; they are not append-only.
grant select, insert, update, delete
on playground_sessions
to playground_app
;
grant select, insert, update
on users
to playground_app
;
grant select, insert
on audit_events
to playground_app
;

grant select, delete
on audit_events
to playground_retention
;

-- +goose StatementBegin
create or replace function audit_events_are_append_only()
returns trigger
language plpgsql
as $$
BEGIN
    IF TG_OP = 'DELETE' AND current_user = 'playground_retention' THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit_events is append-only';
END;
$$
;
-- +goose StatementEnd
-- The session purge's work list: DELETED rows, oldest ending first.
CREATE INDEX sessions_deleted_by_terminated ON playground_sessions (terminated_at)
    WHERE status = 'DELETED';

-- +goose Down
-- For resetting a local database only. The roles stay: other databases in
-- the cluster may use them.
DROP INDEX IF EXISTS sessions_deleted_by_terminated;

-- +goose StatementBegin
create or replace function audit_events_are_append_only()
returns trigger
language plpgsql
as $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only';
END;
$$
;
-- +goose StatementEnd
revoke all
on audit_events
from playground_app, playground_retention
;
revoke all
on users
from playground_app
;
revoke all
on playground_sessions
from playground_app
;
revoke usage
on schema public
from playground_app, playground_retention
;
