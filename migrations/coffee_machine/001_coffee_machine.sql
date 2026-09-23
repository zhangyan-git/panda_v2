-- coffee_machine：咖啡机领域表结构与中文注释
--
-- 属于 panda_coffee_machine（coffee-machine-service，方案 5.14）：
--   1. 设备厂商及其接入凭据、令牌生命周期   → manufacturers / manufacturer_credentials
--   2. 咖啡机设备主数据                     → devices / device_payment_methods
--   3. 饮品主数据、三级价格、上下架、所属设备 → drinks
--   4. 设备事件日志、设备余额账变流水        → device_events / device_balance_ledger
--
-- 没有「设备与饮品的供应关系」这张中间表：饮品自带 device_id，一行即「某台设备上的一杯
-- 饮品」，价格就是这台设备上的售价（见下面 drinks 的说明）。
--
-- 门店、商户、用户、订单、支付方式目录、库存、出杯任务都不是本库的。外部服务的 ID
-- 只作为值引用保存，本库不建跨数据库外键；本文件里的 REFERENCES 只指向本库自己的表。
-- 设备通过 store_id 关联部署点位，仅此一层引用，不持有 merchant_id。
--
-- 末尾另带平台样板的一对 message_outbox / message_inbox（不属于上面列的四项，四个
-- 库各自带一份），它是本服务发审计事件的前半截。
--
-- 金额一律使用 BIGINT 保存最小货币单位（分），旧系统 float64「元」按方案 8.2 换算。
-- 只建结构，不导入旧数据；旧 Mongo 文档通过各表的 legacy_id 回溯。

BEGIN;

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
-- 饮品
-- ============================================================

-- 老系统 drinks 集合就是每台设备一行饮品（后台那个表单里 device_id 与 manufacturer_id
-- 并存，两者不是二选一）。V2 起初把它拆成「厂商级目录 drinks + 供应关系 device_drinks」，
-- 拆完之后没有任何入口能把目录行挂到设备上——设备详情那一屏永远是空的。现在按老系统的
-- 形状并回来：设备直接存在饮品行上，价格就是这台设备上的售价，不再有「每机覆盖价 /
-- 目录价」两套说法，也没有单独的供应关系表。

-- (manufacturer_id, origin_id) 是厂商侧自然键，带上设备后才是完整的判重键（见索引一节）；
-- origin_id 允许为空，后台手工新建的饮品没有厂商侧 ID，这类行不参与同步判重，所以唯一性
-- 用部分索引。
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
    -- 这行饮品挂在哪台设备上。一台设备有 N 杯饮品，一对多直接建在这一列上，没有中间表。
    --
    -- 可空，因为库里可能已经有还没挂设备的行（本仓 dev 库就有两行）。挂设备这件事由
    -- 表单和接口强制，等库里没有空行之后再单独一版收紧成 NOT NULL。
    device_id UUID REFERENCES devices(id) ON DELETE CASCADE,
    CHECK (price > 0 OR (vip_price = 0 AND pickup_code_price = 0))
);

-- 设备是否供应这杯饮品、展示排序，都不另开列：本表已有 status（on_shelf / off_shelf）与
-- sort，语义相同，直接用。

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

-- 与 identity、merchant、coupon 三个集里的同名两表逐列一致，各库自带一份，
-- 跨库不共享表。
--
-- 本服务的 outbox 不是可选件：后台的咖啡余额调整属于方案 11.6 L893 必审清单，
-- 而审计记录的写法（platform/audit）就是「在业务事务内往自己的 outbox 追加一条
-- admin.operation.logged」，由 relay 投到 RabbitMQ、再落到身份库的
-- admin_operation_logs。没有这张表，余额调整就没有留痕的地方。
--
-- 这里是全新库，lease 列直接建在表里，所以不需要 identity 集里那组
-- ALTER TABLE ADD COLUMN IF NOT EXISTS（那组是为了升级早期 schema 建出来的表）。
-- 列名和顺序保持不变，十一份拷贝才能逐列对得起来。

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

-- relay 取待投递消息、以及续租，都只看未发布的行。
CREATE INDEX message_outbox_pending_idx
    ON message_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;

CREATE INDEX message_outbox_lease_idx
    ON message_outbox (lease_until)
    WHERE published_at IS NULL;

-- <<< message-tables:end <<<

-- ============================================================
-- 索引
-- ============================================================

