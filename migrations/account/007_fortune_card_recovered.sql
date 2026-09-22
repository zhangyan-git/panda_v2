-- account/007：退款成功后追回福卡
--
-- 背景：003 让「申请退款」冻住这一单的福卡，驳回或用户撤销后解冻；**退款成功这一拍原来
-- 什么都不做**，于是走到 refunded 的售后单，它的冻结行永远停在 frozen，卡继续留在用户
-- 账上——钱退了，赠品没退。这一刀把后半段补上：退款成功 ⇒ 解冻 + 冲正这几笔发放。
--
-- 为什么不是把 status 直接写成 released：解冻与追回在账上是**相反**的两件事。解冻只是
-- 「这些卡又能抽了」，余额一分不动；追回是「这些卡收回去了」，余额真的少了。用同一个取值
-- 会让「已解冻」这一格在页面上同时指两件事，而这一格正是客服与审计照着回答问题的那个词。
--
-- 也顺手补上「退款失败 ⇒ 解冻」缺的那一半（原来那条路也漏着）：失败之后用户要重新申请，
-- 而重新申请时在途冻结已经把可用吃光，新冻结行只冻得到 0 张——第二次退款成功时一张卡都
-- 追不回来。那个洞不需要新的状态，走的就是 released。

-- ============================================================
-- 状态多一个取值
-- ============================================================

-- 约束名是 003 里那条内联 CHECK 的自动命名（已在本库 pg_constraint 里核过），不是新起的名字。
ALTER TABLE fortune_card_freezes DROP CONSTRAINT fortune_card_freezes_status_check;
ALTER TABLE fortune_card_freezes ADD CONSTRAINT fortune_card_freezes_status_check
    CHECK (status IN ('frozen', 'released', 'recovered'));

-- ============================================================
-- 追回时刻
-- ============================================================

-- 与 released_at 分成两列而不是共用一列：两件事发生在不同的时刻、由不同的事件驱动，
-- 共用会让「解冻时间」这一格在追回的行上显示一个不是解冻的时间。
ALTER TABLE fortune_card_freezes ADD COLUMN recovered_at TIMESTAMPTZ;

-- ============================================================
-- 订正注释
-- ============================================================

-- account/004 写的是「退款成功后的追回是下一轮的事」——这一刀就是那一轮。
COMMENT ON TABLE fortune_card_freezes IS '退款申请期间的福卡冻结，一张售后单一行；冻的是「可用」不是余额。三种结局：驳回或用户撤销 ⇒ released（解冻，余额不动）、退款失败 ⇒ released（解冻，余额不动）、退款成功 ⇒ recovered（先解冻再冲正那几笔发放，余额真的少了）';
COMMENT ON COLUMN fortune_card_freezes.status IS '冻结状态：frozen=冻结中，released=已解冻（申请被驳回、用户撤销、或退款失败），recovered=已追回（退款成功，那几笔发放已被冲正、钱收了回去）';
COMMENT ON COLUMN fortune_card_freezes.released_at IS '解冻时间（驳回 / 用户撤销 / 退款失败）；未解冻为空';
COMMENT ON COLUMN fortune_card_freezes.recovered_at IS '追回时间（退款成功）；未追回为空。与 released_at 互斥：一条冻结行只会走到两者之一';
COMMENT ON COLUMN fortune_card_freezes.amount IS '实际冻住的张数，从流水现算；允许为 0（申请早于发放，或这张卡已经被抽掉）。解冻与追回都不改它——追回冲正的正是这个数，审计要在一行里看见「当初冻了多少」';
