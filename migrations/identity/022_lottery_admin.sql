-- identity/022: 抽奖域后台权限与菜单
-- 可重复执行；仅为 super_admin 绑定。
--
-- 三个码，按动作拆，理由不一样：
--   lottery:read   看开通门店、活动、期次、中奖记录。这一档迟早要发给客服，是查单的延伸。
--   lottery:manage 开通/停用门店、增删改活动与奖池、作废期次。这一档改的是「用户能抽什么」，
--                  直接决定中奖名额的分配，破坏力比看列表高一档。
--   lottery:draw   人工开奖。这是**替系统决定谁中奖**，是本域唯一能凭空造出一份中奖名单的
--                  动作，与 lottery:manage 分开正是为了让放开前两个码的那天不必连带放开它。
--
-- 今天三个码都只绑 super_admin（迁移只负责让平台内置的那个角色能用，其余角色去角色管理里
-- 勾）。所以**本轮真正的收窄不在绑定上**，而在那一路上：人工开奖强制填原因、请求体带
-- expected_round_status + expected_participant_count（管理员拿着过期页面开一局已经变了的奖
-- 会被 409 挡下）、开奖记录只增不可改（lottery_draws 有触发器）、每次人工开奖写一条平台审计。
-- 方案 §18.3 要求的「审批」这一环**本轮没做**，补偿控制就是上面这四件事——先把话说清楚，
-- 免得事后以为这里有审批。（这个写法与 021 对咖啡豆余额调整的处理逐字同构：同样是直接
-- 动用户资产的能力，同样只绑 super_admin，同样靠 reason + 审计 + 只增流水代替审批。）
--
-- 菜单：/lottery/* 四页已经落地（开通门店、抽奖活动、期次、中奖记录），照 015/018 的写法
-- 一次挂全。目录 path 留空——后台侧边栏来自 admin_menus（menuDataRender），path 不为空的
-- 目录会被 ProLayout 当成可跳转的菜单项渲染。
--
-- 「期次」单独一页而不是并进活动详情：一期一期滚动开是这套模型的主轴（原型里 roundNo 已经
-- 到 LAKE-202608-12），运营要在一屏里看所有在跑的期次进度，逐层点进活动看不到这个视角。

BEGIN;

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  (gen_random_uuid(), 'lottery:read', '抽奖管理', '查看抽奖',
   '查看开通门店、抽奖活动、期次进度与中奖记录'),
  (gen_random_uuid(), 'lottery:manage', '抽奖管理', '管理抽奖活动',
   '开通或停用门店抽奖、增删改活动与奖池、作废未开奖期次，操作会写入平台审计'),
  (gen_random_uuid(), 'lottery:draw', '抽奖管理', '人工开奖',
   '人工触发一次开奖并生成中奖名单，需填写原因，操作会写入平台审计')
ON CONFLICT (code) DO NOTHING;

INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code IN ('lottery:read', 'lottery:manage', 'lottery:draw')
ON CONFLICT (role_id, permission_id) DO NOTHING;

INSERT INTO admin_menus (name, path, icon, sort)
SELECT '抽奖管理', '', 'TrophyOutlined', 9
WHERE NOT EXISTS (
  SELECT 1 FROM admin_menus WHERE parent_id IS NULL AND name = '抽奖管理' AND path = ''
);

INSERT INTO admin_menus (parent_id, name, path, icon, sort)
SELECT p.id, v.name, v.path, v.icon, v.sort
FROM admin_menus p
CROSS JOIN (VALUES
  ('开通门店', '/lottery/activations', 'EnvironmentOutlined', 1),
  ('抽奖活动', '/lottery/campaigns', 'GiftOutlined', 2),
  ('期次', '/lottery/rounds', 'FieldTimeOutlined', 3),
  ('中奖记录', '/lottery/wins', 'CrownOutlined', 4)
) AS v(name, path, icon, sort)
WHERE p.path = ''
  AND p.name = '抽奖管理'
  AND p.parent_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM admin_menus m WHERE m.path = v.path);

INSERT INTO admin_role_menus (role_id, menu_id)
SELECT r.id, m.id
FROM admin_roles r
CROSS JOIN admin_menus m
WHERE r.code = 'super_admin'
  AND (
    m.path LIKE '/lottery/%'
    -- 目录节点本身没有 path，只能按名字认
    OR (m.path = '' AND m.name = '抽奖管理' AND m.parent_id IS NULL)
  )
ON CONFLICT (role_id, menu_id) DO NOTHING;

COMMIT;