CREATE INDEX devices_manufacturer_idx ON devices (manufacturer_id, status);
CREATE INDEX devices_store_idx ON devices (store_id, status);
CREATE INDEX device_payment_methods_device_idx ON device_payment_methods (device_id, sort_order);
CREATE INDEX drinks_manufacturer_idx ON drinks (manufacturer_id, status);
CREATE INDEX device_events_device_idx ON device_events (device_id, received_at);
CREATE INDEX device_events_type_idx ON device_events (event_type, received_at);
CREATE INDEX device_balance_ledger_device_idx ON device_balance_ledger (device_id, created_at);

-- 饮品判重键带上设备：同一款饮品在 N 台设备上就是 N 行，这是这个模型的应有之义。厂商侧同步
-- 将来按 (设备, 厂商, 厂商侧 ID) 判重，而不是原来那个全局唯一的厂商侧 ID。
CREATE UNIQUE INDEX drinks_device_origin_unique
    ON drinks (device_id, manufacturer_id, origin_id)
    WHERE origin_id <> '';

-- 设备详情那一屏就是按设备取饮品，排序列与 devices_store_idx 同形。
CREATE INDEX drinks_device_idx ON drinks (device_id, status);

-- 同一请求号只允许一条账变：旧库这张表没有业务唯一键，重试一次充值就多一条无法分辨的
-- 流水，而提货扣减那条路径根本不写流水。
CREATE UNIQUE INDEX device_balance_ledger_one_per_request
    ON device_balance_ledger (request_id)
    WHERE request_id <> '';

-- 厂商回调去重。
CREATE UNIQUE INDEX device_events_external_id_unique
    ON device_events (external_event_id)
    WHERE external_event_id IS NOT NULL;

-- ============================================================
-- 表与关键字段的中文注释
--
-- 与建表语句放在同一个文件里：每个集只有这一个文件，改了列就得在同一屏里把它的注释一起
-- 改掉，不会再出现「列改了这个文件没改」这种两处各说各话的情况。
-- ============================================================

COMMENT ON TABLE manufacturers IS '设备厂商';
COMMENT ON COLUMN manufacturers.id IS '主键';
COMMENT ON COLUMN manufacturers.legacy_id IS '旧系统 MongoDB ObjectID，仅用于迁移对账';
COMMENT ON COLUMN manufacturers.code IS '厂商编码，稳定业务标识，创建后不可修改';
COMMENT ON COLUMN manufacturers.name IS '厂商名称';
COMMENT ON COLUMN manufacturers.contact_name IS '联系人';
COMMENT ON COLUMN manufacturers.contact_phone IS '联系电话';
COMMENT ON COLUMN manufacturers.status IS '状态：active=启用，disabled=停用';
COMMENT ON COLUMN manufacturers.created_at IS '创建时间';
COMMENT ON COLUMN manufacturers.updated_at IS '更新时间';

COMMENT ON TABLE manufacturer_credentials IS '厂商接入凭据与访问令牌';
COMMENT ON COLUMN manufacturer_credentials.manufacturer_id IS '厂商 ID，引用本库 manufacturers';
COMMENT ON COLUMN manufacturer_credentials.api_base_url IS '生产环境接口地址';
COMMENT ON COLUMN manufacturer_credentials.test_api_base_url IS '测试环境接口地址';
COMMENT ON COLUMN manufacturer_credentials.username IS '厂商接口账号';
COMMENT ON COLUMN manufacturer_credentials.user_secret IS '厂商接口密钥，不回传前端';
COMMENT ON COLUMN manufacturer_credentials.signing_secret IS '回调签名校验密钥';
COMMENT ON COLUMN manufacturer_credentials.access_token IS '当前访问令牌';
COMMENT ON COLUMN manufacturer_credentials.token_expires_at IS '令牌过期时间';
COMMENT ON COLUMN manufacturer_credentials.created_at IS '创建时间';
COMMENT ON COLUMN manufacturer_credentials.updated_at IS '更新时间';

