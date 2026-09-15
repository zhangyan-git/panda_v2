-- lottery/001：抽奖领域基础表结构
--
-- 属于 panda_lottery（lottery-service，方案 5.7）。本库只有「抽奖」这件事：
--   1. 门店开通抽奖的记录            → lottery_activations
--   2. 抽奖活动（门店级 / 设备级）    → lottery_campaigns
--   3. 活动的奖池与中奖名额          → lottery_campaign_prizes
--   4. 期次（原型里的 roundNo）      → lottery_rounds
--   5. 用户的参与记录                → lottery_participations
--   6. 每一次开奖（只增）            → lottery_draws
--   7. 中奖记录（可变状态行）        → lottery_wins
--   8. 中奖记录的流水（只增）        → lottery_win_events
--
-- 边界（方案 5.7 的原话：「Lottery 只拥有抽奖和奖品数据；福卡余额由 Account 负责，
-- 订单事实由 Order 负责」）：
--   * 福卡张数**不在这里**。参与时调 account-service 的 DeductFortuneCards，把回来的
--     entry_id 存成值引用；本库没有余额列，也不打算有。
--   * 门店、咖啡机、订单、用户都只存不透明 UUID + 展示用快照名，不建跨库外键
--     （migrations_test.go 的跨库边界测试会拦，本库也在那份名单里）。
--   * 奖品的兑付链路本轮**没有**：prize_kind 与 coupon_template_id 只存不消费，
--     券服务至今没有 gRPC（contracts/proto/coupon/v1/coupon.proto 还是空壳）。
--     中奖记录停在 pending，领取/核销/换奖是下一轮的事——列与状态机先按最终形态建好。
--
-- 主键用 gen_random_uuid()（v4），不是方案 8.2 写的 UUIDv7：全仓 91 处主键都是它，
-- 文档那句才是错的那一个（已回写）。PG 内建，不需要扩展。
--
-- 末尾另带平台样板的一对 message_outbox / message_inbox（各库自带一份，逐列一致）。

-- ============================================================
-- 开通抽奖
-- ============================================================

-- 一个门店一行：这家店开通了抽奖。开通是一个**动作**（有操作人、有时间），不是一个
-- 「有没有启用中的活动」派生出来的状态。
--
-- 为什么必须存成一行而不是从活动推导：推导会让门店在两次开奖之间的空档里、或活动被
-- 临时停用时，显示成「没开通」——抽奖中心会对着一个其实完全正常的状态亮空页。派生还
-- 表达不了「谁在什么时候开的」。
--
-- 默认活动**不在这里存 ID**：它由 lottery_campaigns.is_default 上的部分唯一索引认出来。
-- 两处存同一件事（一列 + 一个布尔）迟早会有一处先改；而且外键会绕成环
-- （活动指向开通、开通指向活动），建表顺序还得靠 ALTER 绕过去。
-- 「默认活动不能删、只能停用」是服务层的规则，不是外键。
CREATE TABLE lottery_activations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- 门店 ID，属于商户服务，本库仅作跨库值引用。
    location_id UUID NOT NULL,
    -- 门店名的展示快照，开通时由后台选择器写入，之后不订阅改名事件。
    -- 列表页显示旧名字是已知代价，比每次列表多跳一次跨服务读划算。
    location_name TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled', 'disabled')),
    remark TEXT NOT NULL DEFAULT '',
    activated_by UUID,
    activated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deactivated_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((status = 'disabled') = (deactivated_at IS NOT NULL))
);

-- ============================================================
-- 抽奖活动
-- ============================================================

