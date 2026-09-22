-- payment/001：支付领域核心表结构（支付单、出资、退款、回调、流水）
--
-- 本迁移属于 panda_payment。订单、用户、账户（咖啡豆/福卡）、优惠券、会员、履约、结算等
-- 外部服务的 ID 与单号只作为值引用保存；本库不得创建跨数据库外键。
--
-- 边界（方案 5.9 / 5.10 / 5.11）：
--   Payment   拥有支付单、渠道配置、支付回调、退款单、委托代扣、支付与退款流水、渠道对账、支付幂等
--   Order     拥有订单与订单状态（payment 只发布结果事件，不写订单表）
--   Account   拥有咖啡豆/福卡余额与账变（payment 的账户出资只记一个 account_entry_id 值引用）
--   Settlement 不单独建服务：分账、分润规则与结算单并入 payment-service（资金域），
--             表见 008_settlement_core.sql。这句话在 2026-09-18 之前写的是「本库不建结算表」，
--             已作废——方案里 settlement 独立的那套口径改了，改的是归属不是能力。
--
-- 两条被别的服务固定、必须对齐的约定：
--
--   1. 出资词表与 order 库的 order_payment_lines **逐字一致**：line_type ∈ wechat | unionpay |
--      coffee_bean | wallet | other，status ∈ reserved | succeeded | failed |
--      released | reversed。order-service 消费 payment.succeeded / payment.failed 时按同一套值落
--      它那份出资分摊（backend/services/order-service/internal/dto/payment.go），两边不做翻译。
--      改这里的词表等于改跨服务契约，必须两边同时改。（福卡曾经在这个词表里，是凭空写进来的：
--      规划里没有把它列成出资渠道，见 payment/004。）
--   2. payment_methods.id 被 coffee_machine 的 device_payment_methods.payment_method_id 按值引用
--      （「支付方式 ID，属于支付服务」）。这张表的 id 因此是跨服务的稳定标识，不能重构掉。
--
-- 敏感数据不落库（方案 18.3 / 11.5）：渠道密钥、API Secret、签名原文一律不写进本库的列，
-- payment_channels 只存 secret_ref（环境变量名或挂载文件路径），由部署侧注入；payment_provider_calls
-- 存的是脱敏摘要。这与 manufacturer_credentials 直接存密钥列的先例**有意不一致**——那张表先于
-- 18.3 存在，本库是新建的，按 18.3 建。
--
-- 金额单位一律是「分」（BIGINT）。本文件不带 goose 的 Down 段：platform/database/migrate 把整个
-- 文件丢给一次 Exec。

-- ============================================================
-- 渠道与支付方式
-- ============================================================

