-- identity/023: 订货 / 库存域后台权限与菜单
-- 可重复执行；仅为 super_admin 绑定。
--
-- 三个码，按动作拆，理由与 022 的抽奖那三个同源（同样是「看着像一回事、破坏力差两档」）：
--   inventory:read    看物料、配方、仓库、结余、流水、入库单。这一档迟早要发给客服，
--                     是查单的延伸；**看单只要它**，包括看那张单写了什么。
--   inventory:manage  增删改物料与配方、建改仓库、设补货线。改的是**基础资料**——改错了
--                     会让下一次入库按错的换算或错的线走，但不动已经记下的数。
--   inventory:receipt 开入库单、改草稿、**确认**、**作废**。这一档动的是结余与流水本身：
--                     确认把货记进账，作废把已经记进账的货抹掉。它是本域唯一能让库存数字
--                     变化的动作，所以刻意**不与 manage 合并**——「能改物料名字的人也能改
--                     库存数字」是不该发生的事，而两枚码分开之后，要放开前者不必连带后者。
--
-- 今天三个码都只绑 super_admin（迁移只负责让平台内置的那个角色能用，其余角色去角色管理里
-- 勾）。所以**本轮真正的收窄不在绑定上**，而在那一路上：
--   * 作废**强制填原因**（`stock_receipts` 上有一条 CHECK 钉着：`status='void'` 必须配非空的
--     `void_reason`，不是靠前端自觉）；
--   * 作废是**人工干预**——它把一笔已经入账的货抹掉，而方案 §11.6 把「影响资产归属的人工
--     操作」列进必审清单，所以每次作废写一条平台审计（确认不写：那是日常动作，每次都记只会
--     把真正要看的那几条淹掉，痕迹在 `stock_receipts.confirmed_by` 上）；
--   * 流水**只增**：`stock_movements` 上 UPDATE / DELETE 被触发器挡着，作废不是删单而是另写
--     一条 `reverse`，原来那条 `receipt` 一个字不动——「作废过」这件事因此是**查得到的**，
--     而不是「查不到那张单了」。
-- 与 021 对咖啡豆余额调整的处理逐字同构：同样是直接动数字的能力，同样只绑 super_admin，
-- 同样靠 reason + 审计 + 只增流水代替审批。
--
-- 菜单：/inventory/* 六页（物料、饮品配方、仓库、结余、流水、入库单），照 015/018/022 的写法
-- 一次挂全。目录 path 留空——后台侧边栏来自 admin_menus（menuDataRender），path 不为空的
-- 目录会被 ProLayout 当成可跳转的菜单项渲染。
--
-- 「结余」与「流水」分成两页而不是一页两个 tab：它们回答的是两个问题——「现在还剩多少」
-- （可以改，靠入库/作废）与「为什么会是这个数」（只增，是账）。合成一页会让人以为流水也能改。
-- 入库单详情页不单独挂菜单（它是列表点进去的），所以这里只有六条。
--
-- 图标名要与 admin-web 的 src/menuIcons.tsx 登记的名字**同名**，否则侧栏渲染不出图标
-- （不报错，只是空着）。这一条在 022 上漏过一次，见那个文件里补登记注释。

BEGIN;

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  (gen_random_uuid(), 'inventory:read', '订货管理', '查看库存',
   '查看物料、饮品配方、仓库、结余与流水、入库单'),
  (gen_random_uuid(), 'inventory:manage', '订货管理', '管理库存基础资料',
   '增删改物料与饮品配方、建改仓库、设置按仓库/物料覆盖的补货线'),
  (gen_random_uuid(), 'inventory:receipt', '订货管理', '入库开单与确认',
   '开入库单、改草稿、确认入库（把货记进结余与流水）、作废已入库的单（需填原因，操作会写入平台审计）')
ON CONFLICT (code) DO NOTHING;

INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code IN ('inventory:read', 'inventory:manage', 'inventory:receipt')
ON CONFLICT (role_id, permission_id) DO NOTHING;

INSERT INTO admin_menus (name, path, icon, sort)
SELECT '订货管理', '', 'DatabaseOutlined', 10
WHERE NOT EXISTS (
  SELECT 1 FROM admin_menus WHERE parent_id IS NULL AND name = '订货管理' AND path = ''
);

INSERT INTO admin_menus (parent_id, name, path, icon, sort)
SELECT p.id, v.name, v.path, v.icon, v.sort
FROM admin_menus p
CROSS JOIN (VALUES
  ('物料', '/inventory/materials', 'GoldOutlined', 1),
  ('饮品配方', '/inventory/recipes', 'ExperimentOutlined', 2),
  ('仓库', '/inventory/warehouses', 'HomeOutlined', 3),
  ('结余', '/inventory/stock-levels', 'BarChartOutlined', 4),
  ('流水', '/inventory/stock-movements', 'SwapOutlined', 5),
  ('入库单', '/inventory/receipts', 'InboxOutlined', 6)
) AS v(name, path, icon, sort)
WHERE p.path = ''
  AND p.name = '订货管理'
  AND p.parent_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM admin_menus m WHERE m.path = v.path);

INSERT INTO admin_role_menus (role_id, menu_id)
SELECT r.id, m.id
FROM admin_roles r
CROSS JOIN admin_menus m
WHERE r.code = 'super_admin'
  AND (
    m.path LIKE '/inventory/%'
    -- 目录节点本身没有 path，只能按名字认
    OR (m.path = '' AND m.name = '订货管理' AND m.parent_id IS NULL)
  )
ON CONFLICT (role_id, menu_id) DO NOTHING;

COMMIT;
