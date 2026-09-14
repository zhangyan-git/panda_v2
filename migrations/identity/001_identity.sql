-- identity/001: 身份库表结构（拆库后的终态）
--
-- 单库时代身份域的建表分散在 001_init.sql、004_brands_stores.sql（merchant_users
-- 的范围列）与 006_admin_operation_logs.sql。拆库后 identity 库只承载身份域，
-- 这里把它们整理成一份，并直接写成拆库后的终态：
--
--   * 不建 merchants / brands / stores 等商户域表——它们属于 merchant 库；
--   * merchant_users.merchant_id 不再 REFERENCES merchants：列、NOT NULL 与
--     (merchant_id, username) 唯一约束全部保留，它变成软指针，归属由
--     merchant-service 经 gRPC 校验（阶段 1 的 GetBrandMerchant/GetStoreMerchant）；
--   * 不建 merchant_roles / merchant_permissions / merchant_role_permissions /
--     merchant_user_role_bindings 四张孤儿表：单库时代由 009 删除，这里直接不建；
--   * merchant_users 直接带上 004 追加的 is_admin / scope_type / scope_id 等列。
--
-- 全新身份库跑完 identity 迁移集之后的表结构与「单库时代 001→009 之后」的
-- 身份域部分逐列一致；跨库外键与孤儿表在那边是被删掉的，在这里是从来没建过。

BEGIN;

-- ============================================================
-- 平台管理员
-- ============================================================