-- 一个渠道 = 一套对接配置。微信支付、银联商务、丰选万联、友联各一行；同一渠道的沙箱与生产是两行
-- （mode 不同），因为商户号与密钥本来就不同。
--
-- provider 是适配器键，**故意不加 CHECK**：接一个新渠道应该是插一行配置，不是开一次迁移改约束
-- ——这与 status 那种封闭状态机不同。
--
-- 取值是**协议族**，不是渠道名：manual / form_md5 / hmac_body / ums / wechat_v3。丰选万联、优联、
-- 首创饭卡、北方工业饭卡四家的收单协议是同一套骨架，差别只在几个 config 项，在库里是四行、在
-- 代码里是同一个 form_md5。所以「加一家渠道」分两种：族内加 = 插一行数据；族外加 = 连适配器一起
-- 加。写 provider 值时按这条判断，别照着渠道名填。
--
-- status 里的 legacy_readonly 是方案 6 最后一段要的：渠道停止新交易之后仍要能退款、能对账，
-- 所以不能删行，只能把它标成只读。老库 payment_methods.config 里的 sign_key 这类字段在迁移时
-- 不搬进 config，改成 secret_ref。
CREATE TABLE payment_channels (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- 老库 payment_methods._id（MongoDB ObjectID 的 24 位 hex），仅迁移过来的行有值。
    legacy_id TEXT,
    -- 渠道代码，如 wechat_miniapp、unionpay、fengxuan_wanlian、youlian。
    code TEXT NOT NULL UNIQUE CHECK (char_length(trim(code)) > 0),
    name TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL CHECK (char_length(trim(provider)) > 0),
    mode TEXT NOT NULL DEFAULT 'sandbox' CHECK (mode IN ('sandbox', 'live')),
    status TEXT NOT NULL DEFAULT 'disabled' CHECK (status IN ('enabled', 'disabled', 'legacy_readonly')),
    -- 非敏感对接参数：商户号、appid、回调地址、证书序列号等。
    config JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- 密钥在受控 Secret 管理系统里的键名（或环境变量名），不是密钥本身。
    secret_ref TEXT NOT NULL DEFAULT '',
    remark TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 前台/设备上「用户可以选哪几种支付方式」。老库把它和渠道配置混在一张 payment_methods 里
-- （code/name/icon/config/scope 同一行），V2 拆开：渠道是「怎么对接」，支付方式是「给用户看什么、
-- 怎么起支付」。一个渠道可以对应多个支付方式（同一个微信商户号下的小程序支付与扫码支付，action
-- 不同），一个支付方式也可能没有外部渠道（咖啡豆这类账户出资由 account-service 扣）。
--
-- 「哪台设备能用哪几种方式」不在本表：那是 coffee_machine 库的 device_payment_methods，按设备显式
-- 配置（可只配一条，也可配几条）。老库设备文档里的 payment_method_ids 数组迁到那边去。
--
-- action 是「这条方式被选中之后怎么起支付」，客户端只认它、不认 code：
--   jump_miniapp = 跳对方小程序（丰选万联、优联、首创饭卡这类）
--   native_pay   = 小程序内 requestPayment（微信、银联）
--   direct_pay   = 对接方直接扣款、不跳转（北方工业饭卡）
--   qrcode       = 扫普通二维码
--   h5           = 跳 H5 收银台
--   account      = 走本仓库的账户服务扣余额（咖啡豆）
-- 新接一家只要它的 action 属于上面已有形态，插一行数据 + 填 params 就能用，客户端一行不改；
-- 出现全新形态才需要加一个取值——那种情况本来就要改客户端，所以这里的 CHECK 不构成额外负担。
CREATE TABLE payment_methods (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    legacy_id TEXT,
    -- 支付方式代码，老库风格的点数/饭卡类为 points_xxx、meal_card_xxx。
    -- 给人看的标识（后台配置、迁移对账、日志排查），**不用来做行为分派**——分派认下面的 action。
    code TEXT NOT NULL UNIQUE CHECK (char_length(trim(code)) > 0),
    name TEXT NOT NULL CHECK (char_length(trim(name)) > 0),
    description TEXT NOT NULL DEFAULT '',
    icon TEXT NOT NULL DEFAULT '',
    -- 外部渠道；账户出资方式（咖啡豆）为空。
    channel_id UUID REFERENCES payment_channels(id) ON DELETE RESTRICT,
    -- 选中这条方式之后怎么起支付，取值见文件头；客户端只 switch 它，不 switch code。
    action TEXT NOT NULL CHECK (action IN (
        'jump_miniapp', 'native_pay', 'direct_pay', 'qrcode', 'h5', 'account'
    )),
    -- 方式级启动参数（跳转 path、附加 query、查单与退款策略等），**不放密钥**；
    -- 渠道级参数（appid、商户号、回调地址）在 payment_channels.config，返回客户端时两层合并。
    params JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- 本方式对应的出资类型，词表与 order 库的 order_payment_lines.line_type 一致（见文件头）。
    funding_type TEXT NOT NULL CHECK (funding_type IN (
        'wechat', 'unionpay', 'coffee_bean', 'fortune_card', 'wallet', 'other'
    )),
    status TEXT NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled', 'disabled')),
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ============================================================
-- 支付单与出资
-- ============================================================

-- 一次「向用户收这笔钱」的尝试。一个订单可以有多个支付单：第一次支付失败、超时被关，用户可以再发起
-- 一次，那是新的一行；但**成功只能有一张**（见下面的部分唯一索引），因为订单只能被收一次钱。
CREATE TABLE payments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    payment_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(payment_no)) > 0),
    legacy_id TEXT,
    -- 订单号（orders.order_no），值引用：支付手里只有下单时拿到的那张单，
    -- 两边各自维护对方的内部 ID 只会多一份对不上的可能。
    order_no TEXT NOT NULL CHECK (char_length(trim(order_no)) > 0),
    user_id UUID NOT NULL,
    -- 应付总额，单位为分；等于下面各笔出资之和（由应用在同一事务里写，跨表没法用 CHECK 表达）。
    amount BIGINT NOT NULL CHECK (amount > 0),
    -- 主出资类型：支付结果事件里的 paymentMethod 取它，落到 orders.payment_method 上。
    funding_type TEXT NOT NULL CHECK (funding_type IN (
        'wechat', 'unionpay', 'coffee_bean', 'fortune_card', 'wallet', 'other'
    )),
    -- 走哪套渠道配置；账户出资（纯咖啡豆）为空。
    channel_id UUID REFERENCES payment_channels(id) ON DELETE RESTRICT,
    -- 用户是在哪一档支付方式下发起的（展示与统计用）；渠道侧的支付可以没有。
    payment_method_id UUID REFERENCES payment_methods(id) ON DELETE RESTRICT,
    -- created  = 已建单、还没向渠道发起
    -- pending  = 已向渠道发起（或已返回支付参数），等回调或轮询
    -- succeeded/failed = 终态
    -- closed   = 订单取消等主动关单；expired = 待支付超时
    status TEXT NOT NULL DEFAULT 'created' CHECK (status IN (
        'created', 'pending', 'succeeded', 'failed', 'closed', 'expired'
    )),
    -- 渠道收银台/账单上显示的商品描述。
    subject TEXT NOT NULL DEFAULT '',
    -- 渠道附加数据（小程序 openid、设备号等）。**不放密钥**，见文件头。
    attach JSONB NOT NULL DEFAULT '{}'::jsonb,
    provider_transaction_id TEXT NOT NULL DEFAULT '',
    failure_code TEXT NOT NULL DEFAULT '',
    failure_message TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ,
    paid_at TIMESTAMPTZ,
    closed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 一个支付单下的逐笔出资：微信/银联是渠道出资，咖啡豆是账户余额出资。
