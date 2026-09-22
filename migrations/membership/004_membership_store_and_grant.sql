-- membership/004：后台直接开通会员所依赖的两件事
--
-- **一、memberships 加归属门店列。**
--
-- 001 上那段注释写着「V2 的会员是全平台的，不分门店……所以这里没有 store_id」。权益那一半
-- 仍然成立：会员价在哪家店用都一样，归属门店不参与任何权益判定、不影响核销、不影响下单，
-- 更不参与分账——它只是一条留痕，回答「这个人是谁拉来的」。
--
-- 但留痕本身是老系统一直在记的（users.member_store_id），而「不做店铺码活动」这句话在
-- V2 也不再成立（见 006）。所以这一列按老系统的口径补回来，并沿用同一条固化规则：
--
--   第一次成为会员那一刻固化；之后续费、升级**不覆盖**；只有会员**已经过期**之后重新
--   开通，归属门店才可以变更——上一段会员关系结束了，这一次是新的拉新。
--
-- 判据落在服务层，且挂在一个已经存在的分叉上（renewMembership 里决定 base 取 occurredAt
-- 还是 current.ExpireAt 的那个判断），不在这里用 CASE 再写一遍。
--
-- 可空、无外键、无索引：stores 在商户库，跨库建不了外键（与全仓一致，见本文件末尾那段
-- 注释被 migrations_test.go 钉着）；今天也没有「按门店筛会员」的查询。
ALTER TABLE memberships ADD COLUMN store_id UUID;

COMMENT ON COLUMN memberships.store_id IS
    '归属门店 ID：第一次成为会员时固化，续费与升级不覆盖，会员过期后重新开通才可变；仅作跨库值引用（商户库），不参与权益判定与分账';

-- **二、membership_changes.request_id 加一条部分唯一索引。**
--
-- 后台开通会员要幂等：操作员点一次提交、网络抖动后客户端重试，不能开出两条会员，也不能
-- 让重试那一次看到「这个人已经是会员了」——那在这种场景下是假警报，他刚亲手建的那条。
--
-- request_id 列 001 就有了（注释写「操作留痕串，与后台操作审计对得上」），但今天没有任何
-- 唯一约束，四个后台动作也都不填它。加索引之前要先确认它不会误伤：这四个动作写的是空串，
-- 部分索引的 WHERE 把它们排除在外，行为一个字不改。
--
-- 与 003 那条同一条理由：幂等是**不变式**，钉在库上而不是只靠服务层先查一遍——漏掉的那
-- 一次没有任何东西会报错。
CREATE UNIQUE INDEX membership_changes_request_unique
    ON membership_changes (user_id, change_type, request_id)
    WHERE request_id <> '';

COMMENT ON INDEX membership_changes_request_unique IS
    '同一个用户在同一个变更类型下，一个 request_id 只对应一条流水；后台开通会员的重试靠它兜底';
