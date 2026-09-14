-- identity/015: 设备域后台菜单
-- 可重复执行；仅为 super_admin 绑定。
--
-- 013/014 发权限时特意把菜单留了下来，理由写在那里：页面还没写，先挂菜单只会让
-- 侧边栏多出一个点进去 404 的入口。后台的 /coffee-machines 三页已经落地，照 007
-- 的写法把菜单补上，权限码沿用 013/014 那三个，这里不再重复发。
--
-- super_admin 之外的角色要看到这三页，得自己去角色管理里勾菜单与权限——迁移只负责
-- 让平台内置的那个角色能用。

BEGIN;

-- 一个顶级目录 + 三个子菜单，与 .umirc.ts 里的父子路由一一对应。目录 path 留空：
-- 后台侧边栏来自这张表（menuDataRender），path 不为空的目录会被 ProLayout 当成
-- 可跳转的菜单项渲染。
INSERT INTO admin_menus (name, path, icon, sort)
SELECT '设备管理', '', 'CoffeeOutlined', 7
WHERE NOT EXISTS (
  SELECT 1 FROM admin_menus WHERE parent_id IS NULL AND name = '设备管理' AND path = ''
);

INSERT INTO admin_menus (parent_id, name, path, icon, sort)
SELECT p.id, v.name, v.path, v.icon, v.sort
FROM admin_menus p
CROSS JOIN (VALUES
  ('咖啡机设备', '/coffee-machines/devices', 'CoffeeOutlined', 1),
  ('厂商管理', '/coffee-machines/manufacturers', 'ShopOutlined', 2),
  ('饮品管理', '/coffee-machines/drinks', 'AppstoreOutlined', 3)
) AS v(name, path, icon, sort)
WHERE p.path = ''
  AND p.name = '设备管理'
  AND p.parent_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM admin_menus m WHERE m.path = v.path);

INSERT INTO admin_role_menus (role_id, menu_id)
SELECT r.id, m.id
FROM admin_roles r
CROSS JOIN admin_menus m
WHERE r.code = 'super_admin'
  AND (
    m.path LIKE '/coffee-machines/%'
    -- 目录节点本身没有 path，只能按名字认
    OR (m.path = '' AND m.name = '设备管理' AND m.parent_id IS NULL)
  )
ON CONFLICT (role_id, menu_id) DO NOTHING;

COMMIT;