-- 混合出资必须逐笔留行：退款要按来源分别冲正，对账要按渠道核对，只在支付单上存一个支付方式就丢了依据。
--
-- 这张表与 order 库的 order_payment_lines 是**同一事实的两个视角**（资金侧 / 订单侧），不是冗余：
-- 支付侧是「钱从哪儿来的」，订单侧是「这一单谁出的钱」，靠 payment.succeeded 事件对齐。
-- 两边都不许直接改对方的那份——发现不一致正是对账要报出来的差异。
CREATE TABLE payment_fundings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    payment_id UUID NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
    line_no INTEGER NOT NULL CHECK (line_no > 0),
    line_type TEXT NOT NULL CHECK (line_type IN (
        'wechat', 'unionpay', 'coffee_bean', 'fortune_card', 'wallet', 'other'
    )),
    amount BIGINT NOT NULL CHECK (amount > 0),
    -- 与 order_payment_lines.status 同词表：reserved=已预占（账户余额先冻结待扣），succeeded=已成功，
    -- failed=已失败，released=已释放（预占解开、不再扣），reversed=已冲正。
    status TEXT NOT NULL DEFAULT 'reserved' CHECK (status IN (
        'reserved', 'succeeded', 'failed', 'released', 'reversed'
    )),
    provider_transaction_id TEXT NOT NULL DEFAULT '',
    failure_code TEXT NOT NULL DEFAULT '',
    -- account-service 的账变 ID：账户出资扣的是余额，退款要按这笔账变冲正。
    account_entry_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    succeeded_at TIMESTAMPTZ,
    reversed_at TIMESTAMPTZ,
    UNIQUE (payment_id, line_no)
);

-- ============================================================
-- 退款
-- ============================================================

