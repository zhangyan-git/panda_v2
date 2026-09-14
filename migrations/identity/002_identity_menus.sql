-- identity/002: 后台控制台的定义——权限码、内置角色、菜单树
--
-- 文件名为什么不是 002_menus.sql：schema_migrations 以文件名为版本键，而身份库和
-- 冻结的 legacy 链（001~009）共用同一张表。legacy 里已经有一个 002_menus.sql，
-- 于是这个名字在「从 legacy 基线过来的库」（dev，以及将来从单库迁来的生产库）上
-- 恒被判为已执行、整份跳过，只有全新的空库才会跑到它。跳过这件事当时不产生差异
-- （legacy 002 + 007/008 收敛出同样的 22 条权限码和 10 个菜单），但下一份改动如果
-- 有人加在这个文件里，就会在 dev 上静默不生效、在新库上生效——两边分叉且极难查。
-- 换一个不重名的版本号，让它在两种库上语义一致。
--
-- 单库时代这些内容由 002_menus.sql、003_merchants.sql、005_brands_stores.sql 三家
-- 分头写入，再由 007 把动作词表从 read/write 统一成 view/manage、由 008 把
-- perm_group 翻成中文；users / roles / permissions / bindings 四组权限码则一直由
-- services/user-service/cmd/seed 写入，从没进过迁移。
--
-- 拆库后这里直接写终态：动作词表就是 view / manage / delete，perm_group 就是中文
-- 显示名，22 条权限码一次写全。于是身份库不再需要「先跑 007/008 收敛」，
-- 也不再依赖 cmd/seed 提供权限行——cmd/seed 仍然负责带凭据的管理员账号，
-- 它按 code 查已有权限行，重跑幂等。
--
-- 内置角色 super_admin 也在这里建：它是控制台自带角色，不是某台机器的凭据。
-- 带密码的管理员账号仍然由 cmd/seed 创建，迁移不碰凭据。
--
-- 本迁移同时要服务两种库：全新的空库（这些表由本文件建），以及从 legacy 基线过来、
-- 表已经由 003/005 建好的库。所以建表一律 IF NOT EXISTS，权限/角色/绑定一律
-- ON CONFLICT，菜单树按 (父, 名) 判重——对已经正确的库是空操作。

BEGIN;

-- ============================================================
-- 权限码
-- ============================================================

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  (gen_random_uuid(), 'admin:users:view', '用户管理', '查看管理员', ''),
  (gen_random_uuid(), 'admin:users:manage', '用户管理', '管理管理员', ''),
  (gen_random_uuid(), 'admin:roles:view', '角色管理', '查看角色', ''),
  (gen_random_uuid(), 'admin:roles:manage', '角色管理', '管理角色', ''),
  (gen_random_uuid(), 'admin:roles:delete', '角色管理', '删除角色', ''),
  (gen_random_uuid(), 'admin:permissions:view', '权限管理', '查看权限', ''),
  (gen_random_uuid(), 'admin:permissions:manage', '权限管理', '管理权限', ''),
  (gen_random_uuid(), 'admin:permissions:delete', '权限管理', '删除权限', ''),
  (gen_random_uuid(), 'admin:bindings:view', '绑定管理', '查看绑定', '查看角色权限与用户角色绑定'),
  (gen_random_uuid(), 'admin:bindings:manage', '绑定管理', '管理绑定', '调整角色权限与用户角色绑定'),
  (gen_random_uuid(), 'admin:menus:view', '菜单管理', '查看菜单', '查看后台菜单树'),
  (gen_random_uuid(), 'admin:menus:manage', '菜单管理', '管理菜单', '新建/编辑后台菜单'),
  (gen_random_uuid(), 'admin:menus:delete', '菜单管理', '删除菜单', '删除后台菜单'),
  (gen_random_uuid(), 'admin:merchants:view', '商户管理', '查看商户', '查看商户列表与商户账号'),
  (gen_random_uuid(), 'admin:merchants:manage', '商户管理', '管理商户', '新建/编辑商户、状态流转、维护商户账号'),
  (gen_random_uuid(), 'admin:merchants:delete', '商户管理', '删除商户', '删除商户与商户账号'),
  (gen_random_uuid(), 'admin:brands:view', '品牌管理', '查看品牌', '查看品牌列表'),
  (gen_random_uuid(), 'admin:brands:manage', '品牌管理', '管理品牌', '新建/编辑品牌、启用禁用、审核'),
  (gen_random_uuid(), 'admin:brands:delete', '品牌管理', '删除品牌', '删除品牌'),
  (gen_random_uuid(), 'admin:stores:view', '门店管理', '查看门店', '查看门店列表'),
  (gen_random_uuid(), 'admin:stores:manage', '门店管理', '管理门店', '新建/编辑门店、启用禁用、审核'),
  (gen_random_uuid(), 'admin:stores:delete', '门店管理', '删除门店', '删除门店')
