-- payment/016：删掉分账账户的三个主体引用（关联门店 / 关联品牌 / 关联商户）
--
-- 这三列与它们的派生列 subject_ref 是 008 / 009 / 015 一路带过来的，今天**一条读路径都没有**：
--
--   * 命中规则（`repository/settlement.go` 的 FindSettlementRule）挑账户只按
--     `a.id = i.account_id AND a.status = 'enabled' AND a.provider = $2`——规则项挂的是
--     account_id，不按主体筛。所以「这个账户为哪个门店服务」不参与任何一次分账。
--   * 金额计算不看它们。
--   * subject_ref 全仓 Go 里 **0 个读者**（`grep -rn "subject_ref\|SubjectRef" --include="*.go"`
--     为空），所有对 settlement_accounts 的访问都按主键走。账户列表上那一列「主体引用」是
--     admin-web 自己 `storeRef || brandRef || merchantRef` 现算的，不走这一列。
--   * payment-service 之外没有任何服务碰这张表（库隔离）。
--
-- 015 的 §1 当初给这条派生列写的理由是「同一个门店可以有两条启用中的账户，结算单会劈成两张」。
-- 那个危害在 009→015 改完之后**不成立了**：没有任何路径按主体去找账户，规则项指向哪条就是哪条。
-- 于是 subject_ref 只剩一个效果——同一个 (party_type, 主体, 渠道) 上第二条启用账户被拒（23505）。
-- 删掉它是**少报一个错**，不是少一个行为。
--
-- # 删的是哪几张表上的列
--
--   * settlement_accounts：三列 + 派生列 subject_ref + 建在它上面的唯一索引，**本文件全删**。
--   * settlement_receivers：它也有同名的三列（008 的快照），但**本文件不动**——那是另一张表上
--     的历史留痕，要删是单独一条迁移的事。账户这三列没了之后它会一直是空串（服务层不再从账户
--     抄，见 internal/service/settlement.go），而全仓没有一处渲染它。
--   * settlement_tasks：同样有这三列，那是**订单侧推过来的归属维度**，有真读者（明细列表的
--     筛选条件与详情页的「归属门店/品牌/商户」），**本文件不动**。
--
-- # 删完还剩什么唯一性
--
-- settlement_accounts_receiver_uniq (provider, receiver_type, receiver_id) WHERE status='enabled'
-- 原样留着（015 §2 建的）——那才是真正护着渠道报文的那条：receiver_id 就是子单里的 mid，
-- 同一个号在一个渠道上登记两次会让钱分给谁取决于查询顺序。
--
-- # 上生产的注意
--
-- 与 006 / 010 / 011 / 015 同一条：迁移在事务里跑，用不了 CONCURRENTLY。DROP COLUMN 与
-- DROP INDEX 都只动元数据（DROP COLUMN 会把该列从堆里标记掉，真正的空间回收要等 VACUUM），
-- settlement_accounts 是张配置表（dev 4 行），这里是瞬时的。
--
-- apply 之前先数一遍有没有真填过的行。**有值的话它们会随列一起丢掉**（本机 dev 是 0，生产未验）：
--
--   SELECT count(*) FROM settlement_accounts
--    WHERE store_ref <> '' OR brand_ref <> '' OR merchant_ref <> '';
--
-- 想留下痕迹就先把它导出来再 apply。

BEGIN;

-- 顺序不能换：subject_ref 是 GENERATED ALWAYS AS (...) STORED，表达式直接引用下面那三列，
-- 所以它必须先走——否则 DROP COLUMN 会被 PostgreSQL 挡住：
--   ERROR: cannot drop column brand_ref of table settlement_accounts because other objects depend on it
--   DETAIL: column subject_ref of table settlement_accounts depends on column brand_ref
DROP INDEX settlement_accounts_subject_uniq;
ALTER TABLE settlement_accounts DROP COLUMN subject_ref;

ALTER TABLE settlement_accounts
    DROP COLUMN merchant_ref,
    DROP COLUMN brand_ref,
    DROP COLUMN store_ref;

COMMIT;
