-- payment/013：代扣那一行要存住自己的渠道单号
--
-- 给 payment_agreement_charges 加一列 out_trade_no，并给它一条部分唯一索引。
--
-- # 为什么非有这一列不可
--
-- 扣款的结果是**异步**回来的：微信把「这一笔扣成没扣成」推到一个 notify_url 上，报文里带着
-- 它认这笔单的唯一凭据——out_trade_no（我们的商户单号）。而这张表此前一个能把它对上的列都
-- 没有：agreement_no 是协议的号、biz_period 是期次，两个都不在渠道的报文里。
--
-- 老系统正是死在这里，而且是永久性的：它的续费扣款把 out_trade_no 取成交易记录的 _id，
-- notify_url 又和签约回调共用一条路由，回调按 out_trade_no 去查 user_subscriptions ——
-- 那个单号从来没写进那张表，于是**必然查不到 → 回 FAIL → 微信按重试策略反复推、永不收敛**
-- （subscription_service.go:327/331/334 与 subscription_handler.go:130）。所以这一列不是
-- 「多存一个便于排查的字段」，它是那条回调能落地的**唯一**关联键。
--
-- # 值什么时候生成、能不能重算
--
-- **建行那一次生成，之后每一次重试都复用这一列的值。** 这是本列最要紧的一条约束：重算一个新
-- 单号等于在微信侧开出**第二笔**订单，而两笔单号不同，微信那边无法去重——用户会被扣两次钱。
-- 唯一索引挡的正是这件事的另一种形态：同一时间点两次重试都想写同一个新号。
--
-- 生成规则照 service.paymentNo（PAY + YmdHis + 三位毫秒 + 六位随机），把前缀换成 CHG：26 个
-- 字符，微信对 out_trade_no 的上限是 32。**不带协议号/期次**：微信只要求全局唯一，而把业务
-- 语义编进单号会让「换个期次算法」变成一次历史数据的迁移。
--
-- # 为什么是部分唯一索引
--
-- 存量行（本表今天还没有任何写路径，dev 上应当是空的）与将来的 cancelled/skipped 行都不该
-- 占号：空串不是单号。谓词 `out_trade_no <> ''` 与 002 给 payment_no 建的那条同一个形状，理由
-- 也同一条——「没有单号」是一个合法状态，不该被唯一性约束当成一个重复的值。
--
-- # 上生产的注意
--
-- 与 006 / 010 / 011 同一条：迁移在事务里跑（见 platform/database/migrate），用不了
-- CREATE INDEX CONCURRENTLY。本表是代扣的流水表，行数等于「签约用户数 × 期数」，比 payments
-- 小得多；真到几百万行那天再按 011 里那段办法处理。

BEGIN;

-- 1. 先加列。NOT NULL DEFAULT '' 让存量行（本表今天理论上为空）直接合法，不需要回填，
--    也不需要「先加可空、回填、再收紧」那三步。
ALTER TABLE payment_agreement_charges
    ADD COLUMN out_trade_no TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN payment_agreement_charges.out_trade_no IS
    '这一期扣款在渠道那边的商户单号（微信报文里的 out_trade_no），也是扣款结果通知回来时唯一的关联键；建行时生成一次，重试复用同一个值，绝不重算';

-- 2. 再建唯一索引。放最后是因为它扫全表：先加列再建索引，中间不会有半张表被锁两次。
CREATE UNIQUE INDEX payment_agreement_charges_trade_no_unique
    ON payment_agreement_charges (out_trade_no)
    WHERE out_trade_no <> '';

-- 3. 订正 002 给 payment_no 写的那句注释。
--
--    它写着「扣款成功时对应的支付单号（payments.payment_no），值引用」，读起来像是代扣会去建
--    一张 payments 行。**V2 不建**：payments.order_no 是 NOT NULL 且语义是订单号，而代扣没有
--    订单——为它编一个订单号是往账本里掺假。这一列的用途是「将来哪天真要落支付单时有个位置」，
--    今天它恒为空串，代扣的账在**本表自己**的状态与 out_trade_no 上。
COMMENT ON COLUMN payment_agreement_charges.payment_no IS
    '扣款落到 payments 时对应的支付单号，值引用；代扣今天**不建 payments 行**（代扣没有订单），所以这一列恒为空串，一期的账看本行的 status / out_trade_no / charged_at';

COMMIT;