-- 活动的粒度不用 scope_type + scope_id 两列，而是「门店来自开通记录 + 一个可空的
-- machine_id」：machine_id 为 NULL 是门店级，非 NULL 是本店某台咖啡机级。
-- 两列写法要靠服务层自觉才能保证「这台设备属于这个门店」，而这里结构上就写不出来——
-- 活动挂在哪家店完全由 activation_id 决定，没有第二个地方可以填错。
-- 原型里的 scopeType: 'machine' | 'location' 就是 machine_id 非空与否。
CREATE TABLE lottery_campaigns (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    activation_id UUID NOT NULL REFERENCES lottery_activations(id) ON DELETE RESTRICT,
    -- 咖啡机 ID，属于咖啡机服务，本库仅作跨库值引用；NULL = 整个门店。
    machine_id UUID,
    -- 期次号的前缀（round_no = '{code}-{seq:04d}'），也是后台认这个活动的短名。
    code TEXT NOT NULL CHECK (code ~ '^[A-Z0-9]{1,16}$'),
    name TEXT NOT NULL CHECK (char_length(trim(name)) > 0),
    description TEXT NOT NULL DEFAULT '',
    -- 开通抽奖时按内置模板自动建的那一个。一个开通记录只能有一个（部分唯一索引见下）。
    is_default BOOLEAN NOT NULL DEFAULT FALSE,
    -- 新期次的默认门槛（原型里的 threshold）：累计到这么多人参与就开奖。
    -- 期次开门时把它冻结到自己身上，之后改活动不会回头改一期正在跑的奖。
    participant_target INTEGER NOT NULL CHECK (participant_target > 0),
    start_at TIMESTAMPTZ NOT NULL,
    end_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'enabled', 'paused', 'ended')),
    created_by UUID,
    updated_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (end_at > start_at)
);

-- ============================================================
-- 奖池
-- ============================================================

-- 一个活动的奖品清单。quantity 是**每期**的中奖名额：开期时把 SUM(quantity) 冻结成
-- 期次的 winner_count，开奖时按 sort_order 依次分配名额。
--
-- 原型一个活动只有一个 prize 字符串，那落在这一张表的一行上——不为单奖品另设一条路。
CREATE TABLE lottery_campaign_prizes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id UUID NOT NULL REFERENCES lottery_campaigns(id) ON DELETE RESTRICT,
    sort_order INTEGER NOT NULL CHECK (sort_order >= 0),
    -- 本期只用于展示与后台分组，没有任何兑付动作（券服务没有 gRPC）。
    prize_kind TEXT NOT NULL DEFAULT 'custom' CHECK (prize_kind IN ('coupon', 'coffee', 'physical', 'custom')),
    name TEXT NOT NULL CHECK (char_length(trim(name)) > 0),
    -- 值引用，仅 prize_kind='coupon' 时有值。**本轮无人消费**——等券服务长出
    -- IssueCoupon 的 gRPC 才轮到它，在那之前它只是一个备注。
    coupon_template_id TEXT NOT NULL DEFAULT '',
    image_url TEXT NOT NULL DEFAULT '',
    claim_instructions TEXT NOT NULL DEFAULT '',
    quantity INTEGER NOT NULL CHECK (quantity > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ============================================================
-- 期次
-- ============================================================

-- 活动下面滚动开的一期一期。原型里 LAKE-202608-12 已经到第 12 期，所以不是一期一活动：
-- 开奖后同一事务里开下一期，一直到活动窗口结束或活动停用。
CREATE TABLE lottery_rounds (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id UUID NOT NULL REFERENCES lottery_campaigns(id) ON DELETE RESTRICT,
    seq INTEGER NOT NULL CHECK (seq > 0),
    round_no TEXT NOT NULL CHECK (char_length(trim(round_no)) > 0),
    status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed', 'drawn', 'cancelled')),
    -- 开期时从活动冻结下来，之后改活动不影响这一期。
    participant_target INTEGER NOT NULL CHECK (participant_target > 0),
    -- 已达标的参与数。存下来而不是 COUNT(*)：它既是抽奖中心每次渲染都要读的数，
    -- 又是开奖 worker 的扫描判据——列比较能走索引，聚合不能。代价是可能与参与记录
    -- 漂移，所以有一把行锁 + 一条集成测试盯着「= COUNT(*) WHERE status='confirmed'」，
    -- 与账户域盯着「SUM(amount) = balance」是同一套办法。
    participant_count INTEGER NOT NULL DEFAULT 0 CHECK (participant_count >= 0),
    -- 开期时 = 奖池 SUM(quantity)。
    winner_count INTEGER NOT NULL CHECK (winner_count > 0),
    starts_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 一律取 campaign.end_at：一期的截止就是活动的截止。
    ends_at TIMESTAMPTZ NOT NULL,
    drawn_at TIMESTAMPTZ,
    cancelled_at TIMESTAMPTZ,
    cancel_reason TEXT NOT NULL DEFAULT '',
    cancelled_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (ends_at > starts_at),
    CHECK ((status = 'drawn') = (drawn_at IS NOT NULL)),
    CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL))
);

