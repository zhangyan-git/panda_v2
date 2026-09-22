-- membership/005：连续包月订阅的「从哪来」三列
--
-- 老后台的订阅列表有两列表头——「签约场景」与「首月支付」——而老库上**这两列一个字段都
-- 没有**：它们都是查询时算出来的。
--
--   签约场景 = coffee_order_id 非零 → 咖啡订单；否则 campaign_claim_id 非零 → 店铺码活动；
--              否则 → 会员中心
--   首月支付 = 拿 out_trade_no 回 orders 查（且只对「会员中心」那一种场景填）
--
-- V2 照抄这个口径：**派生值不落库**，只把三个来源 id 记下来，场景在服务层现算。
-- 给派生值建列的话，两个字段迟早会有一个和 id 对不上，而那时候没有任何判据能说哪个是对
-- 的。同理，那三个 id 也**不建外键**：orders 在订单库，campaign_claim 在本库 006，跨库建
-- 不了（与全仓一致，见 004 里同一段说明）；coffee_order 与普通订单在 V2 都落 orders。
--
-- 可空、无索引：今天没有「按来源筛订阅」的查询，三列只用来算上面那两个表头。
--
-- ⚠️ **今天没有任何代码写这三列**：能写它们的只有小程序端的签约（用户从店铺码活动页进来、
-- 或从一杯咖啡的订单页进来签约），而小程序端这一轮不写。所以后台订阅列表上「签约场景」恒为
-- 「会员中心」、首月支付恒为「待同步」——**那是数据还没来，不是算错了**。

ALTER TABLE membership_subscriptions ADD COLUMN order_id UUID;
ALTER TABLE membership_subscriptions ADD COLUMN coffee_order_id UUID;
ALTER TABLE membership_subscriptions ADD COLUMN campaign_claim_id UUID;

COMMENT ON COLUMN membership_subscriptions.order_id IS
    '首月那张订单（会员中心支付并签约）；跨库值引用（订单库），无外键';
COMMENT ON COLUMN membership_subscriptions.coffee_order_id IS
    '咖啡订单仅签约；跨库值引用（订单库），无外键';
COMMENT ON COLUMN membership_subscriptions.campaign_claim_id IS
    '店铺码活动签约对应的那条领取记录（本库 membership_campaign_claims.id），无外键';
