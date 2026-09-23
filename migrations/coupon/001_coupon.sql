-- coupon：优惠券领域表结构与中文注释
--
-- 本迁移属于 panda_coupon。商户、品牌、门店、用户、员工、订单等外部服务
-- 的 ID 只作为值引用保存；本库不得创建跨数据库外键。
--
-- 金额一律是「分 + BIGINT」，本域一列 numeric(18,2) 都没有；折扣比例相关
-- 的两列也一并不留。三件事都是有意为之：
--
--   * 单位取「分」，与 panda_serve 已有的约定一致（membership_card.price 分、
--     user_balance.balance 分），顺带消掉小数解析这条——它本身就是 500 的来源：
--     请求里的垃圾串会一路进到 numeric 列，Postgres 抛 22P02，最后被兜成
--     INTERNAL_ERROR。改成 BIGINT 后，类型不对在 JSON 解码阶段就是 400，压根到不了
--     数据库。全仓其余服务（user / merchant / gateway / platform）没有任何金额列，
--     所以只有本域要按「分」来。
--   * 金额列上的 CHECK 直接写成「bigint >= 0」，不带 (0)::numeric 这类会跟着列类型
--     变的表达式：常量在表达式里由 Postgres 自行重解析，规则（尤其常量这一侧的类型）
--     在不同版本间不保证一致，显式写死的结果才是确定的。
--   * discount_rate / discount_cap 不建：产品确认优惠券不做折扣比例（券类型只有
--     代金券 / 兑换券 / 会员价体验券，没有按比例打折这一类）。discount_cap 是「折扣
--     金额上限」，没有折扣比例它约束不了任何东西。
--
-- 两张账本（coupon_state_transitions、coupon_inventory_ledger）带只增触发器；两张
-- message 表与其余十个域的同一段逐列相同，用哨兵注释夹住，改动前先看那一节开头的说明。
--
-- 本文件不带 goose 的 Down 段：runner 只 apply，不了解任何 goose 指令，连带 Down 段的
-- 文件都会当场拒收（migrations_test.go 的 TestSetsAreUsable 也在守这条）。真要撤销什么，
-- 见文件里各处的说明（尤见只增触发器那一节末尾的手工回滚命令）。
--
-- 券类型字典的种子数据在 002_coupon_seed.sql。

BEGIN;

-- ============================================================
-- 优惠券业务类型字典
-- ============================================================

