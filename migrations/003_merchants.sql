-- 003: 商户管理（平台侧）
--
-- merchants / merchant_users 表已在 001 建好，本迁移补齐平台后台
-- 「商户管理」所需的权限码、菜单、super_admin 绑定，并把商户账号
-- username 提升为全局唯一（登录只需 username+password，商户由账号反查）。

BEGIN;

-- 商户管理权限码
INSERT INTO admin_permissions (id, code, resource, action, perm_group, name, description, created_at)
VALUES
  (gen_random_uuid(), 'admin:merchants:read',   'merchants', 'read',   'merchants', '查看商户', '查看商户列表与商户账号', NOW()),
  (gen_random_uuid(), 'admin:merchants:write',  'merchants', 'write',  'merchants', '管理商户', '新建/编辑商户、状态流转、维护商户账号', NOW()),
  (gen_random_uuid(), 'admin:merchants:delete', 'merchants', 'delete', 'merchants', '删除商户', '删除商户与商户账号', NOW())
ON CONFLICT (code) DO NOTHING;

-- 菜单：「商户管理」挂在系统管理目录下（防重：按 path 判断）
INSERT INTO admin_menus (parent_id, name, path, icon, sort)
SELECT p.id, '商户管理', '/merchants', 'ShopOutlined', 5
FROM admin_menus p
WHERE p.name = '系统管理' AND p.parent_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM admin_menus m WHERE m.path = '/merchants')
LIMIT 1;

-- super_admin 绑定权限与菜单（RequirePermission 对超管直接放行，
-- 这里补绑定是为了前端 access/菜单回显拿到权限码）
INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code IN ('admin:merchants:read', 'admin:merchants:write', 'admin:merchants:delete')
ON CONFLICT (role_id, permission_id) DO NOTHING;

INSERT INTO admin_role_menus (role_id, menu_id)
SELECT r.id, m.id
FROM admin_roles r
CROSS JOIN admin_menus m
WHERE r.code = 'super_admin' AND m.path = '/merchants'
ON CONFLICT (role_id, menu_id) DO NOTHING;

-- 商户账号 username 全局唯一（原 (merchant_id, username) 复合约束保留无害）
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'merchant_users_username_key'
  ) THEN
    ALTER TABLE merchant_users ADD CONSTRAINT merchant_users_username_key UNIQUE (username);
  END IF;
END $$;

COMMENT ON COLUMN merchant_users.username IS '登录用户名，全局唯一，登录只需 username+password';

COMMIT;