CREATE TABLE admin_users (
  id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  username      TEXT        NOT NULL UNIQUE,
  password_hash TEXT        NOT NULL,
  name          TEXT        NOT NULL,
  email         TEXT        NOT NULL UNIQUE,
  status        TEXT        NOT NULL DEFAULT 'active',
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  admin_users          IS '平台后台管理员账号';
COMMENT ON COLUMN admin_users.username IS '登录用户名，全局唯一';
COMMENT ON COLUMN admin_users.status   IS '账号状态：active=正常 disabled=禁用';

-- ============================================================
-- 商户账号
--
-- 账号域（凭据、状态、数据范围）归身份库，写入方全部是 user-service：
-- 登录、/v1/admin/merchant-users/*、/v1/admin/merchants/{id}/users。
-- merchant-service 只在删除商户前判断「商户下还有没有账号」，走 HasUsers RPC。
-- ============================================================

CREATE TABLE merchant_users (
  id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  merchant_id   UUID        NOT NULL,
  username      TEXT        NOT NULL UNIQUE,
  password_hash TEXT        NOT NULL,
  name          TEXT        NOT NULL,
  email         TEXT,
  phone         TEXT,
  status        TEXT        NOT NULL DEFAULT 'active',
  is_admin      BOOLEAN     NOT NULL DEFAULT FALSE,
  scope_type    TEXT        NOT NULL DEFAULT 'merchant',
  scope_id      UUID,
  avatar        TEXT        NOT NULL DEFAULT '',
  last_login_at TIMESTAMPTZ,
  last_login_ip TEXT        NOT NULL DEFAULT '',
  login_count   INTEGER     NOT NULL DEFAULT 0,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (merchant_id, username)
);

COMMENT ON TABLE  merchant_users                 IS '商户员工账号，username 全局唯一';
COMMENT ON COLUMN merchant_users.merchant_id     IS '所属商户 ID；拆库后是软指针，不再有外键，归属由 merchant-service 校验';
COMMENT ON COLUMN merchant_users.username        IS '登录用户名，全局唯一，登录只需 username+password';
COMMENT ON COLUMN merchant_users.status          IS '账号状态：active=正常 disabled=禁用';
COMMENT ON COLUMN merchant_users.is_admin        IS '是否商户管理员';
COMMENT ON COLUMN merchant_users.scope_type      IS '数据范围：merchant=本商户全部 brand=指定品牌旗下 store=指定门店';
COMMENT ON COLUMN merchant_users.scope_id        IS '范围目标 ID：scope_type=brand 时为品牌 ID，store 时为门店 ID，merchant 时为 NULL';
COMMENT ON COLUMN merchant_users.last_login_at   IS '最后登录时间';
COMMENT ON COLUMN merchant_users.login_count     IS '登录次数';

-- scope_id 是多态指针（指向 merchant 库的 brands / stores），不建外键；
-- 写入时由服务层校验归属，删除品牌/门店时把指向它的账号回收为 merchant 级。
CREATE INDEX idx_merchant_users_merchant ON merchant_users(merchant_id);
CREATE INDEX idx_merchant_users_scope    ON merchant_users(scope_type, scope_id);

-- ============================================================
-- 自动更新 updated_at 的触发器函数
-- ============================================================

CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at = NOW();
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_admin_users_updated_at
  BEFORE UPDATE ON admin_users
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER trg_merchant_users_updated_at
  BEFORE UPDATE ON merchant_users
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================
-- 平台侧权限
-- ============================================================

CREATE TABLE admin_roles (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  code        TEXT        NOT NULL UNIQUE,
  name        TEXT        NOT NULL,
  description TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  admin_roles             IS '平台管理员角色定义表';
COMMENT ON COLUMN admin_roles.code        IS '角色唯一标识码，用于 Casbin subject，如 super_admin / operator';
COMMENT ON COLUMN admin_roles.name        IS '角色显示名，如 超级管理员、运营人员';
COMMENT ON COLUMN admin_roles.description IS '角色职责说明';

CREATE TRIGGER trg_admin_roles_updated_at
  BEFORE UPDATE ON admin_roles
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- resource / action 两列已删除：它们是把 code 拆成「资源 + 动作」的旧结构，
-- 但没有任何读取方（Go、前端、Casbin 都只用 code），而 AdminPermissionService.Create
-- 这条路径一直没写它们，导致 POST /v1/admin/permissions 恒 500。见 005 的说明。
CREATE TABLE admin_permissions (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  code        TEXT        NOT NULL UNIQUE,
  perm_group  TEXT        NOT NULL DEFAULT '',
  name        TEXT        NOT NULL,
  description TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  admin_permissions             IS '平台权限定义表；code 是唯一标识，也是鉴权与绑定使用的键';
COMMENT ON COLUMN admin_permissions.code        IS '权限唯一标识码，如 admin:roles:view；语义由 code 自身承载';
COMMENT ON COLUMN admin_permissions.perm_group  IS '前端分组展示用（中文显示名），如 商户管理、订单管理、设备管理';

CREATE TABLE admin_role_permissions (
  role_id       UUID NOT NULL REFERENCES admin_roles(id) ON DELETE CASCADE,
  permission_id UUID NOT NULL REFERENCES admin_permissions(id) ON DELETE CASCADE,
  PRIMARY KEY (role_id, permission_id)
);

COMMENT ON TABLE admin_role_permissions IS '平台角色与权限的绑定关系，多对多';

CREATE TABLE admin_user_role_bindings (
  admin_user_id UUID        NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
  role_id       UUID        NOT NULL REFERENCES admin_roles(id) ON DELETE CASCADE,
  granted_by    UUID        REFERENCES admin_users(id),
  granted_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (admin_user_id, role_id)
);

COMMENT ON TABLE  admin_user_role_bindings            IS '平台管理员与角色的绑定关系，带授权人和时间记录';
COMMENT ON COLUMN admin_user_role_bindings.granted_by IS '执行授权操作的管理员ID，NULL 表示系统初始化写入';
COMMENT ON COLUMN admin_user_role_bindings.granted_at IS '角色被授予的时间';

-- ============================================================
-- 后台业务操作日志
--
-- merchant_id 原本外键指向 merchants（006）。拆库后商户表在 merchant 库，
-- 该列降级为软指针：只保留 ID 用于按商户筛选历史日志，不做任何归属校验。
-- 日志自身仍不对目标对象建外键，目标删除后日志照常可查。
-- ============================================================

CREATE TABLE admin_operation_logs (
  id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),

  -- 操作人快照
  admin_user_id       UUID REFERENCES admin_users(id) ON DELETE SET NULL,
  admin_username      TEXT NOT NULL DEFAULT '',
  admin_name          TEXT NOT NULL DEFAULT '',

  -- 业务操作
  module              TEXT NOT NULL DEFAULT '',
  action              TEXT NOT NULL DEFAULT '',
  operation           TEXT NOT NULL DEFAULT '',

  -- 被操作对象；不对目标对象建立外键，保证目标删除后日志仍可查询
  target_type         TEXT NOT NULL DEFAULT '',
  target_id           UUID,
  target_name         TEXT NOT NULL DEFAULT '',
  merchant_id         UUID,

  -- 操作结果
  result              TEXT NOT NULL DEFAULT 'success',
  error_code          TEXT NOT NULL DEFAULT '',
  error_message       TEXT NOT NULL DEFAULT '',

  -- 经过脱敏的业务数据快照
  before_data         JSONB,
  after_data          JSONB,

  occurred_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE admin_operation_logs IS
  '平台后台业务操作日志，不记录接口全链路访问日志';
COMMENT ON COLUMN admin_operation_logs.admin_user_id IS
  '执行操作的平台管理员 ID；管理员删除后置空，保留日志';
COMMENT ON COLUMN admin_operation_logs.admin_username IS
  '操作发生时的平台管理员用户名快照';
COMMENT ON COLUMN admin_operation_logs.admin_name IS
  '操作发生时的平台管理员名称快照';
COMMENT ON COLUMN admin_operation_logs.module IS
  '业务模块，例如 merchants、brands、stores、roles';
COMMENT ON COLUMN admin_operation_logs.action IS
  '标准动作，例如 create、update、delete、audit、enable、disable';
COMMENT ON COLUMN admin_operation_logs.operation IS
  '面向后台展示的操作描述，例如 创建商户、审核品牌';
COMMENT ON COLUMN admin_operation_logs.target_type IS
  '目标类型，例如 merchant、brand、store、admin_user、role';
COMMENT ON COLUMN admin_operation_logs.target_id IS
  '目标对象 ID；不加外键，目标删除后仍保留日志';
COMMENT ON COLUMN admin_operation_logs.target_name IS
  '操作发生时的目标名称快照';
COMMENT ON COLUMN admin_operation_logs.merchant_id IS
  '关联商户 ID；跨库软指针，不加外键，用于按商户筛选历史日志';
COMMENT ON COLUMN admin_operation_logs.result IS
  '操作结果：success=成功，failure=失败';
COMMENT ON COLUMN admin_operation_logs.before_data IS
  '变更前快照，仅保存经过脱敏的业务字段';
COMMENT ON COLUMN admin_operation_logs.after_data IS
  '变更后快照，仅保存经过脱敏的业务字段';

CREATE INDEX idx_admin_operation_logs_occurred_at
  ON admin_operation_logs (occurred_at DESC);
CREATE INDEX idx_admin_operation_logs_admin_time
  ON admin_operation_logs (admin_user_id, occurred_at DESC);
CREATE INDEX idx_admin_operation_logs_module_time
  ON admin_operation_logs (module, occurred_at DESC);
CREATE INDEX idx_admin_operation_logs_action_time
  ON admin_operation_logs (action, occurred_at DESC);
CREATE INDEX idx_admin_operation_logs_target
  ON admin_operation_logs (target_type, target_id, occurred_at DESC)
  WHERE target_id IS NOT NULL;
CREATE INDEX idx_admin_operation_logs_merchant_time
  ON admin_operation_logs (merchant_id, occurred_at DESC)
  WHERE merchant_id IS NOT NULL;
CREATE INDEX idx_admin_operation_logs_result_time
  ON admin_operation_logs (result, occurred_at DESC);

-- ============================================================
-- Casbin policy 存储
-- 表名 casbin_rule（单数）与 casbin-go PostgreSQL adapter 默认一致
-- p 规则示例：('p', 'super_admin', '', 'admin:roles:view', '*')
-- g 规则示例：('g', 'admin_user_id', 'super_admin', '')
-- ============================================================

CREATE TABLE casbin_rule (
  id    BIGSERIAL   PRIMARY KEY,
  ptype TEXT        NOT NULL,
  v0    TEXT        NOT NULL DEFAULT '',
  v1    TEXT        NOT NULL DEFAULT '',
  v2    TEXT        NOT NULL DEFAULT '',
  v3    TEXT        NOT NULL DEFAULT '',
  v4    TEXT        NOT NULL DEFAULT '',
  v5    TEXT        NOT NULL DEFAULT ''
);

COMMENT ON TABLE  casbin_rule       IS 'Casbin policy 存储表，由 casbin-go adapter 管理，禁止业务代码直接操作';
COMMENT ON COLUMN casbin_rule.ptype IS '规则类型：p=权限策略 g=用户角色分组';
COMMENT ON COLUMN casbin_rule.v0    IS 'ptype=p: 角色标识(subject)；ptype=g: 用户ID';
COMMENT ON COLUMN casbin_rule.v1    IS 'ptype=p: 保留为空；ptype=g: 角色code';
COMMENT ON COLUMN casbin_rule.v2    IS 'ptype=p: 权限码(action)；ptype=g: 保留为空';
COMMENT ON COLUMN casbin_rule.v3    IS 'ptype=p: 作用域(domain)，平台域为 *；ptype=g: 保留为空';

CREATE UNIQUE INDEX idx_casbin_rule_unique
  ON casbin_rule (ptype, v0, v1, v2, v3, v4, v5)
  NULLS NOT DISTINCT;

CREATE INDEX idx_casbin_ptype_v0 ON casbin_rule(ptype, v0);
CREATE INDEX idx_casbin_ptype_v2 ON casbin_rule(ptype, v2);
CREATE INDEX idx_casbin_ptype_v3 ON casbin_rule(ptype, v3);

-- ============================================================
-- 索引
-- ============================================================

CREATE INDEX idx_admin_role_perms_role_id ON admin_role_permissions(role_id);
CREATE INDEX idx_admin_role_perms_perm_id ON admin_role_permissions(permission_id);
CREATE INDEX idx_admin_user_role_user_id  ON admin_user_role_bindings(admin_user_id);
CREATE INDEX idx_admin_user_role_role_id  ON admin_user_role_bindings(role_id);
CREATE INDEX idx_admin_perms_group        ON admin_permissions(perm_group);

COMMIT;
