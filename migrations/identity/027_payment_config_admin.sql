-- identity/027: 支付方式与渠道的后台配置权限
-- 可重复执行；仅为 super_admin 绑定，其余角色去角色管理里勾。
--
-- 一枚管理码 payment:manage。这是 026 里那句「等到真做写入接口那天再补」的那一天：
-- payment-service 现在有六个写接口了（支付方式与渠道的新增 / 修改 / 启停，见
-- routes/admin.go），所以这枚码有对应的行为了，不再是权限页上的摆设。
--
-- # 它盖住什么、不盖住什么
--
-- 只盖**两张配置表**：payment_methods 与 payment_channels。改的是「用户在小程序上能点哪几个
-- 按钮」与「那几个按钮背后连着谁的通道」。
--
-- **支付单仍然没有写接口**，所以也不在这枚码的射程里：payments 是钱的既成事实，一笔
-- succeeded 的单子后面挂着出资行、记账流水、渠道调用、回调通知四张只增表。关单、重放回调、
-- 发起退款都要先有退款与对账的语义，而它们没做（见 docs/architecture.md 支付那一段）。
-- 换句话说：**拿到这枚码不等于能动钱**，只能动「将来怎么收钱」。
--
-- # 为什么它不并进 read
--
-- read 是给客服的：查一笔支付单走在哪个渠道、失败原因是什么、回调报文长什么样。manage 改的是
-- 配置，一次改错能让**所有**新订单走不下去（比如把在用的方式停掉、把 params 写成非法 JSON 之外
-- 的东西）。两者的破坏半径差一档，与 023 的 manage / receipt 同一条思路。
--
-- # 写操作有审计
--
-- 六个写接口都在同一个事务里往 message_outbox 记一条审计（含**改动前的快照**），
-- 经 RabbitMQ 落进身份库的 admin_operation_logs。改坏了能照着 before 回滚。
--
-- 菜单不动：026 已经把「支付管理 > 支付单 / 支付方式与渠道」建好了，这一刀只是把同一页上
-- 那几个按钮从「点了没用」变成「点了有用」。

BEGIN;

INSERT INTO admin_permissions (id, code, perm_group, name, description)
VALUES
  (gen_random_uuid(), 'payment:manage', '支付管理', '管理支付方式与渠道',
   '新增与修改支付方式（名称、说明、交互方式、出资类型、所属渠道、启动参数、排序）与支付渠道（名称、提供方、模式、密钥引用、非密钥配置、备注），以及两者的启用与停用；不包含删除，也不包含对已发生的支付单的任何操作')
ON CONFLICT (code) DO NOTHING;

-- 超管在中间件里直接放行，这里补绑定是为了前端 access 与权限回显能拿到码。
INSERT INTO admin_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM admin_roles r
CROSS JOIN admin_permissions p
WHERE r.code = 'super_admin'
  AND p.code IN ('payment:read', 'payment:manage')
ON CONFLICT (role_id, permission_id) DO NOTHING;

COMMIT;
