-- order/003：收窄出资词表——去掉 fortune_card
--
-- 福卡不是出资来源。它是下单赠送的抽奖凭证：发放与余额归账户服务，消耗只有抽奖一条路
-- （lottery-service），两个服务都还没建。规划里从来没有把它列成出资渠道——§3.1 只说它
-- 「订单完成发放、用于参与抽奖」，§5.6 把它的余额归在 Account 下。是 001 的这条 CHECK 与
-- 002 的列注释凭空把它写成了一种出资方。
--
-- 收窄是安全的：从来没有过写入 fortune_card 的路径（payment 侧只有渠道出资一条线，订单侧
-- 的出资行只在支付成功后落），所以不存在需要清理的存量行。真要有，下面的 ALTER 会直接
-- 失败——这是有意的：那种行需要一个只退不进的处置方案，不该被这条迁移悄悄漏过去。
--
-- 词表在 payment 库有一份逐字对应的（payment_methods / payments / payment_fundings /
-- payment_refund_fundings / payment_transactions 五条 CHECK），由 payment/004 同时收窄。

ALTER TABLE order_payment_lines
    DROP CONSTRAINT order_payment_lines_line_type_check,
    ADD CONSTRAINT order_payment_lines_line_type_check CHECK (line_type IN (
        'wechat', 'unionpay', 'coffee_bean', 'wallet', 'other'
    ));

-- 002 那条列注释里也列着 fortune_card，这里重发一遍，让已经建好的库跟上（新库会先跑 002
-- 拿到旧文本，再由这条覆盖）。
COMMENT ON COLUMN order_payment_lines.line_type IS '出资来源：wechat=微信，unionpay=银联，coffee_bean=咖啡豆，wallet=其他钱包，other=其他；福卡不在其中，它是抽奖凭证而不是出资渠道';
