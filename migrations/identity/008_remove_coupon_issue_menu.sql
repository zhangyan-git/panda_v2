-- identity/008: 发放优惠券入口从独立页面并入优惠券模板列表页
-- 可重复执行；只摘掉侧栏菜单项，coupon:issue 权限保留（发券接口仍在用）。

BEGIN;

-- admin_role_menus.menu_id 是 ON DELETE CASCADE，删菜单行会一并清掉角色绑定。
-- 007 里这条菜单是 WHERE NOT EXISTS 插进去的，这里按 path 删同样幂等：重复执行删到 0 行。
DELETE FROM admin_menus WHERE path = '/coupons/issue';

COMMIT;
