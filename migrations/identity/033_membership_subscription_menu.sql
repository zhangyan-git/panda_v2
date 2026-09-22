-- identity/033: 会员管理下新增「包月订阅」菜单
-- 可重复执行；仅为 super_admin 绑定。
--
-- **没有新权限码**：这一页读的是「谁签了连续包月、这一期扣了没」，与会员列表同一档，用
-- membership:read；页面上唯一一个写动作「取消」用 membership:manage（它是配置与管理，
-- 不动任何人的会员有效期，所以不是 adjust 那一枚——见 025 对三枚码的分工）。
--
-- 为什么挂在会员管理下而不是单开一个目录：订阅是会员身上的一个附属物（一个会员至多一条活着的
-- 订阅），离开那个会员就没有意义，与会员详情里的变更流水同一条道理。区别只在于它值得一页
-- ——按用户查、按状态筛是客服的日常动作，塞进详情页做不到。
--
-- 老后台这一页在「会员管理 > 包月订阅管理」下，位置一致。
--
-- 图标 SyncOutlined 要先在 admin-web 的 src/menuIcons.tsx 里登记（与 025 那条注意事项同一
-- 件事：名字对不上不报错，只是侧栏那一格空着）。本次改动已同时登记。

BEGIN;

INSERT INTO admin_menus (parent_id, name, path, icon, sort)
SELECT p.id, v.name, v.path, v.icon, v.sort
FROM admin_menus p
CROSS JOIN (VALUES
  ('包月订阅', '/membership/subscriptions', 'SyncOutlined', 3)
) AS v(name, path, icon, sort)
WHERE p.path = ''
  AND p.name = '会员管理'
  AND p.parent_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM admin_menus m WHERE m.path = v.path);

-- 绑定必须**排在插入之后**：025 那条按 path LIKE '/membership/%' 的绑定是它自己那次执行的
-- 快照，新插进来的这一行不在里面。同一个角色、同一条规则，这里只补新路径。
INSERT INTO admin_role_menus (role_id, menu_id)
SELECT r.id, m.id
FROM admin_roles r
CROSS JOIN admin_menus m
WHERE r.code = 'super_admin'
  AND m.path = '/membership/subscriptions'
ON CONFLICT (role_id, menu_id) DO NOTHING;

COMMIT;
