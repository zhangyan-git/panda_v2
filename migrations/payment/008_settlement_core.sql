-- payment/008：分账与结算（渠道分账执行 + 分润规则 + 结算单）
--
-- 本库第一次装下「钱在各方之间怎么分」这件事。
--
-- 归属订正：001 文件头第 10 行写着「Settlement 拥有分账与结算（即便同仓部署，库与契约也各自
-- 独立，本库不建结算表）」——**那句已作废**。不单独建 settlement-service，渠道分账与结算单
-- 并入 payment-service（本服务的定位从「支付」扩成「资金域」：渠道执行 + 支付单 + 退款 +
-- 对账 + 分账 + 结算单）。001 已经应用过，按「已应用的迁移不改」的惯例不动它，以本文件为准。
--
-- 三条边界，落地时不要越线：
--   1. 权限码必须拆开：settlement:read / settlement:manage（打款单独一条 settlement:payout）。
--      **不能并进 payment:manage**——分润比例是运营随手会改的东西，渠道密钥是另一回事，
--      把两个权限码合成一个，等于给「改比例」的人同时开了改密钥的门。
--   2. 结算侧只存值引用 + **冻结快照**：比例、金额、主体名、子商户号都是「当时的那个值」。
--      规则改了、门店改名了、账户停用了，历史明细一个字都不能跟着变。所以下面每张流水表
--      都另存了一份快照列，看起来冗余，但那是账，不是缓存。
--   3. 依赖单向：本库不反向读订单/门店/券的库；结算单不进任何别的域的表。
--
-- 老系统六个集合的落法（只读对照，不改老系统）：
--   settle_rules       → settlement_rules        业务分类与范围档位逐字保留
--   settle_rule_items  → settlement_rule_items   每个接收方一行，算法与比例保留
--   settle_accounts    → settlement_accounts     渠道侧子商户号（V2 多一个 channel_id，见下）
--   settle_details     → settlement_receivers    「订单 × 接收方 × 金额」，形状一样
--   settle_logs        → **不建表**：它就是渠道调用日志，本库已有 payment_provider_calls
--   settle_roles       → **不建表**：纯标签，老系统自己的注释就写着「不参与计算」，
--                        改用 settlement_rule_items.party_type 的受控词表（下方逐个列出）
--
-- 老系统踩过、这里用约束钉住的六个坑：
--   1. 金额存 float64 —— 这里一律 BIGINT 分；比例 NUMERIC(20,6)（老系统也是 0~1 的比值）。
--   2. 六个集合**一个索引都没有**，幂等全靠应用「先查再插」——这里每条不变量都有唯一约束。
--   3. settle_details.order_id 没有唯一约束，同一笔订单重复分账拦不住。
--   4. status 列了 4 个值，实际只写过 0 和 2，另外两个从未赋值 —— 这里每个状态都写明**谁写它**。
--   5. 没有任何退款/取消的分账回退、解冻、冲正 —— 这里 settlement_reversals 是单独一层。
--   6. 没有结算单/结算批次/打款这一层 —— 这里 settlement_statements 是新的，老系统无从对照。
--
-- 分账基数就是**用户实付金额（券后）**，券成本不单独考虑（2026-09-18 定）。这意味着券的
-- 让利按分成比例在各方之间摊掉，而不是由发券方独担——
--
--     订单 100、券 30、门店分成 45%（无券时门店 45 / 平台 55）
--     实付 70 → 门店 70×45% = 31.5、平台差额 38.5
--     门店少拿 13.5、平台少拿 16.5，30 元按 45:55 摊掉了
--
-- 所以规则项上**不要**加「折扣承担方 / 券前券后基数」这类维度，也不要按发券方去分摊；
-- 这是有意不做的（真要做就得让一笔分账有两个基数，恒等式也要跟着改）。钱是平的，
-- 只是谁该出多少不按券的归属走——这台账认的就是这个口径。
--
-- 分账任务**在发起支付时就建好**，支付成功之后只剩推进状态（2026-09-18 定）。理由、
-- 那个「没付成」的出口（cancelled），以及它给扫描与结算带来的两处必须记住的过滤条件，
-- 都写在 settlement_tasks 与 settlement_statement_items 的头上。
--
-- **这里的「缺口 A」已经补上了**（2026-09-18，本文件写完之后）：当时支付库里没有门店/设备
-- 维度，而任务在发起支付那一刻建，那些列必须在那一刻就有值——契约不补，这条链路就断在
-- 「安静地建不出任务」上（没有报错，只是永远没有分账发生）。
--
-- 补法是 `CreatePaymentRequest` 追加三个字段（`store_id` / `device_id` / `biz_type`），
-- order-service 从订单与订单行推出来随发起支付一起送；payment-service 在建支付单的**同一个
-- 事务里**命中规则、算出各家金额，写出 settlement_tasks + settlement_receivers。三个维度的
-- 来路与 biz_type 的推法写在 service/settlement.go 的 buildSettlementPlan 与 order-service 的
-- pay.go 里。落点是下面 settlement_tasks 的 scope_* / store_ref 快照列，与本文件当初设想一致。
--
-- 补上之后**仍然命中不了两档**，这是有意的、写在代码注释里的：
--   brand   订单库没有品牌（brands/stores 在商户域），要命中得先从商户域经 gRPC 取
--           「门店 → 品牌」；
--   product 一笔支付可能含多个商品，而这里是「一笔支付一条任务」，没有单一商品可指。
-- 命中函数的签名收全五档（device → store → brand → product → global），今天的调用方只给得出
-- 两档。**写全但暂时命不中**，等值来源出现时不用改命中逻辑。
--
-- 落到哪一步了：**建任务这一刀已经通了**（任务与接收方在发起支付的事务里建成 pending，支付单
-- 进 failed / expired 时两边一起转 cancelled）。**还没做**的是任务建好之后那半段——向渠道发起
-- 分账（pending → submitted → succeeded）、冲正、以及 settlement_statements 那两张结算单表，
-- 一行代码都还没有。
--
-- 金额恒等式：settlement_tasks.base_amount = platform_amount + Σ settlement_receivers.amount。
-- 跨表，CHECK 表达不了（与 payments.amount 同一处境），由应用在同一事务里写、由对账核。
-- 平台金额用**差额倒挤**（老系统的算法，也是唯一能吸收固定额与舍入尾差的算法），保留。