-- ============================================================
-- 参与记录
-- ============================================================

-- 一次参与一行。「同一活动可多次参与」是原型的明确文案，所以**没有** (round_id, user_id)
-- 唯一约束——键不同就是不同的参与。
CREATE TABLE lottery_participations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    round_id UUID NOT NULL REFERENCES lottery_rounds(id) ON DELETE RESTRICT,
    -- 冗余一份活动，为了「我的参与」少一次 join。
    campaign_id UUID NOT NULL REFERENCES lottery_campaigns(id) ON DELETE RESTRICT,
    campaign_name TEXT NOT NULL DEFAULT '',
    round_no TEXT NOT NULL DEFAULT '',
    -- 小程序用户 ID，属于身份服务，仅作跨库值引用。
    user_id UUID NOT NULL,
    -- 这一笔参与是从哪张订单的福卡来的（原型参与详情里的「来源订单」）。
    -- 为空 = 「直接参与」。同样是值引用，不建外键。
    source_order_id UUID,
    source_order_no TEXT NOT NULL DEFAULT '',
    -- 参与当时这台设备/这个点位的快照，用于中奖记录里的「来源点位」。
    source_machine_id UUID,
    source_location_id UUID,
    cost INTEGER NOT NULL DEFAULT 1 CHECK (cost > 0),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'confirmed', 'failed', 'reversed')),
    failure_code TEXT NOT NULL DEFAULT '',
    -- 账户服务回的账变流水 ID（值引用），对账时从这一笔参与反查那次扣卡。
    fortune_entry_id UUID,
    -- 补偿时冲正那一笔的账变流水 ID。
    reverse_entry_id UUID,
    -- 修复 worker 用：看不见的失败次数与最后一次错。都不是状态，只是观测。
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error TEXT,
    -- 幂等键，唯一索引见下。由**服务端**派生，不给客户端自由发挥：
    --   order:{orderId}          从某张订单参与——「一笔订单只能参与一次」就落在这个键上
    --   {Idempotency-Key 请求头}  从抽奖中心直接参与
    idempotency_key TEXT NOT NULL CHECK (char_length(trim(idempotency_key)) > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    confirmed_at TIMESTAMPTZ,
    CHECK ((status = 'confirmed') = (confirmed_at IS NOT NULL))
);

-- ============================================================
-- 开奖记录
-- ============================================================