ON CONFLICT (code) DO NOTHING;

-- ============================================================
-- 内置超级管理员角色
-- ============================================================

INSERT INTO admin_roles (id, code, name, description, created_at, updated_at)
SELECT gen_random_uuid(), 'super_admin', '超级管理员', '拥有全部权限', NOW(), NOW()
WHERE NOT EXISTS (SELECT 1 FROM admin_roles WHERE code = 'super_admin');

-- 超管在中间件里直接放行，这里补绑定是为了前端 access/菜单回显能拿到权限码。
INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
ON CONFLICT (role_id, permission_id) DO NOTHING;

-- ============================================================
-- 菜单树
-- ============================================================

CREATE TABLE IF NOT EXISTS admin_menus (
  id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  parent_id  UUID        REFERENCES admin_menus(id) ON DELETE CASCADE,
  name       TEXT        NOT NULL,
  path       TEXT        NOT NULL DEFAULT '',
  icon       TEXT        NOT NULL DEFAULT '',
  sort       INTEGER     NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  admin_menus            IS '后台菜单树：目录与菜单；path 为空的节点仅作为分组目录';
COMMENT ON COLUMN admin_menus.parent_id  IS '父菜单 ID，NULL 表示顶级';
COMMENT ON COLUMN admin_menus.path       IS '前端路由路径；目录节点为空字符串';
COMMENT ON COLUMN admin_menus.icon       IS '图标名（Ant Design Icons 组件名）';

CREATE INDEX IF NOT EXISTS idx_admin_menus_parent ON admin_menus(parent_id);
CREATE INDEX IF NOT EXISTS idx_admin_menus_sort   ON admin_menus(sort, created_at);

CREATE TABLE IF NOT EXISTS admin_role_menus (
  role_id UUID NOT NULL REFERENCES admin_roles(id) ON DELETE CASCADE,
  menu_id UUID NOT NULL REFERENCES admin_menus(id) ON DELETE CASCADE,
  PRIMARY KEY (role_id, menu_id)
);

COMMENT ON TABLE admin_role_menus IS '角色-菜单绑定：决定角色可见的侧栏菜单';

-- 顶级：概览 + 系统管理目录 + 商户管理目录（对齐老项目「商户管理」分组结构）
INSERT INTO admin_menus (name, path, icon, sort)
SELECT v.name, v.path, v.icon, v.sort
FROM (VALUES
  ('概览',     '/dashboard', 'DashboardOutlined', 1),
  ('系统管理', '',           'SettingOutlined',   2),
  ('商户管理', '',           'ShopOutlined',      3)
) AS v(name, path, icon, sort)
WHERE NOT EXISTS (
  SELECT 1 FROM admin_menus m WHERE m.parent_id IS NULL AND m.name = v.name
);

-- 子级：按父名挂载，父行无论刚插入还是已存在都能取到
INSERT INTO admin_menus (parent_id, name, path, icon, sort)
SELECT p.id, v.name, v.path, v.icon, v.sort
FROM (VALUES
  ('系统管理', '权限管理',   '/permissions', 'SafetyCertificateOutlined', 1),
  ('系统管理', '角色管理',   '/roles',       'TeamOutlined',              2),
  ('系统管理', '管理员用户', '/admin-users', 'UserOutlined',              3),
  ('系统管理', '菜单管理',   '/menus',       'MenuOutlined',              4),
  ('商户管理', '商户列表',   '/merchants',   'ShopOutlined',              1),
  ('商户管理', '品牌管理',   '/brands',      'TagOutlined',               2),
  ('商户管理', '门店管理',   '/stores',      'EnvironmentOutlined',       3)
) AS v(parent, name, path, icon, sort)
JOIN admin_menus p ON p.name = v.parent AND p.parent_id IS NULL
WHERE NOT EXISTS (
  SELECT 1 FROM admin_menus m WHERE m.parent_id = p.id AND m.name = v.name
);

INSERT INTO admin_role_menus (role_id, menu_id)
SELECT r.id, m.id
FROM admin_roles r
CROSS JOIN admin_menus m
WHERE r.code = 'super_admin'
ON CONFLICT (role_id, menu_id) DO NOTHING;

COMMIT;
