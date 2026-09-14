-- 002: 后台菜单树与角色-菜单绑定
--
-- admin_menus 存放侧栏菜单树（目录 + 菜单），前端侧栏按当前用户
-- 角色绑定的菜单渲染；页面访问控制仍由权限码（casbin）负责。

BEGIN;

CREATE TABLE admin_menus (
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

CREATE INDEX idx_admin_menus_parent ON admin_menus(parent_id);
CREATE INDEX idx_admin_menus_sort   ON admin_menus(sort, created_at);

CREATE TABLE admin_role_menus (
  role_id UUID NOT NULL REFERENCES admin_roles(id) ON DELETE CASCADE,
  menu_id UUID NOT NULL REFERENCES admin_menus(id) ON DELETE CASCADE,
  PRIMARY KEY (role_id, menu_id)
);

COMMENT ON TABLE admin_role_menus IS '角色-菜单绑定：决定角色可见的侧栏菜单';

-- 菜单管理权限码
INSERT INTO admin_permissions (id, code, resource, action, perm_group, name, description, created_at)
VALUES
  (gen_random_uuid(), 'admin:menus:read',   'menus', 'read',   'menus', '查看菜单', '查看后台菜单树', NOW()),
  (gen_random_uuid(), 'admin:menus:write',  'menus', 'write',  'menus', '管理菜单', '新建/编辑后台菜单', NOW()),
  (gen_random_uuid(), 'admin:menus:delete', 'menus', 'delete', 'menus', '删除菜单', '删除后台菜单', NOW())
ON CONFLICT (code) DO NOTHING;

-- 默认菜单树：概览 + 系统管理目录（权限/角色/管理员用户/菜单）
WITH overview AS (
  INSERT INTO admin_menus (name, path, icon, sort)
  VALUES ('概览', '/dashboard', 'DashboardOutlined', 1)
  RETURNING id
), system_dir AS (
  INSERT INTO admin_menus (name, path, icon, sort)
  VALUES ('系统管理', '', 'SettingOutlined', 2)
  RETURNING id
)
INSERT INTO admin_menus (parent_id, name, path, icon, sort)
SELECT system_dir.id, v.name, v.path, v.icon, v.sort
FROM system_dir
CROSS JOIN (VALUES
  ('权限管理',   '/permissions', 'SafetyCertificateOutlined', 1),
  ('角色管理',   '/roles',       'TeamOutlined',              2),
  ('管理员用户', '/admin-users', 'UserOutlined',              3),
  ('菜单管理',   '/menus',       'MenuOutlined',              4)
) AS v(name, path, icon, sort);

COMMIT;
