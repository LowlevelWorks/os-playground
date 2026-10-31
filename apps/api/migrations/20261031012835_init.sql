-- +goose Up
CREATE TYPE STATUS AS ENUM ('CREATED', 'PROVISIONING', 'STARTING','RUNNING','STOPPING', 'STOPPED','FAILED', 'DELETING', 'DELETED');

CREATE TABLE users (
    id  uuid PRIMARY KEY DEFAULT uuidv7(),
    issuer text NOT NULL, 
    subject text NOT NULL, 
    email text, 
    display_name text, 
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    disabled_at timestamptz,

    UNIQUE(issuer,subject)
);

CREATE TABLE playground_sessions (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    owner_id uuid NOT NULL  REFERENCES users(id),
    client_key text NOT NULL,
    profile text NOT NULL, 
    spec jsonb NOT NULL,
    image_ref text NOT NULL, 
    status STATUS NOT NULL DEFAULT 'CREATED',
    failure_reason text,
   
    created_at         timestamptz NOT NULL DEFAULT now(),
    started_at         timestamptz,
    terminated_at      timestamptz,
    vm_released_at     timestamptz,
    expires_at         timestamptz NOT NULL,
    idle_expires_at    timestamptz NOT NULL,
    last_activity_at   timestamptz,

    attach_holder      text,
    attach_lease_until timestamptz,

    claimed_by         text,
    claim_until        timestamptz,
    next_attempt_at    timestamptz NOT NULL DEFAULT now(),
    attempts           integer NOT NULL DEFAULT 0,

    CHECK (status <> 'DELETED' OR vm_released_at IS NOT NULL),
    CHECK ((attach_holder IS NULL) = (attach_lease_until IS NULL)),
    CHECK ((claimed_by IS NULL) = (claim_until IS NULL))

);

CREATE INDEX sessions_not_deleted ON playground_sessions(next_attempt_at) where status <> 'DELETED';
CREATE INDEX sessions_holding_vm ON playground_sessions(id) where vm_released_at IS NULL; 


CREATE TABLE audit_events (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    event_type  text NOT NULL,
    request_id  text,
    user_id     text,
    session_id  text,
    metadata    jsonb NOT NULL DEFAULT '{}'
);

CREATE INDEX audit_by_session ON audit_events(session_id, id);
CREATE INDEX audit_by_user ON audit_events(user_id, id);


-- +goose StatementBegin 
create function audit_events_are_append_only()
returns trigger
language plpgsql
as $$ 
BEGIN 
    RAISE EXCEPTION 'audit_events is append-only';
END;
$$
;
-- +goose StatementEnd
CREATE TRIGGER audit_events_no_change 
BEFORE UPDATE OR DELETE on 
audit_events FOR EACH ROW 
EXECUTE FUNCTION audit_events_are_append_only();

CREATE TRIGGER audit_events_no_truncate 
BEFORE TRUNCATE ON 
audit_events FOR EACH STATEMENT 
EXECUTE FUNCTION audit_events_are_append_only();

-- +goose Down
DROP TABLE audit_events;
drop function audit_events_are_append_only()
;
DROP TABLE playground_sessions;
DROP TABLE IF EXISTS users;
