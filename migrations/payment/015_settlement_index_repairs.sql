-- payment/015：分账账户的唯一性口径对齐 + 五条外键的索引
--
-- 只动 settlement_accounts 上一条派生列与两条唯一索引，外加五张表上的索引。没有业务列的变化，
-- 也没有任何 Go 代码要跟着改（那条派生列不出现在仓储的列清单里，理由见 §1 最后一段）。
--
-- 三处问题彼此独立，读的时候可以分开看：
--
--   §1 账户的主体唯一性今天**过强又过弱**——同一个渠道上只能有一条「没写主体」的启用账户，
--      而同一个主体写在不同的列上却不算撞。
--   §2 接收方号的唯一性今天**把停用的账户也关在里面**，于是「换个子商户号」这件事无路可走。
--   §3 五条外键没有能定位的索引，删父行那一侧的检查要在子表上扫一遍。
--
-- # 上生产的注意
--
-- 与 006 / 010 / 011 同一条：迁移在事务里跑（见 platform/database/migrate），用不了
-- CREATE INDEX CONCURRENTLY。settlement_accounts 是张配置表（dev 个位数行），重建那两条索引
-- 是瞬时的；五张表里只有 settlement_tasks 会长，给它加索引在存量数据上**短暂持写锁**，
-- 真到几百万行那天按同一条办法处理（挑低峰窗口，或由 DBA 手工 CONCURRENTLY 建好再
-- `panda-migrate adopt payment 015`）。

BEGIN;

-- ============================================================
-- 1) 账户的主体键：一个主体在一个渠道上只能有一条启用中的账户
-- ============================================================
--
-- 009 把这条唯一性写在 (party_type, merchant_ref, brand_ref, store_ref, provider) 上。那五列
-- 里有三列是**选填的主体引用**（页面上的三个下拉都能留空，表单自己的注释写着「它们今天没人
-- 读」），实际配账户时多半只填渠道与子商户号——dev 库现有的四条账户一条都没填。于是这条键退
-- 化成了 (party_type, '', '', '', provider)，两头都不对：
--
--   * 过强：**同一个渠道上第二条「三个引用都空」的账户直接 23505**。一家连锁店要给第二家门店
--     配账户，页面上只会得到一句「同一主体在同一渠道上只能有一条启用中的账户」，而它压根没说
--     清是哪个主体——因为这三个引用一个都没填。这条索引实际上把口径变成了「一个渠道一条启用
--     账户」，而渠道下的主体显然不止一个。
--   * 过弱：同一个主体填在不同的列上不算撞（一条只填 store_ref，另一条把 brand_ref 也一起填
--     了），于是同一个门店可以有两条启用中的账户，结算单会劈成两张。
--
-- 改法是把「这个账户说的是哪个主体」归到**一列**上：
--
--     subject_ref = COALESCE(NULLIF(store_ref,''), NULLIF(brand_ref,''), NULLIF(merchant_ref,''), '')
--
-- 从**最具体**的那一列往上取（门店 → 品牌 → 商户；与 FindSettlementRule 命中规则的精度顺序
-- 同一个方向），三列全空时是空串，意思是「这个账户没说是哪个主体」。唯一性只对**说得清主体**
-- 的那些行成立：
--
--   * 空串不进索引（谓词里的 subject_ref <> ''）：没说主体的账户彼此之间不构成冲突。这既是
--     「过强」那一条的出口，也是「第二家门店配账户」能配得下去的原因——主体引用今天可选。
--   * 说了主体的，无论填在哪一列上都归一成同一个值，于是「过弱」那一条关掉了。
--
-- 它是**派生列而不是可写列**（GENERATED ALWAYS AS … STORED）：它必须与那三列永远一致，而一条
-- 写在 Go 里的赋值语句做不到「永远」——漏掉一处写入路径就是一个能静默绕过唯一性的空洞。让
-- 数据库算，写入方就没有机会把它写歪；仓储的列清单也因此不用动（它前面没这一列，INSERT /
-- UPDATE 的列清单里也没有，两边都对得上）。
ALTER TABLE settlement_accounts ADD COLUMN subject_ref TEXT
    GENERATED ALWAYS AS (
        COALESCE(NULLIF(store_ref, ''), NULLIF(brand_ref, ''), NULLIF(merchant_ref, ''), '')
    ) STORED;

COMMENT ON COLUMN settlement_accounts.subject_ref IS
    '这个账户登记的是哪个主体：按固定优先级取 store_ref → brand_ref → merchant_ref，三列全空时'
    '是空串（「没说主体」）。**派生列，不要写它**——它与那三列必须永远一致，而这件事只有数据库'
    '保证得住。唯一索引 settlement_accounts_subject_uniq 建在它上面：一个主体在一个渠道上只能有'
    '一条启用中的账户，空串不参与。';

