-- identity/024: 出库单的后台权限与菜单
-- 可重复执行；仅为 super_admin 绑定。
--
-- 一个码：`inventory:issue`——开出库单与它的三个动作（发货 / 送达 / 作废）。
--
-- # 为什么与 `inventory:receipt` 平级，而不是并进它
--
-- 023 把 read / manage / receipt 拆开的理由是「manage 改的是基础资料，receipt 动的是结余和
-- 流水」。出库按同一条逻辑落进来：**它动的也是结余和流水，只是方向相反**——入库把货记进来，
-- 出库把货发出去。既然「能改物料名字的人也能改库存数字」不该发生（023 的头一段），那
-- 「能收货的人就能发货」同样不该是默认的：仓里收货与发货常常是两个人，而这一枚码就是那条
-- 分界线。合并的话，要放开其中一边就必须连带另一边，这正是 023 拒绝过的形状。
--
-- # 今天它盖住的破坏力
--
-- 三个动作里只有发货与送达**动账**（开单与作废都不动：作废只在 pending，那时一条流水都
-- 没写过）。动的方向与入库相反但破坏力同档，所以待遇照抄收进那一侧：
--   * 发货与送达各写**两条**流水（出库腿：总部仓 −、在途仓 +；送达腿：在途仓 −、门店仓 +），
--     四条键互不相同（见 inventory/003 里 `movement_key` 那段重新写过的注释——它是全库唯一，
--     一条腿两条流水就必须靠后缀区分）；
--   * 流水只增这条约束在出库上同样生效（`stock_movements` 的触发器挡着 UPDATE / DELETE），
--     而这一刀的出库里**没有 reverse**：反悔的路径是作废，而作废只在 pending 允许——
--     发货之后没有回头路，这是 001:398 拍过的板，也是这一刀明知的缺口。
--
-- 仍然只绑 super_admin（迁移只负责让平台内置的那个角色能用，其余角色去角色管理里勾）。
-- 财务列**不需要新权限码**，它跟着读写页走。
--
-- 菜单：`/inventory/issues` 一条。出库单详情页不单独挂菜单（它是列表点进去的），与 023 里
-- 入库单只挂一条同形——023 头一段那句「所以这里只有六条」说的就是这个规矩。
--
-- 图标名要与 admin-web 的 src/menuIcons.tsx 登记的名字**同名**，否则侧栏渲染不出图标
-- （不报错，只是空着）。这一条在 022 上漏过一次，见那个文件里补登记注释。

BEGIN;

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  (gen_random_uuid(), 'inventory:issue', '订货管理', '出库开单与发货',
   '开出库单、发货（总部仓调出）、送达（门店仓调入）、作废未发货的单（需填原因）')
ON CONFLICT (code) DO NOTHING;

INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code = 'inventory:issue'
ON CONFLICT (role_id, permission_id) DO NOTHING;

INSERT INTO admin_menus (parent_id, name, path, icon, sort)
SELECT p.id, '出库单', '/inventory/issues', 'SendOutlined', 7
FROM admin_menus p
WHERE p.path = ''
  AND p.name = '订货管理'
  AND p.parent_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM admin_menus m WHERE m.path = '/inventory/issues');

INSERT INTO admin_role_menus (role_id, menu_id)
SELECT r.id, m.id
FROM admin_roles r
CROSS JOIN admin_menus m
WHERE r.code = 'super_admin'
  AND m.path = '/inventory/issues'
ON CONFLICT (role_id, menu_id) DO NOTHING;

COMMIT;
