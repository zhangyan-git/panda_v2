# User Service

## Administrator binding replacement

`POST /v1/admin/roles/{roleId}/permissions` replaces the complete permission set with `permissionIds`; `POST /v1/admin/users/{userId}/roles` replaces the complete role set with `roleIds`. An empty list clears the corresponding bindings. Both operations write the binding tables in one transaction, locking the target row to serialize replacements. Invalid target or member IDs fail the operation; failed transactions preserve the previous set. The policy is derived from those binding tables, so nothing else needs updating. The existing service layer reloads the in-process enforcer after commit and propagates reload errors.

Run PostgreSQL regression tests from this module with `TEST_DATABASE_URL=<test-dsn> go test -race -count=1 -v ./internal/repository`. The tests create and remove their own temporary schema and do not modify business tables. Without `TEST_DATABASE_URL`, these integration tests are skipped.

## Role code rename and authorization identity

`POST /v1/admin/roles` and `PUT /v1/admin/roles/{id}` accept `code`, `name`, and `description`. The role code is the administrator-domain authorization subject, not a display label: it is the role half of the `p` and `g` rules the enforcer derives from `admin_role_permissions` and `admin_user_role_bindings`. Renaming therefore updates `admin_roles.code` in one transaction, locking the role row first and reading the stored code inside the lock; because the policy is derived from the role row and its bindings, the rules follow the new code without a second write. The role UUID is unchanged, so `admin_role_permissions`, `admin_user_role_bindings`, and `admin_role_menus` keep their bindings. Binding operations lock the user first and then the involved roles in stable ID order, so a concurrent rename cannot write a stale code back.

- **400**: empty code, surrounding whitespace, CSV-breaking characters (comma, quote, newline, NUL), or the reserved codes `super_admin` / `超级管理员` (`INVALID_REQUEST`). Reserved codes cannot be created, renamed into, or renamed away from; their name and description stay editable.
- **409**: the target code already exists (`CONFLICT`, the unique `admin_roles_code_key`). The transaction rolls back; the message is the fixed `角色代码已存在或存在同名平台授权规则，请使用其他代码` and does not include driver text. (The message still names a second cause that no longer exists — leftover `p`/`g` rows in `casbin_rule` — because the policy is no longer read from that table; the string is left unchanged to avoid churning clients.)
- **404**: the role does not exist (`NOT_FOUND`). Other database failures are **500**, not a disguised 404.

After a successful commit the service reloads the in-process Casbin enforcer; a reload failure returns **500** with `角色已保存，但权限刷新失败，请重试保存`. The database commit and the reload are **not atomic** — a reload failure means the change is already persisted, and retrying the same update safely triggers another reload. Existing JWTs are unaffected either way: authorization reads the live bindings per request (super admin by role, everyone else through the derived policy), so a client never needs to re-authenticate for a permission change to take effect.

## Merchant session validation

`GET /v1/merchant/users/me` validates the identity's nonblank user ID, matching subject, nonblank tenant, and current account profile on every request. The returned profile must match the identity's user ID and tenant. Login and this endpoint share `MerchantAuthService.CheckAccess` to require an active account and active merchant; no cached JWT status or super-admin flag bypasses these checks.

- **401**: missing/invalid identity or account no longer exists (`UNAUTHORIZED`).
- **403**: non-merchant identity, tenant mismatch, disabled account, pending/suspended merchant, or missing merchant (`FORBIDDEN`). A missing merchant is reported as `商户不存在`, not as a missing account.
- **503**: profile/status/name dependency failures, nil or inconsistent profiles, blank IDs/names, or invalid merchant status (`SERVICE_UNAVAILABLE`). Failures return no success data and do not fall back to token claims or stale values.

Success retains `id`, `username`, `name`, `email`, `merchantId`, and `merchantName`. Login retains its existing known error mappings: invalid credentials/account lookup failures are 401; disabled accounts and pending/suspended merchants are 403; merchant lookup failures remain 500. Token fields/claims and best-effort login auditing are unchanged.

