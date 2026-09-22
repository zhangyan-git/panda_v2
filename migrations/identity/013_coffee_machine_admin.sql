-- identity/013: 设备域后台权限
-- 可重复执行；仅为 super_admin 绑定设备域权限。
--
-- 只发权限，不发菜单。菜单由 015_coffee_machine_menus.sql 单独补，照的是 007 的写法：
-- 写这个文件时后台的 /coffee-machines 页面还没落地，先挂菜单只会让侧边栏多出一个点
-- 进去 404 的入口。页面与菜单后来都补齐了，本文件不回改——015 挂的菜单绑的就是这里
-- 发的权限码。

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
