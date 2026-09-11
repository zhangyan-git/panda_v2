-- identity/005: 删除 admin_permissions 的 resource / action 两列
--
-- 这两列是旧结构：把权限码拆成「被操作资源 + 动作」，由 legacy 的 002_menus.sql、
-- 007、008 和历史 cmd/seed 手工写死（身份集现在叫 002_identity_menus.sql，
-- 它直接写终态、不含这两列，改名理由见那个文件的头部）。拆库整理身份库时核对过，
-- 全仓没有任何读取方——
-- Go 模型（model.AdminPermission）、前端权限页、Casbin 策略都只用 code。
--
-- 更要紧的是它一直在坏：两列都是 NOT NULL 且没有默认值，而
-- AdminPermissionService.Create 从来不写它们，于是
-- POST /v1/admin/permissions 恒 500（400 分支之前的那一步就炸）。
-- 这个缺陷早于拆库，是阶段 2 的真实浏览器验收在权限页建权限时暴露出来的。
--
-- 修法不是让服务层去补两列：现有数据的 resource 与 code 之间没有可推导的规则
-- （admin:users:view -> admin_users，但 admin:menus:view -> menus），
-- 补出来的值必然是编的。既然无人读，就把它们删掉，语义完全由 code 承载。
--
-- 全新的库由 001_identity.sql 直接建成终态（已无这两列），本迁移对它是空操作；
-- 已经跑过 001 的库在这里收敛到同一形状。005 之所以不是就地改 001，
-- 是因为 001 已经登记进 schema_migrations，改它只会让新老库分叉。

BEGIN;

ALTER TABLE admin_permissions DROP COLUMN IF EXISTS resource;
ALTER TABLE admin_permissions DROP COLUMN IF EXISTS action;

COMMIT;
