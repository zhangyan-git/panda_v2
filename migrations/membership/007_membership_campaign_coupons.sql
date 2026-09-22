-- membership/007：店铺码活动的赠券（活动上是配置，领取记录上是快照）
--
-- 006 建表时逐字写着「本服务不发票，所以活动表上也就没有券的列」。**那句话现在不成立了**：
-- coupon-service 从这一刀起消费本服务的 `membership.campaign.claimed`，一次扫码领会员会同时
-- 送出 N 天会员（本服务自己发）与 M 张券（券服务发）。
--
-- 两边的账**仍然各自独立**：这里只记「这次承诺发多少」，实际发了几张去券库按
-- `user_coupons.campaign_claim_id` 数——本库不重复记一份券的账，与
-- `001_membership_core.sql` 里那段「券的余额全归 coupon-service」是同一条规矩。所以这两列
-- 是**承诺**，不是余额：它们不会随发券结果变，也没有「已发几张」这种列。
--
-- 券模板是**跨库值引用**（coupon-service 的 coupon_templates），UUID、无外键——与 006 的
-- store_id、001 的 membership_plans.member_price_coupon_template_id 是同一写法。本服务没有
-- 任何办法校验那个 id 存不存在：发券时券服务找不到模板会记一条日志并 ack（见那边的
-- service/consumer.go），不会回头改这里。
--
-- 两列**要么都有、要么都没有**：只配模板不配张数等于「该发券」静默变成「什么都不发」，而
-- 用户在小程序上看到的是活动写着送券。这与 001 那条 member_price_mode='coupon' 的 CHECK 是
-- 同一条理由——配置里的半截组合不该能存下去。

ALTER TABLE membership_campaigns
    ADD COLUMN coupon_template_id UUID,
    ADD COLUMN coupon_count INTEGER CHECK (coupon_count IS NULL OR coupon_count > 0);

ALTER TABLE membership_campaigns ADD CONSTRAINT membership_campaigns_coupon_pair
    CHECK ((coupon_template_id IS NULL) = (coupon_count IS NULL));

COMMENT ON COLUMN membership_campaigns.coupon_template_id IS
    '这场活动每领一次送几张什么券；跨库值引用（券服务的 coupon_templates），无外键。为空 = 只送会员天数';
COMMENT ON COLUMN membership_campaigns.coupon_count IS
    '每次领取送的券张数。与 coupon_template_id 同生共死（见上面那条 CHECK）；实际发了几张去券库按 campaign_claim_id 数';

-- 领取记录上的两列是**发放那一刻的快照**：活动后来改了券的配置，已经领过的那次不受影响。
-- 与同一个表上的 store_id / gift_days 是同一条规矩。
--
-- 券与领取记录的对应关系**不止这一处**：券库里 user_coupons.campaign_claim_id 也指着这条
-- 记录（老系统同一列、同一用途）。这边存的是「承诺给什么」，那边存的是「实际给了什么」，
-- 两条记录各归各的库，谁都不改对方。
ALTER TABLE membership_campaign_claims
    ADD COLUMN coupon_template_id UUID,
    ADD COLUMN coupon_count INTEGER CHECK (coupon_count IS NULL OR coupon_count > 0);

ALTER TABLE membership_campaign_claims ADD CONSTRAINT membership_campaign_claims_coupon_pair
    CHECK ((coupon_template_id IS NULL) = (coupon_count IS NULL));

COMMENT ON COLUMN membership_campaign_claims.coupon_template_id IS
    '这次领取承诺发的券模板（活动上那一份的快照）；为空 = 只送了会员天数';
COMMENT ON COLUMN membership_campaign_claims.coupon_count IS
    '这次领取承诺发的券张数；实际发了几张在券库的 user_coupons 里按 campaign_claim_id 数';
