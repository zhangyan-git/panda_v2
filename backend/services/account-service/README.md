# account-service

Provides admin and merchant authentication. There is no public registration endpoint.

## Development accounts

Optional development account initialization runs at service startup only when both `PANDA_ENV=dev` and `DEV_ACCOUNT_INIT_ENABLED=true`. Configure `DEV_ADMIN_USERNAME`/`DEV_ADMIN_PASSWORD` and/or `DEV_MERCHANT_USERNAME`/`DEV_MERCHANT_PASSWORD` through environment variables or a local, ignored `.env` file. Passwords are bcrypt-hashed before persistence; plaintext passwords are never logged or stored in migrations. The feature is disabled by default and does not run in production environments.

## Refresh tokens

When `DATABASE_URL` is configured, refresh-token JTIs are stored in PostgreSQL by migration `002_create_refresh_tokens.sql`. Refresh tokens are registered by JTI only (never token plaintext), consumed atomically once, and may be revoked before expiry. Without PostgreSQL, the process-local memory store is used as a development/test fallback. Expired rows can be removed with `PostgreSQLRefreshTokenStore.CleanupExpired`.