CREATE TABLE coupon_types (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    code TEXT NOT NULL UNIQUE CHECK (char_length(trim(code)) > 0),
    name TEXT NOT NULL CHECK (char_length(trim(name)) > 0),
    description TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 类型编码是稳定的业务标识，只允许新增类型或修改展示字段，不允许改编码。
CREATE FUNCTION prevent_coupon_type_code_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.code IS DISTINCT FROM OLD.code THEN
        RAISE EXCEPTION 'coupon type code is immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER coupon_types_code_immutable
    BEFORE UPDATE ON coupon_types
    FOR EACH ROW EXECUTE FUNCTION prevent_coupon_type_code_change();

-- ============================================================
-- 优惠券模板、适用范围与发行批次
-- ============================================================

CREATE TABLE coupon_templates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    coupon_type_id UUID NOT NULL REFERENCES coupon_types(id) ON DELETE RESTRICT,
    merchant_id UUID,
    name TEXT NOT NULL CHECK (char_length(trim(name)) > 0),
    short_title TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    cover_image TEXT NOT NULL DEFAULT '',
    use_rule_description TEXT NOT NULL DEFAULT '',
    face_value BIGINT NOT NULL DEFAULT 0 CHECK (face_value >= 0),
    min_purchase_amount BIGINT NOT NULL DEFAULT 0 CHECK (min_purchase_amount >= 0),
    purchase_price BIGINT NOT NULL DEFAULT 0 CHECK (purchase_price >= 0),
    total_quantity BIGINT NOT NULL CHECK (total_quantity > 0),
    issued_quantity BIGINT NOT NULL DEFAULT 0 CHECK (issued_quantity >= 0 AND issued_quantity <= total_quantity),
    reserved_quantity BIGINT NOT NULL DEFAULT 0 CHECK (reserved_quantity >= 0 AND reserved_quantity + issued_quantity <= total_quantity),
    validity_mode TEXT NOT NULL CHECK (validity_mode IN ('fixed', 'relative')),
    valid_from TIMESTAMPTZ,
    valid_to TIMESTAMPTZ,
    valid_days INTEGER,
    claim_limit_mode TEXT NOT NULL DEFAULT 'once_ever' CHECK (claim_limit_mode IN ('once_ever', 'unlimited_after_use', 'periodic')),
    claim_period_unit TEXT CHECK (claim_period_unit IS NULL OR claim_period_unit IN ('day', 'week', 'month', 'year')),
    claim_period_quantity INTEGER CHECK (claim_period_quantity IS NULL OR claim_period_quantity > 0),
    redemption_type TEXT NOT NULL DEFAULT 'platform' CHECK (redemption_type IN ('platform', 'external_code', 'show_qr')),
    external_use_method TEXT CHECK (external_use_method IS NULL OR external_use_method IN ('copy_code', 'download_qr')),
    audit_status TEXT NOT NULL DEFAULT 'pending' CHECK (audit_status IN ('pending', 'approved', 'rejected')),
    audit_remark TEXT NOT NULL DEFAULT '',
    audited_at TIMESTAMPTZ,
    audited_by UUID,
    status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'active', 'disabled', 'closed')),
    is_hot BOOLEAN NOT NULL DEFAULT FALSE,
    is_recommended BOOLEAN NOT NULL DEFAULT FALSE,
    sort_order INTEGER NOT NULL DEFAULT 0,
    visible BOOLEAN NOT NULL DEFAULT TRUE,
    created_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((validity_mode = 'fixed' AND valid_from IS NOT NULL AND valid_to IS NOT NULL AND valid_to > valid_from AND valid_days IS NULL)
        OR (validity_mode = 'relative' AND valid_from IS NULL AND valid_to IS NULL AND valid_days IS NOT NULL AND valid_days > 0)),
    CHECK ((claim_limit_mode = 'periodic' AND claim_period_unit IS NOT NULL AND claim_period_quantity IS NOT NULL)
        OR (claim_limit_mode <> 'periodic' AND claim_period_unit IS NULL AND claim_period_quantity IS NULL)),
    CHECK (redemption_type <> 'external_code' OR external_use_method IS NOT NULL),
    CHECK (audit_status <> 'approved' OR audited_at IS NOT NULL),
    CHECK (status <> 'active' OR audit_status = 'approved')
);

CREATE TABLE coupon_template_scopes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id UUID NOT NULL REFERENCES coupon_templates(id) ON DELETE CASCADE,
    scope_type TEXT NOT NULL CHECK (scope_type IN ('brand', 'store', 'device', 'category')),
    scope_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (template_id, scope_type, scope_id)
);

CREATE TABLE coupon_batches (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id UUID NOT NULL REFERENCES coupon_templates(id) ON DELETE RESTRICT,
    batch_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(batch_no)) > 0),
    source TEXT NOT NULL CHECK (source IN ('platform', 'admin', 'merchant', 'event', 'purchase')),
    total_quantity BIGINT NOT NULL CHECK (total_quantity > 0),
    reserved_quantity BIGINT NOT NULL DEFAULT 0 CHECK (reserved_quantity >= 0 AND reserved_quantity <= total_quantity),
    issued_quantity BIGINT NOT NULL DEFAULT 0 CHECK (issued_quantity >= 0 AND issued_quantity + reserved_quantity <= total_quantity),
    released_quantity BIGINT NOT NULL DEFAULT 0 CHECK (released_quantity >= 0),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('pending', 'active', 'exhausted', 'closed', 'cancelled')),
    order_id UUID,
    user_id UUID,
    request_id TEXT,
    created_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ============================================================
-- 用户券实例与领取时的范围快照
-- ============================================================