-- 每一次开奖一行，只允许追加。
--
-- seed 是**派生值而不是随机数**：sha256(round_id || 首个参与 id || 末个参与 id ||
-- 参与数 || trigger)。参与 id 是 v4 UUID，所以开奖前谁也预测不了；开奖后任何人都能
-- 拿 lottery_draws + lottery_participations 把中奖名单完整重算一遍——这正是审计要的
-- 性质。**这不是可证明公平**：有本库读权限的运维在「最后一人参与到扫描之间」那个窗口
-- 里能预测结果，commit–reveal 不在本轮范围。能保证的只有「记录的种子 + 记录的参与集合
-- = 记录的中奖名单」，外加这张表改不动。
CREATE TABLE lottery_draws (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    round_id UUID NOT NULL REFERENCES lottery_rounds(id) ON DELETE RESTRICT,
    campaign_id UUID NOT NULL REFERENCES lottery_campaigns(id) ON DELETE RESTRICT,
    mode TEXT NOT NULL CHECK (mode IN ('auto', 'manual')),
    trigger TEXT NOT NULL CHECK (trigger IN ('threshold', 'deadline', 'manual')),
    algorithm TEXT NOT NULL DEFAULT 'sha256-sort-v1' CHECK (char_length(trim(algorithm)) > 0),
    seed TEXT NOT NULL CHECK (char_length(trim(seed)) > 0),
    -- 开奖那一刻的合格参与数（= 参与记录里 status='confirmed' 的行数）。
    participant_count INTEGER NOT NULL CHECK (participant_count >= 0),
    -- 实际抽出的人数 = min(winner_count, participant_count)。
    winner_count INTEGER NOT NULL CHECK (winner_count >= 0),
    -- 自动开奖没有操作人也没有理由；人工干预两者都必填。
    drawn_by UUID,
    reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((mode = 'manual') = (trigger = 'manual')),
    CHECK (mode <> 'auto' OR (drawn_by IS NULL AND reason = '')),
    CHECK (mode <> 'manual' OR (drawn_by IS NOT NULL AND char_length(trim(reason)) > 0))
);

-- ============================================================
-- 中奖记录
-- ============================================================

-- 只给中奖记录的凭证号用（下面的 DEFAULT 会引用它，所以必须建在表前面——
-- nextval('名字') 里的名字在 DDL 时就要解析成 regclass，序列不存在就建不出表）。
-- 它要短、连续、能被人念出来，UUID 做不到这三件事。
CREATE SEQUENCE lottery_claim_no_seq;

-- 中奖记录是**可变的状态行**（领取、换奖、核销都会改它），所以它和别的资产表不一样，
-- 没有只增触发器；不可篡改的那一半由 lottery_win_events 承担。
--
-- 本轮只有 pending 可达：领取、核销、换奖都延后了。其余取值是最终状态机的一部分，
-- 先写进 CHECK，免得下一轮为了加一个词表再来一次迁移。同理，claim_no / testimonial* /
-- redeem_* 这几列本轮**没有写入方**，列注释里逐条写明谁是将来写它的人。
CREATE TABLE lottery_wins (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    draw_id UUID NOT NULL REFERENCES lottery_draws(id) ON DELETE RESTRICT,
    round_id UUID NOT NULL REFERENCES lottery_rounds(id) ON DELETE RESTRICT,
    campaign_id UUID NOT NULL REFERENCES lottery_campaigns(id) ON DELETE RESTRICT,
    -- 中奖的那一条参与记录。一次开奖里同一条参与只能中一次。
    participation_id UUID NOT NULL REFERENCES lottery_participations(id) ON DELETE RESTRICT,
    user_id UUID NOT NULL,
    round_no TEXT NOT NULL DEFAULT '',
    campaign_name TEXT NOT NULL DEFAULT '',
    prize_id UUID NOT NULL REFERENCES lottery_campaign_prizes(id) ON DELETE RESTRICT,
    prize_kind TEXT NOT NULL CHECK (prize_kind IN ('coupon', 'coffee', 'physical', 'custom')),
    -- 原奖品与现奖品分两列：换奖改的是 current_，original_ 永远留着原样。
    -- 原型的中奖记录就是这个形状（wins[].originalPrize / currentPrize）。
    original_prize_name TEXT NOT NULL CHECK (char_length(trim(original_prize_name)) > 0),
    current_prize_name TEXT NOT NULL CHECK (char_length(trim(current_prize_name)) > 0),
    -- 领取/核销的凭证号，给人念的（LW20260915-000123）。序号来自下面的序列：
    -- 它要短、要连续、要能被人抄在纸上，UUID 做不到这三件事。
    claim_no TEXT NOT NULL DEFAULT ('LW' || to_char(NOW(), 'YYYYMMDD') || '-' ||
        lpad(nextval('lottery_claim_no_seq')::TEXT, 6, '0')),
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'claimed', 'redeemed', 'expired', 'revoked', 'superseded')),
    -- 获奖感言与图片（原型领取时必填 1–5 张）。图片上传随小程序一起延后，
    -- 本轮两列都没有写入方。
    testimonial TEXT NOT NULL DEFAULT '' CHECK (char_length(testimonial) <= 200),
    testimonial_images JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- 来源快照，从参与记录抄过来，让中奖详情不必回头 join。
    source_order_id UUID,
    source_order_no TEXT NOT NULL DEFAULT '',
    source_machine_id UUID,
    source_location_id UUID,
    -- NULL = 不过期，与福卡一致（福卡到今天也没有过期这个概念）。
    expires_at TIMESTAMPTZ,
    claimed_at TIMESTAMPTZ,
    -- 核销：谁、在哪家店、什么时候。核销本轮没做，三列都还没有写入方。
    redeemed_at TIMESTAMPTZ,
    redeemed_by UUID,
    redeem_location_id UUID,
    redeem_location_name TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (status NOT IN ('claimed', 'redeemed') OR claimed_at IS NOT NULL),
    CHECK ((status = 'redeemed') = (redeemed_at IS NOT NULL)),
    CHECK (status <> 'redeemed' OR redeemed_by IS NOT NULL)
);