-- ============================================================
-- 配置层：分润规则
-- ============================================================

-- 分账接收账户：一个主体在一个渠道上收钱用的那个号（微信服务商的 sub_mchid、银联商务的 mid）。
--
-- 老系统只有一列 sub_merchant_id，因为它的分账只走银联商务一套。V2 是多渠道的：同一个门店
-- 在微信和银联商务各有一个子商户号，所以这里必须有 channel_id——它不是「多加的一列」，
-- 而是老系统那套结构在第二个渠道上必然要长出来的东西。
--
-- merchant_id / brand_id / store_id 是别的域的事实，只做值引用，不建跨库外键。
CREATE TABLE settlement_accounts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- 主体账户编号，结算单上写它；渠道那边的号（下面是 receiver_id）是另一回事，
    -- 两边的号混在一列里是老系统 settle_details.settle_target_id 的老毛病。
    account_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(account_no)) > 0),
    -- 老库 settle_accounts._id（ObjectID 的 24 位 hex），存量迁过来时有值。
    legacy_id TEXT,
    -- 主体名称快照：账户改名不改历史结算单（历史那份在流水表里另有快照，这里是当前值）。
    party_name TEXT NOT NULL DEFAULT '',
    -- 主体是谁，词表与 settlement_rule_items.party_type 一致。
    party_type TEXT NOT NULL CHECK (party_type IN (
        'partner', 'city_center', 'agent', 'member_store', 'platform'
    )),
    -- 主体的值引用（merchant_id / brand_id / store_id），按 party_type 决定填哪个；
    -- 跨域的 ID 不进外键。
    merchant_ref TEXT NOT NULL DEFAULT '',
    brand_ref TEXT NOT NULL DEFAULT '',
    store_ref TEXT NOT NULL DEFAULT '',
    -- 走哪套渠道配置。分账是渠道能力，没有渠道就没有可用的接收方。
    channel_id UUID NOT NULL REFERENCES payment_channels(id) ON DELETE RESTRICT,
    -- 渠道侧的接收方类型。微信服务商分账要它（MERCHANT_ID / PERSONAL_OPENID）；
    -- 银联商务按子商户号直接分，取不到这个概念的默认 MERCHANT_ID。
    receiver_type TEXT NOT NULL DEFAULT 'MERCHANT_ID' CHECK (receiver_type IN (
        'MERCHANT_ID', 'PERSONAL_OPENID'
    )),
    -- 渠道侧的子商户号本体。老系统叫 sub_merchant_id，这里与 receiver_type 配对。
    receiver_id TEXT NOT NULL CHECK (char_length(trim(receiver_id)) > 0),
    receiver_name TEXT NOT NULL DEFAULT '',
    -- enabled = 可参与新分账；disabled = 停止使用。历史的用历史快照，与此处无关。
    status TEXT NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled', 'disabled')),
    remark TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 同一渠道下的同一个接收方号只能被一个账户占用；占用两次意味着同一笔钱有两个主。
