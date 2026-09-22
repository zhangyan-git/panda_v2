-- identity/014: 设备域后台写权限
-- 可重复执行；仅为 super_admin 绑定。
--
-- 读权限（coffee_machine:read）在 013 里发过了，这里补两个写码。分成写和资金两个码，
-- 是因为它们的破坏力差一档：coffee_machine:manage 能改设备资料与饮品价格，
-- coffee_machine:balance 能直接动设备的咖啡余额。合成一个码之后，想让运营维护饮品
-- 目录，就顺手把加钱的能力也给了。
--
-- 仍然不发菜单：菜单由 015_coffee_machine_menus.sql 单独补（理由同 013——写这个文件时
-- 后台的 /coffee-machines 页面还没落地）。

BEGIN;

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  (gen_random_uuid(), 'coffee_machine:manage', '设备管理', '编辑设备主数据', '新增/修改厂商、咖啡机设备与饮品，以及启用停用与上下架'),
  (gen_random_uuid(), 'coffee_machine:balance', '设备管理', '调整设备余额', '调整咖啡机设备的咖啡余额，操作全程留痕')
ON CONFLICT (code) DO NOTHING;

INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code IN ('coffee_machine:manage', 'coffee_machine:balance')
ON CONFLICT (role_id, permission_id) DO NOTHING;

COMMIT;
