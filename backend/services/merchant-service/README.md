# Merchant service

Merchant service owns merchant, brand, and store master data. It deliberately does not own merchant users, authentication, JWT, IAM, roles, permissions, or account-to-merchant bindings; those contracts remain with the identity platform.

## Runtime

`cmd/main.go` loads `merchant-service` configuration through the shared platform config, creates the shared PostgreSQL adapter, and starts the shared Kratos HTTP/gRPC runtime. `DATABASE_URL` may be omitted for local startup (the platform supplies a no-op database adapter); a configured PostgreSQL database is verified during startup.

## Versioned ownership contract (v1)

- `merchants`: merchant legal/business master records.
- `brands`: merchant-owned brand records, referenced by `merchant_id`.
- `stores`: merchant-owned store records, referenced by `merchant_id` and optionally `brand_id`.
- No merchant identity tables or auth endpoints are exposed here.
- HTTP routes are intentionally limited to internal master-data endpoints under `/v1/merchant-service/...`; authorization is supplied by the platform gateway in a later phase.

The repository and service layers currently provide the compileable ownership boundary and validation contract. Persistence migrations and write/read endpoints will be added in the next approved phase.