CREATE UNIQUE INDEX settlement_accounts_receiver_uniq
    ON settlement_accounts (channel_id, receiver_type, receiver_id);

-- 一个主体（门店/品牌/代理商）在同一个渠道上只该有一个收款账户，否则结算单会劈成两张。
CREATE UNIQUE INDEX settlement_accounts_party_uniq
    ON settlement_accounts (party_type, merchant_ref, brand_ref, store_ref, channel_id)
    WHERE status = 'enabled';

-- 分润规则：哪个范围（门店/设备/品牌…）上的哪类业务，按什么算法分。
--
-- scope_ref 是范围的值引用（device id / store id / brand id），global 档为空——两者的关系用
-- CHECK 钉住，免得出现「global 却指着一个门店」这种看不清规则到底命中谁的行。
--
-- 命中顺序按范围精度从具体到宽泛：device → store → brand → product → global。老系统也是这个
-- 语义（先按设备找，找不到再往上兼容），只是它没有任何约束保证同档不撞车——两条同档规则同时
-- 启用时，命中哪条取决于查询的返回顺序。这里用部分唯一索引把那种库拦在配置期。
CREATE TABLE settlement_rules (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    legacy_id TEXT,
    name TEXT NOT NULL CHECK (char_length(trim(name)) > 0),
    -- 业务分类，照老系统 settle_type 的词表逐字保留（存量迁过来不用做映射）。
    biz_type TEXT NOT NULL CHECK (biz_type IN (
        'coffee', 'membership', 'store_consume', 'addon_product'
    )),
    -- 范围档位，照老系统 scope_type 的词表逐字保留。
    scope_type TEXT NOT NULL CHECK (scope_type IN (
        'device', 'store', 'brand', 'product', 'global'
    )),
    scope_ref TEXT NOT NULL DEFAULT '',
    -- normal              = 各接收方按比例/固定额各拿各的，剩下的归平台
    -- fixed_then_remaining= 先扣掉固定额，剩余部分再按比例分（老系统的两档，算法见下）
    allocation_mode TEXT NOT NULL DEFAULT 'normal' CHECK (allocation_mode IN (
        'normal', 'fixed_then_remaining'
    )),
    -- enabled = 参与命中；disabled = 保留配置但不再命中（历史任务仍指着它，所以不删行）。
    status TEXT NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled', 'disabled')),
    remark TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- global 与 scope_ref 必须同步为空/非空。
    CHECK ((scope_type = 'global') = (scope_ref = ''))
);

-- 同一业务分类 + 同一档位 + 同一范围，同时只能有一条启用中的规则。
CREATE UNIQUE INDEX settlement_rules_scope_uniq
    ON settlement_rules (biz_type, scope_type, scope_ref) WHERE status = 'enabled';

