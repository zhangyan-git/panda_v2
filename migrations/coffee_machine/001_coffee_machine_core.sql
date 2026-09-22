-- coffee_machine/001：咖啡机领域基础表结构
--
-- 属于 panda_coffee_machine（coffee-machine-service，方案 5.14）：
--   1. 设备厂商及其接入凭据、令牌生命周期   → manufacturers / manufacturer_credentials
--   2. 咖啡机设备主数据                     → devices / device_payment_methods
--   3. 饮品主数据、三级价格、上下架          → drinks
--   4. 设备与饮品的供应关系                 → device_drinks（003 已 DROP：饮品直接挂在
--                                            设备上，见那个文件的说明）
--   5. 设备事件日志、设备余额账变流水        → device_events / device_balance_ledger
--
-- 门店、商户、用户、订单、支付方式目录、库存、出杯任务都不是本库的。外部服务的 ID
-- 只作为值引用保存，本库不建跨数据库外键；本文件里的 REFERENCES 只指向本库自己的表。
-- 设备通过 store_id 关联部署点位，仅此一层引用，不持有 merchant_id。
--
-- 末尾另带平台样板的一对 message_outbox / message_inbox（不属于上面列的五项，四个
-- 库各自带一份），它是本服务发审计事件的前半截。
--
-- 金额一律使用 BIGINT 保存最小货币单位（分），旧系统 float64「元」按方案 8.2 换算。
-- 只建结构，不导入旧数据；旧 Mongo 文档通过各表的 legacy_id 回溯。

-- ============================================================
-- 设备厂商
-- ============================================================

CREATE TABLE manufacturers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    legacy_id TEXT UNIQUE,
    code TEXT NOT NULL UNIQUE CHECK (char_length(trim(code)) > 0),
    name TEXT NOT NULL CHECK (char_length(trim(name)) > 0),
    contact_name TEXT NOT NULL DEFAULT '',
    contact_phone TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 厂商编码是稳定业务标识：旧系统靠 code 关联设备、饮品与出杯分支，只允许新增厂商或修改
-- 展示字段，不允许修改编码。
CREATE FUNCTION prevent_manufacturer_code_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.code IS DISTINCT FROM OLD.code THEN
        RAISE EXCEPTION 'manufacturer code is immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER manufacturers_code_immutable
    BEFORE UPDATE ON manufacturers
    FOR EACH ROW EXECUTE FUNCTION prevent_manufacturer_code_change();

