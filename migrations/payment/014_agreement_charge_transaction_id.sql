-- payment/014：代扣那一行也要记渠道方的流水号
--
-- 给 payment_agreement_charges 加 provider_transaction_id（微信的 transaction_id），并给它一条
-- 部分唯一索引。
--
-- # 它回答的是哪个问题
--
-- 「这笔扣款在渠道那边是哪一笔」。013 给这张表加的 out_trade_no 是**我们**发给渠道的商户单号
-- ——拿它可以去查，但客服/财务在微信商户平台上看到的、以及用户在微信账单里看到的是**渠道那
-- 一侧的号**（transaction_id）。两者之间此前没有任何一列能对上：出了争议只能靠人工去翻
-- payment_notifications 的原文报文（那一表按报文摘要找行，不是按单号找）。
--
-- 列名与 payments.provider_transaction_id 逐字相同，理由也一样——**它是渠道方流水号，不是
-- 我们的任何编号**。代扣与支付在这件事上没有区别，不该各叫一个名。
--
-- # 为什么唯一
--
-- 渠道的 transaction_id 在同一个商户号下全局唯一，所以「同一笔渠道流水挂在两行扣款上」在
-- 事实上不可能。而它一旦发生，含义非常具体：**同一笔钱被记成了两期**。那正是代扣这张表最怕
-- 的错误（比漏记严重得多——漏记只是没扣，重复记是账面上多收了用户一期的钱），所以这里用
-- 唯一约束把它挡在库这一层，而不是指望服务层每一处都判对。
--
-- 谓词 `<> ''` 与 013 给 out_trade_no 建的那条同一个形状：待扣款、失败、取消的行都没有渠道
-- 流水号，空串不是号，不该被唯一性当成一个重复的值。
--
-- # 上生产的注意
--
-- 与 006 / 010 / 011 / 013 同一条：迁移在事务里跑，READ COMMITTED 下这条索引建在存量行上会
-- 短暂持写锁。本表行数等于「签约用户数 × 期数」，dev/prod 都还很小。

BEGIN;

ALTER TABLE payment_agreement_charges
    ADD COLUMN provider_transaction_id TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN payment_agreement_charges.provider_transaction_id IS
    '这一期扣款在渠道那边的流水号（微信的 transaction_id），由扣款结果通知写进来；渠道同一商户号下全局唯一，所以「同一笔渠道流水挂在两期上」被唯一索引挡住——那意味着同一笔钱被记成了两期';

CREATE UNIQUE INDEX payment_agreement_charges_transaction_unique
    ON payment_agreement_charges (provider_transaction_id)
    WHERE provider_transaction_id <> '';

COMMIT;
