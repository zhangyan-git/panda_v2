-- identity/011: 后台「小程序用户」页面的权限码与菜单
--
-- 可重复执行；只给内置超管 super_admin 绑定，其它角色由后台自己分配。
--
-- 权限码用 admin:miniapp-users:* 而不是 admin:users:*：后者是 002 里给平台
-- 管理员（admin_users）用的，两个页面管的是两批完全不同的人。共用同一个码
-- 意味着「能看后台管理员」的人自动能看全部小程序用户的手机号——这是要把
-- 两批人的授权分开的原因，不是命名偏好。

BEGIN;

-- ============================================================
-- 权限码
-- ============================================================

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  (gen_random_uuid(), 'admin:miniapp-users:view', '小程序用户', '查看小程序用户',
   '查看小程序用户列表、资料与登录记录'),
  (gen_random_uuid(), 'admin:miniapp-users:manage', '小程序用户', '管理小程序用户',
   '启用/禁用小程序用户；禁用会同时撤销其全部有效会话')
ON CONFLICT (code) DO NOTHING;

-- 超管在中间件里直接放行，这里补绑定是为了前端 access 与权限回显能拿到码。
INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code IN ('admin:miniapp-users:view', 'admin:miniapp-users:manage')
ON CONFLICT (role_id, permission_id) DO NOTHING;

-- ============================================================
-- 菜单
-- ============================================================
--
-- 顶级菜单，与 007 的「优惠券管理」同级（排序 5）。这里不建目录再挂子项：
-- 目前只有一个页面，多一层目录只是多一次点击。

INSERT INTO admin_menus (name, path, icon, sort)
SELECT '小程序用户', '/miniapp-users', 'ContactsOutlined', 5
WHERE NOT EXISTS (
  SELECT 1 FROM admin_menus WHERE parent_id IS NULL AND path = '/miniapp-users'
);

INSERT INTO admin_role_menus (role_id, menu_id)
SELECT r.id, m.id
FROM admin_roles r
CROSS JOIN admin_menus m
WHERE r.code = 'super_admin'
  AND m.path = '/miniapp-users'
ON CONFLICT (role_id, menu_id) DO NOTHING;

COMMIT;
