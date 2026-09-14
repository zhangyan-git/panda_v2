-- identity/012: 后台「操作日志」页面的权限码与菜单
--
-- 可重复执行；只给内置超管 super_admin 绑定，其它角色由后台自己分配。
--
-- 日志表本身由 identity/001 建好，这里只补「谁能看」和「入口在哪」。
--
-- 只有 view 一个码，没有 manage：admin_operation_logs 是审计证据，后台页面只读，
-- 不提供改和删。加一个 manage 码等于对外承诺有对应的写操作，而那正是这张表
-- 不应该有的东西——要清理历史得走 DBA 的留存策略，不是管理员在界面上点。

BEGIN;

-- ============================================================
-- 权限码
-- ============================================================

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  (gen_random_uuid(), 'admin:operation-logs:view', '操作日志', '查看操作日志',
   '查看后台操作日志；只读，不包含任何修改或删除能力')
ON CONFLICT (code) DO NOTHING;

-- 超管在中间件里直接放行，这里补绑定是为了前端 access 与权限回显能拿到码。
INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code = 'admin:operation-logs:view'
ON CONFLICT (role_id, permission_id) DO NOTHING;

-- ============================================================
-- 菜单
-- ============================================================
--
-- 顶级菜单，排在「小程序用户」之后（排序 6）。不挂到「系统管理」下面：那个目录
-- 装的是权限/角色/管理员/菜单，是 IAM 那一摊；操作日志覆盖的是全部业务模块
-- （商户、优惠券、小程序用户都会往里写），放进去反而把它说小了。
--
-- 图标 HistoryOutlined 要在 admin-web/src/menuIcons.tsx 里登记，否则侧栏这一项
-- 渲染不出图标（后端只存组件名，图标组件由前端映射）。

INSERT INTO admin_menus (name, path, icon, sort)
SELECT '操作日志', '/operation-logs', 'HistoryOutlined', 6
WHERE NOT EXISTS (
  SELECT 1 FROM admin_menus WHERE parent_id IS NULL AND path = '/operation-logs'
);

INSERT INTO admin_role_menus (role_id, menu_id)
SELECT r.id, m.id
FROM admin_roles r
CROSS JOIN admin_menus m
WHERE r.code = 'super_admin'
  AND m.path = '/operation-logs'
ON CONFLICT (role_id, menu_id) DO NOTHING;

COMMIT;
