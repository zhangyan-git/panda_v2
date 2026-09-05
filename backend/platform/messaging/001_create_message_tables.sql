-- Message durability tables. Execute this migration in each service-owned database.
-- +goose Up
CREATE TABLE IF NOT EXISTS message_outbox (
    event_id TEXT PRIMARY KEY CHECK (char_length(trim(event_id)) > 0),
    event_type TEXT NOT NULL DEFAULT '',
    event_version TEXT NOT NULL DEFAULT '',
    trace_id TEXT NOT NULL DEFAULT '',
    payload BYTEA NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at TIMESTAMPTZ,
    lease_owner TEXT,
    lease_token TEXT,
    lease_until TIMESTAMPTZ,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS message_inbox (
    event_id TEXT PRIMARY KEY CHECK (char_length(trim(event_id)) > 0),
    claimed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_owner TEXT,
    lease_token TEXT,
    lease_until TIMESTAMPTZ,
    completed_at TIMESTAMPTZ
);

-- Add columns before creating indexes so this migration also works on tables
-- created by the original schema, which did not include lease columns.
ALTER TABLE message_outbox ADD COLUMN IF NOT EXISTS lease_owner TEXT;
ALTER TABLE message_outbox ADD COLUMN IF NOT EXISTS lease_token TEXT;
ALTER TABLE message_outbox ADD COLUMN IF NOT EXISTS lease_until TIMESTAMPTZ;
ALTER TABLE message_outbox ADD COLUMN IF NOT EXISTS last_error TEXT;
ALTER TABLE message_inbox ADD COLUMN IF NOT EXISTS lease_owner TEXT;
ALTER TABLE message_inbox ADD COLUMN IF NOT EXISTS lease_token TEXT;
ALTER TABLE message_inbox ADD COLUMN IF NOT EXISTS lease_until TIMESTAMPTZ;
ALTER TABLE message_inbox ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS message_outbox_pending_idx
    ON message_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;

CREATE INDEX IF NOT EXISTS message_outbox_lease_idx
    ON message_outbox (lease_until)
    WHERE published_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS message_inbox;
DROP TABLE IF EXISTS message_outbox;
