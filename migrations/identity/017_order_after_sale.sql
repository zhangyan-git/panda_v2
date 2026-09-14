-- identity/017: 售后审核权限
-- 可重复执行；仅为 super_admin 绑定。
--
-- 一个码：order:after-sale:audit，通过或驳回退款申请。它管的是**用户的资产**——审核通过
-- 意味着同意把这笔钱退回去，比 order:read（看）和 order:manage（取消待支付单，钱还没进来）
-- 都重，所以单独一个码，而不是并进 order:manage：016 里那个码的描述已经写死成「取消待支付
-- 订单」，拿来当审核退款会让权限描述骗人。coupon 那边 coupon:template:audit 是先例。
--
-- 审核会写平台审计（进 admin_operation_logs）：能回答「是谁在什么时候同意了这笔退款、
-- 有没有确认福卡没被抽奖用掉」。
--
-- 不发菜单：后台的售后页面还没写（同 013/014/016 的理由，先挂菜单只会让侧边栏多出一个
-- 点进去 404 的入口）。等页面落地时照 015 的写法补。
--
-- 不改 016，也不为「查看售后」加码：售后列表用的是 order:read，改权限描述不该靠新迁移，
-- 而列表在 016 的描述「查看订单列表与订单详情」的范围内。

BEGIN;

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  (gen_random_uuid(), 'order:after-sale:audit', '订单管理', '审核退款申请',
   '通过或驳回用户提交的退款申请，操作会写入平台审计')
ON CONFLICT (code) DO NOTHING;

INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code = 'order:after-sale:audit'
ON CONFLICT (role_id, permission_id) DO NOTHING;

COMMIT;
