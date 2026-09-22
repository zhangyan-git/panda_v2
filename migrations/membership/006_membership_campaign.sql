-- membership/006：店铺码会员活动（活动配置 + 领取记录）
--
-- 「扫码赠 VIP」——顾客扫店里的活动码，领 N 天会员，归属门店记成这家店。老系统
-- （panda_serve 的 store_membership_campaign）有两个 collection，这里照抄形状。
--
-- # 四处 V2 化改造，都不是随手改的
--
-- **一、`vip_level_id` → `plan_id`。** 老系统有一层「VIP 等级」，V2 没有，等价物是套餐。
-- 老系统那个等级门槛（`IsSubscription==true` 且签约模板非空）在这里译作
-- `membership_plans.auto_renew = true`——**注意它今天只是一个形状**：V2 的领取不签约
-- （见下面的「三」），所以这条门槛的实际作用只剩下「只能拿连续包月的套餐做活动」。
-- 另外，coupon 模式的套餐（包月就是）领到的会员，会员价要靠会员价券。**这一句改过**：建表
-- 时这里写着「本服务不发票，所以运营配活动时应当挑 auto 模式的套餐」——007 把活动的赠券接上
-- 了（本服务发 `membership.campaign.claimed`，coupon-service 消费它发券），所以现在活动上
-- 可以配券，挑 coupon 模式的套餐也不再是死路：会员价那一半由券服务按同一场活动发。
-- 券的两列在 007 里，不在这一份里。
--
-- **二、`store_id` 是跨库值引用，无外键。** stores 在商户库，建不了外键——与
-- `membership_plans.member_price_coupon_template_id` 同一写法（那是本库现成的先例）。
-- campaigns 与 claims 两张表上的 store_id 都是这个性质。
--
-- **三、不建老系统的签约/预锁一族列。** 老系统那张 claims 上有 `subscription_id`、
-- `contract_code`、`coupon_stock_reserved`、`grant_lease_token`、`signing*`、`sign_*`
-- 十来个字段——它们全是为「先预扣券库存、再跳微信签约、回来确认、最后发放」那条两阶段
-- 链路服务的，两把分布式锁（5 分钟租约 + 租约围栏）也是。V2 这条链路**不签约**：
-- 「送 N 天会员」就是一次会员发放，「归属门店」是 memberships.store_id 的一个取值。
-- 一次事务做得完的事不需要租约，抄过来只会留下一堆永远为空的列。
--
-- **四、不建 `per_user_limit`。** 老系统那个字段的硬校验只接受 1，真正生效的是
-- `(campaign_id, user_id)` 唯一索引。抄过来会把「可配」这个假选项也一起抄来——
-- 一个能填 3 却按 1 执行的输入框，比没有这个输入框糟得多。这里直接只用唯一索引表达。
--
-- 同样不建的还有 `merchant_id`：老系统有，但 V2 归属门店能推出商户（商户域的
-- StoreClient 就在手上），存一份没有读取方的冗余副本不值当。
--
-- **领取记录上没有 `status`。** 老系统那五个状态（pending_sign / signed / granting /
-- completed / failed）是签约链路的产物；V2 的领取是一次幂等发放，**行的存在就是「发放
-- 完成」**。失败的那次整体回滚、不留行——留一行 failed 会被唯一索引挡住，那个人就再也
-- 领不了了，而失败的真正原因是可重试的。