-- ============================================================
-- 中奖流水
-- ============================================================

-- 中奖记录那一行是可变的，它改过什么由这张只增的表回答。
-- 本轮只会写 'created'（开奖时），其余取值留给领取/换奖/核销/过期。
CREATE TABLE lottery_win_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    win_id UUID NOT NULL REFERENCES lottery_wins(id) ON DELETE RESTRICT,
    event_type TEXT NOT NULL CHECK (event_type IN
        ('created', 'claimed', 'testimonial_updated', 'redeemed', 'swapped', 'superseded', 'revoked', 'expired')),
    from_status TEXT NOT NULL DEFAULT '',
    to_status TEXT NOT NULL DEFAULT '',
    actor_type TEXT NOT NULL CHECK (actor_type IN ('user', 'merchant', 'admin', 'system')),
    actor_id UUID,
    actor_name TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ============================================================
-- 触发器：只增表
-- ============================================================

CREATE FUNCTION prevent_lottery_draw_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'lottery draw record is append-only';
END;
$$;

CREATE TRIGGER lottery_draws_append_only
    BEFORE UPDATE OR DELETE ON lottery_draws
    FOR EACH ROW EXECUTE FUNCTION prevent_lottery_draw_change();

CREATE FUNCTION prevent_lottery_win_event_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'lottery win event log is append-only';
END;
$$;

CREATE TRIGGER lottery_win_events_append_only
    BEFORE UPDATE OR DELETE ON lottery_win_events
    FOR EACH ROW EXECUTE FUNCTION prevent_lottery_win_event_change();

-- ============================================================
-- 平台样板：消息 outbox / inbox
-- ============================================================

-- 与 identity/003、merchant/002、coupon/001、coffee_machine/001、order/001、payment/001、
-- account/001 里的同名两表逐列一致，各库自带一份，跨库不共享表。
--
-- outbox 对开奖不是可选件：lottery.round.drawn 与开奖记录在同一次事务里追加，
-- relay 才不可能漏投；后台的人工开奖与作废还要在同一个事务里追加一条
-- admin.operation.logged（platform/audit 的写法），由 relay 投到身份库的
-- admin_operation_logs——本库不建自己的审计表。
--
-- 这里是全新库，lease 列直接建在表里，所以不需要 identity/003 那组
-- ALTER TABLE ADD COLUMN IF NOT EXISTS。列名和顺序保持不变，八份拷贝才能逐列对得起来。

