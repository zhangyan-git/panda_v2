-- payment/017：删掉分账账户的两个标签列（账户号 / 接收方名），主体名接手做这一行的身份
--
-- 两列的处境与 016 那三个主体引用是同一类：**全仓没有程序性读者**，只有「自己那一列在列表上
-- 显示一下」这一种用途。但各有一条自己的说法，写在这里：
--
-- # receiver_name（接收方名）
--
--   * 进不了渠道报文：`service/create.go` 的 divisionFor 只往子单里放 Mid 与 Amount，
--     适配器那条 divisionSubOrder 是 {mid, totalAmount, remark}，而 Remark 恒空
--     （`provider/ums/create.go:83`）。也就是说这一列从来没有随支付发出去过。
--   * 进不了快照：008 建 settlement_receivers 时快照的是 party_name，那张表里**根本没有**
--     receiver_name 这一列。所以任务详情、结算单都看不到它。
--   * 规则页看不见它：规则项的账户下拉 label 用的是 partyName。
--   * 008 的 DDL 里相邻每一列都有注释，**只有这一列一个字都没有**——从建表起它就没有被想清楚
--     过一次。
--
-- 它设想的是「这一方在渠道登记的名字」（可以与主体名不同）。但一个没人读的字段，实践中就
-- 会长成 dev 库那样：4 行里 party_name 是「E2E 收款主体 …」、receiver_name 是「E2E 子商户」，
-- 谁也不去对齐它。
--
-- # account_no（账户号）
--
--   * 008 给它的注释是「结算单上写它」——**这句话没有兑现**：settlement_statements 里没有
--     account_no 这一列（结算单本身有意没做）。将来真做也是挂 account_id，不是抄一个自由文本。
--   * 它的 UNIQUE 约束与 016 删掉的 subject_uniq 是同一种：只保证两个标签不重名，不保护任何
--     系统依赖的东西。真正护着渠道报文的仍然是 receiver_uniq。
--   * 它唯一占住的位置是**主体名为空时的兜底标签**（列表「主体名」列与规则页下拉都写着
--     `partyName || accountNo`）。所以本文件在删它的同时把 party_name 变成**必填**，
--     见下面那条 CHECK——否则会多出一种「这一行只有子商户号、没有名字」的账户。
--
-- # 上生产的注意
--
-- 与 016 不同，**这一条不是无损的**：这两列 dev 的 4 行都有值（`ACC-CHG-…` / `ST-E2E-…`、
-- 「E2E 子商户」），删掉就没了。生产有没有真值未验。
--
-- CHECK 那条更要先数：**只要有哪怕一行 party_name 是空的，ADD CONSTRAINT 就会失败**
-- （不是只跳过那一行），整条迁移回滚。apply 之前先跑：
--
--   SELECT count(*) FROM settlement_accounts WHERE trim(party_name) = '';   -- 必须是 0
--   SELECT account_no, party_name, receiver_name FROM settlement_accounts;   -- 想留痕就先导出来
--
-- 两条都是配置表（dev 4 行）上的元数据改动，瞬时完成。

BEGIN;

-- account_no 上挂的 UNIQUE 约束（settlement_accounts_account_no_key）会随列一起消失，
-- 不需要显式 DROP CONSTRAINT。008 那条 CHECK 同理。
ALTER TABLE settlement_accounts
    DROP COLUMN receiver_name,
    DROP COLUMN account_no;

-- 主体名从「可空」变成这一行的身份。约束名字与 008 给 account_no 起的那条对齐——
-- 它挡住的正是同一件事（一个空的名字等于没有名字），只是现在挂到了活下来的那一列上。
--
-- 列上的 DEFAULT '' 留着不动（008 建的）：它现在**永远满足不了**这条 CHECK，所以一条漏写
-- party_name 的 INSERT 会以 23514 失败，而不是静默存进一个没有名字的账户——那正是这里要的
-- 结果，不必再去摘那个默认值（改它就要动这一列，而这一列只是从「可空」变成「必填」）。
ALTER TABLE settlement_accounts
    ADD CONSTRAINT settlement_accounts_party_name_check
        CHECK (char_length(TRIM(BOTH FROM party_name)) > 0);

COMMIT;
