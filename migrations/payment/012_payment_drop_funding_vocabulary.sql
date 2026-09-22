-- payment/012：出资渠道词表退场，payments.funding_type 删列
--
-- 001 与 004 给四列立了一套「出资渠道」词表（wechat / unionpay / coffee_bean / wallet /
-- other），并给 payments 单独加了一列 funding_type 记同一件事。这一条把那套词表整份退掉：
-- 五处（payments.funding_type 一列连同 payment_fundings / payment_transactions /
-- payment_refund_fundings 三列的 CHECK）只剩一种值——**支付方式的 code**，也就是 catalog 里的
-- ums_h5_alipay / ums_h5_wechat / ums_miniapp_wechat / coffee_bean …。出资行、记账流水、退款
-- 出资行的 line_type 与 payments.payment_method 从此是同一个值。
--
-- # 订正 001 文件头那两条「必须对齐的约定」
--
--   第 1 条写着「出资词表与 order 库的 order_payment_lines **逐字一致**：line_type ∈ wechat |
--   unionpay | coffee_bean | wallet | other」。**这条约定作废**（001 已应用，按「已应用的迁移
--   不改」的惯例不动它，以本文件为准）。词表本身不存在了，两边仍然对齐，但对齐的是 catalog 的
--   code。order-service 消费 payment.succeeded / payment.failed 时拿到的 paymentMethod 就是
--   这个 code，直接落它那份出资分摊。
--
--   第 2 条（payment_methods.id 被 coffee_machine 按值引用）已被 009 作废——那张表连同
--   payment_channels 一起删了。今天没有代码引用任何一个 code 的**主键**，引用的是 code 本身。
--
-- 顺带作废 004 文件头那句「两个库的词表必须逐字一致，所以这两条迁移要一起上」：词表没了，
-- 但「两边一起上」仍然成立（order/008 是本条在订单库的另一半），理由换成新的——**跨服务的
-- 值域契约**：订单侧拿到的 paymentMethod 必须与这里存的是同一套 code。
--
-- # 为什么退掉
--
-- 加一种支付方式本来只该在 catalog 里加一条，但两套词表并存时要同时在出资渠道里给它找一档——
-- 找不到就得改 DDL。支付宝就卡在这里：它在词表里没有档位，catalog 自己写着「这是词表的缺口」，
-- 于是只能落 other，后台把一笔支付宝单显示成「其他」。
--
-- # 换成什么约束
--
-- 三条 line_type 的 CHECK 换成 `CHECK (line_type <> '')`。比原来弱得多，跨库那层一致性保护
-- 换不回来：payment 库看得见自己的目录，但目录是**代码里的常量**，SQL 写不出它认识的 code
-- 全集，写死在 CHECK 里等于把「加一条 catalog 记录」重新变回一次迁移——正是这次要拆掉的东西。
-- 能守的只剩非空。
--
-- # 回填
--
-- 三条 line_type 在本库内回填，判据统一是 `line_type = payments.funding_type`——**只有老代码
-- 写下的行会命中**：咖啡豆单的值本来就等于 code（coffee_bean 在两套词表里同字面量），
-- 已经写成 code 的行（这一轮新代码写的）也命中不了。所以回填是幂等的，重跑不会改坏任何东西。
--
-- payments.funding_type 那一列**不回填、直接删**：它的内容逐行等同于同一行的 payment_method
-- 或它的老写法，而「这一单从哪条出货」这个问题的答案已经是 provider 列（009 保留的那一列）。
-- 留着它只会让下一个人以为还有第二种口径可读。
--
-- 订单库那两列（orders.payment_method、order_payment_lines.line_type）**不在本文件里回填**：
-- 跨库没有 dblink。它们由一次性的历史订正脚本按支付单号精确改，见 order/008 的头部。
--
-- 顺序：**apply order/008（先把订单库的 CHECK 松开）→ 回填脚本 → apply 本文件**。
-- order/008 必须排在最前——回填写进去的是 code，旧 CHECK 不认，判据再对也插不进去。

