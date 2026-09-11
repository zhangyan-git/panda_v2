# User Service

## Administrator binding replacement

`POST /v1/admin/roles/{roleId}/permissions` replaces the complete permission set with `permissionIds`; `POST /v1/admin/users/{userId}/roles` replaces the complete role set with `roleIds`. An empty list clears the corresponding bindings. Both operations update the binding table and administrator-domain Casbin rules in one transaction, locking the target row to serialize replacements. Invalid target or member IDs fail the operation; failed transactions preserve the previous set. Other targets and non-administrator domains are unchanged. The existing service layer reloads Casbin after commit and propagates reload errors.

Run PostgreSQL regression tests from this module with `TEST_DATABASE_URL=<test-dsn> go test -race -count=1 -v ./internal/repository`. The tests create and remove their own temporary schema and do not modify business tables. Without `TEST_DATABASE_URL`, these integration tests are skipped.

## Role code rename and authorization identity

`POST /v1/admin/roles` and `PUT /v1/admin/roles/{id}` accept `code`, `name`, and `description`. The role code is the administrator-domain authorization subject, not a display label: `casbin_rule.v0` (`ptype='p'`, `v1=''`) and `casbin_rule.v1` (`ptype='g'`, `v2=''`) both key on it. Renaming therefore updates `admin_roles.code` and those two rule sets in one transaction, locking the role row first and reading the stored code inside the lock. The role UUID is unchanged, so `admin_role_permissions`, `admin_user_role_bindings`, and `admin_role_menus` keep their bindings; other domains and other roles are untouched. Binding operations lock the user first and then the involved roles in stable ID order, so a concurrent rename cannot write a stale code back.

- **400**: empty code, surrounding whitespace, CSV-breaking characters (comma, quote, newline, NUL), or the reserved codes `super_admin` / `超级管理员` (`INVALID_REQUEST`). Reserved codes cannot be created, renamed into, or renamed away from; their name and description stay editable.
- **409**: the target code already exists, or leftover administrator-domain `p`/`g` rules still use it (`CONFLICT`). The transaction rolls back; the message is the fixed `角色代码已存在或存在同名平台授权规则，请使用其他代码` and does not include driver text.
- **404**: the role does not exist (`NOT_FOUND`). Other database failures are **500**, not a disguised 404.

After a successful commit the service reloads the in-process Casbin enforcer; a reload failure returns **500** with `角色已保存，但权限刷新失败，请重试保存`. The database commit and the reload are **not atomic** — a reload failure means the change is already persisted, and retrying the same update safely triggers another reload. Existing JWTs keep their original permissions until the client re-authenticates.

## Merchant session validation

`GET /v1/merchant/users/me` validates the identity's nonblank user ID, matching subject, nonblank tenant, and current account profile on every request. The returned profile must match the identity's user ID and tenant. Login and this endpoint share `MerchantAuthService.CheckAccess` to require an active account and active merchant; no cached JWT status or super-admin flag bypasses these checks.

- **401**: missing/invalid identity or account no longer exists (`UNAUTHORIZED`).
- **403**: non-merchant identity, tenant mismatch, disabled account, pending/suspended merchant, or missing merchant (`FORBIDDEN`). A missing merchant is reported as `商户不存在`, not as a missing account.
- **503**: profile/status/name dependency failures, nil or inconsistent profiles, blank IDs/names, or invalid merchant status (`SERVICE_UNAVAILABLE`). Failures return no success data and do not fall back to token claims or stale values.

Success retains `id`, `username`, `name`, `email`, `merchantId`, and `merchantName`. Login retains its existing known error mappings: invalid credentials/account lookup failures are 401; disabled accounts and pending/suspended merchants are 403; merchant lookup failures remain 500. Token fields/claims and best-effort login auditing are unchanged.

This is a **session-validation checkpoint, not global token revocation**. Existing JWTs are not invalidated server-side, and other protected endpoints do not automatically inherit this live account/merchant check. Clients must invoke this endpoint to observe changed access. Profile, merchant status and merchant name are separate reads, not an atomic snapshot; access can change between reads or after the response. This change does not alter JWT issuance, refresh, scope, or permission infrastructure.

Run regression checks from this module with `go test ./...`, `go test -race -count=1 ./internal/handler ./internal/service`, and `go build ./...`. Handler tests cover fail-closed dependencies, same-identity status changes, response compatibility, and login compatibility without requiring external services.

## Merchant access ports (first-phase transition)

Merchant status/name and brand/store ownership lookup ports default to the remote merchant service. Configure:

- `MERCHANT_SERVICE_URL`: an absolute HTTP(S) base URL, optionally with a path prefix; credentials, query strings and fragments are rejected.
- `MERCHANT_INTERNAL_TOKEN`: the shared service credential, at least 32 bytes with no whitespace or control characters. Supply it through the environment, not source control.
- `MERCHANT_OWNERSHIP_TIMEOUT_MS`: request timeout, 1000–30000 milliseconds (default 5000). Earlier caller deadlines and cancellation still apply.

The client sends `X-Service-Token` on each lookup, refuses all redirects (including same-origin redirects), and accepts only HTTP 200 with a valid success envelope and a nonblank value. Responses are limited to 1 MiB, must contain exactly one JSON document, and cannot contain unknown envelope/data fields. Merchant statuses must be `pending`, `active` or `suspended`; invalid responses and network failures fail closed. A valid HTTP 404 `NOT_FOUND` error envelope maps to `pgx.ErrNoRows`, preserving existing service behavior.

These ports are served only by the remote merchant-service client. There is no local repository implementation, and remote failures do not fall back to local repositories. Merchant, brand and store CRUD live in merchant-service; this service keeps only merchant accounts (`merchant_users`) and their scope validation.

This is **not a complete merchant-service cutover or database separation**. User service still shares the database and JWT claims with merchant-service, and user scope-label queries still join merchant, brand and store tables. Only the four access ports described above are remote; removing the remaining shared-table dependencies requires later phases.
