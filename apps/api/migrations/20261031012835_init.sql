-- +goose Up

-- IDs are uuidv7: time-ordered like ULIDs, native in PostgreSQL 18, 16 bytes.
-- Their text form is lowercase hex and hyphens, a valid DNS label for KubeVirt.

CREATE TABLE users (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    issuer       text NOT NULL,
    subject      text NOT NULL,
    email        text,
    display_name text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    disabled_at  timestamptz,
    UNIQUE (issuer, subject)
);

CREATE TABLE playground_sessions (
    id                 uuid PRIMARY KEY DEFAULT uuidv7(),
    -- Opaque: the anonymous owner token hash until OIDC lands, then the user
    -- id. No foreign key until then (ADR 0010, section 3).
    owner_id           text NOT NULL,
    client_key         text NOT NULL,
    profile            text NOT NULL,
    spec               jsonb NOT NULL,
    image_ref          text NOT NULL,

    status             text NOT NULL DEFAULT 'CREATED'
        CHECK (status IN ('CREATED','PROVISIONING','STARTING','RUNNING',
                          'STOPPING','STOPPED','FAILED','DELETING','DELETED')),
    failure_reason     text,

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

-- The reconciler's work queue: everything that is not DELETED.
CREATE INDEX sessions_not_deleted ON playground_sessions (next_attempt_at)
    WHERE status <> 'DELETED';
-- Capacity: a row holds a VM (or is about to) until vm_released_at is set.
CREATE INDEX sessions_holding_vm ON playground_sessions (id)
    WHERE vm_released_at IS NULL;

CREATE TABLE audit_events (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    event_type  text NOT NULL,
    request_id  text,
    user_id     text,
    session_id  text,
    metadata    jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_by_session ON audit_events (session_id, id);
CREATE INDEX audit_by_user    ON audit_events (user_id, id);

-- +goose StatementBegin
CREATE FUNCTION audit_events_are_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_events_no_change
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_are_append_only();
CREATE TRIGGER audit_events_no_truncate
    BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION audit_events_are_append_only();

-- +goose Down
-- For resetting a local database only. Production migrations move forward.
DROP TABLE audit_events;
DROP FUNCTION audit_events_are_append_only();
DROP TABLE playground_sessions;
DROP TABLE users;