-- 一张退款单 = 把某一笔支付里的钱按售后结论退回去。售后申请本身归 order-service
-- （order_after_sales），本库只认 after_sale_no 这个值引用与它给出的金额、行。
--
-- 不复制 order_after_sales.scope（整单退/按行退）：范围是订单侧的权威事实，支付这边照它的结论执行，
-- 存一份副本只会多一份对不上的可能；需要时按 order_line_id 值引用回查。
CREATE TABLE payment_refunds (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    refund_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(refund_no)) > 0),
    -- 老库 refund_orders._id（ObjectID 的 24 位 hex），存量的退款单迁进来时有值。
    legacy_id TEXT,
    payment_id UUID NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
    payment_no TEXT NOT NULL CHECK (char_length(trim(payment_no)) > 0),
    order_no TEXT NOT NULL CHECK (char_length(trim(order_no)) > 0),
    -- order-service 的售后单号，值引用；一张售后单只能落一张退款单，所以它整表唯一。
    after_sale_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(after_sale_no)) > 0),
    -- 退的是哪一行（order_lines.id），值引用；整单退为空。金额与券的返还都落在这一行上。
    order_line_id UUID,
    user_id UUID NOT NULL,
    amount BIGINT NOT NULL CHECK (amount > 0),
    reason TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN (
        'pending', 'processing', 'succeeded', 'failed', 'cancelled'
    )),
    provider_refund_id TEXT NOT NULL DEFAULT '',
    failure_code TEXT NOT NULL DEFAULT '',
    failure_message TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    succeeded_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 退款也要逐笔：原支付是「微信 + 咖啡豆」混合出资时，钱要分别从渠道退回、从账户余额冲正，
-- 渠道退款单号与账户账变 ID 各自独立，合成一笔就再也说不清哪一半退成了。
CREATE TABLE payment_refund_fundings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    refund_id UUID NOT NULL REFERENCES payment_refunds(id) ON DELETE RESTRICT,
    -- 冲正的是哪一笔出资；存量迁移过来的退款可能找不到对应出资行，所以可空。
    funding_id UUID REFERENCES payment_fundings(id) ON DELETE RESTRICT,
    line_no INTEGER NOT NULL CHECK (line_no > 0),
    line_type TEXT NOT NULL CHECK (line_type IN (
        'wechat', 'unionpay', 'coffee_bean', 'fortune_card', 'wallet', 'other'
    )),
    amount BIGINT NOT NULL CHECK (amount > 0),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'succeeded', 'failed')),
    provider_refund_id TEXT NOT NULL DEFAULT '',
    failure_code TEXT NOT NULL DEFAULT '',
    -- 账户出资冲正产生的反向账变 ID，属于账户服务时仅作跨库值引用。
    account_entry_id UUID,
    succeeded_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (refund_id, line_no)
);

-- ============================================================
-- 渠道回调与调用流水
-- ============================================================

-- 渠道回来的每一条通知都先落在这里，再由处理器改支付/退款状态——回调本身是证据，不能只留在日志里。
--
-- 防重放靠 (provider, notification_id) 唯一：渠道重投、网络重放、我们自己点了两次，都会撞在这条上。
-- 验签失败的也落库（status='failed'）以便排查，但**绝不因此改支付状态**——这正是伪造回调想要的效果。
--
-- body 是渠道原始报文，只落在这一张表里：方案 11.5 禁止「未脱敏的第三方完整回调报文」进日志，
-- 所以日志侧只允许记 body_sha256 与摘要。
CREATE TABLE payment_notifications (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider TEXT NOT NULL CHECK (char_length(trim(provider)) > 0),
    channel_id UUID REFERENCES payment_channels(id) ON DELETE RESTRICT,
    -- 渠道侧的通知/流水唯一号（微信 resource.id、银联请求流水号、丰选通知序号）。
    notification_id TEXT NOT NULL CHECK (char_length(trim(notification_id)) > 0),
    event_type TEXT NOT NULL DEFAULT '',
    payment_no TEXT NOT NULL DEFAULT '',
    refund_no TEXT NOT NULL DEFAULT '',
    body BYTEA NOT NULL,
    body_sha256 TEXT NOT NULL CHECK (char_length(trim(body_sha256)) > 0),
    -- 只留定位用的请求头（请求号、时间戳、签名算法名）；签名原文与密钥不在这里。
    headers JSONB NOT NULL DEFAULT '{}'::jsonb,
    signature_verified BOOLEAN NOT NULL DEFAULT FALSE,
    status TEXT NOT NULL DEFAULT 'received' CHECK (status IN (
        'received', 'processed', 'ignored', 'failed'
    )),
    failure_reason TEXT NOT NULL DEFAULT '',
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ,
    UNIQUE (provider, notification_id)
);

