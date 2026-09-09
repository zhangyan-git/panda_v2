# Gateway service

This service is a deliberately thin HTTP compatibility facade for the browser APIs. It does not implement authentication, authorization, or business/database logic; those remain in the upstream services.

## Configuration

- `GATEWAY_ADDR` (optional, defaults to `:8080`)
- `MERCHANT_SERVICE_URL` (required absolute HTTP URL)
- `USER_SERVICE_URL` (required absolute HTTP URL)

Run it from this directory with `go run ./cmd/gateway`.

## Routing contract

The facade accepts both `/api/v1/...` (browser requests) and `/v1/...`; it removes only the optional `/api` prefix before proxying. Successful upstream status codes, headers, response bodies, request bodies, query strings, and `Authorization` headers are passed through unchanged.

| Public paths | Upstream |
| --- | --- |
| `/v1/admin/merchants...`, `/v1/admin/brands...`, `/v1/admin/stores...` | `MERCHANT_SERVICE_URL` |
| `/v1/admin/auth/...`, `/v1/admin/accounts...`, `/v1/admin/users/...`, `/v1/merchant/auth/...`, `/v1/merchant/users/...` | `USER_SERVICE_URL` |

Unknown paths return `404`. Upstream failures use the standard reverse-proxy `502` response. The gateway does not add a response envelope, so the browser's existing API contract remains the upstream service's responsibility.

The upstream services must be started separately. In particular, this facade does not initialize their databases or replace their runtime startup requirements.
