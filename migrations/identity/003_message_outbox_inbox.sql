-- identity/003: outbox / inbox 表（身份库）
--
-- 与 merchant/002_message_outbox_inbox.sql 内容相同：两个库各自需要一对
-- outbox / inbox，跨库写不共享表。
--
-- 本文件绝不能带 goose 的 Down 段：platform/database/migrate.Apply 把整个文件
-- 丢给一次 Exec、不识别 goose 指令，带上就会在同一个事务里建完表再删掉，
-- 而且不报错。Apply 会显式拒绝这种文件（见 migrate.go 里的字符串判断）。
--
-- 顺序也是硬要求：ALTER 必须先于 CREATE INDEX，否则对「由早期 schema 建出来、
-- 没有 lease 列」的表执行本文件时会因缺列而失败。

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