-- 上生产之前先跑一次这个，它会告诉你有没有**历史遗留**的同一个主体两条启用账户（本地/dev 是
-- 空的，prod 未验）：
--
--   SELECT party_type,
--          COALESCE(NULLIF(store_ref,''), NULLIF(brand_ref,''), NULLIF(merchant_ref,''), '') AS subject_ref,
--          provider, count(*)
--     FROM settlement_accounts WHERE status = 'enabled'
--    GROUP BY 1, 2, 3 HAVING count(*) > 1;
--
-- 有行的话下面这条索引建不出来，整条迁移回滚——**那正是想要的**：一句 23505 比一个「钱按哪条
-- 账户走取决于查询返回顺序」的账户好查得多。处理办法是把多出来的那条停用（停用即让位，见 §2）。
DROP INDEX settlement_accounts_party_uniq;

CREATE UNIQUE INDEX settlement_accounts_subject_uniq
    ON settlement_accounts (party_type, subject_ref, provider)
    WHERE status = 'enabled' AND subject_ref <> '';

-- ============================================================
-- 2) receiver_uniq：停用之后把接收方号让出来
-- ============================================================
--
-- 「一个渠道下的接收方号只能登记一次」（009 的原话）今天**没有谓词**：停用的账户也占着这个
-- 位置。而账户是删不掉的（三处 ON DELETE RESTRICT：规则项、历史接收方、结算单），唯一的退场
-- 动作就是停用。
--
-- 于是会有这样一个局面：子商户号 R 原先挂在账户 A 上，A 已经停用了（换了主体，或者银联重发
-- 了号），运营要把 R 挂到新账户 B 上。B 建不出来——A 虽然停用了还占着 R，报的又是那句笼统的
-- 「这个账户号、子商户号或主体已经有一条记录」，而用户看着自己刚停用的那条很难想到是它；
-- A 也删不掉。R 就这么废了，除非有人去写库。放开之后 B 能建出来，A 继续停着不影响任何事
-- （停用的账户不参与新分账，历史明细原样保留）。
--
-- 加上 WHERE status = 'enabled' 之后，口径与刚改过的 subject 索引、以及规则那一条
-- settlement_rules_scope_uniq 一致：**启用才占位，停用即让位**。反面也照样成立——把一条停用的
-- 账户重新启用时，如果已经有一条启用中的账户占着同一个接收方号，那次启用会 23505 被拒，而那
-- 正是这条索引要防的事（分账把钱打给同一个接收方两次）。
--
-- 加了谓词是**放宽**，所以不可能与现有的行冲突（旧的无谓词版本本来就要求全局唯一）。
DROP INDEX settlement_accounts_receiver_uniq;

CREATE UNIQUE INDEX settlement_accounts_receiver_uniq
    ON settlement_accounts (provider, receiver_type, receiver_id)
    WHERE status = 'enabled';

-- ============================================================
-- 3) 五条外键的索引
-- ============================================================
--
-- PostgreSQL **不会为外键自动建索引**。缺索引的代价不在写入（这些父行几乎不删），在**删父行**
-- 那一侧：RESTRICT / CASCADE 要在子表上确认一遍，而今天这五条要么一条索引都没有，要么只有
-- 一条**定位不到**的：
--
--   settlement_tasks.rule_id                RESTRICT  没有任何索引
--   settlement_rule_items.rule_id           CASCADE   (rule_id, account_id) WHERE account_id IS NOT NULL
--   settlement_reversals.receiver_id        RESTRICT  (refund_id, receiver_id) —— receiver_id 不是前导列
--   settlement_statement_items.statement_id RESTRICT  没有任何索引
--   settlement_statements.account_id        RESTRICT  (account_id, …) WHERE status <> 'void'
--
-- 那两条部分索引**用不上**：外键检查那句 `WHERE col = $1` 推不出 `account_id IS NOT NULL` /
-- `status <> 'void'` 这类谓词，规划器只能退回去扫全表——在 dev 库上 EXPLAIN 过，
-- settlement_rule_items 与 settlement_statements 都给的是 Seq Scan，**即便把 seqscan 判成
-- 禁止**（另外两条压根没有索引，自然也扫全表）。
--
-- settlement_reversals 那一条又是另一种：它不是部分索引、receiver_id 也在键里，只是个**非
-- 前导列**——规划器只能把整条索引从头扫一遍再用 receiver_id 过滤（EXPLAIN 出来就是个 bitmap
-- index scan on settlement_reversals_refund_receiver_uniq）。比全表扫便宜，但拿不到一次定位，
-- 代价还随退款单数线性涨。
--
-- 所以下面这几条不是「已经有了、不用加」，将来也不要把它们当成重复索引删掉。五条都建成**不带
-- 谓词**的普通索引：让规划器去证明谓词蕴含关系是个没必要的依赖，而这五张表都不大，多存下来的
-- 那几行空值不值一提。
CREATE INDEX settlement_tasks_rule_idx ON settlement_tasks (rule_id);
CREATE INDEX settlement_rule_items_rule_idx ON settlement_rule_items (rule_id);
CREATE INDEX settlement_reversals_receiver_idx ON settlement_reversals (receiver_id);
CREATE INDEX settlement_statement_items_statement_idx ON settlement_statement_items (statement_id);
CREATE INDEX settlement_statements_account_idx ON settlement_statements (account_id);

COMMIT;