CREATE TABLE user_coupons (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id UUID NOT NULL REFERENCES coupon_templates(id) ON DELETE RESTRICT,
    batch_id UUID REFERENCES coupon_batches(id) ON DELETE RESTRICT,
    user_id UUID NOT NULL,
    coupon_type_code TEXT NOT NULL CHECK (char_length(trim(coupon_type_code)) > 0),
    claim_type TEXT NOT NULL CHECK (claim_type IN ('user_claim', 'admin_assign', 'daily_gift', 'event_reward', 'claim_by_code', 'purchase')),
    issue_reason TEXT NOT NULL DEFAULT '',
    campaign_claim_id UUID,
    grant_sequence INTEGER NOT NULL DEFAULT 0 CHECK (grant_sequence >= 0),
    status TEXT NOT NULL DEFAULT 'claimed' CHECK (status IN ('claimed', 'held', 'redeemed', 'expired', 'refunded', 'invalidated')),
    redemption_code_digest TEXT UNIQUE,
    face_value BIGINT NOT NULL DEFAULT 0 CHECK (face_value >= 0),
    min_purchase_amount BIGINT NOT NULL DEFAULT 0 CHECK (min_purchase_amount >= 0),
    redemption_type TEXT NOT NULL CHECK (redemption_type IN ('platform', 'external_code', 'show_qr')),
    valid_from TIMESTAMPTZ NOT NULL,
    expired_at TIMESTAMPTZ NOT NULL CHECK (expired_at > valid_from),
    claimed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    held_at TIMESTAMPTZ,
    redeemed_at TIMESTAMPTZ,
    refunded_at TIMESTAMPTZ,
    invalidated_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((status = 'held' AND held_at IS NOT NULL) OR status <> 'held'),
    CHECK ((status = 'redeemed' AND redeemed_at IS NOT NULL) OR status <> 'redeemed'),
    CHECK ((status = 'refunded' AND refunded_at IS NOT NULL) OR status <> 'refunded'),
    CHECK ((status = 'invalidated' AND invalidated_at IS NOT NULL) OR status <> 'invalidated')
);

CREATE TABLE user_coupon_scopes (
    user_coupon_id UUID NOT NULL REFERENCES user_coupons(id) ON DELETE CASCADE,
    scope_type TEXT NOT NULL CHECK (scope_type IN ('brand', 'store', 'device', 'category')),
    scope_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_coupon_id, scope_type, scope_id)
);

-- ============================================================
-- 核销
-- ============================================================

CREATE TABLE coupon_redemptions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_coupon_id UUID NOT NULL REFERENCES user_coupons(id) ON DELETE RESTRICT,
    template_id UUID NOT NULL REFERENCES coupon_templates(id) ON DELETE RESTRICT,
    request_id TEXT NOT NULL UNIQUE CHECK (char_length(trim(request_id)) > 0),
    redemption_method TEXT NOT NULL CHECK (redemption_method IN ('platform', 'employee', 'qr', 'external_code')),
    store_id UUID,
    employee_id UUID,
    order_id UUID,
    amount_before BIGINT CHECK (amount_before >= 0),
    discount_amount BIGINT CHECK (discount_amount >= 0),
    amount_after BIGINT CHECK (amount_after >= 0),
    status TEXT NOT NULL CHECK (status IN ('succeeded', 'rejected', 'reversed')),
    failure_code TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    reversed_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX coupon_redemptions_one_live_success
    ON coupon_redemptions (user_coupon_id)
    WHERE status = 'succeeded';

-- ============================================================
-- 状态迁移与库存流水（两张只增账本）
-- ============================================================