-- 规则项：一条规则里，钱分给谁、分多少。一条规则的所有项构成完整的一次分配。
--
-- 三种算法各自要求哪些字段有值，用 CHECK 说清楚。老系统的固定额项也带着 ratio，于是
-- 「到底哪个数说了算」只存在于算法代码里，看数据看不出来。
--
-- 第三种 remainder 是给平台项的：平台拿的是**差额**（基数 - 其他各项），既不是比例也不是
-- 固定额。老系统里这一项也挂在 percent/fixed 底下、填一个算不出来的比例，等于让读数据的人
-- 去代码里找答案——所以这里给它一个自己的算法名，「平台自留靠差额倒挤」这件事就从算法
-- 搬进了数据。
--
-- 一条规则**通常**应在平台项上收口；没有平台项也合法，那时各接收方之和必须正好等于基数
-- （没有留给人吸收尾差的余地）。「必须有且只有一条平台项」里的「至少一条」表达不了
-- （要触发器），这里只钉住「至多一条」。
CREATE TABLE settlement_rule_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_id UUID NOT NULL REFERENCES settlement_rules(id) ON DELETE CASCADE,
    -- 收款主体类型，词表与 settlement_accounts.party_type 一致（原来在这里的是老系统的
    -- settle_roles：那份角色表不参与计算，只是一张可维护的标签表，收成受控词表更省事）。
    party_type TEXT NOT NULL CHECK (party_type IN (
        'partner', 'city_center', 'agent', 'member_store', 'platform'
    )),
    -- percent   = 按基数乘 ratio（其他接收方）
    -- fixed     = 拿固定额 fixed_amount，单位分（其他接收方；fixed_then_remaining 模式下先扣它）
    -- remainder = 差额自留（只有平台项能用，见上方说明）
    calc_type TEXT NOT NULL CHECK (calc_type IN ('percent', 'fixed', 'remainder')),
    -- 分成比例，0~1 的比值（0.45 = 45%），与老系统口径一致。
    ratio NUMERIC(20,6) NOT NULL DEFAULT 0 CHECK (ratio >= 0 AND ratio <= 1),
    -- 固定分账额，单位为分。
    fixed_amount BIGINT NOT NULL DEFAULT 0 CHECK (fixed_amount >= 0),
    -- 收款账户。平台项为空：平台是**差额自留**，不向渠道发起分账，永远没有接收方明细。
    account_id UUID REFERENCES settlement_accounts(id) ON DELETE RESTRICT,
    -- 展示顺序（老系统 settle_rule_items.sort_order）。
    sort_order INTEGER NOT NULL DEFAULT 0,
    remark TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 三种算法各自要求哪些字段有值，三选一，不留中间态；算法与主体也绑死
    -- （平台项不可能是比例项，别的项也不可能是 remainder——那会把差额倒挤挪到某个门店头上，
    -- 门店拿「剩下的」既算不清也说不通）。
    CHECK (
        (calc_type = 'remainder' AND party_type = 'platform'
            AND ratio = 0 AND fixed_amount = 0)
        OR (calc_type IN ('percent', 'fixed') AND party_type <> 'platform'
            AND ((calc_type = 'percent' AND ratio > 0 AND fixed_amount = 0)
              OR (calc_type = 'fixed' AND ratio = 0 AND fixed_amount > 0)))
    ),
    -- 平台项没有账户，其余项必须有账户——分账要发给谁，渠道那边得有个号。
    CHECK ((party_type = 'platform') = (account_id IS NULL))
);

-- 平台项一条规则只能有一个：平台拿的是「剩下的」，两条平台项就没有「剩下」可言了。
CREATE UNIQUE INDEX settlement_rule_items_platform_uniq
    ON settlement_rule_items (rule_id) WHERE party_type = 'platform';

-- 同一个账户在同一条规则里只能出现一次（同一笔钱分两次给同一个号 = 配置错了）。
CREATE UNIQUE INDEX settlement_rule_items_account_uniq
    ON settlement_rule_items (rule_id, account_id) WHERE account_id IS NOT NULL;

-- ============================================================
-- 执行层：一笔支付的分账，与逐笔接收方
-- ============================================================

