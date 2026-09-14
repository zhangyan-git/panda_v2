-- ============================================================
-- Panda V2 权限管理（IAM）建表脚本
-- PostgreSQL 16
-- ============================================================

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
-- 商户（租户）
-- ============================================================

CREATE TABLE merchants (
  id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  name          TEXT        NOT NULL,
  status        TEXT        NOT NULL DEFAULT 'pending',
  contact_name  TEXT,
  contact_phone TEXT,
  contact_email TEXT,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  merchants        IS '商户主体（即多租户体系的租户）';
COMMENT ON COLUMN merchants.status IS '商户状态：pending=待审核 active=正常 suspended=已暂停';

CREATE TABLE merchant_users (
  id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  merchant_id   UUID        NOT NULL REFERENCES merchants(id),
  username      TEXT        NOT NULL,
  password_hash TEXT        NOT NULL,
  name          TEXT        NOT NULL,
  email         TEXT,
  phone         TEXT,
  status        TEXT        NOT NULL DEFAULT 'active',
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (merchant_id, username)
);

COMMENT ON TABLE  merchant_users             IS '商户员工账号，username 在同一商户内唯一';
COMMENT ON COLUMN merchant_users.merchant_id IS '所属商户（租户），外键关联 merchants.id';
COMMENT ON COLUMN merchant_users.status      IS '账号状态：active=正常 disabled=禁用';

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

CREATE TRIGGER trg_merchants_updated_at
  BEFORE UPDATE ON merchants
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

-- ============================================================
-- 平台侧权限
-- ============================================================

CREATE TABLE admin_permissions (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  code        TEXT        NOT NULL UNIQUE,
  resource    TEXT        NOT NULL,
  action      TEXT        NOT NULL,
  perm_group  TEXT        NOT NULL DEFAULT '',
  name        TEXT        NOT NULL,
  description TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  admin_permissions             IS '平台权限定义表，resource + action 描述最小权限单元';
COMMENT ON COLUMN admin_permissions.code        IS '权限唯一标识码，格式为 resource:action，如 merchant:approve';
COMMENT ON COLUMN admin_permissions.resource    IS '被操作的资源类型，如 merchant、order、machine、report';
COMMENT ON COLUMN admin_permissions.action      IS '操作动作，如 read、write、delete、approve、export';
COMMENT ON COLUMN admin_permissions.perm_group  IS '前端分组展示用，如 商户管理、订单管理、设备管理';

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
-- 商户侧权限
-- ============================================================

CREATE TABLE merchant_roles (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  merchant_id UUID        NOT NULL REFERENCES merchants(id),
  code        TEXT        NOT NULL,
  name        TEXT        NOT NULL,
  description TEXT,
  is_system   BOOLEAN     NOT NULL DEFAULT FALSE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (merchant_id, code)
);

COMMENT ON TABLE  merchant_roles             IS '商户角色定义表，每个商户独立管理自己的角色';
COMMENT ON COLUMN merchant_roles.merchant_id IS '所属商户（租户），实现数据隔离，不同商户角色码可以相同';
COMMENT ON COLUMN merchant_roles.code        IS '角色标识码，在同一商户内唯一，用于 Casbin domain RBAC';
COMMENT ON COLUMN merchant_roles.is_system   IS '平台预置角色标记，true 时商户不允许删除或修改 code，如 owner';

CREATE TRIGGER trg_merchant_roles_updated_at
  BEFORE UPDATE ON merchant_roles
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE merchant_permissions (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  code        TEXT        NOT NULL UNIQUE,
  resource    TEXT        NOT NULL,
  action      TEXT        NOT NULL,
  perm_group  TEXT        NOT NULL,
  name        TEXT        NOT NULL,
  description TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  merchant_permissions             IS '商户侧权限定义表，由平台统一维护，商户只能分配不能增删';
COMMENT ON COLUMN merchant_permissions.code        IS '权限标识码，格式 resource:action，如 machine:view、order:refund';
COMMENT ON COLUMN merchant_permissions.resource    IS '被操作的资源类型，如 machine、order、report、staff、brand、shop';
COMMENT ON COLUMN merchant_permissions.action      IS '操作动作，如 view、edit、delete、export、manage';
COMMENT ON COLUMN merchant_permissions.perm_group  IS '前端分组展示用，如 机器管理、订单管理、数据报表、员工管理';

CREATE TABLE merchant_role_permissions (
  role_id       UUID NOT NULL REFERENCES merchant_roles(id) ON DELETE CASCADE,
  permission_id UUID NOT NULL REFERENCES merchant_permissions(id) ON DELETE CASCADE,
  PRIMARY KEY (role_id, permission_id)
);

COMMENT ON TABLE merchant_role_permissions IS '商户角色与权限的绑定关系，多对多';

-- 跨租户完整性校验：确保员工和角色属于同一商户
CREATE OR REPLACE FUNCTION check_merchant_user_role_same_tenant()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
  user_merchant_id UUID;
  role_merchant_id UUID;
BEGIN
  SELECT merchant_id INTO user_merchant_id FROM merchant_users WHERE id = NEW.merchant_user_id;
  SELECT merchant_id INTO role_merchant_id FROM merchant_roles  WHERE id = NEW.role_id;
  IF user_merchant_id IS DISTINCT FROM role_merchant_id THEN
    RAISE EXCEPTION '员工(merchant_user_id=%)与角色(role_id=%)不属于同一商户', NEW.merchant_user_id, NEW.role_id;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TABLE merchant_user_role_bindings (
  merchant_user_id UUID        NOT NULL REFERENCES merchant_users(id) ON DELETE CASCADE,
  role_id          UUID        NOT NULL REFERENCES merchant_roles(id) ON DELETE CASCADE,
  merchant_id      UUID        NOT NULL REFERENCES merchants(id),
  granted_by       UUID        REFERENCES merchant_users(id),
  granted_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (merchant_user_id, role_id)
);

COMMENT ON TABLE  merchant_user_role_bindings             IS '商户员工与角色的绑定关系，带 merchant_id 冗余字段和授权记录';
COMMENT ON COLUMN merchant_user_role_bindings.merchant_id IS '冗余字段，等于 role.merchant_id，加速按商户查全体成员权限';
COMMENT ON COLUMN merchant_user_role_bindings.granted_by  IS '执行授权的商户员工ID，通常为商户 owner';
COMMENT ON COLUMN merchant_user_role_bindings.granted_at  IS '角色被授予的时间';

CREATE TRIGGER trg_merchant_user_role_same_tenant
  BEFORE INSERT OR UPDATE ON merchant_user_role_bindings
  FOR EACH ROW EXECUTE FUNCTION check_merchant_user_role_same_tenant();

-- ============================================================
-- Casbin policy 存储
-- 表名 casbin_rule（单数）与 casbin-go PostgreSQL adapter 默认一致
-- p 规则示例：('p', 'owner', 'machine', 'view', 'merchant_id_xxx')
-- g 规则示例：('g', 'user_id_yyy', 'owner', 'merchant_id_xxx')
-- Enforce 调用：enforcer.Enforce(userID, resource, action, merchantID)
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
COMMENT ON COLUMN casbin_rule.v1    IS 'ptype=p: 资源类型(object)；ptype=g: 角色code';
COMMENT ON COLUMN casbin_rule.v2    IS 'ptype=p: 操作动作(action)；ptype=g: 商户ID(domain)';
COMMENT ON COLUMN casbin_rule.v3    IS 'ptype=p: 商户ID(domain)；ptype=g: 通常为空';

CREATE UNIQUE INDEX idx_casbin_rule_unique
  ON casbin_rule (ptype, v0, v1, v2, v3, v4, v5)
  NULLS NOT DISTINCT;

-- ============================================================
-- 索引
-- ============================================================

CREATE INDEX idx_merchant_users_merchant        ON merchant_users(merchant_id);

CREATE INDEX idx_admin_role_perms_role_id       ON admin_role_permissions(role_id);
CREATE INDEX idx_admin_role_perms_perm_id       ON admin_role_permissions(permission_id);
CREATE INDEX idx_admin_user_role_user_id        ON admin_user_role_bindings(admin_user_id);
CREATE INDEX idx_admin_user_role_role_id        ON admin_user_role_bindings(role_id);
CREATE INDEX idx_admin_perms_group              ON admin_permissions(perm_group);

CREATE INDEX idx_merchant_roles_merchant_id     ON merchant_roles(merchant_id);
CREATE INDEX idx_merchant_role_perms_role_id    ON merchant_role_permissions(role_id);
CREATE INDEX idx_merchant_role_perms_perm_id    ON merchant_role_permissions(permission_id);
CREATE INDEX idx_merchant_perms_group           ON merchant_permissions(perm_group);
CREATE INDEX idx_merchant_user_role_user_id     ON merchant_user_role_bindings(merchant_user_id);
CREATE INDEX idx_merchant_user_role_role_id     ON merchant_user_role_bindings(role_id);
CREATE INDEX idx_merchant_user_role_merchant    ON merchant_user_role_bindings(merchant_id);

CREATE INDEX idx_casbin_ptype_v0                ON casbin_rule(ptype, v0);
CREATE INDEX idx_casbin_ptype_v2                ON casbin_rule(ptype, v2);
CREATE INDEX idx_casbin_ptype_v3                ON casbin_rule(ptype, v3);

COMMIT;
