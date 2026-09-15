-- 手工渠道（provider=manual）的种子数据：一行 payment_channels + 一行 payment_methods。
--
-- 它**刻意不是一条迁移**。渠道与支付方式是运营数据，不是表结构：生产环境接的是真实渠道
-- （微信、银联、丰选万联、优联……），本来就该由运营在后台插一行、或者在发布时按环境导一份
-- 数据，而不是跟着二进制一起升级。把它写成迁移有两个具体的坏处：
--
--   1. 每个环境都会长出一条名叫「手工渠道」的、能收钱的支付方式。它没有真实收单方，
--      验签密钥还是我们自己定的——出现在生产就是一个可以被自助「付款成功」的入口。
--   2. 迁移不能删（写 Down 也没用：老库里已经有引用了），于是它永久留在表里，
--      而「这条渠道为什么存在」的答案会在几次人员更替之后失传。
--
-- 换成 Go 的 cmd/seed 也不合适：那套（user-service 的 DEV_ACCOUNT_INIT_ENABLED）是给
-- 身份域建初始管理员用的，要进构建产物、要走 Dockerfile；为两行 INSERT 加一个二进制不划算。
--
-- 用法（dev 栈）：
--     docker exec -i dev-postgres-1 psql -U panda -d panda_payment < deploy/dev-seed/payment_manual_channel.sql
--
-- 前置：payment 迁移已应用（panda-migrate -database "$PAYMENT_DATABASE_URL" apply payment）。
--
-- 密钥不在这里。payment_channels.secret_ref 只存**键名**，值从环境变量读：手工渠道的
-- 适配器读 ref 指向的那一个变量（下面写的 PAYMENT_MANUAL_SECRET），读不到就**拒绝验签**
-- ——不是跳过验签。所以这份种子跑完还必须把 PAYMENT_MANUAL_SECRET 配上，否则回调一律被拒。

BEGIN;

-- 渠道。code 是回调路径里的那一段（POST /v1/payments/callback/{channelCode}），
-- 所以它的取值决定了联调脚本打哪个 URL——这里就是 manual_dev。
--
-- provider='manual' 是 Registry 里的注册名；接真实渠道时这一列换成 wechat / unionpay 之类，
-- 代码那边只要那个适配器已经注册。
--
-- mode='sandbox' 而不是 live：适配器**必须看它**（沙箱与生产的密钥、域名都不同），
-- 这一行是本地联调用的，标成 live 会让将来某个只看 mode 的适配器把测试单打到生产渠道上。
--
-- status='enabled'：disabled 的渠道查不出支付方式，联调会以「没有可用支付方式」失败。
INSERT INTO payment_channels (code, name, provider, mode, status, config, secret_ref, remark)
VALUES (
    'manual_dev',
    '手工渠道（仅联调）',
    'manual',
    'sandbox',
    'enabled',
    '{}'::jsonb,
    'PAYMENT_MANUAL_SECRET',
    '本地端到端联调用的模拟渠道，不接入任何真实收单方。生产不要有这一行。'
)
ON CONFLICT (code) DO UPDATE SET
    name       = EXCLUDED.name,
    provider   = EXCLUDED.provider,
    mode       = EXCLUDED.mode,
    status     = EXCLUDED.status,
    config     = EXCLUDED.config,
    secret_ref = EXCLUDED.secret_ref,
    remark     = EXCLUDED.remark,
    updated_at = NOW();

-- 支付方式。_id 由下面那条 SELECT 取，不硬编码 UUID：重复执行时要落在同一行上。
--
-- action='h5' 是**给客户端看的**：客户端只按它决定怎么调起支付（打开 manualPayUrl 那个
-- 网页）。手工渠道的适配器自己不按 action 分派——它只认 provider 那个名字。选 h5 是因为
-- 它返回的确实是一个 URL，语义上最贴近；六种 action 里除 account 之外都走得通。
--
-- funding_type 的词表与 order 库的 order_payment_lines.line_type 逐字一致（见迁移 001 文件头）。
-- 手工渠道不对应任何一种真实出资渠道，所以是 'other'——它会一路带到 orders.payment_method 上，
-- 联调时看到 'other' 就说明这一单确实走的是模拟渠道，而不是某条被误认的真实渠道。
--
-- params.payUrl 是客户端要打开的那个地址，留空也行（手工渠道允许空）：空的时候客户端显示
-- 「请联系店员」，而联调脚本本来就是直接打回调地址，用不到它。要填就给一个你自己的说明页。
INSERT INTO payment_methods (code, name, description, channel_id, action, params, funding_type, status, sort_order)
VALUES (
    'manual_dev',
    '手工渠道（仅联调）',
    '本地端到端验证用，不产生真实收付。',
    (SELECT id FROM payment_channels WHERE code = 'manual_dev'),
    'h5',
    '{"payUrl": ""}'::jsonb,
    'other',
    'enabled',
    9999
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

-- 回显这一行，让「跑完了但没插进去」没法被忽略（比如 panda_payment 连错了库）。
SELECT m.id            AS payment_method_id,
       m.code          AS method_code,
       m.action,
       m.funding_type,
       c.code          AS channel_code,
       c.provider,
       c.mode,
       c.secret_ref
FROM payment_methods m
JOIN payment_channels c ON c.id = m.channel_id
WHERE m.code = 'manual_dev';