-- 我们主动打给渠道的每一次调用（下单、查单、关单、退款、代扣、对账拉单），方案 6 要求所有第三方
-- 适配器都有调用流水。超时与「结果未知」必须留痕：一笔状态不明的转账是运维要拿它去渠道查的线索。
--
-- request_summary / response_summary 是**脱敏后**的摘要：不放签名原文、密钥、完整卡号与身份证
-- （方案 11.5），只放能定位这一笔的键（商户单号、渠道单号、金额、返回码）。
CREATE TABLE payment_provider_calls (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    channel_id UUID REFERENCES payment_channels(id) ON DELETE RESTRICT,
    provider TEXT NOT NULL DEFAULT '',
    operation TEXT NOT NULL CHECK (operation IN (
        'create', 'query', 'close', 'refund', 'query_refund',
        'agreement_sign', 'agreement_charge', 'agreement_terminate', 'reconcile'
    )),
    payment_no TEXT NOT NULL DEFAULT '',
    refund_no TEXT NOT NULL DEFAULT '',
    agreement_no TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    trace_id TEXT NOT NULL DEFAULT '',
    attempt_no INTEGER NOT NULL DEFAULT 1 CHECK (attempt_no > 0),
    request_summary JSONB NOT NULL DEFAULT '{}'::jsonb,
    response_summary JSONB NOT NULL DEFAULT '{}'::jsonb,
    http_status INTEGER,
    provider_code TEXT NOT NULL DEFAULT '',
    provider_message TEXT NOT NULL DEFAULT '',
    result TEXT NOT NULL DEFAULT 'unknown' CHECK (result IN ('success', 'failed', 'timeout', 'unknown')),
    duration_ms INTEGER,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ============================================================
-- 不可变资金流水
-- ============================================================

-- 支付与退款流水（方案 5.9）。操作表（payments / payment_refunds）记的是当前状态，会一直被改写；
-- 这一张只追加，记的是「什么时候真的进出过一笔钱」，对账（5.9、14.3）以它为基准。
--
-- 冲正不删不改原记录，新增一条反向记录（方案 8.2）——kind='reversal'，direction 与原记录相反，
-- 于是「这条流水现在还剩多少」永远是整表求和，而不是某一行被改成了什么。
--
-- channel_id 在这里**不建外键**：流水是对账基准，渠道配置行将来怎样都不该让已发生的流水对不上。
CREATE TABLE payment_transactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    legacy_id TEXT,
    -- payment=一笔出资成功入账，refund=一笔出资被退回，reversal=冲正（对账差异、人工纠正）
    kind TEXT NOT NULL CHECK (kind IN ('payment', 'refund', 'reversal')),
    -- 三个定位键都写成空串而不是 NULL：NULL 在唯一索引里互不相等，空串才能真的挡住重复记账。
    payment_no TEXT NOT NULL DEFAULT '',
    refund_no TEXT NOT NULL DEFAULT '',
    -- 出资序号，对应 payment_fundings.line_no 或 payment_refund_fundings.line_no；没有具体行时为 0。
    funding_line_no INTEGER NOT NULL DEFAULT 0 CHECK (funding_line_no >= 0),
    line_type TEXT NOT NULL CHECK (line_type IN (
        'wechat', 'unionpay', 'coffee_bean', 'fortune_card', 'wallet', 'other'
    )),
    direction TEXT NOT NULL CHECK (direction IN ('in', 'out')),
    amount BIGINT NOT NULL CHECK (amount > 0),
    channel_id UUID,
    provider_transaction_id TEXT NOT NULL DEFAULT '',
    account_entry_id UUID,
    -- 渠道给的成交时间，不是我们记账的时间。
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (kind, payment_no, refund_no, funding_line_no)
);

