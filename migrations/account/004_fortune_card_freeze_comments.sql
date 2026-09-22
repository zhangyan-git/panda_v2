-- account/004：冻结相关列与表的中文注释

COMMENT ON COLUMN fortune_card_accounts.frozen_balance IS '退款冻结中的福卡张数；可用张数 = balance - frozen_balance，不落库。冻结不是账变，它改的是「可用」，balance 与流水都不动';

-- 本行与下面 status 那一行的文案已被 007 覆盖：003 时点的结局只有 released 一种，退款失败解冻
-- 与退款成功追回是 007 补上的。留在这里的是这一轮写下它时的事实。
COMMENT ON TABLE fortune_card_freezes IS '退款申请期间的福卡冻结，一张售后单一行；冻的是「可用」不是余额，驳回或用户撤销后解冻，退款成功后的追回是下一轮的事';
COMMENT ON COLUMN fortune_card_freezes.user_id IS '冻结归属的账户，指向本库的 fortune_card_accounts；账户行在该用户第一次收到福卡或第一次被冻结时懒创建';
COMMENT ON COLUMN fortune_card_freezes.after_sale_no IS '售后单号，跨库值引用（售后单在 order 库）；幂等键，同一条申请事件重投多少次都只记一行';
COMMENT ON COLUMN fortune_card_freezes.order_id IS '订单 ID，跨库值引用';
COMMENT ON COLUMN fortune_card_freezes.order_no IS '订单号，供客服与订单详情页直接检索';
COMMENT ON COLUMN fortune_card_freezes.entry_keys IS '被冻的那几笔发放的幂等键（order:{orderId}:base / order:{orderId}:bonus:{campaignId}），由订单域按退款范围拆好；记键而不是记流水 ID，因为申请可能早于发放';
COMMENT ON COLUMN fortune_card_freezes.amount IS '实际冻住的张数，从流水现算；允许为 0（申请早于发放，或这张卡已经被抽掉），解冻时保留原值供审计';
COMMENT ON COLUMN fortune_card_freezes.status IS '冻结状态：frozen=冻结中，released=已解冻（申请被驳回或用户撤销）';
COMMENT ON COLUMN fortune_card_freezes.reason IS '冻结或解冻的原因文案';
COMMENT ON COLUMN fortune_card_freezes.occurred_at IS '业务发生时间（申请时刻）；用事件的时刻而不是 NOW()，补投旧事件时明细顺序才不变';
COMMENT ON COLUMN fortune_card_freezes.released_at IS '解冻时间；未解冻为空';
COMMENT ON COLUMN fortune_card_freezes.created_at IS '入库时间';
COMMENT ON COLUMN fortune_card_freezes.updated_at IS '最近一次状态变更时间（发放联动补冻也会改它）';
