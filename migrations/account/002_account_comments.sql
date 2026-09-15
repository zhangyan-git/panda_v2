-- account/002：账户表和关键字段中文注释

COMMENT ON TABLE fortune_card_accounts IS '福卡余额账户，一个用户一行，第一次发放时懒创建；余额与流水在同一次事务里更新';
COMMENT ON COLUMN fortune_card_accounts.user_id IS '小程序用户 ID，属于身份服务时仅作跨库值引用';
COMMENT ON COLUMN fortune_card_accounts.balance IS '可用福卡张数；等于该用户全部流水 amount 之和，存下来是为了只读查询与「余额不会被扣穿」这条 CHECK——只有一列能被约束，流水表上没法表达这个不变式';
COMMENT ON COLUMN fortune_card_accounts.created_at IS '账户创建时间（第一次发放到账的时间）';
COMMENT ON COLUMN fortune_card_accounts.updated_at IS '最近一次余额变动时间';

COMMENT ON TABLE fortune_card_entries IS '福卡收支流水，只允许追加：冲正是新增反向记录，不原地改数';
COMMENT ON COLUMN fortune_card_entries.user_id IS '账户归属用户，指向本库的 fortune_card_accounts';
COMMENT ON COLUMN fortune_card_entries.entry_type IS '账变类型：grant=发放（订单完成赠送），draw=抽奖扣减，reverse=冲正';
COMMENT ON COLUMN fortune_card_entries.amount IS '变动张数，有符号：发放为正，扣减为负，冲正与它冲掉的那笔相反';
COMMENT ON COLUMN fortune_card_entries.balance_after IS '变动后余额；明细页显示的就是它，冲正重放时也靠它回答「当时是多少」';
COMMENT ON COLUMN fortune_card_entries.title IS '这一行在用户那边显示成什么（订单完成赠送、参与抽奖等）；文案由账户服务拥有，调用方给的是起因不是文案';
COMMENT ON COLUMN fortune_card_entries.reference_type IS '账变起因的对象类型：order=订单，draw=抽奖参与，entry=冲正指向的流水；本列记的是「为什么余额变了」，与事件日志不是一回事';
COMMENT ON COLUMN fortune_card_entries.reference_id IS '账变起因的对象 ID，仅作跨库值引用';
COMMENT ON COLUMN fortune_card_entries.reference_no IS '账变起因的对外单号（订单号），供客服与订单详情页直接检索';
COMMENT ON COLUMN fortune_card_entries.entry_key IS '幂等键，全局唯一；三种形状：order:{orderId}:base 与 order:{orderId}:bonus:{campaignId}（发放）、draw:{requestId}（扣减）、reverse:{entryId}（冲正）';
COMMENT ON COLUMN fortune_card_entries.reverses_entry_id IS '冲正对应的原始流水 ID，指向本表；非空当且仅当 entry_type=reverse';
COMMENT ON COLUMN fortune_card_entries.remark IS '备注';
COMMENT ON COLUMN fortune_card_entries.occurred_at IS '业务发生时间；发放用订单的完成时间而不是 NOW()，补投或重放旧事件才不会把上周的单记成刚才';
COMMENT ON COLUMN fortune_card_entries.created_at IS '入库时间';

COMMENT ON TABLE message_outbox IS '账户服务待发布消息事件';
COMMENT ON COLUMN message_outbox.event_id IS '事件唯一 ID';
COMMENT ON COLUMN message_outbox.event_type IS '事件类型';
COMMENT ON COLUMN message_outbox.event_version IS '事件版本';
COMMENT ON COLUMN message_outbox.payload IS '事件内容二进制数据';
COMMENT ON COLUMN message_outbox.attempts IS '发布尝试次数';
COMMENT ON COLUMN message_outbox.published_at IS '成功发布时间';
COMMENT ON COLUMN message_outbox.lease_until IS '消息处理租约截止时间';

COMMENT ON TABLE message_inbox IS '账户服务已接收消息事件及消费租约';
COMMENT ON COLUMN message_inbox.event_id IS '已接收事件唯一 ID，用于消费去重';
COMMENT ON COLUMN message_inbox.claimed_at IS '首次领取消费时间';
COMMENT ON COLUMN message_inbox.lease_until IS '消息消费租约截止时间';
COMMENT ON COLUMN message_inbox.completed_at IS '成功完成消费时间';
