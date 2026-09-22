-- identity/016: 订单域后台权限
-- 可重复执行；仅为 super_admin 绑定。
--
-- 两个码：order:read 看列表与详情，order:manage 取消订单。分开的理由是破坏力差一档——
-- 取消是一次替用户动他的资产的操作，让客服能查单不等于让客服能关单。后台的取消会写审计
-- （进 admin_operation_logs），所以「谁关的」事后查得到，但权限上仍要先把口子收窄。
--
-- 不发菜单：菜单由 018_order_menus.sql 单独补（同 013/014 的理由——写这个文件时后台的
-- 订单页面还没落地，先挂菜单只会让侧边栏多出一个点进去 404 的入口）。
--
-- 也没有 order:*:export 之类的码：导出订单数据目前只有列表分页接口，没有单独的文件导出，
-- 为一个还不存在的动作发码等于发一个永远勾不动的权限。

BEGIN;

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  (gen_random_uuid(), 'order:read', '订单管理', '查看订单', '查看订单列表与订单详情，含订单行、支付流水与状态流水'),
  (gen_random_uuid(), 'order:manage', '订单管理', '取消订单', '取消待支付订单，操作会写入平台审计')
ON CONFLICT (code) DO NOTHING;

INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code IN ('order:read', 'order:manage')
ON CONFLICT (role_id, permission_id) DO NOTHING;

COMMIT;
