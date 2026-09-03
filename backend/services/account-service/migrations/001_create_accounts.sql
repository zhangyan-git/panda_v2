CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS accounts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username TEXT NOT NULL CHECK (char_length(trim(username)) > 0),
    type TEXT NOT NULL CHECK (type IN ('admin', 'merchant')),
    user_id TEXT,
    password_hash TEXT NOT NULL CHECK (char_length(password_hash) > 0),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT accounts_username_type_unique UNIQUE (username, type)
);

CREATE INDEX IF NOT EXISTS accounts_user_id_idx ON accounts (user_id) WHERE user_id IS NOT NULL;
