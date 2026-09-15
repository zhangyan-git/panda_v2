-- lottery/002：抽奖表和关键字段中文注释
--
-- 几列本轮**没有写入方**（领取、核销、换奖都延后了）。它们的注释里逐条写明了谁是将来
-- 写它的人——空列不会告诉后来的人任何事，注释会。这是账户域对
-- fortune_card_freezes.entry_keys 用过的同一套办法。

COMMENT ON TABLE lottery_activations IS '门店开通抽奖的记录，一个门店一行；开通是一个动作（有操作人和时间），不是从「有没有启用中的活动」推导出来的';
COMMENT ON COLUMN lottery_activations.location_id IS '门店 ID，属于商户服务，本库仅作跨库值引用，不建外键';
COMMENT ON COLUMN lottery_activations.location_name IS '门店名的展示快照，开通时写入，之后不订阅改名事件；列表页显示旧名字是已知代价';
COMMENT ON COLUMN lottery_activations.status IS '开通状态：enabled=开通中，disabled=已停用；停用不影响历史活动与中奖记录';
COMMENT ON COLUMN lottery_activations.remark IS '开通/停用的备注';
COMMENT ON COLUMN lottery_activations.activated_by IS '开通操作者的后台账号 ID';
COMMENT ON COLUMN lottery_activations.activated_at IS '开通时间';
COMMENT ON COLUMN lottery_activations.deactivated_at IS '停用时间；非空当且仅当 status=disabled';

COMMENT ON TABLE lottery_campaigns IS '抽奖活动；粒度由 activation_id（门店）+ 可空的 machine_id（本店某台设备）决定，不用 scope_type+scope_id 两列';
COMMENT ON COLUMN lottery_campaigns.activation_id IS '所属的门店开通记录，指向本库的 lottery_activations；活动挂在哪家店完全由它决定，没有第二个地方可以填错';
COMMENT ON COLUMN lottery_campaigns.machine_id IS '咖啡机 ID，属于咖啡机服务，本库仅作跨库值引用；NULL=整个门店，非 NULL=本店某台咖啡机';
COMMENT ON COLUMN lottery_campaigns.code IS '期次号前缀（round_no = code-seq），也是后台认这个活动的短名；大写字母数字，1-16 位';
COMMENT ON COLUMN lottery_campaigns.name IS '活动名称';
COMMENT ON COLUMN lottery_campaigns.description IS '活动描述，展示在活动详情页';
COMMENT ON COLUMN lottery_campaigns.is_default IS '是否为门店开通抽奖时按内置模板自动建的那一个；一个开通记录至多一个（部分唯一索引保证）';
COMMENT ON COLUMN lottery_campaigns.participant_target IS '新期次的默认开奖门槛（原型 threshold）；期次开门时冻结到自己身上，之后改活动不影响正在跑的那一期';
COMMENT ON COLUMN lottery_campaigns.start_at IS '活动开始时间；期次只在这个窗口内滚动';
COMMENT ON COLUMN lottery_campaigns.end_at IS '活动结束时间；最后一期的 ends_at 就是它，过了就不开新期';
COMMENT ON COLUMN lottery_campaigns.status IS '活动状态：draft=草稿，enabled=进行中，paused=暂停，ended=已结束；参与只认 enabled 且期次 open';
COMMENT ON COLUMN lottery_campaigns.created_by IS '创建者的后台账号 ID';
COMMENT ON COLUMN lottery_campaigns.updated_by IS '最近一次修改者的后台账号 ID';

COMMENT ON TABLE lottery_campaign_prizes IS '活动的奖池；quantity 是每期的中奖名额，开期时求和冻结成期次的 winner_count，开奖按 sort_order 依次分配';
COMMENT ON COLUMN lottery_campaign_prizes.campaign_id IS '所属活动，指向本库的 lottery_campaigns';
COMMENT ON COLUMN lottery_campaign_prizes.sort_order IS '开奖时的分配顺序，从 0 开始；同一活动内唯一';
COMMENT ON COLUMN lottery_campaign_prizes.prize_kind IS '奖品类型：coupon=券，coffee=饮品，physical=实物，custom=自定义；本轮只用于展示与后台分组，没有任何兑付动作';
COMMENT ON COLUMN lottery_campaign_prizes.name IS '奖品名（如「10 元咖啡兑换券」）；开奖时快照进中奖记录，之后改这里不会改写历史';
COMMENT ON COLUMN lottery_campaign_prizes.coupon_template_id IS '券模板 ID，仅 prize_kind=coupon 时有值；**本轮无人消费**——等券服务长出 IssueCoupon 的 gRPC 才轮到它，在那之前它只是一个备注';
COMMENT ON COLUMN lottery_campaign_prizes.image_url IS '奖品图片地址';
COMMENT ON COLUMN lottery_campaign_prizes.claim_instructions IS '领取说明，展示在中奖详情页';
COMMENT ON COLUMN lottery_campaign_prizes.quantity IS '每期的中奖名额；同一活动内求和就是期次开奖时抽几个人';