-- ============================================================
-- 状态流水
-- ============================================================

-- 与 order_state_transitions 逐列同形：支付单、出资、退款、签约、扣款、对账都跨多个请求与回调，
-- 只看当前状态查不出「怎么走到这一步的」。这张表只追加，不改不删。
CREATE TABLE payment_state_transitions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type TEXT NOT NULL CHECK (aggregate_type IN (
        'payment', 'funding', 'refund', 'agreement', 'charge', 'reconciliation'
    )),
    aggregate_id UUID NOT NULL,
    from_status TEXT NOT NULL DEFAULT '',
    to_status TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    -- admin 的人工退款/关单同时会在身份库 admin_operation_logs 留一条后台操作审计，
    -- 这里记的是支付自己的口径。
    actor_type TEXT NOT NULL DEFAULT 'system' CHECK (actor_type IN ('user', 'merchant', 'admin', 'system')),
    actor_id UUID,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE FUNCTION prevent_payment_transaction_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'payment transactions are append-only';
END;
$$;

CREATE TRIGGER payment_transactions_append_only
    BEFORE UPDATE OR DELETE ON payment_transactions
    FOR EACH ROW EXECUTE FUNCTION prevent_payment_transaction_change();

CREATE FUNCTION prevent_payment_transition_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'payment state transitions are append-only';
END;
$$;

CREATE TRIGGER payment_state_transitions_append_only
    BEFORE UPDATE OR DELETE ON payment_state_transitions
    FOR EACH ROW EXECUTE FUNCTION prevent_payment_transition_change();

-- ============================================================
-- 支付幂等
-- ============================================================

-- 与 order_idempotency_keys、coupon_idempotency_keys 逐列同形：同一套幂等语义在仓库里只有一种表结构。
-- 发起支付、发起退款、代扣扣款都要用它：回调可能重投、客户端可能重试，扣两次钱不行。
CREATE TABLE payment_idempotency_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope TEXT NOT NULL CHECK (char_length(trim(scope)) > 0),
    idempotency_key TEXT NOT NULL CHECK (char_length(trim(idempotency_key)) > 0),
    request_hash TEXT NOT NULL CHECK (char_length(trim(request_hash)) > 0),
    resource_type TEXT NOT NULL,
    resource_id UUID,
    response JSONB NOT NULL DEFAULT '{}'::jsonb,
    status TEXT NOT NULL CHECK (status IN ('processing', 'succeeded', 'failed')),
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (scope, idempotency_key)
);

-- ============================================================
-- 索引
-- ============================================================

-- 历史映射：只有迁移过来的行才有 legacy_id。
CREATE UNIQUE INDEX payment_channels_legacy_id_key ON payment_channels (legacy_id) WHERE legacy_id IS NOT NULL;
CREATE UNIQUE INDEX payment_methods_legacy_id_key ON payment_methods (legacy_id) WHERE legacy_id IS NOT NULL;
CREATE UNIQUE INDEX payments_legacy_id_key ON payments (legacy_id) WHERE legacy_id IS NOT NULL;
CREATE UNIQUE INDEX payment_refunds_legacy_id_key ON payment_refunds (legacy_id) WHERE legacy_id IS NOT NULL;
CREATE UNIQUE INDEX payment_transactions_legacy_id_key ON payment_transactions (legacy_id) WHERE legacy_id IS NOT NULL;

-- 一个订单只能有一张成功的支付单：重试可以开新单，但成功只能是那一次。
-- 与 order_payment_lines_one_live_success 同一个思路，只是作用在支付单上。
CREATE UNIQUE INDEX payments_one_succeeded_per_order
    ON payments (order_no) WHERE status = 'succeeded';

