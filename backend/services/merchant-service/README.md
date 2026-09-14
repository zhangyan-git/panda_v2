# Merchant service

Merchant service owns merchant, brand, and store master data. It deliberately does not own merchant users, authentication, JWT, IAM, roles, permissions, or account-to-merchant bindings; those contracts remain with the identity platform.

## Runtime

`cmd/main.go` loads `merchant-service` configuration through the shared platform config, creates the shared PostgreSQL adapter, dials user-service once over the shared platform gRPC client, and starts the shared Kratos HTTP/gRPC runtime. Administrative APIs require a configured PostgreSQL database; startup rejects the no-op adapter. `USER_GRPC_ADDR` must point directly to the trusted user-service (not the gateway), which verifies the same JWT issuer and secret. Live authorization uses a five-second per-call deadline on every request.

## Versioned ownership contract (v1)

- `merchants`: merchant legal/business master records.
- `brands`: merchant-owned brand records, referenced by `merchant_id`.
- `stores`: merchant-owned store records, referenced by `merchant_id` and optionally `brand_id`.
- No merchant identity tables or auth endpoints are exposed here.
- Internal calls are gRPC only: `MerchantService.GetMerchant`, `GetBrandMerchant`, and `GetStoreMerchant` require the shared service token in metadata and answer `NOT_FOUND` for a missing target. The former `/v1/merchant-service/...` HTTP routes and their `X-Service-Token` header are gone; merchant CRUD, listing, and the merchant self-service reads remain HTTP-only.
- Administrative CRUD routes under `/v1/admin/merchants`, `/v1/admin/brands`, and `/v1/admin/stores` first verify the Bearer access JWT and reject every nonempty tenant, including whitespace. Each request then calls `AdminAccessService.GetAdminAccess` on user-service, forwarding the caller's own verified access token in gRPC metadata. That RPC answers for the presented token only and rejects missing/disabled admins, so a successful response identifies an active admin.
- Authorization requires the matching current `admin:<resource>:{read,write,delete}` permission, or the current `super_admin` role. Status and audit operations require `write`. The live identity ID must match both JWT subject and user ID. Roles, permissions, and `IsSuper` in request context are replaced with live data before permission/scope checks: revoked grants cannot fall back to the JWT snapshot, and newly granted access works without refreshing the token.
- User-service `UNAUTHENTICATED`/`PERMISSION_DENIED` responses remain 401/403. A missing authorizer, any other RPC error including transport failure and timeout, or a mismatched user ID returns HTTP 503 without reaching business handlers. No authorization cache is used for this lookup.
- Brand/store deletion performs scope reset (`UserService.ResetAccountScope`) before deleting the target. A reset error blocks deletion; an unreachable user-service maps to `ErrUnavailable`, hence HTTP 503, while a reset user-service refuses surfaces as an internal error. Merchant deletion separately asks `UserService.HasUsers` before deleting.

`merchant_users` is no longer read locally: account presence and scope reset are remote calls, so no migration is needed and identity ownership does not move.

## Image uploads

`POST /v1/admin/uploads/images`, `multipart/form-data`, field name `file`. The response is the usual envelope around `{url, key, name, size, contentType, md5}`; `url` is absolute (the browser renders it directly) and `key` carries no host, so a later CDN swap is a string replace and a delete endpoint has something to work with.

- Authorization reuses the existing `admin:brands:manage` and `admin:stores:manage` permissions rather than introducing a new code. A new code would need an identity migration plus grants for every existing role, and missing one silently restricts uploads to super admins; anyone who legitimately attaches an image already holds one of these two. The chain is unchanged, so a disabled admin or a revoked grant is rejected live.
- An unconfigured uploader is **not** a reason to skip the route: the endpoint is always registered and answers 503 naming exactly which `OSS_*` variables are missing. The binary's route table stays identical across environments, and a gateway-forwarded 404 never has to be distinguished from a misconfiguration.
- Keys are content-addressed: `<UPLOAD_PATH>/images/<md5[:2]>/<md5[2:4]>/<md5><ext>`. Identical bytes land on the same key, which is what makes a client retry idempotent. `UPLOAD_USE_MD5=false` is accepted for legacy-config compatibility but ignored with a startup warning — a key built from the client filename can escape `UPLOAD_PATH`.
- The request body is capped with `http.MaxBytesReader` at `UPLOAD_MAX_FILE_SIZE + 1MiB` before any parsing, and the part is read through `io.LimitReader`. Without it, `ParseMultipartForm` bounds only the in-memory part and spills the rest to `/tmp` without limit, so any valid admin could fill the container disk. The limit is checked against decoded bytes, not `FileHeader.Size`.
- Oversized uploads return 413; a request deadline surfaces as 408/503 rather than a generic 500, because `khttp.Timeout` reports an i/o timeout.
- Timeouts layer, outer to inner: gateway 120s > this service's `HTTP_TIMEOUT_MS` (default 90s for merchant-service, via the code default — **not** an env value, since one `.env` is shared by every service) > the OSS SDK's 60s read/write. The SDK needs its own ceiling because `PutObject` does not honour a context: without `oss.Timeout`, a stalled PUT can hang for 200s, returning failure to the client while the goroutine keeps writing the object.

**Manual smoke test against a temporary bucket** — the real SDK call is the one thing unit tests cannot cover (the Go tests use a fake `Store` and need no credentials):

1. Put `OSS_ACCESS_KEY`, `OSS_SECRET_KEY`, `OSS_ENDPOINT`, `OSS_BUCKET` (and `OSS_CNAME` if the bucket has one) into your local, untracked `.env`; restart merchant-service.
2. Upload a PNG through the admin UI or `curl -F file=@x.png -H "Authorization: Bearer <token>" $GATEWAY/api/v1/admin/uploads/images`.
3. Open the returned `url` in a browser: it must render, not download. A download means the object was stored without a `Content-Type`, or the bucket is not readable at that base.
4. Upload the same file again: the returned `key` must be identical.
5. Break the configuration on purpose (empty `OSS_ACCESS_KEY`, restart): the service still starts, brand/store CRUD still works, and upload answers 503 naming the missing variable.