CREATE TABLE message_outbox (
    event_id TEXT PRIMARY KEY CHECK (char_length(trim(event_id)) > 0),
    event_type TEXT NOT NULL DEFAULT '',
    event_version TEXT NOT NULL DEFAULT '',
    trace_id TEXT NOT NULL DEFAULT '',
    payload BYTEA NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at TIMESTAMPTZ,
    lease_owner TEXT,
    lease_token TEXT,
    lease_until TIMESTAMPTZ,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE message_inbox (
    event_id TEXT PRIMARY KEY CHECK (char_length(trim(event_id)) > 0),
    claimed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_owner TEXT,
    lease_token TEXT,
    lease_until TIMESTAMPTZ,
    completed_at TIMESTAMPTZ
);

-- ============================================================
-- 唯一约束（不变式）
-- ============================================================

-- 一个门店只能开通一次。重复开通是「按钮点了两下」，不是两笔业务。
CREATE UNIQUE INDEX lottery_activations_location_unique
    ON lottery_activations (location_id);

-- 一个开通记录只能有一个默认活动。默认活动不能删、只能停用，由服务层拦。
CREATE UNIQUE INDEX lottery_campaigns_one_default_per_activation
    ON lottery_campaigns (activation_id) WHERE is_default;

CREATE UNIQUE INDEX lottery_campaigns_code_unique
    ON lottery_campaigns (code);

CREATE UNIQUE INDEX lottery_campaign_prizes_order_unique
    ON lottery_campaign_prizes (campaign_id, sort_order);

CREATE UNIQUE INDEX lottery_rounds_no_unique
    ON lottery_rounds (round_no);

CREATE UNIQUE INDEX lottery_rounds_seq_unique
    ON lottery_rounds (campaign_id, seq);

-- 一个活动同时只能有一期在跑。这条不变量是「期次连开」的地基：开下一期之前，
-- 上一期必须已经 drawn 或 cancelled。
CREATE UNIQUE INDEX lottery_rounds_one_live_per_campaign
    ON lottery_rounds (campaign_id) WHERE status IN ('open', 'closed');

-- 开奖 worker 的扫描索引：closed（已达门槛）与 open+到点两条路都走它。
CREATE INDEX lottery_rounds_sweep_idx
    ON lottery_rounds (status, ends_at);

-- 幂等：同一个键只允许一条参与。
CREATE UNIQUE INDEX lottery_participations_key_unique
    ON lottery_participations (idempotency_key);

-- 「我的参与」：一个用户按时间倒序。含 id 是为了同毫秒的两条也有稳定顺序。
CREATE INDEX lottery_participations_user_idx
    ON lottery_participations (user_id, created_at DESC, id);

-- 开奖取件：按稳定的 id 序取这一期全部已确认的参与。
CREATE INDEX lottery_participations_round_idx
    ON lottery_participations (round_id, status, id);

-- 修复 worker：只看卡在 pending 的行。
CREATE INDEX lottery_participations_repair_idx
    ON lottery_participations (created_at) WHERE status = 'pending';

-- 一期只能开一次奖。多副本 worker 不需要选主，全部依据就是这一条。
CREATE UNIQUE INDEX lottery_draws_round_unique
    ON lottery_draws (round_id);

-- 一次开奖里同一条参与只能中一次。
CREATE UNIQUE INDEX lottery_wins_participation_unique
    ON lottery_wins (draw_id, participation_id);

CREATE UNIQUE INDEX lottery_wins_claim_no_unique
    ON lottery_wins (claim_no);

CREATE INDEX lottery_wins_user_idx
    ON lottery_wins (user_id, created_at DESC, id);

CREATE INDEX lottery_wins_campaign_idx
    ON lottery_wins (campaign_id, created_at DESC, id);

-- 待领取/待核销的列表与将来的过期扫描。都还是活跃状态的行，部分索引足够。
CREATE INDEX lottery_wins_open_idx
    ON lottery_wins (status, expires_at) WHERE status IN ('pending', 'claimed');

CREATE INDEX lottery_win_events_win_idx
    ON lottery_win_events (win_id, created_at, id);

-- relay 取待投递消息、以及续租，都只看未发布的行。
CREATE INDEX message_outbox_pending_idx
    ON message_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;

CREATE INDEX message_outbox_lease_idx
    ON message_outbox (lease_until)
    WHERE published_at IS NULL;
