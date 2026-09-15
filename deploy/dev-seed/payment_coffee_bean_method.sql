-- 咖啡豆（账户出资）的种子数据：一行 payment_methods，**没有** payment_channels。
--
-- 与 payment_manual_channel.sql 一样，它刻意不是一条迁移：支付方式是运营数据，不是表结构。
-- 一份迁移会让每个环境都长出这一行，而**这一行就是「用户可以拿豆付款」的开关**——
-- 只做后台人工调整、豆还永不过期的今天，把它带进生产等于给了一个没有充值入口的支付方式。
--
-- 这一份与手工渠道那份有一处结构性不同，也是它值得单独写一个文件的原因：
-- **channel_id 是 NULL**。payment_methods.channel_id 为空只对 action='account' 合法
-- （见 001 的列注释），service 侧的 resolveMethod 也正是按这个 action 跳过渠道检查的
-- （payment-service/internal/service/create.go）。所以下面那条回显用的是 LEFT JOIN——
-- 内连接会让这一行在回显里消失，而「跑完了但没插进去」和「插进去但渠道是空的」是两件事。
--
-- 用法（dev 栈）：
--     docker exec -i dev-postgres-1 psql -U panda -d panda_payment < deploy/dev-seed/payment_coffee_bean_method.sql
--
-- 前置：
--   1. payment 迁移已应用（panda-migrate -database "$PAYMENT_DATABASE_URL" apply payment）。
--   2. **account-service 在跑、且它的咖啡豆表已建**（account 迁移 005/006）。否则这一行插进去
--      之后，用户选它支付会得到一次「账户域不可达」——支付单停在 created，等超时关单收走。
--   3. payment-service 配了 ACCOUNT_GRPC_ADDR（config 里必填，缺了它服务拒绝启动）。
--
-- 生产环境要接豆支付，得有人手工配这一行——payment-service 没有支付方式的
-- 后台管理页（唯一的写入者就是这份种子）。这是本切片的已知缺口，不是遗漏。

BEGIN;

-- 支付方式。
--
-- action='account'：客户端只认它，见到它就知道不需要调起任何东西——扣豆成功即支付成功，
-- 返回的 status 直接是 succeeded（见 payment-service 的 createAccountPayment）。
--
-- funding_type='coffee_bean'：它会一路带到 payment_fundings.line_type、
-- order_payment_lines.line_type 与 orders.payment_method 上。词表与 order 库逐字一致。
--
-- sort_order 排在手工程序渠道（9999）之后：联调时列表里默认选中的不该是「会真的扣豆」的那一条。
INSERT INTO payment_methods (code, name, description, channel_id, action, params, funding_type, status, sort_order)
VALUES (
    'coffee_bean',
    '咖啡豆',
    '用账户里的咖啡豆支付。豆由后台人工调整，永不过期。',
    NULL,
    'account',
    '{}'::jsonb,
    'coffee_bean',
    'enabled',
    9998
)
ON CONFLICT (code) DO UPDATE SET
    name         = EXCLUDED.name,
    description  = EXCLUDED.description,
    channel_id   = EXCLUDED.channel_id,
    action       = EXCLUDED.action,
    params       = EXCLUDED.params,
    funding_type = EXCLUDED.funding_type,
    status       = EXCLUDED.status,
    sort_order   = EXCLUDED.sort_order,
    updated_at   = NOW();

COMMIT;

-- 回显这一行。channel_code 一列**应该是空的**——它非空就说明这一行被改成了渠道支付方式，
-- 那种情况下 action 也该跟着改，而两者不一致时 service 会在查渠道时失败。
SELECT m.id AS payment_method_id,
       m.code AS method_code,
       m.action,
       m.funding_type,
       m.status,
       c.code AS channel_code,
       c.provider,
       c.mode
FROM payment_methods m
LEFT JOIN payment_channels c ON c.id = m.channel_id
WHERE m.code = 'coffee_bean';