COMMENT ON TABLE lottery_rounds IS '活动下面滚动开的一期一期（原型里的 roundNo）；开奖后同一事务开下一期，直到活动窗口结束';
COMMENT ON COLUMN lottery_rounds.campaign_id IS '所属活动，指向本库的 lottery_campaigns';
COMMENT ON COLUMN lottery_rounds.seq IS '期次序号，从 1 开始，同一活动内唯一';
COMMENT ON COLUMN lottery_rounds.round_no IS '期次号（code-seq，如 A1-202608-03），全局唯一，展示给用户';
COMMENT ON COLUMN lottery_rounds.status IS '期次状态：open=可参与，closed=已达门槛停止收人，drawn=已开奖，cancelled=已作废';
COMMENT ON COLUMN lottery_rounds.participant_target IS '开奖门槛，开期时从活动冻结；达到它就转 closed';
COMMENT ON COLUMN lottery_rounds.participant_count IS '已达标的参与数（status=confirmed 的行数）；存下来是为了只读渲染与开奖扫描能走索引，代价是可能与参与记录漂移，由行锁和一条集成测试盯着';
COMMENT ON COLUMN lottery_rounds.winner_count IS '本期开奖抽几个人，开期时 = 奖池 quantity 之和';
COMMENT ON COLUMN lottery_rounds.starts_at IS '本期开始时间';
COMMENT ON COLUMN lottery_rounds.ends_at IS '本期截止时间，一律等于活动的 end_at；到点必开，不流局';
COMMENT ON COLUMN lottery_rounds.drawn_at IS '开奖时间；非空当且仅当 status=drawn';
COMMENT ON COLUMN lottery_rounds.cancelled_at IS '作废时间；非空当且仅当 status=cancelled';
COMMENT ON COLUMN lottery_rounds.cancel_reason IS '作废原因';
COMMENT ON COLUMN lottery_rounds.cancelled_by IS '作废操作者的后台账号 ID';

COMMENT ON TABLE lottery_participations IS '用户的抽奖参与记录，一次参与一行；同一活动可多次参与，所以没有 (round_id, user_id) 唯一约束';
COMMENT ON COLUMN lottery_participations.id IS '参与记录 ID；**这个值就是交给账户服务的 request_id**（福卡流水的 entry_key 是 draw:{这个值}）';
COMMENT ON COLUMN lottery_participations.round_id IS '所参与的期次，指向本库的 lottery_rounds';
COMMENT ON COLUMN lottery_participations.campaign_id IS '所在活动，冗余一份是为了「我的参与」少一次 join';
COMMENT ON COLUMN lottery_participations.campaign_name IS '参与当时的活动名快照';
COMMENT ON COLUMN lottery_participations.round_no IS '参与当时的期次号快照';
COMMENT ON COLUMN lottery_participations.user_id IS '小程序用户 ID，属于身份服务，仅作跨库值引用';
COMMENT ON COLUMN lottery_participations.source_order_id IS '这一笔参与用了哪张订单的福卡，属于订单服务，仅作跨库值引用；为空表示「直接参与」';
COMMENT ON COLUMN lottery_participations.source_order_no IS '来源订单号，展示在参与详情的「来源订单」上';
COMMENT ON COLUMN lottery_participations.source_machine_id IS '参与当时的咖啡机快照，属于咖啡机服务，仅作跨库值引用';
COMMENT ON COLUMN lottery_participations.source_location_id IS '参与当时的门店快照，属于商户服务，仅作跨库值引用';
COMMENT ON COLUMN lottery_participations.cost IS '这一笔参与消耗的福卡张数，目前恒为 1';
COMMENT ON COLUMN lottery_participations.status IS '参与状态：pending=已登记待扣卡，confirmed=扣卡成功计入期次，failed=确定失败，reversed=已被冲正';
COMMENT ON COLUMN lottery_participations.failure_code IS '确定失败时的业务码（insufficient_fortune_cards / invalid_request / round_closed）';
COMMENT ON COLUMN lottery_participations.fortune_entry_id IS '账户服务返回的福卡账变流水 ID，属于账户服务，仅作跨库值引用；对账时从这一笔参与反查那次扣卡';
COMMENT ON COLUMN lottery_participations.reverse_entry_id IS '补偿时冲正那一笔的账变流水 ID，仅作跨库值引用';
COMMENT ON COLUMN lottery_participations.attempts IS '修复 worker 重试过几次；只是观测，不构成重试上限——传输层一直失败的行是真的不知道扣没扣，判它 failed 就是撒谎';
COMMENT ON COLUMN lottery_participations.last_error IS '最后一次失败的错误文本，仅供排障';
COMMENT ON COLUMN lottery_participations.idempotency_key IS '幂等键，全局唯一，由服务端派生：order:{orderId}（从订单参与，「一笔订单只能参与一次」就落在这个键上）或客户端给的 Idempotency-Key 头';
COMMENT ON COLUMN lottery_participations.confirmed_at IS '计入期次的时间；非空当且仅当 status=confirmed';

