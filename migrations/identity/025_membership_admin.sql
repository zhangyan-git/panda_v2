-- identity/025: 会员域后台权限与菜单
-- 可重复执行；仅为 super_admin 绑定。
--
-- 三个码，按动作拆，理由与 022/023 同源（同样是「看着像一回事、破坏力差两档」）：
--   membership:read    看套餐、看会员列表与详情、看变更流水。这一档迟早要发给客服——
--                     用户问「我是不是会员、什么时候到期、为什么被冻了」，答这三句只需要它。
--   membership:manage  建改套餐：改价、改时长、改会员价路径、上下架。改的是**接下来卖什么**，
--                     它一个已经卖出去的会员都动不了——memberships 上存着成交快照
--                     （plan_code / plan_name / member_price_mode 那一组），套餐再改也不会
--                     追溯改变已购会员的权益。这是本域唯一一处「改了也不用怕」的写入口。
--   membership:adjust  后台人工调整会员：冻结 / 解冻 / 撤销 / 直接改有效期。
--                     **这一枚直接白送钱**：把一个人的 expire_at 往后挪一年，等于送他一年会员价，
--                     没有任何订单、支付、流水跟着发生。所以刻意不与 manage 合并——「能改套餐
--                     文案的人也能给人加一年会员」是不该发生的事。它也**不在**小程序那条路上
--                     （那条路只能碰调用者自己的会员，且只能开关自动续费）。
--
-- 今天三个码都只绑 super_admin（迁移只负责让平台内置的那个角色能用，其余角色去角色管理里
-- 勾）。所以**本轮真正的收窄不在绑定上**，而在那一路上：
--   * adjust 的每一次调用都写一条**平台审计**（方案 §11.6「影响用户资产归属的人工操作」），
--     经 admin.operation.logged → 身份库的 admin_operation_logs，本域不建自己的审计表；
--   * 它同时在 membership_changes 里留一条流水，带上操作人、原因与备注——审计答「谁在什么时候
--     动了手」，流水答「会员本身因此变成了什么样」，两句话都要能查到；
--   * membership_changes 只增不改不删（触发器物理阻断，与 stock_movements 同套写法），
--     所以「这次调整发生过」是查得到的，而不是被下一条覆盖掉的。
-- 与 021 对咖啡豆余额调整、023 对入库作废的处理逐字同构：同样是直接动数字的能力，同样只绑
-- super_admin，同样靠 reason + 审计 + 只增流水代替审批。
--
-- 菜单：/membership/* 两页。**只有两页**，因为会员域只有两个可看的东西——「卖了什么」
-- （套餐）与「谁在会员中」（会员）。变更流水不单独挂菜单：它是会员详情页里的时间线，离开
-- 那个会员就没有意义（与 023 里入库单详情不挂菜单同一条理由）。
-- 目录 path 留空——后台侧边栏来自 admin_menus（menuDataRender），path 不为空的目录会被
-- ProLayout 当成可跳转的菜单项渲染。
--
-- 图标名要与 admin-web 的 src/menuIcons.tsx 登记的名字**同名**，否则侧栏渲染不出图标
-- （不报错，只是空着）。022 上漏过一次，所以这里用的 CrownOutlined / TagOutlined /
-- ContactsOutlined 三个都是先确认过已登记的。

BEGIN;

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  (gen_random_uuid(), 'membership:read', '会员管理', '查看会员与套餐',
   '查看会员套餐、会员列表与详情、会员变更流水'),
  (gen_random_uuid(), 'membership:manage', '会员管理', '管理会员套餐',
   '新增与修改会员套餐（价格、时长、是否自动续费、会员价路径）、上下架；不影响已购会员'),
  (gen_random_uuid(), 'membership:adjust', '会员管理', '调整会员资格',
   '冻结 / 解冻 / 撤销会员、后台直接调整有效期（需填原因，操作会写入平台审计与会员流水）')
ON CONFLICT (code) DO NOTHING;

INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code IN ('membership:read', 'membership:manage', 'membership:adjust')
ON CONFLICT (role_id, permission_id) DO NOTHING;

INSERT INTO admin_menus (name, path, icon, sort)
SELECT '会员管理', '', 'CrownOutlined', 11
WHERE NOT EXISTS (
  SELECT 1 FROM admin_menus WHERE parent_id IS NULL AND name = '会员管理' AND path = ''
);

INSERT INTO admin_menus (parent_id, name, path, icon, sort)
SELECT p.id, v.name, v.path, v.icon, v.sort
FROM admin_menus p
CROSS JOIN (VALUES
  ('会员套餐', '/membership/plans', 'TagOutlined', 1),
  ('会员列表', '/membership/members', 'ContactsOutlined', 2)
) AS v(name, path, icon, sort)
WHERE p.path = ''
  AND p.name = '会员管理'
  AND p.parent_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM admin_menus m WHERE m.path = v.path);

INSERT INTO admin_role_menus (role_id, menu_id)
SELECT r.id, m.id
FROM admin_roles r
CROSS JOIN admin_menus m
WHERE r.code = 'super_admin'
  AND (
    m.path LIKE '/membership/%'
    -- 目录节点本身没有 path，只能按名字认
    OR (m.path = '' AND m.name = '会员管理' AND m.parent_id IS NULL)
  )
ON CONFLICT (role_id, menu_id) DO NOTHING;

COMMIT;