COMMENT ON TABLE devices IS '咖啡机设备主数据';
COMMENT ON COLUMN devices.id IS '主键';
COMMENT ON COLUMN devices.legacy_id IS '旧系统 MongoDB ObjectID，仅用于迁移对账';
COMMENT ON COLUMN devices.serial_unique IS '设备序列号，全局唯一';
COMMENT ON COLUMN devices.device_name IS '设备名称';
COMMENT ON COLUMN devices.manufacturer_id IS '厂商 ID，引用本库 manufacturers';
COMMENT ON COLUMN devices.store_id IS '部署点位 ID，属于商户服务，仅作跨库值引用';
COMMENT ON COLUMN devices.status IS '是否启用：active=启用，disabled=停用';
COMMENT ON COLUMN devices.vendor_online IS '设备是否在线，由厂商上报；NULL 表示从未同步过，不等于离线';
COMMENT ON COLUMN devices.last_synced_at IS '最近一次厂商同步时间';
COMMENT ON COLUMN devices.version_number IS '设备版本号';
COMMENT ON COLUMN devices.android_version IS 'Android 系统版本';
COMMENT ON COLUMN devices.main_board_version IS '主板版本';
COMMENT ON COLUMN devices.last_fault_code IS '最近一次故障码';
COMMENT ON COLUMN devices.last_fault_message IS '最近一次故障描述';
COMMENT ON COLUMN devices.last_fault_at IS '最近一次故障时间';
COMMENT ON COLUMN devices.last_active_at IS '最近一次活跃时间';
COMMENT ON COLUMN devices.pickup_password IS '店员打咖啡用的静态验证码';
COMMENT ON COLUMN devices.coffee_balance IS '咖啡余额，单位为分';
COMMENT ON COLUMN devices.show_vip IS '是否展示会员价';
COMMENT ON COLUMN devices.enable_coupon_verification IS '是否允许核销会员价体验券';
COMMENT ON COLUMN devices.warranty_end_at IS '保修到期时间';
COMMENT ON COLUMN devices.qrcode_type IS '二维码类型：miniprogram=小程序码，regular=常规码';
COMMENT ON COLUMN devices.regular_qrcode_payment_method IS '常规码使用的支付渠道：fengxuan_wanlian=丰选万联，youlian=友联';
COMMENT ON COLUMN devices.created_at IS '创建时间';
COMMENT ON COLUMN devices.updated_at IS '更新时间';

COMMENT ON TABLE device_payment_methods IS '设备支持的支付方式';
COMMENT ON COLUMN device_payment_methods.id IS '主键';
COMMENT ON COLUMN device_payment_methods.device_id IS '设备 ID，引用本库 devices';
COMMENT ON COLUMN device_payment_methods.payment_method_id IS '支付方式 ID，属于支付服务，仅作跨库值引用';
COMMENT ON COLUMN device_payment_methods.sort_order IS '展示排序，值越小越靠前';
COMMENT ON COLUMN device_payment_methods.created_at IS '创建时间';

COMMENT ON TABLE drinks IS '饮品目录，按厂商维护';
COMMENT ON COLUMN drinks.id IS '主键';
COMMENT ON COLUMN drinks.legacy_id IS '旧系统 MongoDB ObjectID，仅用于迁移对账';
COMMENT ON COLUMN drinks.manufacturer_id IS '厂商 ID，引用本库 manufacturers';
COMMENT ON COLUMN drinks.origin_id IS '厂商侧饮品 ID；为空表示后台手工新建、不参与同步判重的饮品';
COMMENT ON COLUMN drinks.product_num IS '制作饮品用的商品编号';
COMMENT ON COLUMN drinks.product_name IS '饮品名称';
COMMENT ON COLUMN drinks.en_name IS '饮品英文名称';
COMMENT ON COLUMN drinks.drink_type IS '饮品类型：milk_coffee=奶咖，black_coffee=黑咖，other=其他';
COMMENT ON COLUMN drinks.product_img IS '饮品图片地址';
COMMENT ON COLUMN drinks.product_desc IS '饮品描述';
COMMENT ON COLUMN drinks.price IS '原价，单位为分';
COMMENT ON COLUMN drinks.vip_price IS '会员价，单位为分';
COMMENT ON COLUMN drinks.pickup_code_price IS '提货码价，单位为分';
COMMENT ON COLUMN drinks.status IS '上下架状态：on_shelf=上架，off_shelf=下架';
COMMENT ON COLUMN drinks.sort IS '展示排序，值越小越靠前';
COMMENT ON COLUMN drinks.created_at IS '创建时间';
COMMENT ON COLUMN drinks.updated_at IS '更新时间';
COMMENT ON COLUMN drinks.device_id IS '设备 ID，引用本库 devices；为空表示这行还没挂到设备上';

