CREATE TABLE IF NOT EXISTS refresh_tokens (
    jti TEXT PRIMARY KEY CHECK (char_length(trim(jti)) > 0),
    account_id UUID,
    user_id TEXT,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked BOOLEAN NOT NULL DEFAULT FALSE,
    consumed BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS refresh_tokens_expires_at_idx ON refresh_tokens (expires_at);
CREATE INDEX IF NOT EXISTS refresh_tokens_account_id_idx ON refresh_tokens (account_id);