CREATE TABLE membership_campaigns (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    -- 小程序码里带的 scene 参数，扫码进来靠它找回这场活动（老系统同一个口径）。
    -- 格式与老系统逐字相同：`smc_` 前缀 + URL 安全字符。
    scene TEXT NOT NULL CHECK (scene ~ '^smc_[A-Za-z0-9_-]+$'),
    store_id UUID NOT NULL,
    -- 「送的是哪个套餐的会员」。老系统的 vip_level_id；建表时要求该套餐 auto_renew = true，
    -- 那个校验在服务层（跨表 CHECK 写不了）。
    plan_id UUID NOT NULL REFERENCES membership_plans(id) ON DELETE RESTRICT,
    -- 送多少天。与套餐的 period/period_count 无关：这是一次赠送，按天数算。
    gift_days INTEGER NOT NULL CHECK (gift_days > 0),
    start_at TIMESTAMPTZ NOT NULL,
    end_at TIMESTAMPTZ NOT NULL,
    -- 老系统三个取值逐字照抄：draft=刚建好还不能扫，enabled=可在有效期内扫，
    -- disabled=下架（已经领过的人不受影响）。
    status TEXT NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'enabled', 'disabled')),
    -- 小程序码。**今天恒为空**：生成它要走微信的 wxacode.getUnlimited，而
    -- appid/secret 与 access_token 缓存都在 user-service（本服务没有任何微信配置）。
    -- 两列仍然建着——它们描述的是活动的事实，没有码的时候空着，语义清楚。
    qr_code_url TEXT NOT NULL DEFAULT '',
    qr_code_generated_at TIMESTAMPTZ,
    -- 操作人：身份库的 admin_users.id，仅作值引用（跨库，无外键）。
    created_by UUID,
    updated_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (end_at > start_at)
);

-- 一个 scene 只能挂一场活动。扫码进来的请求手里只有 scene，撞了就没有任何判据能说是哪一场。
CREATE UNIQUE INDEX membership_campaigns_scene_unique
    ON membership_campaigns (scene);

COMMENT ON TABLE membership_campaigns IS
    '店铺码会员活动：扫码送 N 天会员并把归属门店记成活动门店';
COMMENT ON COLUMN membership_campaigns.store_id IS
    '活动门店 ID；跨库值引用（商户库），无外键。领取时作为会员的归属门店';
COMMENT ON COLUMN membership_campaigns.plan_id IS
    '送的是哪个套餐的会员；本库外键。建活动时要求该套餐 auto_renew = true（服务层校验）';

-- 领取记录。一次领取一行，**行的存在即发放完成**（见头部「三」）。
CREATE TABLE membership_campaign_claims (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id UUID NOT NULL REFERENCES membership_campaigns(id) ON DELETE RESTRICT,
    user_id UUID NOT NULL,
    -- 快照：活动后来改了门店或天数，已经领过的那次不受影响。
    store_id UUID NOT NULL,
    plan_id UUID NOT NULL,
    gift_days INTEGER NOT NULL,
    -- 这次领取派生的那条会员。老系统把这个关系记在会员行上（campaign_claim_id），
    -- 这里反过来记——会员表今天没有那一列，而这边的方向查起来更直接
    -- （「这条领取给了什么」是后台领取记录页要回答的问题）。
    membership_id UUID NOT NULL REFERENCES memberships(id) ON DELETE RESTRICT,
    -- 发放那一刻算出来的到期时刻。会员行上也有 expire_at，但那条会随后台调整、
    -- 续费而变；这里记的是「这次领取当时送出到哪天」。
    membership_expire_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 一个人一场活动只能领一次。老系统那五个状态的链路里这是发放的幂等凭据，V2 里它同时
-- 是「重复扫码」的答案：撞上就回原来那条，不再发第二次。
CREATE UNIQUE INDEX membership_campaign_claims_user_unique
    ON membership_campaign_claims (campaign_id, user_id);

-- 后台领取记录页按活动查，一页一查。没有它这条查询会随活动数增长扫全表。
CREATE INDEX membership_campaign_claims_campaign_idx
    ON membership_campaign_claims (campaign_id, created_at DESC);

COMMENT ON TABLE membership_campaign_claims IS
    '店铺码活动的领取记录：一行即一次发放完成（失败的那次整体回滚、不留行）';
COMMENT ON COLUMN membership_campaign_claims.store_id IS
    '领取时的活动门店快照，也就是这次送的会员的归属门店；跨库值引用（商户库），无外键';