-- 接入凭据与令牌单独一张表：厂商列表查询永远不该把密钥带出来，令牌刷新只影响这一行。
CREATE TABLE manufacturer_credentials (
    manufacturer_id UUID PRIMARY KEY REFERENCES manufacturers(id) ON DELETE CASCADE,
    api_base_url TEXT NOT NULL DEFAULT '',
    test_api_base_url TEXT NOT NULL DEFAULT '',
    username TEXT NOT NULL DEFAULT '',
    user_secret TEXT NOT NULL DEFAULT '',
    signing_secret TEXT NOT NULL DEFAULT '',
    access_token TEXT NOT NULL DEFAULT '',
    token_expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ============================================================
-- 咖啡机设备
-- ============================================================

CREATE TABLE devices (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    legacy_id TEXT UNIQUE,
    serial_unique TEXT NOT NULL UNIQUE CHECK (char_length(trim(serial_unique)) > 0),
    device_name TEXT NOT NULL DEFAULT '',
    manufacturer_id UUID NOT NULL REFERENCES manufacturers(id) ON DELETE RESTRICT,
    store_id UUID,
    -- 本系统是否启用这台设备。只分启用/停用，对齐旧字段 status（0=启用 / 1=禁用）与
    -- 后台的上下架操作。方案 L139 提到的「补货中 / 维护中」是业务流程状态，不进这一列。
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    -- 设备是否在线，只有这一个来源：厂商上报。与上面的 status 是两件事——
    -- status 是「我们让不让它用」，vendor_online 是「厂商说它此刻通不通」。
    -- NULL 表示从未同步过，未同步不等于离线。
    vendor_online BOOLEAN,
    last_synced_at TIMESTAMPTZ,
    version_number TEXT NOT NULL DEFAULT '',
    android_version TEXT NOT NULL DEFAULT '',
    main_board_version TEXT NOT NULL DEFAULT '',
    last_fault_code TEXT NOT NULL DEFAULT '',
    last_fault_message TEXT NOT NULL DEFAULT '',
    last_fault_at TIMESTAMPTZ,
    last_active_at TIMESTAMPTZ,
    -- 店员打咖啡用的静态验证码，后台设备详情页会展示。
    pickup_password TEXT NOT NULL DEFAULT '',
    coffee_balance BIGINT NOT NULL DEFAULT 0 CHECK (coffee_balance >= 0),
    show_vip BOOLEAN NOT NULL DEFAULT FALSE,
    enable_coupon_verification BOOLEAN NOT NULL DEFAULT FALSE,
    warranty_end_at TIMESTAMPTZ,
    qrcode_type TEXT NOT NULL DEFAULT 'miniprogram' CHECK (qrcode_type IN ('miniprogram', 'regular')),
    regular_qrcode_payment_method TEXT CHECK (regular_qrcode_payment_method IS NULL OR regular_qrcode_payment_method IN ('fengxuan_wanlian', 'youlian')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (qrcode_type <> 'regular' OR regular_qrcode_payment_method IS NOT NULL)
);

-- 这台设备支持哪些支付方式。支付方式目录本身归支付服务，本库只存 ID。
CREATE TABLE device_payment_methods (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    payment_method_id UUID NOT NULL,
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (device_id, payment_method_id)
);

-- ============================================================
-- 饮品与设备供应关系
-- ============================================================

-- 饮品是厂商级目录。(manufacturer_id, origin_id) 是稳定自然键；origin_id 允许为空，
-- 后台手工新建的饮品没有厂商侧 ID，这类行不参与同步判重，所以唯一性用部分索引。
CREATE TABLE drinks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    legacy_id TEXT UNIQUE,
    manufacturer_id UUID NOT NULL REFERENCES manufacturers(id) ON DELETE RESTRICT,
    origin_id TEXT NOT NULL DEFAULT '',
    product_num TEXT NOT NULL DEFAULT '',
    product_name TEXT NOT NULL CHECK (char_length(trim(product_name)) > 0),
    en_name TEXT NOT NULL DEFAULT '',
    drink_type TEXT CHECK (drink_type IS NULL OR drink_type IN ('milk_coffee', 'black_coffee', 'other')),
    product_img TEXT NOT NULL DEFAULT '',
    product_desc TEXT NOT NULL DEFAULT '',
    -- 三级价格：原价 / 会员价 / 提货码价。
    price BIGINT NOT NULL DEFAULT 0 CHECK (price >= 0),
    vip_price BIGINT NOT NULL DEFAULT 0 CHECK (vip_price >= 0),
    pickup_code_price BIGINT NOT NULL DEFAULT 0 CHECK (pickup_code_price >= 0),
    status TEXT NOT NULL DEFAULT 'on_shelf' CHECK (status IN ('on_shelf', 'off_shelf')),
    sort INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (price > 0 OR (vip_price = 0 AND pickup_code_price = 0))
);

-- 设备与饮品的供应关系。旧系统 drinks 集合是每台设备一行饮品，这里拆出来。
-- 每机覆盖价可空，NULL 表示沿用 drinks 的目录价。
CREATE TABLE device_drinks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    legacy_id TEXT UNIQUE,
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    drink_id UUID NOT NULL REFERENCES drinks(id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    sort_order INTEGER NOT NULL DEFAULT 0,
    price BIGINT CHECK (price IS NULL OR price >= 0),
    vip_price BIGINT CHECK (vip_price IS NULL OR vip_price >= 0),
    pickup_code_price BIGINT CHECK (pickup_code_price IS NULL OR pickup_code_price >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (device_id, drink_id)
);

-- ============================================================
-- 设备事件日志与余额账变流水
-- ============================================================

-- 设备主数据与状态类事件：上下线、故障码、换点位、配置变更、厂商同步结果。
-- 出杯结果、提货、交易这类事件属于 fulfillment-service（方案 5.11 L496-499、L547），
-- 所以这张表没有 order_id / trade_no：出杯那条链路从头到尾不经过本服务，留下列会诱导
-- 后来人把出杯结果写进来。需要按订单对账的，去 fulfillment 那侧查。
CREATE TABLE device_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    legacy_id TEXT UNIQUE,
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE RESTRICT,
    source TEXT NOT NULL DEFAULT 'vendor' CHECK (source IN ('vendor', 'machine', 'admin', 'system')),
    event_type TEXT NOT NULL CHECK (char_length(trim(event_type)) > 0),
    occurred_at TIMESTAMPTZ,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 厂商推送的设备状态/故障事件去重键。老系统这几类事件（设备错误、上线、下线）
    -- 与出杯回调同在一个处理器里；V2 把出杯那部分给了 fulfillment，剩下的落在本域。
    external_event_id TEXT,
    error_code TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    payload JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE TABLE device_balance_ledger (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    legacy_id TEXT UNIQUE,
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE RESTRICT,
    type TEXT NOT NULL CHECK (type IN ('recharge', 'deduct', 'adjust', 'reverse')),
    -- 有符号：充值为正、扣减为负。
    amount BIGINT NOT NULL CHECK (amount <> 0),
    balance_after BIGINT NOT NULL CHECK (balance_after >= 0),
    -- 冲正 = 新增一条反向记录，不原地改数。
    reverses_entry_id UUID REFERENCES device_balance_ledger(id) ON DELETE RESTRICT,
    reference_type TEXT NOT NULL DEFAULT '',
    reference_id UUID,
    request_id TEXT NOT NULL DEFAULT '',
    remark TEXT NOT NULL DEFAULT '',
    operator_id UUID,
    operator_name TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((type = 'reverse') = (reverses_entry_id IS NOT NULL))
);

-- 只允许追加，冲正走反向记录。
CREATE FUNCTION prevent_device_balance_ledger_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'device balance ledger is append-only';
END;
$$;

CREATE TRIGGER device_balance_ledger_append_only
    BEFORE UPDATE OR DELETE ON device_balance_ledger
    FOR EACH ROW EXECUTE FUNCTION prevent_device_balance_ledger_change();

-- ============================================================
-- 平台样板：消息 outbox / inbox
-- ============================================================

-- 与 identity/003、merchant/002、coupon/001 里的同名两表逐列一致，各库自带一份，
-- 跨库不共享表。
--
-- 本服务的 outbox 不是可选件：后台的咖啡余额调整属于方案 11.6 L893 必审清单，
-- 而审计记录的写法（platform/audit）就是「在业务事务内往自己的 outbox 追加一条
-- admin.operation.logged」，由 relay 投到 RabbitMQ、再落到身份库的
-- admin_operation_logs。没有这张表，余额调整就没有留痕的地方。
--
-- 这里是全新库，lease 列直接建在表里，所以不需要 identity/003 那组
-- ALTER TABLE ADD COLUMN IF NOT EXISTS（那组是为了升级早期 schema 建出来的表）。
-- 列名和顺序保持不变，四份拷贝才能逐列对得起来。

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
-- 索引
-- ============================================================

CREATE INDEX devices_manufacturer_idx ON devices (manufacturer_id, status);
CREATE INDEX devices_store_idx ON devices (store_id, status);
CREATE INDEX device_payment_methods_device_idx ON device_payment_methods (device_id, sort_order);
CREATE INDEX drinks_manufacturer_idx ON drinks (manufacturer_id, status);
CREATE INDEX device_drinks_drink_idx ON device_drinks (drink_id);
CREATE INDEX device_events_device_idx ON device_events (device_id, received_at);
CREATE INDEX device_events_type_idx ON device_events (event_type, received_at);
CREATE INDEX device_balance_ledger_device_idx ON device_balance_ledger (device_id, created_at);

-- 饮品目录自然键，只对参与厂商同步的行生效。
CREATE UNIQUE INDEX drinks_manufacturer_origin_unique
    ON drinks (manufacturer_id, origin_id)
    WHERE origin_id <> '';

-- 同一请求号只允许一条账变：旧库这张表没有业务唯一键，重试一次充值就多一条无法分辨的
-- 流水，而提货扣减那条路径根本不写流水。
CREATE UNIQUE INDEX device_balance_ledger_one_per_request
    ON device_balance_ledger (request_id)
    WHERE request_id <> '';

-- 厂商回调去重。
CREATE UNIQUE INDEX device_events_external_id_unique
    ON device_events (external_event_id)
    WHERE external_event_id IS NOT NULL;

-- relay 取待投递消息、以及续租，都只看未发布的行。
CREATE INDEX message_outbox_pending_idx
    ON message_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;

CREATE INDEX message_outbox_lease_idx
    ON message_outbox (lease_until)
    WHERE published_at IS NULL;
