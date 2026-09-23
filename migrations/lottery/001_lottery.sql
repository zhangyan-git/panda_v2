-- lottery：抽奖领域表结构与中文注释
--
-- 属于 panda_lottery（lottery-service，方案 5.7）。本库只有「抽奖」这件事：
--   1. 门店开通抽奖的记录            → lottery_activations
--   2. 抽奖活动（门店级 / 设备级）    → lottery_campaigns
--   3. 活动的奖池（一个活动一行）     → lottery_campaign_prizes
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
--   * 奖品的兑付链路本轮**没有**：奖池只有奖品名、说明与两张图，券服务至今没有 gRPC
--     （contracts/proto/coupon/v1/coupon.proto 还是空壳）。中奖记录停在 pending，
--     领取/核销/换奖是下一轮的事——列与状态机先按最终形态建好。
--
-- 抽奖没有时间窗口：开奖只由「收满门槛」触发，没满就一直等着。
--   活动与期次都没有起止时间。窗口那套东西运营侧看下来是多余的，而且和真实心理模型相反——
--   说好「一期收满 10 次就开奖」，就该收满才开；设一个截止时间只会制造出「10 次没到也开了奖」
--   和「窗口一过活动自己结束」这两种没人预期过的结果。
--   所以一期只有三条出路：收满门槛转 closed 并开奖、管理员人工开奖、管理员作废。
--   **期次可以一直开着是有意的**，不是兜底。零人参与 / 长期没人参与的活动，出口是人工作废，
--   作废会在同一事务里开出下一期（repository.CancelRound）。
--
-- 奖池收敛成一个奖品：一个活动恰好一行（唯一索引是「只留一个」的执行者，不只是约束）。
--   原型一个活动只有一个 prize 字符串，那落在这一张表的一行上——不为单奖品另设一条路。
--   表格里因此没有排序号、没有奖品类型，「加一个奖品」这件事不存在。
--
-- 主键用 gen_random_uuid()（v4），不是方案 8.2 写的 UUIDv7：全仓 91 处主键都是它，
-- 文档那句才是错的那一个（已回写）。PG 内建，不需要扩展。
--
-- 末尾另带平台样板的一对 message_outbox / message_inbox（各库自带一份，逐列一致）。
--
-- 本文件不带 goose 的 Down 段：platform/database/migrate 把整个文件丢给一次 Exec、
-- 不识别 goose 指令，带上就会在同一个事务里建完表再删掉，而且不报错。

BEGIN;

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
--
-- 门店名不落库，显示时向商户域实时解析。这是被一条更硬的约束逼出来的：列表页想按
-- **门店名**做模糊搜，可商户域的 gRPC 没有「按名字查门店」的能力——ListStores 只收
-- merchant_id，ResolveScopeNames 只收 id。于是「显示时实时查」和「按名字搜」两件事只能
-- 留一件：留的是实时查，换掉的是那个交互（筛选改成从门店下拉里选门店，传 location_id
-- 走等值比较——这条筛选后端本来就有）。存一份名字快照等于留着第二份会和商户域走偏的事实。
--
-- 顺带把「幽灵门店」堵掉：开通前先向商户域要一次 GetStore，不存在的门店直接回 404，
-- 而不是把一条查无此店的开通记录建出来（见 service.Activate）。
CREATE TABLE lottery_activations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- 门店 ID，属于商户服务，本库仅作跨库值引用。
    location_id UUID NOT NULL,
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
--
-- 活动没有起止时间：收满门槛才开奖，期次一直滚到人工停用为止（见文件头）。
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
    status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'enabled', 'paused', 'ended')),
    created_by UUID,
    updated_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ============================================================
-- 奖池
-- ============================================================

