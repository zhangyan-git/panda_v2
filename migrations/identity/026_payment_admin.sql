-- identity/026: 支付域后台权限与菜单
-- 可重复执行；仅为 super_admin 绑定，其余角色去角色管理里勾。
--
-- 一个码：payment:read 看支付单列表与详情、看支付方式与渠道的配置。
--
-- **这里刻意没有写码**——落地时本服务一条写接口都没有，与 020 对福卡账户的处理逐字同一条
-- 理由：发一个今天永远勾不出对应行为、勾了也没有任何接口认的 manage 码，只会让权限页在骗人。
-- 后来真做了写入接口，写码发在 027（payment:manage，管两张配置表的增改与启停）；两枚码
-- 分开的理由也写在那边。
--
-- 所以后台这一刀只补上了「能看」，「能配」是 027 的事，两页在菜单上共用（026 建的那两个叶子）。
--
-- 只读而不是「顺便加几个按钮」，还因为支付单是**钱的既成事实**：一笔 succeeded 的单子后面
-- 挂着出资行、记账流水、渠道调用、回调通知四张只增表。开任何一个写入口（关单、改状态、重放
-- 回调）都要先有退款与对账那两件事的语义，而它们本轮没做（见 docs/architecture.md 支付那一段）。
--
-- 菜单：目录「支付管理」下面两页。
--   * 支付单 —— 列表 + 详情。详情里带出资行 / 记账流水 / 状态流转 / 渠道调用 / 回调通知五张
--     子表，它们**不单独挂菜单**：离开那张支付单就没有意义（与 023 的入库单详情形同）。
--   * 支付方式与渠道 —— 两张配置表摆在一起。它们是一件事的两面：「这台机器能点哪几个按钮」
--     与「那几个按钮背后连着谁的通道」，分开两页会让人来回翻。
--
-- 目录 path 留空——后台侧边栏来自 admin_menus（menuDataRender），path 不为空的目录会被
-- ProLayout 当成可跳转的菜单项渲染。
--
-- 图标名要与 admin-web 的 src/menuIcons.tsx 登记的名字**同名**，否则侧栏渲染不出图标
-- （不报错，只是空着）。022 上漏过一次，所以这里用的 PayCircleOutlined / CreditCardOutlined /
-- ApiOutlined 三个都是先在 menuIcons.tsx 里登记好、再写在这儿的。

BEGIN;

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  (gen_random_uuid(), 'payment:read', '支付管理', '查看支付单与支付方式',
   '查看支付单列表与详情（出资行、记账流水、状态流转、渠道调用、回调通知）、支付方式与渠道配置；只读，不包含任何发起、关单或退款能力')
ON CONFLICT (code) DO NOTHING;

-- 超管在中间件里直接放行，这里补绑定是为了前端 access 与权限回显能拿到码。
INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code = 'payment:read'
ON CONFLICT (role_id, permission_id) DO NOTHING;

-- 排在一级菜单最后（12）。前 11 个位置已被占满，别再插到中间去——那会让所有人的侧边栏
-- 顺序在升级的那一刻整体挪位。
INSERT INTO admin_menus (name, path, icon, sort)
SELECT '支付管理', '', 'PayCircleOutlined', 12
WHERE NOT EXISTS (
  SELECT 1 FROM admin_menus WHERE parent_id IS NULL AND name = '支付管理' AND path = ''
);

INSERT INTO admin_menus (parent_id, name, path, icon, sort)
SELECT p.id, v.name, v.path, v.icon, v.sort
FROM admin_menus p
CROSS JOIN (VALUES
  ('支付单', '/payments', 'CreditCardOutlined', 1),
  ('支付方式与渠道', '/payments/methods', 'ApiOutlined', 2)
) AS v(name, path, icon, sort)
WHERE p.path = ''
  AND p.name = '支付管理'
  AND p.parent_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM admin_menus m WHERE m.path = v.path);

-- 菜单绑定。
--
-- **这里比 025 多一个 `m.path = '/payments'` 的等值条件，不能照抄 025 的 `LIKE '/payments/%'`**：
-- 支付单列表正好落在 `/payments` 本身，而 `/payments` 匹配不上 `LIKE '/payments/%'`（它需要
-- 后面还有一段）。只写 LIKE 的话目录与两片叶子都建出来了、权限也绑了，独独 super_admin 的
-- 侧边栏里那一项点不开——一个只在「用超管登录」时才看得见的错。
INSERT INTO admin_role_menus (role_id, menu_id)
SELECT r.id, m.id
FROM admin_roles r
CROSS JOIN admin_menus m
WHERE r.code = 'super_admin'
  AND (
    m.path = '/payments'
    OR m.path LIKE '/payments/%'
    -- 目录节点本身没有 path，只能按名字认
    OR (m.path = '' AND m.name = '支付管理' AND m.parent_id IS NULL)
  )
ON CONFLICT (role_id, menu_id) DO NOTHING;

COMMIT;
