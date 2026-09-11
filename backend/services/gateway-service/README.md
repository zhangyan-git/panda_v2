# Gateway service

The gateway is a thin HTTP compatibility facade. It routes browser API paths to the user or merchant service. It does not validate JWTs or perform live IAM authorization; merchant-service owns live IAM checks for merchant resources.

## Configuration

- `GATEWAY_ADDR` optional, defaults to `:8080`
- `MERCHANT_SERVICE_URL` required absolute `http` or `https` URL without credentials, query, or fragment; its path must be empty or `/` (non-root base paths are rejected, not silently dropped)
- `USER_SERVICE_URL` same URL requirements as `MERCHANT_SERVICE_URL`
- `MERCHANT_INTERNAL_TOKEN` required secret of at least 32 bytes; sent only to the merchant upstream as `X-Service-Token`
- `GATEWAY_REQUEST_TIMEOUT_MS` optional integer from 0 through 9223372036854 milliseconds; absent or zero uses the 10000 ms default; out-of-range values are rejected before duration conversion
- `GATEWAY_UPLOAD_TIMEOUT_MS` same bounds and defaulting rules, applied **only** to the upload path; the default is 120000 ms. It is deliberately a second budget rather than a raised global one: this deadline spans the client's upload *and* the upstream write, and 10 MiB at 1 Mbps upstream is ~80 s, so the 10 s default would produce a steady stream of 504s. Raising the global value instead would pin a gateway goroutine to a stuck route of any kind for two minutes. Keep it above merchant-service's `HTTP_TIMEOUT_MS` — this is the outer of the two.

Run with `go run ./cmd/gateway`. Upstream services must be started separately. When embedding the handler, `Config.HTTPClient` supplies only its transport, not its cookie jar, redirect policy, or client timeout; `Config.RequestTimeout` controls the request deadline.

## Routing

Both `/api/v1/...` and `/v1/...` are accepted. Only one optional `/api` prefix is removed. Prefix matches require an exact match or a following `/`, so `/v1/admin/users-extra` does not match `/v1/admin/users`.

| Destination | Accepted path prefixes |
| --- | --- |
| User service | `/v1/admin/merchant-users`, `/v1/admin/accounts`, `/v1/admin/auth`, `/v1/admin/users`, `/v1/admin/roles`, `/v1/admin/permissions`, `/v1/admin/menus`, `/v1/merchant/auth`, `/v1/merchant/users` |
| Merchant service | `/v1/admin/merchants`, `/v1/admin/brands`, `/v1/admin/stores`, `/v1/admin/uploads` |

The exact nested route `/v1/admin/merchants/{id}/users`, with a nonempty single-segment ID, goes to user-service before the general merchant prefix is considered. A trailing slash or additional suffix does not match this exception and remains on the merchant route. Unknown prefixes return 404 without an upstream call.

Request bodies, query strings, encoded path escaping, and `Authorization` pass through. All client `X-User*`, `X-Tenant*`, `X-Roles`, and `X-Service-Token` headers are removed case-insensitively; the configured merchant token is then added only for merchant upstream requests. Forwarded identity is not used for authorization.

Upstream response status, headers, and bodies pass through, including application errors. Before upstream response headers are committed, gateway failures use these API-compatible JSON envelopes (internal transport errors are not exposed).

- 502: `{"success":false,"errorCode":"BAD_GATEWAY","errorMessage":"Bad Gateway"}`
- 504: `{"success":false,"errorCode":"GATEWAY_TIMEOUT","errorMessage":"Gateway Timeout"}`

The request context deadline is chosen per destination path: the upload prefix gets `GATEWAY_UPLOAD_TIMEOUT_MS`, everything else gets `GATEWAY_REQUEST_TIMEOUT_MS`. It spans the entire proxy `ServeHTTP` call, including reading the upstream response body, and client cancellation propagates upstream. Responses stream without full-body buffering. If a timeout or read failure occurs after response headers have been sent, the stream is terminated: HTTP cannot replace an already committed status/body with a JSON 504.

## Verification

```sh
go test ./...
go build ./...
```

The compatibility tests cover the route matrix with and without `/api` (including `/v1/admin/uploads`), prefix boundaries, spoofed headers, request/response preservation, URL validation, upstream errors, cancellation, streamed bodies, request deadlines, and the per-path timeout split (with a short upload budget, the upload route succeeds while a normal route times out). CLI tests cover timeout defaults, valid bounds, and overflow rejection.
