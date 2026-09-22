-- order/006：售后单补一列「退款失败原因」
--
-- 001 给 order_after_sales 落了 failure_code（退款失败码），但**只有码，没有文案**。而
-- 退款失败这条路上最该给客服看的就是那句文案：`ACQ.TRADE_NOT_EXIST` 谁也读不懂，渠道
-- 回的原文（「原交易不存在」）才说明白发生了什么、要不要找用户重来一次。
--
-- 这句文案本来就在链路上，只是走到订单域被丢掉了：
--
--   UMS refundResponse.failureMessage
--     → payment_refunds.failure_message（payment/001:220，那边一直在存）
--     → 事件体 payment.refund.failed 的 failureMessage 字段
--     → order-service 收口时只写了 failure_code，文案进了审计快照，表上没有它的位置
--
-- 落到**这一张表**而不是让客服去支付域查：后台看退款失败就是看这一行（
-- routes.RegisterAdmin 的售后列表），而 order_after_sales 是这条链在订单侧的落点——
-- 与 refund_no、refunded_at 同一个归属。payment_refunds 那边留着自己那份，两边各自完整，
-- 不做跨库读取。
--
-- 与 failure_code 成对，而且**每次写结论时两个一起写**（AdvanceRefund 的 UPDATE 都取事件里
-- 的值）：成功那条路上事件带的正是两个空串，所以两列永远描述同一个结论，不会出现「已退成
-- 却还挂着上次的失败原因」这种半截状态。一行只落得下一个结论——退款失败之后要再退是**重新
-- 申请**（新售后单、新退款单号），不是在原行上重来。
--
-- 不需要回填：这一列是这一次退款链新接出来的，之前 order_after_sales 里没有一个
-- failed 状态的行（状态机里 failed 的出边只有这条路进来，见 service/state.go）。

ALTER TABLE order_after_sales ADD COLUMN failure_message TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN order_after_sales.failure_message IS '退款失败原因：渠道返回的文案，与 failure_code 成对（成功时两者都是空串）';