-- 0. 先松开三条 CHECK，再回填。
--
--    顺序不能反：新值（code）不在旧词表里，回填的第一条 UPDATE 就会被旧 CHECK 顶回来，
--    整条迁移回滚。松 CHECK 这一步本身不动数据，先做没有任何代价。
ALTER TABLE payment_fundings
    DROP CONSTRAINT payment_fundings_line_type_check,
    ADD CONSTRAINT payment_fundings_line_type_check CHECK (line_type <> '');

ALTER TABLE payment_transactions
    DROP CONSTRAINT payment_transactions_line_type_check,
    ADD CONSTRAINT payment_transactions_line_type_check CHECK (line_type <> '');

ALTER TABLE payment_refund_fundings
    DROP CONSTRAINT payment_refund_fundings_line_type_check,
    ADD CONSTRAINT payment_refund_fundings_line_type_check CHECK (line_type <> '');

-- 1. 同库回填：只改那些还写着老值的行。
UPDATE payment_fundings f
    SET line_type = p.payment_method
    FROM payments p
    WHERE f.payment_id = p.id AND f.line_type = p.funding_type;

-- 流水那张表是**只追加**的：001 的 payment_transactions_append_only 触发器无条件拒绝 UPDATE
-- （与 stock_movements 那条同一个形状）。回填正好是一次 UPDATE，不摘掉它这一句必然失败——
-- **在一个有历史的库上整个迁移都会回滚**，而它跑得通的样子恰恰最危险：只有空库不报错，
-- 也就是说这条迁移会等到第一次真正派上用场的那天在生产上炸掉。写这条时真踩了一次。
--
-- 摘下来的是「禁止改流水」这一条保护，位置就在这三行之间。这里说清它为什么可以摘：改的是
-- line_type 一个列，而且改的是**同一件事的另一种写法**——那一列当天写的值（other / wechat）
-- 和今天的值（ums_h5_alipay / ums_h5_wechat）指的是同一笔出资，只是词表换了一套；金额、方向、
-- 单号、时间戳一个字都没动。这不是在改「发生过什么」，是在改「当时把那件事叫什么」。009 里有
-- 一模一样的先例（channel_id → provider 的同一次换名）。
ALTER TABLE payment_transactions DISABLE TRIGGER payment_transactions_append_only;
UPDATE payment_transactions t
    SET line_type = p.payment_method
    FROM payments p
    WHERE t.payment_no = p.payment_no AND t.line_type = p.funding_type;
ALTER TABLE payment_transactions ENABLE TRIGGER payment_transactions_append_only;

-- 退款出资行隔着一张退款单才认得到支付单：payment_refunds.payment_no 是那个值引用。
UPDATE payment_refund_fundings rf
    SET line_type = p.payment_method
    FROM payment_refunds r
    JOIN payments p ON p.payment_no = r.payment_no
    WHERE rf.refund_id = r.id AND rf.line_type = p.funding_type;

-- 2. payments.funding_type 整列退场。DROP COLUMN 自动带走它自己的 CHECK
--    （payments_funding_type_check），不需要单独 DROP CONSTRAINT。
ALTER TABLE payments DROP COLUMN funding_type;

-- 004 写的那条注释随列一起没了，这里把三列 line_type 的注释重发一遍（002/003 写过的旧文本
-- 会被这条覆盖）。
COMMENT ON COLUMN payment_fundings.line_type IS '这笔出资的支付方式 code（catalog 里的 ums_h5_alipay / coffee_bean 等），与 payments.payment_method 同一个值；出资渠道那套词表已退场（见 012）';

COMMENT ON COLUMN payment_transactions.line_type IS '这条流水对应的支付方式 code，与 payments.payment_method 同一个值；出资渠道那套词表已退场（见 012）';

COMMENT ON COLUMN payment_refund_fundings.line_type IS '被冲正的那笔出资的支付方式 code，与 payments.payment_method 同一个值；出资渠道那套词表已退场（见 012）';