-- 这两张表是优惠券域的账本。coupon_state_transitions 记的是「这张券/批次/模板
-- 从什么状态变成了什么状态、谁推的」；coupon_inventory_ledger 记的是「模板或批次的
-- 可用量为什么变了」。它们不是缓存，是事实来源——幂等重放、对账、审计都拿它们当依据。
--
-- 全仓另外 10 张同性质的账本都在建表时就带了 BEFORE UPDATE OR DELETE 的只增触发器：
-- device_balance_ledger、order_state_transitions、payment_transactions、
-- payment_state_transitions、stock_movements、fortune_card_entries、
-- coffee_bean_entries、lottery_draws、lottery_win_events、membership_changes。
-- 本域原先只有上面 coupon_types 那条 coupon_types_code_immutable。
--
-- 少了这条防线不是「少一层保险」，是账本可以被改写、被删空。本域没有 before-image、
-- 没有备份，删掉一条状态迁移就再也拼不回来；而它恰好是事后回答「这张券什么时候、
-- 被谁停掉的」唯一的地方。与那 10 张表同一套写法：物理阻断，不靠服务层自觉。
--
-- 两条触发函数 + 两条触发器，一表一条。函数名与报错措辞照抄同类表的命名习惯
-- （coupon_state_transitions 对 order_state_transitions / payment_state_transitions，
-- coupon_inventory_ledger 对 device_balance_ledger）：触发器叫 <表名>_append_only，
-- 函数叫 prevent_<表意>_change，报错是一句 "… is append-only"。
--
-- 现状是安全的：production 代码对这两张表只有 INSERT（postgres.go 的核销/作废/发券/
-- 后台调状态，template.go 的审核），没有一处 UPDATE 或 DELETE。
--
-- 这两个触发器是后来补的，补的时候没有回头改建表语句：改一个已经 apply 过的文件的
-- DDL 会让「库里的形状」和「文件描述的库」对不上。合并成这一个文件之后，新建库从头跑
-- 拿到的结果与老库补跑那一刻完全一致。
--
-- 回滚：本仓的迁移是单向的——runner 只 apply，不了解任何 goose 指令，连带 Down 段的
-- 文件都会当场拒收（migrations_test.go 的 TestSetsAreUsable 也在守这条）。所以这里
-- 没有配套的 down 文件。真要撤销只能手工执行：
--
--     DROP TRIGGER coupon_state_transitions_append_only ON coupon_state_transitions;
--     DROP TRIGGER coupon_inventory_ledger_append_only ON coupon_inventory_ledger;
--     DROP FUNCTION prevent_coupon_transition_change();
--     DROP FUNCTION prevent_coupon_inventory_ledger_change();
--
-- 但那是**放开**保护，只有在确认就是要改写账本时才做。
--
-- 连带改动：repository/postgres_integration_test.go 的 fixture 清理原本直接
-- `DELETE FROM coupon_state_transitions`，触发器建起来之后那句会被当场拒掉。那里已
-- 改成在事务里先 DISABLE TRIGGER 再删再 ENABLE（与 membership_changes 的测试清理同一
-- 个意图：删的只是用例自己刚写进去的行）。

CREATE TABLE coupon_state_transitions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type TEXT NOT NULL CHECK (aggregate_type IN ('template', 'batch', 'user_coupon', 'redemption')),
    aggregate_id UUID NOT NULL,
    from_status TEXT NOT NULL DEFAULT '',
    to_status TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    actor_id UUID,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- coupon_state_transitions：券的状态迁移历史
CREATE FUNCTION prevent_coupon_transition_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'coupon state transitions are append-only';
END;
$$;

CREATE TRIGGER coupon_state_transitions_append_only
    BEFORE UPDATE OR DELETE ON coupon_state_transitions
    FOR EACH ROW EXECUTE FUNCTION prevent_coupon_transition_change();

