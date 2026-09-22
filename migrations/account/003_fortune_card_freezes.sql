-- account/003：退款期间的福卡冻结
--
-- 背景：一单的福卡发下去之后，用户申请退款，那些福卡今天照旧能拿去抽奖。等退款真的成功
-- 再去追回时，「已经抽过奖的福卡追不回来」——钱退了，卡也花了。所以从**用户提交退款申请
-- 的那一刻**起，这一单的福卡冻结，不可用于抽奖；申请被驳回或用户撤销后自动解冻。
--
-- 冻结**不是账变**：余额、流水、balance_after 一律不动。它改的是「可用」——
--   available = balance - frozen_balance
-- 所以这里没有新的 entry_type，fortune_card_entries 一个字节都不用改，扣减的判据则从
-- 「余额够不够」变成「可用够不够」。
--
-- 冻结粒度跟着退款范围走（scope=drink 冻基础赠送那张，scope=addon 冻加购加赠那张），
-- 所以冻结行上记的是**一组发放幂等键**而不是一个金额：金额要从流水里现算。
--
-- 冻结行本身**不是账本**，是可改的状态行（status/amount 会变），所以不加只追加触发器——
-- 那条触发器是给钱的。

-- ============================================================
-- 账户行上的冻结额
-- ============================================================

-- 与 balance 一样是流水/冻结行求和的结果，在同一次事务里更新，同样是为了让只读查询不必
-- 聚合，以及让「冻结不会被重复计数」有一个能被 CHECK 表达的地方。
ALTER TABLE fortune_card_accounts
    ADD COLUMN frozen_balance BIGINT NOT NULL DEFAULT 0 CHECK (frozen_balance >= 0);

-- 跨列不变式：冻结额永远不超过余额。它是零成本的护栏——冻结重复计数、将来「退款成功
-- 追回福卡」先扣余额后解冻写反了顺序，都会在这里当场炸出来，而不是变成一个可用余额为负、
-- 对着账本也算不平的幽灵。
--
-- 它同时是追回那条路的顺序约束（见 007）：必须先解冻（frozen_balance -= n）再冲正
-- （balance -= n），反过来写这一步会失败。
ALTER TABLE fortune_card_accounts
    ADD CONSTRAINT fortune_card_accounts_frozen_within_balance CHECK (frozen_balance <= balance);

-- ============================================================
-- 冻结明细
-- ============================================================

-- 一张售后申请一行。after_sale_no 是幂等键：同一条 applied 事件重投多少次，冻结只记一次。
--
-- 为什么按售后单而不是按流水行：冻结的起因、生命周期、解冻时点都由那张售后单决定，
-- 按它建行才能让「解冻」这件事有一个明确的、与服务调用顺序无关的落点。
CREATE TABLE fortune_card_freezes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- 冻结归属的账户。外键指向本库自己的账户行，所以这一单没承诺福卡也要先把账户行懒创建
    -- 出来——不然一个「从没收到过福卡、但申请了退款」的订单会把冻结挡在门外。
    user_id UUID NOT NULL REFERENCES fortune_card_accounts(user_id) ON DELETE RESTRICT,
    -- 售后单号，跨库值引用（售后单在 order 库）。幂等键。
    after_sale_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(after_sale_no)) > 0),
    order_id TEXT NOT NULL,
    order_no TEXT NOT NULL,
    -- 被冻的那几笔发放的幂等键，形状与 fortune_card_entries.entry_key 一致：
    --   order:{orderId}:base / order:{orderId}:bonus:{campaignId}
    -- 记键而不是记流水 ID：申请可能早于发放（paid 状态就允许申请退款），那时流水还不存在，
    -- 但键已经定了——发放落库时回头按这些键把张数补进来。
    entry_keys TEXT[] NOT NULL,
    -- 实际冻住的张数，从流水里现算。允许为 0：申请早于发放、或者这张卡已经被抽掉了
    -- （那正是「追不回来」在余额上的样子）。金额保留在行上，解冻不改它，审计要看当初冻了多少。
    amount BIGINT NOT NULL DEFAULT 0 CHECK (amount >= 0),
    -- frozen = 冻结中，released = 已解冻（驳回或用户撤销）。
    status TEXT NOT NULL CHECK (status IN ('frozen', 'released')),
    -- 冻结/解冻的原因文案，来自事件或服务侧渲染。
    reason TEXT NOT NULL DEFAULT '',
    -- 业务发生时间（申请时刻），用事件的 occurredAt 而不是 NOW()：补投旧事件时明细顺序不变。
    occurred_at TIMESTAMPTZ NOT NULL,
    released_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ============================================================
-- 索引
-- ============================================================

-- 订单详情那个「福卡」页签：按订单号取全这一单的冻结。
CREATE INDEX fortune_card_freezes_order_no_idx
    ON fortune_card_freezes (order_no);

-- 发放时的反查：「刚发的这条流水有没有被哪张售后单冻着」。发放是低频写路径，
-- 但它是每个订单完成都要走的一次查询，用 GIN 而不是全表扫。
CREATE INDEX fortune_card_freezes_entry_keys_idx
    ON fortune_card_freezes USING GIN (entry_keys);

-- 客服页：一个用户当前冻着哪些。部分索引只服务 frozen 那部分——解冻行只进审计，不进这里。
CREATE INDEX fortune_card_freezes_user_frozen_idx
    ON fortune_card_freezes (user_id, occurred_at DESC)
    WHERE status = 'frozen';