-- 一个活动恰好一行奖品。quantity 是**每期**的中奖名额：开期时把 SUM(quantity) 冻结成
-- 期次的 winner_count。今天它恒为 1（后台表单里没有这个字段），将来要改成一期发多份，
-- 从它和 service.campaignParams 那一行入手即可。
--
-- 原型一个活动只有一个 prize 字符串，那落在这一张表的一行上——不为单奖品另设一条路。
-- 表格里没有排序号（只有一行时它没有意义），也没有奖品类型与券模板 ID（从落地起就只存
-- 不消费，没有兑付动作，也没有任何读写方读它做判断）。一个活动一行由唯一索引钉住，
-- repository.replacePrize 靠它把「换奖品」压成一次 UPDATE 或一次 INSERT。
CREATE TABLE lottery_campaign_prizes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id UUID NOT NULL REFERENCES lottery_campaigns(id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (char_length(trim(name)) > 0),
    -- 奖品封面图地址，必填；用在活动卡片上。订单 / 优惠券库里叫的也是这个名字，
    -- 同一个东西不该在抽奖库里叫另一个名。**比例待定**，定了之后要同步改后台表单的提示文案。
    cover_image TEXT NOT NULL DEFAULT '',
    claim_instructions TEXT NOT NULL DEFAULT '',
    quantity INTEGER NOT NULL CHECK (quantity > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- poster_image 是最后补上的一列，物理位置就在这里——别顺手往上挪（列顺序也是表形状
    -- 的一部分，挪了会让这个文件建出来的表与既有库对不上）。
    poster_image TEXT NOT NULL DEFAULT ''
);

-- ============================================================
-- 期次
-- ============================================================

-- 活动下面滚动开的一期一期。原型里 LAKE-202608-12 已经到第 12 期，所以不是一期一活动：
-- 开奖后同一事务里开下一期，一直到活动停用。
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
    drawn_at TIMESTAMPTZ,
    cancelled_at TIMESTAMPTZ,
    cancel_reason TEXT NOT NULL DEFAULT '',
    cancelled_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 期次没有单独的起点，created_at 既是开期时刻也是扫描的排序键，所以这两条不变式
    -- 的编号从 check1 起——check 那个名字属于一条随列一起没了的窗口约束。
    CONSTRAINT lottery_rounds_check1 CHECK ((status = 'drawn') = (drawn_at IS NOT NULL)),
    CONSTRAINT lottery_rounds_check2 CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL))
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
    -- 没有时间窗口，所以没有 'deadline'：这个取值随窗口一起去掉了，闭集只剩两个值。
    trigger TEXT NOT NULL CHECK (trigger IN ('threshold', 'manual')),
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

-- 与其它十个域里的同名两表逐列一致，各库自带一份，跨库不共享表。**不要顺手调整**：
-- migrations 的 TestMessageTablesStayInSyncAcrossSets 是按 CREATE TABLE 的列清单
-- （含顺序）比字符串的，注释可以不同，列不能。
--
-- outbox 对开奖不是可选件：lottery.round.drawn 与开奖记录在同一次事务里追加，
-- relay 才不可能漏投；后台的人工开奖与作废还要在同一个事务里追加一条
-- admin.operation.logged（platform/audit 的写法），由 relay 投到身份库的
-- admin_operation_logs——本库不建自己的审计表。
--
-- 这里是全新库，lease 列直接建在表里，所以不需要 identity / merchant 那两集末尾那种
-- ALTER TABLE ADD COLUMN IF NOT EXISTS。列名和顺序保持不变，各份拷贝才能逐列对得起来。

-- >>> message-tables:begin >>>

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

CREATE INDEX message_outbox_pending_idx
    ON message_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;

CREATE INDEX message_outbox_lease_idx
    ON message_outbox (lease_until)
    WHERE published_at IS NULL;

-- <<< message-tables:end <<<

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

-- 一个活动恰好一行奖品。这条唯一索引是「只留一个」的**执行者**，不只是约束：
-- repository.replacePrize 靠它把「换奖品」压成一次 UPDATE 或一次 INSERT。
CREATE UNIQUE INDEX lottery_campaign_prizes_campaign_unique
    ON lottery_campaign_prizes (campaign_id);

