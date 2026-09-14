-- identity/019: 订单管理下的三个分类菜单（咖啡 / 幸运杯套 / 会员）
-- 可重复执行；仅为 super_admin 绑定。
--
-- 018 只挂了「订单列表 / 退款申请」两项。现在列表按**行类型**分成三类，形态与老后台
-- 「订单管理 > 咖啡订单 / 会员订单 / …」按业务类别分子项一致。
--
-- 分类口径是「含哪类行」：V2 的订单是合并单（一杯饮品 + 若干加购品 + 一个会员套餐可以在
-- 一单里），一张单同时含几类就同时出现在几个列表里，三个列表之间**不互斥**。这是用户拍板的
-- 口径，不是重复数据；列表里的「构成」一列就是让人看出「这单还含什么」。
--
-- 「订单列表」保留在最前：客服拿着一个单号来找单时并不知道它属于哪类，需要一个不分类型的入口。
-- 权限码沿用 016 的 order:read（三个分类列表与订单列表是同一个读权限），这里不发新码。
--
-- super_admin 之外的角色要看到这几个入口，得自己去角色管理里勾菜单——迁移只负责让平台
-- 内置的那个角色能用。

BEGIN;

INSERT INTO admin_menus (parent_id, name, path, icon, sort)
SELECT p.id, v.name, v.path, v.icon, v.sort
FROM admin_menus p
CROSS JOIN (VALUES
  ('咖啡订单', '/orders/coffee', 'CoffeeOutlined', 2),
  ('幸运杯套订单', '/orders/cup-sleeve', 'TagOutlined', 3),
  ('会员订单', '/orders/membership', 'TeamOutlined', 4)
) AS v(name, path, icon, sort)
WHERE p.path = ''
  AND p.name = '订单管理'
  AND p.parent_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM admin_menus m WHERE m.path = v.path);

-- 退款申请从 2 挪到 5，排在三个分类之后（侧栏按 ORDER BY sort, created_at 读）。
-- 只在它确实还排在前面时才改：重复执行时这一条不该把 sort 再改一遍。
UPDATE admin_menus m SET sort = 5
FROM admin_menus p
WHERE p.path = '' AND p.name = '订单管理' AND p.parent_id IS NULL
  AND m.parent_id = p.id
  AND m.path = '/after-sales'
  AND m.sort < 5;

INSERT INTO admin_role_menus (role_id, menu_id)
SELECT r.id, m.id
FROM admin_roles r
CROSS JOIN admin_menus m
WHERE r.code = 'super_admin'
  AND m.path IN ('/orders/coffee', '/orders/cup-sleeve', '/orders/membership')
ON CONFLICT (role_id, menu_id) DO NOTHING;

COMMIT;