This is a **session-validation checkpoint, not global token revocation**. Existing JWTs are not invalidated server-side, and merchant endpoints do not automatically inherit this live account/merchant check. Clients must invoke this endpoint to observe changed access. Profile, merchant status and merchant name are separate reads, not an atomic snapshot; access can change between reads or after the response. This change does not alter JWT issuance, refresh, scope, or permission infrastructure.

Platform admin routes are the exception, and by design: every route behind a permission code re-reads the account status on each request (`RequirePermission`), so a disabled admin gets **403** `账号已禁用` with a still-valid token, and a super admin is subject to the same check before the role shortcut. Routes that only authenticate (e.g. `GET /v1/admin/menus/me`) do not.

Run regression checks from this module with `go test ./...`, `go test -race -count=1 ./internal/handler ./internal/service`, and `go build ./...`. Handler tests cover fail-closed dependencies, same-identity status changes, response compatibility, and login compatibility without requiring external services.

## 小程序用户的后台管理

`GET /v1/admin/miniapp-users`、`GET /v1/admin/miniapp-users/{id}`、
`PATCH /v1/admin/miniapp-users/{id}/status` 管理 `users` 里的 C 端顾客账号。
权限码是 `admin:miniapp-users:view` / `admin:miniapp-users:manage`，**刻意不复用**
`admin:users:*`：这是两批人，复用那个码等于让所有能管平台管理员的人拿到全部顾客的
手机号。

`PATCH .../status` 只接受 `active` 和 `disabled`，并且**在一个事务里做完**：先
`FOR UPDATE` 读出当前状态，写 `users.status`，目标状态不是 `active` 时撤销该用户全部
有效会话（`revoked_at = NOW()`、`revoke_reason = 'disabled'`），再往 outbox 落一条审计
事件（由本服务的消费者写进 `admin_operation_logs`）。撤销语句是照抄的，没调
`RevokeUserSessions`——那个用连接池，会把自己的事务提交在管理事务之外。启用不撤销任何
会话，返回 `revokedSessions: 0`；这个计数就是控制台那句「踢下线 N 个登录态」的来源。

- **400**：更新时 `status` 不在允许的两个值内（`status 只能为 active 或 disabled`）、
  列表筛选时 `status` 不在 `active`/`disabled`/`deleted` 内，或关键词超过 64 个字。
  筛选与更新用两条不同的提示，因为 `deleted` 是合法的筛选值、却永远不是合法的目标值。
- **404**：用户不存在（`NOT_FOUND`、`用户不存在`）。
- **409**：账号已注销（`CONFLICT`、`该账号已注销，不能通过后台修改状态`）。注销是用户
  自己留下的终态，后台没有「复活」入口，所以按冲突报，而不是当成参数错误。
- **403**：拿着有效令牌但没有该权限码的任何管理员（`没有权限执行此操作`），包括账号已
  被停用的超管——带权限码的路由每次请求都重新读账号状态，规则同上文。

最近登录记录里的标识是**写入时就已经脱敏**的（`139******12`），审计快照里也把手机号
打码，理由是同一个：审计事件要经 outbox 递到 RabbitMQ，完整的手机号在那里等于多存了
一份 PII。

## 操作日志的后台查询

`GET /v1/admin/operation-logs`、`GET /v1/admin/operation-logs/facets` 是
`admin_operation_logs` 的第一个读取方。这张表以前只有写入方——各业务服务在事务里往
outbox 落审计事件、经 RabbitMQ 送进来、由本服务的消费者落库——所以日志实际存在，
却没有任何页面或接口能看见它。写入侧仍然只由消费者调用，这两个接口是只读的：**没有
UPDATE、没有 DELETE、没有「清空日志」**，权限码也只有 `admin:operation-logs:view`，
**刻意不加 `manage`**。审计证据不该有后台删除入口，保留多久由 DBA 按留存策略处理。