CREATE UNIQUE INDEX lottery_rounds_no_unique
    ON lottery_rounds (round_no);

CREATE UNIQUE INDEX lottery_rounds_seq_unique
    ON lottery_rounds (campaign_id, seq);

-- 一个活动同时只能有一期在跑。这条不变量是「期次连开」的地基：开下一期之前，
-- 上一期必须已经 drawn 或 cancelled。
CREATE UNIQUE INDEX lottery_rounds_one_live_per_campaign
    ON lottery_rounds (campaign_id) WHERE status IN ('open', 'closed');

-- 开奖 worker 的扫描索引：只有「已达门槛（closed）」一条路，排序按开期先后。
CREATE INDEX lottery_rounds_sweep_idx
    ON lottery_rounds (status, created_at);

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

-- ============================================================
-- 表与关键字段的中文注释
--
-- 放在同一个文件里而不是单开一份 _comments：合并之后每个集只有这一个文件，注释和它描述
-- 的列不会再出现「改了列没改注释」这种两个文件各说各话的情况。
-- ============================================================

-- 几列本轮**没有写入方**（领取、核销、换奖都延后了）。它们的注释里逐条写明了谁是将来
-- 写它的人——空列不会告诉后来的人任何事，注释会。这是账户域对
-- fortune_card_freezes.entry_keys 用过的同一套办法。

COMMENT ON TABLE lottery_activations IS '门店开通抽奖的记录，一个门店一行；开通是一个动作（有操作人和时间），不是从「有没有启用中的活动」推导出来的';
COMMENT ON COLUMN lottery_activations.location_id IS '门店 ID，属于商户服务，本库仅作跨库值引用，不建外键';
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
COMMENT ON COLUMN lottery_campaigns.participant_target IS '新期次的默认开奖门槛（原型 threshold）；**数的是参与次数**，开期时冻结到期次上，之后改活动不影响正在跑的那一期';
COMMENT ON COLUMN lottery_campaigns.status IS '活动状态：draft=草稿，enabled=进行中，paused=暂停，ended=已结束；参与只认 enabled 且期次 open';
COMMENT ON COLUMN lottery_campaigns.created_by IS '创建者的后台账号 ID';
COMMENT ON COLUMN lottery_campaigns.updated_by IS '最近一次修改者的后台账号 ID';

COMMENT ON TABLE lottery_campaign_prizes IS '活动的奖池，**一个活动恰好一行**；quantity 是每期的中奖名额（恒为 1），开期时冻结成期次的 winner_count';
COMMENT ON COLUMN lottery_campaign_prizes.campaign_id IS '所属活动，指向本库的 lottery_campaigns；唯一索引确保一个活动只有一行';
COMMENT ON COLUMN lottery_campaign_prizes.name IS '奖品名（如「10 元咖啡兑换券」）；开奖时快照进中奖记录，之后改这里不会改写历史';
COMMENT ON COLUMN lottery_campaign_prizes.cover_image IS '奖品封面图地址，必填；用在活动卡片上。**比例待定**，定了之后要同步改后台表单的提示文案';
COMMENT ON COLUMN lottery_campaign_prizes.claim_instructions IS '领取说明，展示在中奖详情页';
COMMENT ON COLUMN lottery_campaign_prizes.quantity IS '每期的中奖名额，当前恒为 1（后台表单里没有这个字段）；开期时冻结到期次的 winner_count，之后改这里不影响已开出的';
COMMENT ON COLUMN lottery_campaign_prizes.poster_image IS '奖品海报图地址，可留空；用在活动详情顶部的横幅。空串时前端回落到自带的那块占位';

