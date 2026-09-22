-- identity/028: 开放平台（合作方接入）后台权限与菜单
-- 可重复执行；仅为 super_admin 绑定，其余角色去角色管理里勾。
--
-- 两枚码，与 026/027 那一对是同一个切法：
--   * partner:read   —— 看合作方、看密钥（只有掩码）、看调用日志。
--   * partner:manage —— 合作方与密钥的新增 / 修改 / 启停 / 签发新密钥 / 改 IP 白名单与限流。
--
-- **这一刀两枚码一起发，是有意的**，与 026 当时「刻意不写 manage 码」相反。026 不发是因为
-- 那时 payment-service 一条写接口都没有，发出去就是一枚勾了也没人认的码。这一次写接口从
-- 第一行代码起就在（合作方与密钥的 CRUD 是这一域的全部内容），所以两枚码都指向真实行为。
--
-- 签发密钥是 manage，不是 read，且**它的响应是唯一一次出现明文密钥的地方**——这一点在
-- partner-service 那一侧有测试盯着（internal/controller 的 reveal-once 用例）。码只管「谁能
-- 调到这个接口」，挡不住「谁把响应存了下来」，所以数据库里另存了掩码，读接口只回掩码。
--
-- 菜单：目录「开放平台」下面**一页**「合作方」。方案 §3.4 原文就是「新建一页」，所以这里
-- 只发一片叶子，不预先替页面结构做主。
--
-- 页内三件事：合作方与密钥（列表、签发弹窗、启停、IP 白名单、限流）、调用日志查询。
-- 「这家合作方还能不能调」与「他手上那几把钥匙是什么状态」摆在一起，因为合作方停用 →
-- 名下**所有**密钥当场失效，拆开只会让人来回翻着对。
--
-- 调用日志**没有单独发一片叶子**：它是同一页上的一个查询区（按合作方 + 时间段）。量级上它
-- 每次调用一条，将来真独立成一页时，由那条改动自己往 admin_menus 里补一行——现在多发一片，
-- 万一对面的页面不是那个路径，侧边栏里就多一个点不开的项。
--
-- 目录 path 留空——后台侧边栏来自 admin_menus（menuDataRender），path 不为空的目录会被
-- ProLayout 当成可跳转的菜单项渲染。
--
-- 图标名要与 admin-web 的 src/menuIcons.tsx 登记的名字**同名**，否则侧栏渲染不出图标
-- （不报错，只是空着）。022 上漏过一次，所以这里用的 ApiOutlined / SafetyCertificateOutlined
-- 两个都是先在 menuIcons.tsx 里登记好、再写在这儿的。

BEGIN;

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  (gen_random_uuid(), 'partner:read', '开放平台', '查看合作方与调用日志',
   '查看合作方列表、名下密钥（只显示掩码）、调用日志；只读，不含签发、启停或修改任何访问控制'),
  (gen_random_uuid(), 'partner:manage', '开放平台', '管理合作方与密钥',
   '新增 / 修改合作方，签发新密钥（**明文密钥只在创建响应里出现一次**），启用 / 停用合作方与密钥，修改 IP 白名单与每分钟限流')
ON CONFLICT (code) DO NOTHING;

-- 超管在中间件里直接放行，这里补绑定是为了前端 access 与权限回显能拿到码。
INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code IN ('partner:read', 'partner:manage')
ON CONFLICT (role_id, permission_id) DO NOTHING;

-- 排在一级菜单最后（13）。前 12 个位置已被占满，别再插到中间去——那会让所有人的侧边栏
-- 顺序在升级的那一刻整体挪位。
INSERT INTO admin_menus (name, path, icon, sort)
SELECT '开放平台', '', 'ApiOutlined', 13
WHERE NOT EXISTS (
  SELECT 1 FROM admin_menus WHERE parent_id IS NULL AND name = '开放平台' AND path = ''
);

INSERT INTO admin_menus (parent_id, name, path, icon, sort)
SELECT p.id, v.name, v.path, v.icon, v.sort
FROM admin_menus p
CROSS JOIN (VALUES
  ('合作方', '/partners', 'SafetyCertificateOutlined', 1)
) AS v(name, path, icon, sort)
WHERE p.path = ''
  AND p.name = '开放平台'
  AND p.parent_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM admin_menus m WHERE m.path = v.path);

-- 菜单绑定。
--
-- 这里**两个条件都要写**：026 上只写 `LIKE '/payments/%'` 时漏掉过落在前缀本身的那一页
-- （`/payments` 匹配不上 `LIKE '/payments/%'`，它要求后面还有一段），表现是超管登录时那一项
-- 点不开。本域的第一片叶子正好是 `/partners` 本身，同一个坑，所以等值条件留着。
INSERT INTO admin_role_menus (role_id, menu_id)
SELECT r.id, m.id
FROM admin_roles r
CROSS JOIN admin_menus m
WHERE r.code = 'super_admin'
  AND (
    m.path = '/partners'
    OR m.path LIKE '/partners/%'
    -- 目录节点本身没有 path，只能按名字认
    OR (m.path = '' AND m.name = '开放平台' AND m.parent_id IS NULL)
  )
ON CONFLICT (role_id, menu_id) DO NOTHING;

COMMIT;