筛选条件全部可选，`module` / `action` / `result` 精确匹配，`operator` 匹配
`admin_username` 或 `admin_name` 的**前缀**（有人记得登录名，有人记得姓名），
`keyword` 匹配 `target_name` 或 `operation` 的**子串**，`startTime` / `endTime`
按 `occurred_at` 过滤且是**闭区间**。时间参数的格式是 RFC3339。列表按
`occurred_at DESC, id DESC` 排序——`id` 是决胜位，同一次批量操作里的若干条事件时间戳
可能完全相同，只按时间排会在翻页时重复或漏行。列表里同时带 `beforeData` /
`afterData`（写入时已脱敏的快照），所以没有单独的详情接口。

`facets` 返回库中实际出现过的模块与动作，给前端做下拉选项。**从数据里取而不是在代码
里写死一份**：模块名由各服务的审计调用点决定，写死的那份会在下一个模块上线时静默少
一项，而「筛选里没有我要找的模块」看起来就是日志没记上，排查方向完全错了。代价是
每次调用都是一次 `SELECT DISTINCT` 全表扫描，所以前端只在进页面时取一次。

- **400**：`result` 不是 `success`/`failure`、时间不是 RFC3339、开始时间晚于结束时间，
  或任一文本筛选项超过 64 个字。四种提示各不相同——都报「参数错误」的话，调用方得靠
  猜才知道是哪个字段。
- **403**：拿着有效令牌但没有 `admin:operation-logs:view` 的管理员（`没有权限执行
  此操作`），规则同上面带权限码的路由——每次请求都重新读账号状态。

查询侧与写入侧共用**同一个仓储实例**（一个仓库两个入口），筛选条件在 `FindPage` 与
`Count` 之间共用同一份 WHERE 和参数构造，避免两者分叉成「第 3 页是空的，总数却说还有
200 条」。

**`occurred_at` 是落库时间，不是操作发生的时刻。** 事件本身不带时间戳（`audit.Entry`
没有时间字段），这个列取的是消费者写这张表时的 `NOW()`。直连链路下两者差约一秒，
但 relay 积压后补投的那一批会整体晚于实际操作时间——排查时如果发现「日志时间和操作
时间对不上」，先看这中间有没有积压，不要怀疑时钟。要让两者一致，得给 `audit.Entry`
加时间戳并一路透传到 INSERT，那是另一件事。

## Merchant access ports (first-phase transition)

Merchant status/name and brand/store ownership lookup ports default to the remote merchant service. Configure:

- `MERCHANT_SERVICE_URL`: an absolute HTTP(S) base URL, optionally with a path prefix; credentials, query strings and fragments are rejected.
- `MERCHANT_INTERNAL_TOKEN`: the shared service credential, at least 32 bytes with no whitespace or control characters. Supply it through the environment, not source control.
- `MERCHANT_OWNERSHIP_TIMEOUT_MS`: request timeout, 1000–30000 milliseconds (default 5000). Earlier caller deadlines and cancellation still apply.

The client sends `X-Service-Token` on each lookup, refuses all redirects (including same-origin redirects), and accepts only HTTP 200 with a valid success envelope and a nonblank value. Responses are limited to 1 MiB, must contain exactly one JSON document, and cannot contain unknown envelope/data fields. Merchant statuses must be `pending`, `active` or `suspended`; invalid responses and network failures fail closed. A valid HTTP 404 `NOT_FOUND` error envelope maps to `pgx.ErrNoRows`, preserving existing service behavior.

These ports are served only by the remote merchant-service client. There is no local repository implementation, and remote failures do not fall back to local repositories. Merchant, brand and store CRUD live in merchant-service; this service keeps only merchant accounts (`merchant_users`) and their scope validation.

This is **not a complete merchant-service cutover or database separation**. User service still shares the database and JWT claims with merchant-service, and user scope-label queries still join merchant, brand and store tables. Only the four access ports described above are remote; removing the remaining shared-table dependencies requires later phases.
