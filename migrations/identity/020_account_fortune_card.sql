-- identity/020: 资产账户域（福卡）后台权限
-- 可重复执行；仅为 super_admin 绑定。
--
-- 一个码：account:read，看福卡账户与流水。它管的是**用户的资产**，但只是看——账户域没有
-- 人工发放/调整接口（本轮没做），所以没有配套的 manage 码：发一个永远勾不动、勾了也没有
-- 任何接口认的权限，只会让权限页在骗人。等人工调整落地时再加，那时它会走 order:manage
-- 那一路的写法（单独一个码 + 写平台审计）。
--
-- 读为什么也单独发码、而不是并进 order:read：余额与流水是**别的库**的事实，不随订单接口
-- 一起给出。而且订单那边的 order:read 描述已经写死成「查看订单列表与订单详情」，把福卡流水
-- 说成订单详情的一部分会让权限描述骗人。coupon:user-coupon:read 是先例。
--
-- 不发菜单：福卡 tab 挂在已有的订单详情页里（一个页面里的一块），侧边栏没有对应的入口。
-- 同 013/014/016 的理由——先挂菜单只会让侧边栏多出一个点进去 404 的入口。

BEGIN;

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  (gen_random_uuid(), 'account:read', '资产账户', '查看福卡账户与流水',
   '查看用户的福卡余额与收支流水，含订单赠送与冲正记录')
ON CONFLICT (code) DO NOTHING;

INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code = 'account:read'
ON CONFLICT (role_id, permission_id) DO NOTHING;

COMMIT;
