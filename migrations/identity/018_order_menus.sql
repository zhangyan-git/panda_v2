-- identity/018: 订单域后台菜单
-- 可重复执行；仅为 super_admin 绑定。
--
-- 016/017 发权限码时特意把菜单留了下来，理由写在那里：页面还没写，先挂菜单只会让
-- 侧边栏多出一个点进去 404 的入口。后台的订单列表、订单详情与退款申请审核页已经
-- 落地，照 015 的写法把菜单补上，权限码沿用 016/017 那三个，这里不再重复发。
--
-- 第二个子项叫「退款申请」而不是老后台的「退款订单」：这一页审的是用户提交的售后
-- 申请，退款单在 payment-service（还没建）。今天点了「通过申请」钱并不会出去，
-- 菜单名不该比页面上的按钮说得更满。
--
-- super_admin 之外的角色要看到这两页，得自己去角色管理里勾菜单与权限——迁移只负责
-- 让平台内置的那个角色能用。

BEGIN;

INSERT INTO admin_menus (name, path, icon, sort)
SELECT '订单管理', '', 'ShoppingCartOutlined', 8
WHERE NOT EXISTS (
  SELECT 1 FROM admin_menus WHERE parent_id IS NULL AND name = '订单管理' AND path = ''
);

INSERT INTO admin_menus (parent_id, name, path, icon, sort)
SELECT p.id, v.name, v.path, v.icon, v.sort
FROM admin_menus p
CROSS JOIN (VALUES
  ('订单列表', '/orders', 'ProfileOutlined', 1),
  ('退款申请', '/after-sales', 'RollbackOutlined', 2)
) AS v(name, path, icon, sort)
WHERE p.path = ''
  AND p.name = '订单管理'
  AND p.parent_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM admin_menus m WHERE m.path = v.path);

INSERT INTO admin_role_menus (role_id, menu_id)
SELECT r.id, m.id
FROM admin_roles r
CROSS JOIN admin_menus m
WHERE r.code = 'super_admin'
  AND (
    m.path IN ('/orders', '/after-sales')
    -- 目录节点本身没有 path，只能按名字认
    OR (m.path = '' AND m.name = '订单管理' AND m.parent_id IS NULL)
  )
ON CONFLICT (role_id, menu_id) DO NOTHING;

COMMIT;