COMMENT ON TABLE device_events IS '设备事件日志';
COMMENT ON COLUMN device_events.id IS '主键';
COMMENT ON COLUMN device_events.legacy_id IS '旧系统 MongoDB ObjectID，仅用于迁移对账';
COMMENT ON COLUMN device_events.device_id IS '设备 ID，引用本库 devices';
COMMENT ON COLUMN device_events.source IS '事件来源：vendor=厂商，machine=机器，admin=后台，system=系统';
COMMENT ON COLUMN device_events.event_type IS '事件类型，自由字符串，例如 device_store_changed';
COMMENT ON COLUMN device_events.occurred_at IS '事件发生时间';
COMMENT ON COLUMN device_events.received_at IS '事件接收时间';
COMMENT ON COLUMN device_events.external_event_id IS '厂商侧事件 ID，用于回调去重';
COMMENT ON COLUMN device_events.error_code IS '错误码';
COMMENT ON COLUMN device_events.error_message IS '错误描述';
COMMENT ON COLUMN device_events.detail IS '事件可读摘要，例如设备更换点位的前后 store_id';
COMMENT ON COLUMN device_events.payload IS '厂商原始报文';

COMMENT ON TABLE device_balance_ledger IS '设备余额账变流水，只允许追加';
COMMENT ON COLUMN device_balance_ledger.id IS '主键';
COMMENT ON COLUMN device_balance_ledger.legacy_id IS '旧系统 MongoDB ObjectID，仅用于迁移对账';
COMMENT ON COLUMN device_balance_ledger.device_id IS '设备 ID，引用本库 devices';
COMMENT ON COLUMN device_balance_ledger.type IS '账变类型：recharge=充值，deduct=扣减，adjust=调整，reverse=冲正';
COMMENT ON COLUMN device_balance_ledger.amount IS '变动金额，单位为分；有符号，充值为正、扣减为负';
COMMENT ON COLUMN device_balance_ledger.balance_after IS '变动后余额，单位为分';
COMMENT ON COLUMN device_balance_ledger.reverses_entry_id IS '冲正对应的原始流水 ID，引用本表';
COMMENT ON COLUMN device_balance_ledger.reference_type IS '账变起因的对象类型，例如提货单、后台充值单；本列记的是「为什么余额变了」，与事件日志不同';
COMMENT ON COLUMN device_balance_ledger.reference_id IS '账变起因的对象 ID，仅作跨库值引用';
COMMENT ON COLUMN device_balance_ledger.request_id IS '幂等请求号或业务单号，同一请求号只落一条流水';
COMMENT ON COLUMN device_balance_ledger.remark IS '备注';
COMMENT ON COLUMN device_balance_ledger.operator_id IS '操作人 ID，属于身份服务，仅作跨库值引用';
COMMENT ON COLUMN device_balance_ledger.operator_name IS '操作人名称';
COMMENT ON COLUMN device_balance_ledger.created_at IS '创建时间';

COMMENT ON TABLE message_outbox IS '待发布消息事件，审计与通知在业务事务内追加到这里';
COMMENT ON COLUMN message_outbox.event_id IS '事件唯一 ID';
COMMENT ON COLUMN message_outbox.event_type IS '事件类型，也是 RabbitMQ 的 routing key';
COMMENT ON COLUMN message_outbox.event_version IS '事件版本，载荷形状不兼容变更时递增';
COMMENT ON COLUMN message_outbox.trace_id IS '关联调用链 ID';
COMMENT ON COLUMN message_outbox.payload IS '事件内容，JSON 序列化后的字节';
COMMENT ON COLUMN message_outbox.attempts IS '发布尝试次数';
COMMENT ON COLUMN message_outbox.next_attempt_at IS '下次投递时间，失败重试据此退避';
COMMENT ON COLUMN message_outbox.published_at IS '成功发布时间；为空表示尚未投出';
COMMENT ON COLUMN message_outbox.lease_owner IS '当前投递方实例标识';
COMMENT ON COLUMN message_outbox.lease_token IS '投递租约令牌，防止两个副本重复投递';
COMMENT ON COLUMN message_outbox.lease_until IS '投递租约截止时间';
COMMENT ON COLUMN message_outbox.last_error IS '最近一次投递失败原因';
COMMENT ON COLUMN message_outbox.created_at IS '创建时间';

COMMENT ON TABLE message_inbox IS '已接收消息事件及消费租约';
COMMENT ON COLUMN message_inbox.event_id IS '已接收事件唯一 ID，用于消费去重';
COMMENT ON COLUMN message_inbox.claimed_at IS '首次领取消费时间';
COMMENT ON COLUMN message_inbox.lease_owner IS '当前消费方实例标识';
COMMENT ON COLUMN message_inbox.lease_token IS '消费租约令牌，防止两个副本重复消费';
COMMENT ON COLUMN message_inbox.lease_until IS '消费租约截止时间';
COMMENT ON COLUMN message_inbox.completed_at IS '成功完成消费时间';

COMMIT;