COMMENT ON TABLE lottery_draws IS '每一次开奖一行，只允许追加；人工干预与自动开奖用同一套算法，只是 mode/drawn_by/reason 不同';
COMMENT ON COLUMN lottery_draws.round_id IS '被开的那一期，指向本库的 lottery_rounds；**唯一索引保证一期只能开一次**';
COMMENT ON COLUMN lottery_draws.campaign_id IS '所属活动，指向本库的 lottery_campaigns';
COMMENT ON COLUMN lottery_draws.mode IS '开奖方式：auto=worker 自动，manual=后台人工干预';
COMMENT ON COLUMN lottery_draws.trigger IS '触发条件：threshold=达门槛，deadline=到点，manual=人工；与 mode 一一对应';
COMMENT ON COLUMN lottery_draws.algorithm IS '抽签算法标识；目前只有 sha256-sort-v1，记下来是为了将来换算法时旧记录仍可复核';
COMMENT ON COLUMN lottery_draws.seed IS '随机种子；派生而非随机：sha256(round_id || 首个参与 id || 末个参与 id || 参与数 || trigger)。开奖前不可预测，开奖后任何人可据此把中奖名单完整重算一遍。注意这不是可证明公平——有库读权限的运维在那个窗口里能预测结果';
COMMENT ON COLUMN lottery_draws.participant_count IS '开奖那一刻的合格参与数（status=confirmed 的行数）';
COMMENT ON COLUMN lottery_draws.winner_count IS '实际抽出的人数 = min(奖池名额, 合格参与数)';
COMMENT ON COLUMN lottery_draws.drawn_by IS '人工开奖的操作者后台账号 ID；自动开奖恒为空';
COMMENT ON COLUMN lottery_draws.reason IS '人工开奖的原因，必填且非空白；自动开奖恒为空';