CREATE TABLE coupon_inventory_ledger (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id UUID NOT NULL REFERENCES coupon_templates(id) ON DELETE RESTRICT,
    batch_id UUID REFERENCES coupon_batches(id) ON DELETE RESTRICT,
    reference_type TEXT NOT NULL,
    reference_id UUID,
    quantity BIGINT NOT NULL CHECK (quantity <> 0),
    operation TEXT NOT NULL CHECK (operation IN ('reserve', 'issue', 'release', 'expire', 'adjust')),
    request_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- coupon_inventory_ledger：库存变动流水
CREATE FUNCTION prevent_coupon_inventory_ledger_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'coupon inventory ledger is append-only';
END;
$$;

CREATE TRIGGER coupon_inventory_ledger_append_only
    BEFORE UPDATE OR DELETE ON coupon_inventory_ledger
    FOR EACH ROW EXECUTE FUNCTION prevent_coupon_inventory_ledger_change();

-- ============================================================
-- 幂等请求记录
-- ============================================================

CREATE TABLE coupon_idempotency_keys (
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

-- ------------------------------------------------------------
-- 幂等缓存历史行的改写（在全新库上是空操作）
--
-- response 存的是首次处理结果的 JSON 快照，重放时会被原样反序列化回对应的响应结构体。
-- 结构体或接口契约一改，旧行的键/值类型就对不上了，所以下面两条 UPDATE 把旧行原地改写。
-- 幂等靠 WHERE 里的判据：改写之后判据不再命中，重复执行是安全空操作。
-- ------------------------------------------------------------

-- 1. 发券响应（scope='admin.coupons.issue'）的键名由 snake_case 改为 camelCase
--
-- 背景：IssueCouponsResponse 的 json tag 统一到 camelCase（后台接口风格统一）。
-- 旧行的键不改写的话，改名后全部对不上，重放会静默返回一个 batchId 为空串、
-- issuedQuantity 为 0 的空对象——不报错，但调用方拿到的是坏数据。所以这里把旧行原地
-- 改写成新键名。
--
-- 只处理 scope='admin.coupons.issue'：另外两个幂等作用域（coupon.redeem、
-- coupon.revoke）缓存的是 model.UserCoupon，该结构体没有 json tag，序列化出来
-- 是 Go 字段名（PascalCase），不受本次改名影响。
--
-- 注意 request_hash 不在这里迁移、也迁移不了：表里只存了摘要、没存原始请求体，
-- 算不回去。哈希的一致性由代码保证——service 包里的 issueHashPayload 冻结了旧 tag
-- 渲染，改名前后产出的哈希逐字节相同（有黄金摘要测试钉住）。
UPDATE coupon_idempotency_keys
SET response = jsonb_build_object(
        'batchId',        response -> 'batch_id',
        'issuedQuantity', response -> 'issued_quantity',
        'userCouponIds',  response -> 'user_coupon_ids'
    )
WHERE scope = 'admin.coupons.issue'
  AND response ? 'batch_id';

-- 2. coupon.redeem / coupon.revoke 两个作用域里的金额快照
--
-- 这两个作用域缓存的是 model.UserCoupon 的 JSON（该结构体没有 json tag，键是 Go
-- 字段名，金额是字符串）。金额列改成「分 + BIGINT」之后，旧行的
-- "FaceValue": "25.00" 再也反序列化不进 int64，重放会以 500 收场——不是返回
-- 坏数据，是直接报错，且只在重放同一个 Idempotency-Key 时才出现。
--
-- FaceValue / MinPurchaseAmount 换算成数字，DiscountRate / DiscountCap 直接摘掉。
-- 摘不摘其实都不报错（重放走的是裸 json.Unmarshal，多余的键会被静默忽略），
-- 但结构体里已经没有这两个字段了，留着只是垃圾。
--
-- 空串要按 0 处理：库里确实存在 "MinPurchaseAmount": "" 这种行（早期代码路径没
-- 填这个字段），直接 ::numeric 会抛 22P02。
--
-- 幂等：改写后 FaceValue 变成 number，jsonb_typeof 不再等于 'string'，重复执行
-- 命中 0 行。这正是它的守卫。
UPDATE coupon_idempotency_keys
SET response = (response - 'DiscountRate' - 'DiscountCap') || jsonb_build_object(
        'FaceValue',         (COALESCE(NULLIF(response ->> 'FaceValue', ''), '0')::numeric * 100)::bigint,
        'MinPurchaseAmount', (COALESCE(NULLIF(response ->> 'MinPurchaseAmount', ''), '0')::numeric * 100)::bigint
    )
WHERE scope IN ('coupon.redeem', 'coupon.revoke')
  AND jsonb_typeof(response -> 'FaceValue') = 'string';

-- ============================================================
-- 本库的 outbox 与 inbox
-- ============================================================
--
-- 这一对表在十一个集里是**同一份拷贝**，migrations 的
-- TestMessageTablesStayInSyncAcrossSets 逐列比对，TestMessageTablesAddLeaseColumnsBeforeIndexes
-- 钉住租约列必须先于索引出现。列定义、顺序、索引写法都**不要顺手调整**——注释可以不同，
-- 列不能。

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

CREATE INDEX message_outbox_pending_idx ON message_outbox (next_attempt_at, created_at) WHERE published_at IS NULL;
CREATE INDEX message_outbox_lease_idx ON message_outbox (lease_until) WHERE published_at IS NULL;

-- <<< message-tables:end <<<

-- ============================================================
-- 索引
-- ============================================================

CREATE INDEX coupon_templates_listing_idx ON coupon_templates (status, visible, sort_order, created_at);
CREATE INDEX coupon_templates_merchant_idx ON coupon_templates (merchant_id, status, audit_status);
CREATE INDEX coupon_template_scopes_lookup_idx ON coupon_template_scopes (scope_type, scope_id, template_id);
CREATE INDEX coupon_batches_template_status_idx ON coupon_batches (template_id, status);
CREATE INDEX user_coupons_user_status_expiry_idx ON user_coupons (user_id, status, expired_at);
CREATE INDEX user_coupons_template_status_idx ON user_coupons (template_id, status);
CREATE INDEX coupon_redemptions_store_time_idx ON coupon_redemptions (store_id, created_at);
CREATE INDEX coupon_redemptions_employee_time_idx ON coupon_redemptions (employee_id, created_at);
CREATE INDEX coupon_redemptions_order_idx ON coupon_redemptions (order_id);
CREATE INDEX coupon_state_transitions_aggregate_idx ON coupon_state_transitions (aggregate_type, aggregate_id, created_at);
CREATE INDEX coupon_inventory_ledger_batch_idx ON coupon_inventory_ledger (batch_id, created_at);

-- ============================================================
-- 表与关键字段的中文注释
--
-- 与表结构放在同一个文件里：合并之后每个集只有这一个 DDL 文件，注释和它描述的列不会再
-- 出现「改了列没改注释」这种两个文件各说各话的情况。
-- ============================================================

COMMENT ON TABLE coupon_types IS '优惠券业务类型字典';
COMMENT ON COLUMN coupon_types.code IS '稳定的优惠券类型编码，创建后不可修改';
COMMENT ON COLUMN coupon_types.status IS '类型状态：active=启用，disabled=停用';

COMMENT ON TABLE coupon_templates IS '优惠券模板及发行、领取、核销规则';
COMMENT ON COLUMN coupon_templates.coupon_type_id IS '优惠券类型 ID，引用本库 coupon_types';
COMMENT ON COLUMN coupon_templates.merchant_id IS '商户 ID，NULL 表示平台券；仅作跨库值引用';
COMMENT ON COLUMN coupon_templates.face_value IS '优惠券面值，单位为分';
COMMENT ON COLUMN coupon_templates.min_purchase_amount IS '使用优惠券所需的最低消费金额，单位为分';
COMMENT ON COLUMN coupon_templates.purchase_price IS '购买价格，单位为分；0 表示免费领取';
COMMENT ON COLUMN coupon_templates.total_quantity IS '模板总发行数量';
COMMENT ON COLUMN coupon_templates.issued_quantity IS '模板已发行数量；由事务内操作维护';
COMMENT ON COLUMN coupon_templates.reserved_quantity IS '模板已预留但尚未完成发行的数量';
COMMENT ON COLUMN coupon_templates.validity_mode IS '有效期模式：fixed=固定日期，relative=领取后相对天数';
COMMENT ON COLUMN coupon_templates.valid_from IS '固定有效期开始时间';
COMMENT ON COLUMN coupon_templates.valid_to IS '固定有效期结束时间';
COMMENT ON COLUMN coupon_templates.valid_days IS '相对有效期天数';
COMMENT ON COLUMN coupon_templates.claim_limit_mode IS '领取限制：once_ever=永久一次，unlimited_after_use=使用后可再领，periodic=周期限制';
COMMENT ON COLUMN coupon_templates.redemption_type IS '核销方式：platform=平台核销，external_code=外部券码，show_qr=展示二维码';
COMMENT ON COLUMN coupon_templates.audit_status IS '审核状态：pending=待审核，approved=已通过，rejected=已拒绝';
COMMENT ON COLUMN coupon_templates.status IS '模板状态：draft=草稿，active=启用，disabled=停用，closed=关闭';

COMMENT ON TABLE coupon_template_scopes IS '优惠券模板适用范围';
COMMENT ON COLUMN coupon_template_scopes.scope_type IS '范围类型：brand=品牌，store=门店，device=设备，category=分类';
COMMENT ON COLUMN coupon_template_scopes.scope_id IS '范围对象 ID，属于对应主数据服务时仅作跨库值引用';

COMMENT ON TABLE coupon_batches IS '优惠券发行批次及批次库存';
COMMENT ON COLUMN coupon_batches.batch_no IS '发行批次号，业务唯一';
COMMENT ON COLUMN coupon_batches.source IS '批次来源：platform=平台，admin=后台，merchant=商户，event=活动，purchase=购买';
COMMENT ON COLUMN coupon_batches.total_quantity IS '批次总数量';
COMMENT ON COLUMN coupon_batches.reserved_quantity IS '批次已预留数量';
COMMENT ON COLUMN coupon_batches.issued_quantity IS '批次已发行数量';
COMMENT ON COLUMN coupon_batches.released_quantity IS '批次已释放数量';
COMMENT ON COLUMN coupon_batches.order_id IS '关联订单 ID，属于订单服务时仅作跨库值引用';
COMMENT ON COLUMN coupon_batches.user_id IS '关联用户 ID，属于身份服务时仅作跨库值引用';
COMMENT ON COLUMN coupon_batches.request_id IS '创建批次或发券操作的幂等请求号';

COMMENT ON TABLE user_coupons IS '用户持有的优惠券实例及领取时规则快照';
COMMENT ON COLUMN user_coupons.template_id IS '来源优惠券模板 ID';
COMMENT ON COLUMN user_coupons.batch_id IS '来源发行批次 ID';
COMMENT ON COLUMN user_coupons.user_id IS '持券用户 ID，属于身份服务时仅作跨库值引用';
COMMENT ON COLUMN user_coupons.coupon_type_code IS '领取时的优惠券类型编码快照';
COMMENT ON COLUMN user_coupons.claim_type IS '发券来源：主动领取、后台发放、每日赠送、活动奖励、领取码或购买';
COMMENT ON COLUMN user_coupons.status IS '用户券状态：claimed=已领取，held=已预占，redeemed=已核销，expired=已过期，refunded=已退款，invalidated=已作废';
COMMENT ON COLUMN user_coupons.redemption_code_digest IS '核销码摘要，不保存明文核销码';
COMMENT ON COLUMN user_coupons.face_value IS '领取时面值快照，单位为分';
COMMENT ON COLUMN user_coupons.min_purchase_amount IS '领取时最低消费快照，单位为分';
COMMENT ON COLUMN user_coupons.valid_from IS '用户券实际生效时间快照';
COMMENT ON COLUMN user_coupons.expired_at IS '用户券实际过期时间快照';

COMMENT ON TABLE user_coupon_scopes IS '用户券领取时的适用范围快照';
COMMENT ON COLUMN user_coupon_scopes.user_coupon_id IS '用户券 ID';
COMMENT ON COLUMN user_coupon_scopes.scope_type IS '范围类型：brand=品牌，store=门店，device=设备，category=分类';
COMMENT ON COLUMN user_coupon_scopes.scope_id IS '领取时适用范围对象 ID';

COMMENT ON TABLE coupon_redemptions IS '优惠券核销记录及核销金额快照';
COMMENT ON COLUMN coupon_redemptions.user_coupon_id IS '被核销的用户券 ID';
COMMENT ON COLUMN coupon_redemptions.request_id IS '核销请求幂等号';
COMMENT ON COLUMN coupon_redemptions.redemption_method IS '核销方式：platform=平台，employee=员工，qr=二维码，external_code=外部券码';
COMMENT ON COLUMN coupon_redemptions.store_id IS '核销门店 ID，属于商户服务时仅作跨库值引用';
COMMENT ON COLUMN coupon_redemptions.employee_id IS '核销员工 ID，属于身份服务时仅作跨库值引用';
COMMENT ON COLUMN coupon_redemptions.order_id IS '关联订单 ID，属于订单服务时仅作跨库值引用';
-- 下面三个金额列目前没有写入方：唯一的核销写入路径（coupon-service 的
-- internal/repository/postgres.go 里那条 INSERT INTO coupon_redemptions）只写
-- user_coupon_id、template_id、request_id、redemption_method、status、completed_at，
-- 这三列恒为 NULL。单位与其余金额列一样是「分」（见文件头的单位说明）。
COMMENT ON COLUMN coupon_redemptions.amount_before IS '核销前订单金额，单位为分';
COMMENT ON COLUMN coupon_redemptions.discount_amount IS '本次优惠金额，单位为分';
COMMENT ON COLUMN coupon_redemptions.amount_after IS '核销后应付金额，单位为分';
COMMENT ON COLUMN coupon_redemptions.status IS '核销状态：succeeded=成功，rejected=拒绝，reversed=已反转';

COMMENT ON TABLE coupon_state_transitions IS '优惠券领域状态变更审计记录';
COMMENT ON COLUMN coupon_state_transitions.aggregate_type IS '聚合类型：template=模板，batch=批次，user_coupon=用户券，redemption=核销';
COMMENT ON COLUMN coupon_state_transitions.aggregate_id IS '发生状态变化的聚合 ID';
COMMENT ON COLUMN coupon_state_transitions.from_status IS '变更前状态';
COMMENT ON COLUMN coupon_state_transitions.to_status IS '变更后状态';
COMMENT ON COLUMN coupon_state_transitions.request_id IS '触发状态变化的请求幂等号';
COMMENT ON COLUMN coupon_state_transitions.actor_id IS '操作人 ID，属于身份服务时仅作跨库值引用';
COMMENT ON COLUMN coupon_state_transitions.metadata IS '状态变化附加信息';

COMMENT ON TABLE coupon_inventory_ledger IS '优惠券库存变动流水及对账记录';
COMMENT ON COLUMN coupon_inventory_ledger.template_id IS '优惠券模板 ID';
COMMENT ON COLUMN coupon_inventory_ledger.batch_id IS '发行批次 ID';
-- 代码里写进去的只有 admin_issue（coupon-service 的 postgres.go，后台发券那条 IssueCoupons
-- 路径）；本行原来举的 claim、redeem、refund、adjust 四个词一处都没写过。
COMMENT ON COLUMN coupon_inventory_ledger.reference_type IS '关联业务类型，当前只写 admin_issue（后台发券）';
COMMENT ON COLUMN coupon_inventory_ledger.reference_id IS '关联业务记录 ID';
COMMENT ON COLUMN coupon_inventory_ledger.quantity IS '库存变动数量，正负号表示增加或减少';
COMMENT ON COLUMN coupon_inventory_ledger.operation IS '库存操作：reserve=预留，issue=发行，release=释放，expire=过期，adjust=调整';
COMMENT ON COLUMN coupon_inventory_ledger.request_id IS '库存变更请求幂等号';

COMMENT ON TABLE coupon_idempotency_keys IS '优惠券操作幂等请求记录';
COMMENT ON COLUMN coupon_idempotency_keys.scope IS '幂等作用域，例如 claim、redeem、issue';
COMMENT ON COLUMN coupon_idempotency_keys.idempotency_key IS '调用方提供的幂等键';
COMMENT ON COLUMN coupon_idempotency_keys.request_hash IS '请求内容摘要，用于检测同一幂等键复用不同请求';
COMMENT ON COLUMN coupon_idempotency_keys.resource_type IS '幂等请求创建或操作的资源类型';
COMMENT ON COLUMN coupon_idempotency_keys.resource_id IS '幂等请求关联的资源 ID';
COMMENT ON COLUMN coupon_idempotency_keys.response IS '首次处理结果快照';
COMMENT ON COLUMN coupon_idempotency_keys.status IS '处理状态：processing=处理中，succeeded=成功，failed=失败';

COMMENT ON TABLE message_outbox IS '优惠券服务待发布消息事件';
COMMENT ON COLUMN message_outbox.event_id IS '事件唯一 ID';
COMMENT ON COLUMN message_outbox.event_type IS '事件类型';
COMMENT ON COLUMN message_outbox.event_version IS '事件版本';
COMMENT ON COLUMN message_outbox.payload IS '事件内容二进制数据';
COMMENT ON COLUMN message_outbox.attempts IS '发布尝试次数';
COMMENT ON COLUMN message_outbox.published_at IS '成功发布时间';
COMMENT ON COLUMN message_outbox.lease_until IS '消息处理租约截止时间';

COMMENT ON TABLE message_inbox IS '优惠券服务已接收消息事件及消费租约';
COMMENT ON COLUMN message_inbox.event_id IS '已接收事件唯一 ID，用于消费去重';
COMMENT ON COLUMN message_inbox.claimed_at IS '首次领取消费时间';
COMMENT ON COLUMN message_inbox.lease_until IS '消息消费租约截止时间';
COMMENT ON COLUMN message_inbox.completed_at IS '成功完成消费时间';

COMMIT;
