-- identity/021: 咖啡豆账户的后台调整权限
-- 可重复执行；仅为 super_admin 绑定。
--
-- 020 的文件头预告过这一天：「账户域没有人工发放/调整接口（本轮没做），所以没有配套的 manage
-- 码……等人工调整落地时再加，那时它会走 order:manage 那一路的写法（单独一个码 + 写平台审计）」。
-- 这一条就是那个写法，两处注释互相指认。
--
-- 为什么读**不**复用一个码：调整是写，查看是读，混成一个码意味着「能看的人就能改别人的余额」。
-- 账户域的表已经按这个方向拆了路由（GET 走 account:read，POST /adjustments 走 account:manage），
-- 权限数据要跟上，否则那道拆分在库里没有对应的名字。
--
-- 为什么只绑 super_admin：这是**直接加钱**的能力（adjust 金额带符号，充值就是正数），没有审批
-- 环节、没有额度上限，也没有第二个人复核。发宽了等于把「凭空造豆」变成一个普通运营动作。
-- 真要放开，先要做的是给调整加审批流，而不是在这里多绑一个角色。
--
-- 不发菜单：调整入口挂在小程序用户详情抽屉里的「咖啡豆账户」区块上。同 020 的理由。

BEGIN;

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  (gen_random_uuid(), 'account:manage', '资产账户', '调整咖啡豆余额',
   '后台人工调整用户的咖啡豆余额（充值为正、纠错为负），带符号金额、全额留痕')
ON CONFLICT (code) DO NOTHING;

INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code = 'account:manage'
ON CONFLICT (role_id, permission_id) DO NOTHING;

COMMIT;
