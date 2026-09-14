-- coupon/001：优惠券领域核心表结构
--
-- 本迁移属于 panda_coupon。商户、品牌、门店、用户、员工、订单等外部服务
-- 的 ID 只作为值引用保存；本库不得创建跨数据库外键。

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

CREATE TABLE coupon_templates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    coupon_type_id UUID NOT NULL REFERENCES coupon_types(id) ON DELETE RESTRICT,
    merchant_id UUID,
    name TEXT NOT NULL CHECK (char_length(trim(name)) > 0),
    short_title TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    cover_image TEXT NOT NULL DEFAULT '',
    use_rule_description TEXT NOT NULL DEFAULT '',
    face_value NUMERIC(18, 2) NOT NULL DEFAULT 0 CHECK (face_value >= 0),
    min_purchase_amount NUMERIC(18, 2) NOT NULL DEFAULT 0 CHECK (min_purchase_amount >= 0),
    purchase_price NUMERIC(18, 2) NOT NULL DEFAULT 0 CHECK (purchase_price >= 0),
    discount_rate NUMERIC(5, 2) CHECK (discount_rate IS NULL OR (discount_rate > 0 AND discount_rate <= 100)),
    discount_cap NUMERIC(18, 2) CHECK (discount_cap IS NULL OR discount_cap >= 0),
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
    face_value NUMERIC(18, 2) NOT NULL DEFAULT 0 CHECK (face_value >= 0),
    min_purchase_amount NUMERIC(18, 2) NOT NULL DEFAULT 0 CHECK (min_purchase_amount >= 0),
    discount_rate NUMERIC(5, 2) CHECK (discount_rate IS NULL OR (discount_rate > 0 AND discount_rate <= 100)),
    discount_cap NUMERIC(18, 2) CHECK (discount_cap IS NULL OR discount_cap >= 0),
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

CREATE TABLE coupon_redemptions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_coupon_id UUID NOT NULL REFERENCES user_coupons(id) ON DELETE RESTRICT,
    template_id UUID NOT NULL REFERENCES coupon_templates(id) ON DELETE RESTRICT,
    request_id TEXT NOT NULL UNIQUE CHECK (char_length(trim(request_id)) > 0),
    redemption_method TEXT NOT NULL CHECK (redemption_method IN ('platform', 'employee', 'qr', 'external_code')),
    store_id UUID,
    employee_id UUID,
    order_id UUID,
    amount_before NUMERIC(18, 2) CHECK (amount_before IS NULL OR amount_before >= 0),
    discount_amount NUMERIC(18, 2) CHECK (discount_amount IS NULL OR discount_amount >= 0),
    amount_after NUMERIC(18, 2) CHECK (amount_after IS NULL OR amount_after >= 0),
    status TEXT NOT NULL CHECK (status IN ('succeeded', 'rejected', 'reversed')),
    failure_code TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    reversed_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX coupon_redemptions_one_live_success
    ON coupon_redemptions (user_coupon_id)
    WHERE status = 'succeeded';

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
CREATE INDEX message_outbox_pending_idx ON message_outbox (next_attempt_at, created_at) WHERE published_at IS NULL;
CREATE INDEX message_outbox_lease_idx ON message_outbox (lease_until) WHERE published_at IS NULL;