-- 分账任务：一笔支付的分账计划，**在发起支付时就建好**，支付成功之后只剩推进状态。
--
-- 为什么提前建（2026-09-18 定）：发起支付的请求里正好带着这笔钱的全部上下文——订单、门店、
-- 设备、实付金额都由 order 权威给出，那一刻命中规则、算好各家金额最省事；等支付成功了再回头
-- 凑这些维度，要么跨服务反查，要么补一次契约，反而更绕。规则也因此在**发起支付那一刻冻结**：
-- 运营之后改了比例，不影响已经建好的这一笔。
--
-- 代价是要给「没付成」一个出口：支付单进 failed / expired / closed 时，这条任务连同它的
-- 接收方一起置 cancelled（见状态说明）。作废只会发生在 pending 上——那时还没向渠道发起过，
-- 渠道侧没有任何东西要收回。
--
-- 一笔支付只有一条任务（payment_id 唯一）：接收方一次全给完（老系统就是整单一次分完）。
-- 渠道侧临时失败就重试**同一条**，不新建——重试次数记在 attempts 上。
-- 万一将来真要「先分一部分、之后再补分」，把 payment_id 上的唯一约束换成 (payment_id, seq)
-- 即可，那时 seq 才有意义；今天不给没有第二行的东西预留序号。
--
-- 规则命中时用的维度（scope_* / store_ref）在这里存**快照**：规则后来改了、门店后来换了，
-- 这张任务单上记的仍是当初命中的那条规则和那个范围。rule_id 因此是 ON DELETE RESTRICT——
-- 有任务指着的规则删不掉，只能停用。
CREATE TABLE settlement_tasks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(task_no)) > 0),
    legacy_id TEXT,
    -- 同库，走外键（本库的约定：库内的归属关系用 UUID 外键，跨库的才退化成单号值引用）；
    -- 单号另存一份，供对账与排查直接读，与 payment_refunds 存 payment_no 是同一个做法。
    payment_id UUID NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
    payment_no TEXT NOT NULL CHECK (char_length(trim(payment_no)) > 0),
    -- 分账基数，单位为分：实付金额（券后）。老系统也是拿 PayAmount 当基数。
    base_amount BIGINT NOT NULL CHECK (base_amount > 0),
    -- 平台自留 = base_amount - Σ 各接收方金额。差额倒挤，吸收固定额与舍入尾差；
    -- 整单归平台时它等于 base_amount（老系统的兜底）。
    platform_amount BIGINT NOT NULL CHECK (platform_amount >= 0),
    -- 命中的规则。**为空是常态**：门店没配规则时这条任务照样建（整单归平台），
    -- 老系统也是这个兜底。停用/改名都不回改历史；被指着的规则删不掉。
    rule_id UUID REFERENCES settlement_rules(id) ON DELETE RESTRICT,
    -- 命中时的范围与维度快照。store_ref / brand_ref 来自订单侧推过来的值引用——
    -- store_ref 今天有值（订单推得出来），brand_ref 恒为空串：订单库里没有品牌，
    -- 见文件头那两档「补上契约之后仍然命不中」的说明。建任务要用的正是它们
    -- （任务在发起支付时就建，见上方说明）。
    scope_type TEXT NOT NULL DEFAULT '' CHECK (scope_type IN (
        '', 'device', 'store', 'brand', 'product', 'global'
    )),
    scope_ref TEXT NOT NULL DEFAULT '',
    store_ref TEXT NOT NULL DEFAULT '',
    brand_ref TEXT NOT NULL DEFAULT '',
    merchant_ref TEXT NOT NULL DEFAULT '',
    -- pending   = 已建任务、还没向渠道发起。**发起支付时就写它**（那时这笔还等着付款）；
    --             支付成功之后才轮到 submitted。
    --             注意「等付款」和「等发起」在这里是同一个状态：任务提前建，但渠道侧的分账
    --             一律要等支付成功——钱先到账，才谈得上分。
    -- submitted = 请求已发出（或已返回），等渠道回执；**超时会停在这里**，靠查单收口，
    --             不能直接重发（渠道的 Provider 契约里那条不变量：钱可能已经分出去了）
    -- succeeded = 渠道确认分账成功（写它的人：回调或查单）
    -- failed    = 渠道明确拒绝（余额不足、接收方无效等），last_error 记原因
    -- returned  = 已全额回退（写它的人：settlement_reversals 全部回退完时；部分回退不改本列）
    -- cancelled = 这笔支付没成，任务作废（写它的人：支付单进 failed / expired / closed 时，
    --             连同接收方一起置）。**只能从 pending 进**：那时还没发往渠道，没有要收回的钱；
    --             已经发出去的任务没有作废这一说，只能走 settlement_reversals 回退。
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN (
        'pending', 'submitted', 'succeeded', 'failed', 'returned', 'cancelled'
    )),
    -- 渠道侧的分账单号（发起时我们给的那个）。
    provider_task_no TEXT NOT NULL DEFAULT '',
    -- 渠道侧的受理号/交易号。
    provider_transaction_id TEXT NOT NULL DEFAULT '',
    -- 渠道侧**完结**时间。微信分账要单独一步完结（动账 → 完结），一次下发的渠道（银联商务）
    -- 没有这一步，在 succeeded 时就一并置上——用一列而不是把一个状态劈成两半，
    -- 是因为「完结」是渠道相关的能力，不是本地状态机的必经台阶。
    finished_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 一笔支付一条任务（见上面的说明）。
