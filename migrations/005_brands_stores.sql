-- 005: 品牌管理 / 门店管理（平台侧）
--
-- brands / stores / 审核表与 merchant_users 范围列已在 004 建好，
-- 本迁移补齐权限码与菜单，并把 003 挂在系统管理下的「商户管理」
-- 菜单升级为顶级目录：商户列表 / 品牌管理 / 门店管理 三个子菜单
--（对齐老项目「商户管理」分组结构）。

BEGIN;

-- 品牌/门店权限码
INSERT INTO admin_permissions (id, code, resource, action, perm_group, name, description, created_at)
VALUES
  (gen_random_uuid(), 'admin:brands:read',   'brands', 'read',   'brands', '查看品牌', '查看品牌列表', NOW()),
  (gen_random_uuid(), 'admin:brands:write',  'brands', 'write',  'brands', '管理品牌', '新建/编辑品牌、启用禁用、审核', NOW()),
  (gen_random_uuid(), 'admin:brands:delete', 'brands', 'delete', 'brands', '删除品牌', '删除品牌', NOW()),
  (gen_random_uuid(), 'admin:stores:read',   'stores', 'read',   'stores', '查看门店', '查看门店列表', NOW()),
  (gen_random_uuid(), 'admin:stores:write',  'stores', 'write',  'stores', '管理门店', '新建/编辑门店、启用禁用、审核', NOW()),
  (gen_random_uuid(), 'admin:stores:delete', 'stores', 'delete', 'stores', '删除门店', '删除门店', NOW())
ON CONFLICT (code) DO NOTHING;

-- 菜单重构：「商户管理」升级为顶级目录（path 置空），原 /merchants 行复用为目录节点
UPDATE admin_menus
SET parent_id  = NULL,
    path       = '',
    icon       = 'ShopOutlined',
    sort       = 3,
    updated_at = NOW()
WHERE name = '商户管理' AND path = '/merchants';

-- 子菜单：商户列表 / 品牌管理 / 门店管理（防重：按 path 判断）
INSERT INTO admin_menus (parent_id, name, path, icon, sort)
SELECT p.id, v.name, v.path, v.icon, v.sort
FROM admin_menus p
CROSS JOIN (VALUES
  ('商户列表', '/merchants', 'ShopOutlined',        1),
  ('品牌管理', '/brands',    'TagOutlined',         2),
  ('门店管理', '/stores',    'EnvironmentOutlined', 3)
) AS v(name, path, icon, sort)
WHERE p.name = '商户管理' AND p.path = '' AND p.parent_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM admin_menus m WHERE m.path = v.path);

-- super_admin 绑定权限与菜单（RequirePermission 对超管直接放行，
-- 这里补绑定是为了前端 access/菜单回显拿到权限码）
INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code IN ('admin:brands:read', 'admin:brands:write', 'admin:brands:delete',
                 'admin:stores:read', 'admin:stores:write', 'admin:stores:delete')
ON CONFLICT (role_id, permission_id) DO NOTHING;

-- 目录节点按 name 绑定（path 为空会与系统管理撞车），子菜单按 path 绑定
INSERT INTO admin_role_menus (role_id, menu_id)
SELECT r.id, m.id
FROM admin_roles r
CROSS JOIN admin_menus m
WHERE r.code = 'super_admin'
  AND ((m.name = '商户管理' AND m.path = '' AND m.parent_id IS NULL)
       OR m.path IN ('/merchants', '/brands', '/stores'))
ON CONFLICT (role_id, menu_id) DO NOTHING;

COMMIT;
