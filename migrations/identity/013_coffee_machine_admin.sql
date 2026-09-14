-- identity/013: 设备域后台权限
-- 可重复执行；仅为 super_admin 绑定设备域权限。
--
-- 只发权限，不发菜单。优惠券那条（007）一次把权限和后台菜单都发了，这里不发菜单
-- 是有意的：后台的 /coffee-machines 页面还没写，先挂菜单只会让侧边栏多出一个点进去
-- 404 的入口。等页面落地时照 007 的写法把菜单补进来。

BEGIN;

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  (gen_random_uuid(), 'coffee_machine:read', '设备管理', '查看设备主数据', '查看厂商、咖啡机设备、饮品与设备供应关系')
ON CONFLICT (code) DO NOTHING;

INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code = 'coffee_machine:read'
ON CONFLICT (role_id, permission_id) DO NOTHING;

COMMIT;