CREATE UNIQUE INDEX settlement_tasks_payment_uniq ON settlement_tasks (payment_id);
-- 没做完的任务是重试与查单的输入。
--
-- ⚠️ 扫「待发起」时**必须连 payments 一起过滤**（只取 payments.status = 'succeeded' 的）：
-- 任务在发起支付时就建，pending 里混着大量还没付款的。不加这个条件，扫出来的东西会把一笔
-- **还没收到的钱**发去分账——渠道会拒（余额压根没到），但更坏的情况是它不拒。
-- 索引只用来缩小范围，那一句 join 才是对错所在。
CREATE INDEX settlement_tasks_unfinished_idx
    ON settlement_tasks (status, created_at) WHERE status IN ('pending', 'submitted');

-- 分账接收方明细：一条任务发给一个主体的一笔钱。这是「账」的最小单位。
--
-- 与任务一起**在发起支付时**写好（金额当场算得出来，见 settlement_tasks 的说明），
-- 之后只推进状态。
--
-- 主体名、子商户号、比例全部另存快照：settlement_accounts 那一行后来改了名或停用了，
-- 这里记的仍必须是**当初发给渠道的那个号**——不然退款回退时会对不上渠道。
--
-- 老系统 settle_details 的 settle_target_id 一列混装「账户 ID」和「子商户号」，查账时要靠
-- 上下文猜是哪一种。这里 account_id（值引用）与 receiver_id（渠道号）分开。
CREATE TABLE settlement_receivers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- 老库 settle_details._id，存量迁过来时有值。
    legacy_id TEXT,
    task_id UUID NOT NULL REFERENCES settlement_tasks(id) ON DELETE CASCADE,
    account_id UUID NOT NULL REFERENCES settlement_accounts(id) ON DELETE RESTRICT,
    party_type TEXT NOT NULL CHECK (party_type IN (
        'partner', 'city_center', 'agent', 'member_store', 'platform'
    )),
    -- 主体值引用与名称快照。
    merchant_ref TEXT NOT NULL DEFAULT '',
    brand_ref TEXT NOT NULL DEFAULT '',
    store_ref TEXT NOT NULL DEFAULT '',
    party_name TEXT NOT NULL DEFAULT '',
    -- 渠道侧接收方快照（当初实际发出去的那个号）。
    receiver_type TEXT NOT NULL DEFAULT 'MERCHANT_ID',
    receiver_id TEXT NOT NULL CHECK (char_length(trim(receiver_id)) > 0),
    -- 当初生效的比例/固定额快照，单位与规则项一致（比例 0~1；固定额记在 amount 上，
    -- 这里留 0）。规则改了不影响这一行。
    ratio NUMERIC(20,6) NOT NULL DEFAULT 0 CHECK (ratio >= 0 AND ratio <= 1),
    -- 实际分给这一方的金额，单位为分。老系统允许「不足 1 分不分账」，那种接收方**不建行**
    -- （没有金额的行会让「Σ 明细 = base - 平台」这条恒等式变得要打折），而不是建一行 0。
    amount BIGINT NOT NULL CHECK (amount > 0),
    -- 渠道侧的分账明细号（微信的 detail_id）。回退时要按它指回这一笔。
    provider_detail_no TEXT NOT NULL DEFAULT '',
    -- 已回退金额（回退成功时累加）。放在这一行上，是为了让「部分回退不能超过原额」这条
    -- 能写成行内 CHECK——放到 settlement_reversals 里就要跨表求和才判得出来。
    reversed_amount BIGINT NOT NULL DEFAULT 0 CHECK (reversed_amount >= 0),
    -- pending   = 随任务一起写入（还等着付款），之后等渠道回执
    -- succeeded = 渠道确认这一笔分出去了
    -- failed    = 这一笔被渠道拒了（整单可能仍部分成功）
    -- returned  = 已全额回退（reversed_amount = amount）
    -- cancelled = 这笔支付没成，跟着任务一起作废（写它的人：任务置 cancelled 时）
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN (
        'pending', 'succeeded', 'failed', 'returned', 'cancelled'
    )),
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 回退不能超过原额。
    CHECK (reversed_amount <= amount),
    -- returned 状态与金额必须一致，免得出现「标了全额回退、金额只回了一半」。
    CHECK ((status = 'returned') = (reversed_amount = amount))
);

-- 一条任务里同一个账户只有一行。
CREATE UNIQUE INDEX settlement_receivers_task_account_uniq
    ON settlement_receivers (task_id, account_id);
-- 结算要按主体+时间捞（结算单明细从这里来）。
CREATE INDEX settlement_receivers_party_idx
    ON settlement_receivers (account_id, created_at);

