-- payment/004：收窄出资词表——去掉 fortune_card
--
-- 与 order/003 同一件事：福卡不是出资来源，是下单赠送的抽奖凭证（发放与余额归账户服务，
-- 消耗只有抽奖一条路，归 lottery-service）。规划里没有把福卡列成出资渠道（§3.1 只说它
-- 「订单完成发放、用于参与抽奖」，§5.6 归在 Account 的账户余额里），是 001 里的五条 CHECK
-- 与 003 的两条列注释凭空写上了它。
--
-- 两个库的词表必须逐字一致（001 文件头写明 funding_type 与订单库的
-- order_payment_lines.line_type 同词表），所以这两条迁移要一起上，不能只上一边。
--
-- 收窄是安全的：从来没有过写入 fortune_card 的路径。真要有存量行，ALTER 会直接失败——
-- 有意为之，那种行不该被这条迁移悄悄漏过去。

ALTER TABLE payment_methods
    DROP CONSTRAINT payment_methods_funding_type_check,
    ADD CONSTRAINT payment_methods_funding_type_check CHECK (funding_type IN (
        'wechat', 'unionpay', 'coffee_bean', 'wallet', 'other'
    ));

ALTER TABLE payments
    DROP CONSTRAINT payments_funding_type_check,
    ADD CONSTRAINT payments_funding_type_check CHECK (funding_type IN (
        'wechat', 'unionpay', 'coffee_bean', 'wallet', 'other'
    ));

ALTER TABLE payment_fundings
    DROP CONSTRAINT payment_fundings_line_type_check,
    ADD CONSTRAINT payment_fundings_line_type_check CHECK (line_type IN (
        'wechat', 'unionpay', 'coffee_bean', 'wallet', 'other'
    ));

ALTER TABLE payment_refund_fundings
    DROP CONSTRAINT payment_refund_fundings_line_type_check,
    ADD CONSTRAINT payment_refund_fundings_line_type_check CHECK (line_type IN (
        'wechat', 'unionpay', 'coffee_bean', 'wallet', 'other'
    ));

ALTER TABLE payment_transactions
    DROP CONSTRAINT payment_transactions_line_type_check,
    ADD CONSTRAINT payment_transactions_line_type_check CHECK (line_type IN (
        'wechat', 'unionpay', 'coffee_bean', 'wallet', 'other'
    ));

-- 003 的三条列注释里同样列了词表、把福卡当成一种账户出资，重发一遍让已经建好的库跟上
-- （新库会先跑 003 拿到旧文本，再由这三条覆盖）。
COMMENT ON COLUMN payment_methods.funding_type IS '对应的出资类型，词表与 order_payment_lines.line_type 一致：wechat=微信，unionpay=银联，coffee_bean=咖啡豆，wallet=其他钱包，other=其他；福卡不在其中，它是抽奖凭证而不是出资渠道';
COMMENT ON COLUMN payment_methods.channel_id IS '对应的渠道配置；咖啡豆这类账户出资方式为空（由账户服务扣减）';
COMMENT ON COLUMN payments.channel_id IS '走哪套渠道配置；账户出资（纯咖啡豆）为空';