COMMENT ON TABLE lottery_wins IS '中奖记录，可变的状态行（领取、换奖、核销都会改它）；不可篡改的那一半由 lottery_win_events 承担。本轮只有 pending 可达';
COMMENT ON COLUMN lottery_wins.draw_id IS '产生这条中奖记录的那次开奖，指向本库的 lottery_draws';
COMMENT ON COLUMN lottery_wins.round_id IS '所属期次，指向本库的 lottery_rounds';
COMMENT ON COLUMN lottery_wins.campaign_id IS '所属活动，指向本库的 lottery_campaigns';
COMMENT ON COLUMN lottery_wins.participation_id IS '中奖的那一条参与记录；一次开奖里同一条参与只能中一次';
COMMENT ON COLUMN lottery_wins.user_id IS '中奖用户 ID，属于身份服务，仅作跨库值引用';
COMMENT ON COLUMN lottery_wins.round_no IS '开奖当时的期次号快照';
COMMENT ON COLUMN lottery_wins.campaign_name IS '开奖当时的活动名快照';
COMMENT ON COLUMN lottery_wins.prize_id IS '奖品 ID，指向本库的 lottery_campaign_prizes';
COMMENT ON COLUMN lottery_wins.prize_kind IS '奖品类型快照；本轮只用于展示，没有兑付动作';
COMMENT ON COLUMN lottery_wins.original_prize_name IS '原奖品名快照，永远不变；换奖改的是 current_prize_name';
COMMENT ON COLUMN lottery_wins.current_prize_name IS '当前奖品名；换奖改这一列，本轮没有写入方';
COMMENT ON COLUMN lottery_wins.claim_no IS '领取/核销凭证号，全局唯一，给人念的（LW20260915-000123）；序号来自 lottery_claim_no_seq。本轮没有消费方——领取与核销都延后了';
COMMENT ON COLUMN lottery_wins.status IS '中奖状态：pending=待领取（本轮唯一可达），claimed=已领取待核销，redeemed=已核销，expired=已过期，revoked=已撤销，superseded=被重抽取代';
COMMENT ON COLUMN lottery_wins.testimonial IS '获奖感言，领取时填写，最多 200 字；本轮没有写入方';
COMMENT ON COLUMN lottery_wins.testimonial_images IS '领奖图片，1-5 张的 JSON 数组；上传路径随小程序一起延后，本轮没有写入方';
COMMENT ON COLUMN lottery_wins.source_order_id IS '来源订单，从参与记录抄过来的快照，属于订单服务，仅作跨库值引用';
COMMENT ON COLUMN lottery_wins.source_order_no IS '来源订单号快照，展示在中奖详情的「来源订单」上';
COMMENT ON COLUMN lottery_wins.source_machine_id IS '来源咖啡机快照，属于咖啡机服务，仅作跨库值引用';
COMMENT ON COLUMN lottery_wins.source_location_id IS '来源门店快照，属于商户服务，仅作跨库值引用';
COMMENT ON COLUMN lottery_wins.expires_at IS '奖品过期时间；NULL=不过期，与福卡一致；本轮没有写入方';
COMMENT ON COLUMN lottery_wins.claimed_at IS '领取时间；非空当且仅当 status 为 claimed 或 redeemed；本轮没有写入方';
COMMENT ON COLUMN lottery_wins.redeemed_at IS '核销时间；非空当且仅当 status=redeemed；本轮没有写入方';
COMMENT ON COLUMN lottery_wins.redeemed_by IS '核销操作者 ID；本轮没有写入方';
COMMENT ON COLUMN lottery_wins.redeem_location_id IS '核销门店 ID，属于商户服务，仅作跨库值引用；本轮没有写入方';
COMMENT ON COLUMN lottery_wins.redeem_location_name IS '核销门店名快照；本轮没有写入方';

COMMENT ON TABLE lottery_win_events IS '中奖记录的流水，只允许追加；中奖那一行改过什么由这张表回答。本轮只写 created';
COMMENT ON COLUMN lottery_win_events.win_id IS '所属中奖记录，指向本库的 lottery_wins';
COMMENT ON COLUMN lottery_win_events.event_type IS '事件类型：created（开奖，本轮唯一会写的）、claimed、testimonial_updated、redeemed、swapped（换奖）、superseded、revoked、expired';
COMMENT ON COLUMN lottery_win_events.from_status IS '变更前状态；created 时为空';
COMMENT ON COLUMN lottery_win_events.to_status IS '变更后状态';
COMMENT ON COLUMN lottery_win_events.actor_type IS '操作者类型：user=中奖用户，merchant=商户端，admin=平台后台，system=系统';
COMMENT ON COLUMN lottery_win_events.actor_id IS '操作者 ID；system 时为空';
COMMENT ON COLUMN lottery_win_events.actor_name IS '操作者名称快照';
COMMENT ON COLUMN lottery_win_events.reason IS '变更原因';
COMMENT ON COLUMN lottery_win_events.metadata IS '该事件附带的补充信息（如换奖前后的奖品、撤销原因码）';

COMMENT ON TABLE message_outbox IS '抽奖服务待发布消息事件';
COMMENT ON COLUMN message_outbox.event_id IS '事件唯一 ID';
COMMENT ON COLUMN message_outbox.event_type IS '事件类型';
COMMENT ON COLUMN message_outbox.event_version IS '事件版本';
COMMENT ON COLUMN message_outbox.payload IS '事件内容二进制数据';
COMMENT ON COLUMN message_outbox.attempts IS '发布尝试次数';
COMMENT ON COLUMN message_outbox.published_at IS '成功发布时间';
COMMENT ON COLUMN message_outbox.lease_until IS '消息处理租约截止时间';

COMMENT ON TABLE message_inbox IS '抽奖服务已接收消息事件及消费租约';
COMMENT ON COLUMN message_inbox.event_id IS '已接收事件唯一 ID，用于消费去重';
COMMENT ON COLUMN message_inbox.claimed_at IS '首次领取消费时间';
COMMENT ON COLUMN message_inbox.lease_until IS '消息消费租约截止时间';
COMMENT ON COLUMN message_inbox.completed_at IS '成功完成消费时间';