-- 分账回退：退款发生时，把已经分出去的那部分从接收方收回来。
--
-- **老系统完全没有这一层**：`panda_serve` 的分账是随支付请求一次性下发的，退款路径上没有任何
-- 分账回退、解冻、冲正，已分出去的钱在系统里就此与订单脱钩。V2 必须有——退款是常态。
--
-- 一次退款可以对应多条回退（一笔分账分给了三个主体，退款按各自的比例往回退）。
-- (refund_id, receiver_id) 唯一：同一笔退款对同一个明细只能回退一次。
-- 回退金额的上限（不得超过该明细未回退的余额）跨行，由应用保证，对账核。
CREATE TABLE settlement_reversals (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    reversal_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(reversal_no)) > 0),
    receiver_id UUID NOT NULL REFERENCES settlement_receivers(id) ON DELETE RESTRICT,
    -- 退款单，同库走外键（同 settlement_tasks.payment_id）；单号另存一份供排查。
    refund_id UUID NOT NULL REFERENCES payment_refunds(id) ON DELETE RESTRICT,
    refund_no TEXT NOT NULL CHECK (char_length(trim(refund_no)) > 0),
    -- 回退金额，单位为分，恒为正；方向由「这是回退」这件事本身表达。
    amount BIGINT NOT NULL CHECK (amount > 0),
    -- pending = 已建、待发起；submitted = 已发往渠道；succeeded = 渠道确认收回；
    -- failed  = 渠道拒绝（如原分账已结算完），last_error 记原因
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN (
        'pending', 'submitted', 'succeeded', 'failed'
    )),
    provider_reversal_no TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '',
    succeeded_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX settlement_reversals_refund_receiver_uniq
    ON settlement_reversals (refund_id, receiver_id);
CREATE INDEX settlement_reversals_unfinished_idx
    ON settlement_reversals (status, created_at) WHERE status IN ('pending', 'submitted');

-- ============================================================
-- 结算层：周期汇总与打款
-- ============================================================

-- 结算单：某主体在一段周期内应收/应付的钱，以及打款。
--
-- 这一层**老系统没有**（它只做到「渠道把每笔分账的结果写回 settle_details」，没有批次、
-- 没有打款、没有对账口径），所以下面这张表与它的明细表都是新设计的，没有对照物。
-- 方案 5.10 要求结算侧有「不可变的结算流水」，本文件的选择是：**不另设一本 ledger**——
-- settlement_receivers / settlement_reversals 本身就是不可变的流水（只追加、金额不改、
-- 状态单向推进），再记一本账等于两本账互相对；结算单只记「哪几条明细被算进了这张单」。
--
-- 金额恒等式写在行内（CHECK 可表达，照 orders_payable_matches 的先例）：
--   payable = opening + settled - reversed + adjustment
-- payable_amount 允许为负：本期回退大于本期收入时它是负数，含义是「下期冲抵」。
CREATE TABLE settlement_statements (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    statement_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(statement_no)) > 0),
    -- 结算给哪个账户（收款主体）。
    account_id UUID NOT NULL REFERENCES settlement_accounts(id) ON DELETE RESTRICT,
    -- 结算周期，左闭右开 [period_start, period_end)。跨期调整走 adjustment_amount，
    -- 不允许把一条明细同时算进两张单（见明细表的唯一索引）。
    period_start DATE NOT NULL,
    period_end DATE NOT NULL,
    -- 期初未结（上期结转），单位为分。
    opening_amount BIGINT NOT NULL DEFAULT 0,
    -- 本期分账收入合计（正数）。
    settled_amount BIGINT NOT NULL DEFAULT 0,
    -- 本期回退合计，以**正数**存，语义是减项。
    reversed_amount BIGINT NOT NULL DEFAULT 0 CHECK (reversed_amount >= 0),
    -- 人工调整（对账差异、抹零），可正可负；谁调的看 remark 与身份库的审计。
    adjustment_amount BIGINT NOT NULL DEFAULT 0,
    -- 本期应付。正数 = 该打给这个主体；负数 = 下期冲抵。
    payable_amount BIGINT NOT NULL,
    -- draft     = 系统生成、还没确认（此状态下明细可增删）
    -- confirmed = 财务确认、金额冻结（此后只读，改动走新的调整单）
    -- paid      = 已打款（paid_at / paid_by / payout_* 在此刻写）
    -- void      = 作废（明细保留，不进任何统计）
    status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN (
        'draft', 'confirmed', 'paid', 'void'
    )),
    -- 打款方式与凭证：offline = 线下转账（凭据号在 payout_ref），channel = 走渠道打款，
    -- 空 = 还没打。**不把打款做成支付单**：打款是结算自己的事，混进 payments 会让
    -- 「支付单 = 用户付给平台的钱」这条口径不成立。
    payout_method TEXT NOT NULL DEFAULT '' CHECK (payout_method IN ('', 'offline', 'channel')),
    payout_ref TEXT NOT NULL DEFAULT '',
    payout_amount BIGINT NOT NULL DEFAULT 0,
    paid_at TIMESTAMPTZ,
    -- 操作者（身份库的用户 ID，值引用）。
    paid_by TEXT NOT NULL DEFAULT '',
    remark TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (period_end > period_start),
    CHECK (payable_amount = opening_amount + settled_amount - reversed_amount + adjustment_amount),
    -- 打了款才有方式与时间，没打款就不该有。
    CHECK ((status = 'paid') = (paid_at IS NOT NULL))
);