-- 同一支付单、同一出资类型只允许有一条成功线（重复回调不能变成扣两次钱）。
CREATE UNIQUE INDEX payment_fundings_one_live_success
    ON payment_fundings (payment_id, line_type) WHERE status = 'succeeded';

-- 方案 8.2：外部单号必须唯一。空串表示「还没有」，不是「另一个空单号」，所以空串排除在唯一性之外。
CREATE UNIQUE INDEX payments_request_id_key ON payments (request_id) WHERE request_id <> '';
CREATE UNIQUE INDEX payment_refunds_request_id_key ON payment_refunds (request_id) WHERE request_id <> '';
CREATE UNIQUE INDEX payments_provider_transaction_id_key
    ON payments (provider_transaction_id) WHERE provider_transaction_id <> '';
CREATE UNIQUE INDEX payment_refunds_provider_refund_id_key
    ON payment_refunds (provider_refund_id) WHERE provider_refund_id <> '';

-- 订单/用户视角的查询：后台列表、客服按单号查、用户看自己的支付记录。
CREATE INDEX payments_order_idx ON payments (order_no, created_at DESC);
CREATE INDEX payments_user_idx ON payments (user_id, created_at DESC);
-- 关单扫描：待支付且已过期的支付单。
CREATE INDEX payments_pending_expiry_idx ON payments (status, expires_at)
    WHERE status IN ('created', 'pending');
-- 出资线与冲正线都不另建 (父表, line_no) 索引：上面的 UNIQUE 约束已经建了同一个 btree，
-- 再建一份只是多付写放大。
CREATE INDEX payment_fundings_account_entry_idx ON payment_fundings (account_entry_id)
    WHERE account_entry_id IS NOT NULL;
CREATE INDEX payment_refunds_order_idx ON payment_refunds (order_no, created_at DESC);
CREATE INDEX payment_refunds_payment_idx ON payment_refunds (payment_id);
CREATE INDEX payment_refunds_status_idx ON payment_refunds (status, created_at);

-- 回调排查与补偿扫描：某一笔支付收到了哪些回调、哪些还没处理完。
CREATE INDEX payment_notifications_payment_idx ON payment_notifications (payment_no)
    WHERE payment_no <> '';
CREATE INDEX payment_notifications_unprocessed_idx ON payment_notifications (status, received_at)
    WHERE status IN ('received', 'failed');
CREATE INDEX payment_provider_calls_payment_idx ON payment_provider_calls (payment_no, created_at DESC)
    WHERE payment_no <> '';
CREATE INDEX payment_provider_calls_refund_idx ON payment_provider_calls (refund_no, created_at DESC)
    WHERE refund_no <> '';

-- 流水按单号与时间找：对账差异逐笔核对时用。
CREATE INDEX payment_transactions_payment_idx ON payment_transactions (payment_no, funding_line_no)
    WHERE payment_no <> '';
CREATE INDEX payment_transactions_refund_idx ON payment_transactions (refund_no, funding_line_no)
    WHERE refund_no <> '';
CREATE INDEX payment_transactions_occurred_idx ON payment_transactions (occurred_at);

CREATE INDEX payment_state_transitions_aggregate_idx
    ON payment_state_transitions (aggregate_type, aggregate_id, created_at);
CREATE INDEX payment_state_transitions_request_idx ON payment_state_transitions (request_id)
    WHERE request_id <> '';

-- ============================================================
-- 平台样板：消息 outbox / inbox
-- ============================================================

-- 与 identity/003、merchant/002、coupon/001、coffee_machine/001、order/001 里的同名两表逐列一致，
-- 各库自带一份，跨库不共享表。
--
-- 本服务的 outbox 不是可选件：支付结果、退款结果与代扣结果都要以领域事件发出去（方案 7.3 / 7.4），
-- 而审计记录的写法（platform/audit）就是「在业务事务内往自己的 outbox 追加一条
-- admin.operation.logged」。没有这张表，后台的人工退款与关单就没有留痕的地方。

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
CREATE INDEX message_inbox_lease_idx
    ON message_inbox (lease_until)
    WHERE completed_at IS NULL;