COMMENT ON TABLE lottery_rounds IS '活动下面滚动开的一期一期（原型里的 roundNo）；收满门槛开奖，开奖后同一事务开下一期，一直滚下去';
COMMENT ON COLUMN lottery_rounds.campaign_id IS '所属活动，指向本库的 lottery_campaigns';
COMMENT ON COLUMN lottery_rounds.seq IS '期次序号，从 1 开始，同一活动内唯一';
COMMENT ON COLUMN lottery_rounds.round_no IS '期次号（code-seq，如 A1-202608-03），全局唯一，展示给用户';
COMMENT ON COLUMN lottery_rounds.status IS '期次状态：open=可参与，closed=已达门槛停止收人，drawn=已开奖，cancelled=已作废';
COMMENT ON COLUMN lottery_rounds.participant_target IS '开奖门槛，开期时从活动冻结；**数的是参与次数**（同一个人可以参与多次），达到它就转 closed';
COMMENT ON COLUMN lottery_rounds.participant_count IS '已达标的参与数（status=confirmed 的行数）；存下来是为了只读渲染与开奖扫描能走索引，代价是可能与参与记录漂移，由行锁和一条集成测试盯着';
COMMENT ON COLUMN lottery_rounds.winner_count IS '本期开奖抽几个人，开期时 = 奖池 quantity 之和';
COMMENT ON COLUMN lottery_rounds.drawn_at IS '开奖时间；非空当且仅当 status=drawn';
COMMENT ON COLUMN lottery_rounds.cancelled_at IS '作废时间；非空当且仅当 status=cancelled';
COMMENT ON COLUMN lottery_rounds.cancel_reason IS '作废原因';
COMMENT ON COLUMN lottery_rounds.cancelled_by IS '作废操作者的后台账号 ID';
COMMENT ON COLUMN lottery_rounds.created_at IS '开期时刻；期次没有单独的起点，扫描与排序都用它';

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
COMMENT ON COLUMN lottery_draws.trigger IS '触发条件：threshold=收满门槛，manual=人工；与 mode 一一对应';
COMMENT ON COLUMN lottery_draws.algorithm IS '抽签算法标识；目前只有 sha256-sort-v1，记下来是为了将来换算法时旧记录仍可复核';
COMMENT ON COLUMN lottery_draws.seed IS '随机种子；派生而非随机：sha256(round_id || 首个参与 id || 末个参与 id || 参与数 || trigger)。开奖前不可预测，开奖后任何人可据此把中奖名单完整重算一遍。注意这不是可证明公平——有库读权限的运维在那个窗口里能预测结果';
COMMENT ON COLUMN lottery_draws.participant_count IS '开奖那一刻的合格参与数（status=confirmed 的行数）';
COMMENT ON COLUMN lottery_draws.winner_count IS '实际抽出的人数 = min(奖池名额, 合格参与数)';
COMMENT ON COLUMN lottery_draws.drawn_by IS '人工开奖的操作者后台账号 ID；自动开奖恒为空';
COMMENT ON COLUMN lottery_draws.reason IS '人工开奖的原因，必填且非空白；自动开奖恒为空';

COMMENT ON TABLE lottery_wins IS '中奖记录，一次开奖一人一条；奖品名与活动名在这里是快照，之后改奖池不改写历史';
COMMENT ON COLUMN lottery_wins.draw_id IS '产生这条中奖记录的那次开奖，指向本库的 lottery_draws';
COMMENT ON COLUMN lottery_wins.round_id IS '所属期次，指向本库的 lottery_rounds';
COMMENT ON COLUMN lottery_wins.campaign_id IS '所属活动，指向本库的 lottery_campaigns';
COMMENT ON COLUMN lottery_wins.participation_id IS '中奖的那一条参与记录；一次开奖里同一条参与只能中一次';
COMMENT ON COLUMN lottery_wins.user_id IS '中奖用户 ID，属于身份服务，仅作跨库值引用';
COMMENT ON COLUMN lottery_wins.round_no IS '开奖当时的期次号快照';
COMMENT ON COLUMN lottery_wins.campaign_name IS '开奖当时的活动名快照';
COMMENT ON COLUMN lottery_wins.prize_id IS '奖品 ID，指向本库的 lottery_campaign_prizes；ON DELETE RESTRICT——改奖品走原地 UPDATE，不是删了重插';
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

COMMIT;