-- 同一账户的同一周期只能有一张有效单（作废的重开不受限）。
CREATE UNIQUE INDEX settlement_statements_period_uniq
    ON settlement_statements (account_id, period_start, period_end) WHERE status <> 'void';

-- 结算单明细：这张单里算了哪些分账明细、哪些回退。
--
-- 「一条分账明细只能被结算一次」是这张表存在的全部理由——老系统没有这层，同一笔钱重复
-- 打款没有任何东西拦得住。两个部分唯一索引就是那道闸。
-- 金额带符号：分账收入为正，回退为负，与单头的等式相加方向一致。
--
-- 只有 succeeded 的接收方（与 succeeded 的回退）才进这张表：failed 与 cancelled 的一律不进。
-- 任务提前建的代价就是这里——库里躺着注定不会成功的明细，归集时漏一句过滤，结算单上就会
-- 多出一笔没付成的钱。
CREATE TABLE settlement_statement_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    statement_id UUID NOT NULL REFERENCES settlement_statements(id) ON DELETE RESTRICT,
    -- 来源二选一：一条分账明细，或一条回退。
    receiver_id UUID REFERENCES settlement_receivers(id) ON DELETE RESTRICT,
    reversal_id UUID REFERENCES settlement_reversals(id) ON DELETE RESTRICT,
    -- 带符号金额，单位为分。
    amount BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 恰好一个来源。
    CHECK ((receiver_id IS NULL) <> (reversal_id IS NULL)),
    -- 来源与符号必须匹配：分账为正、回退为负。
    CHECK ((receiver_id IS NOT NULL AND amount > 0)
        OR (reversal_id IS NOT NULL AND amount < 0))
);

CREATE UNIQUE INDEX settlement_statement_items_receiver_uniq
    ON settlement_statement_items (receiver_id) WHERE receiver_id IS NOT NULL;
CREATE UNIQUE INDEX settlement_statement_items_reversal_uniq
    ON settlement_statement_items (reversal_id) WHERE reversal_id IS NOT NULL;

-- ============================================================
-- 渠道调用日志：补分账动作的词表
-- ============================================================

-- 分账的每一次渠道调用都要落 payment_provider_calls（老系统那套 settle_logs 在 V2 就是它，
-- 所以不另建表）。**不新增列**：分账调用用 payment_no 关联任务，回退调用用 refund_no 关联
-- 退款单，已有的两列够用。
--
-- 但 operation 的词表里没有分账动作，得补上。照 payment/004 改词表的先例重发约束。
-- 微信分账是四步（发起动账 / 查动账 / 完结 / 回退），银联商务是随支付下发一步到底——
-- 词表按能力并集列，某套渠道用不到哪几个，看它的实现就知道。
ALTER TABLE payment_provider_calls
    DROP CONSTRAINT payment_provider_calls_operation_check,
    ADD CONSTRAINT payment_provider_calls_operation_check CHECK (operation IN (
        'create', 'query', 'close', 'refund', 'query_refund',
        'agreement_sign', 'agreement_charge', 'agreement_terminate', 'reconcile',
        'division', 'query_division', 'finish_division', 'reverse_division'
    ));

COMMENT ON COLUMN payment_provider_calls.operation IS '渠道动作：create/query/close=支付单，refund/query_refund=退款，agreement_*=委托代扣，reconcile=对账，division/query_division/finish_division/reverse_division=分账（发起/查/完结/回退）';
