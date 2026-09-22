-- identity/034: 会员管理下新增「店铺码活动」菜单
-- 可重复执行；仅为 super_admin 绑定。
--
-- **没有新权限码**：这一页读的是「哪家店在做活动、送多少天」，与会员列表同一档，用
-- membership:read；页面上的增改与启停用 membership:manage（它是「接下来送什么」，与套餐
-- 管理同一类，不改任何人的会员有效期，所以不是 adjust 那一枚——见 025 对三枚码的分工）。
--
-- 为什么挂在会员管理下：活动的产出是会员（领一次送一段会员天数），它读的、写的也都是会员域
-- 的表，与包月订阅同一条道理。老后台放在「会员管理 > 店铺码会员活动」下，位置一致。
--
-- 图标 QrcodeOutlined 要先在 admin-web 的 src/menuIcons.tsx 里登记（与 025、033 同一条注意
-- 事项：名字对不上不报错，只是侧栏那一格空着）。本次改动已同时登记。

BEGIN;

INSERT INTO admin_menus (parent_id, name, path, icon, sort)
SELECT p.id, v.name, v.path, v.icon, v.sort
FROM admin_menus p
CROSS JOIN (VALUES
  ('店铺码活动', '/membership/campaigns', 'QrcodeOutlined', 4)
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
  AND m.path = '/membership/campaigns'
ON CONFLICT (role_id, menu_id) DO NOTHING;

COMMIT;
