-- merchant: 商户库表结构
--
-- 单库时代商户域的建表在 001_init.sql（merchants）与 004_brands_stores.sql
-- （brands / stores / 两张审核表）。拆库后 merchant 库独占这一域，这里把它们整理成一份。
--
-- 库内三条外键原样保留，级联语义一概不变：
--   brands.merchant_id -> merchants  ON DELETE CASCADE
--   stores.merchant_id -> merchants  ON DELETE CASCADE
--   stores.brand_id    -> brands     ON DELETE RESTRICT
--
-- 库外指向这里的引用已经不存在：单库时代 merchant_users.merchant_id 与
-- admin_operation_logs.merchant_id 都指向 merchants，拆库时把外键去掉、只留列
-- （那一步在 identity 集里）。
--
-- 门店删除前「品牌下还有没有门店」的判断由 merchant-service 在库内自己查
-- （stores.brand_id RESTRICT + service 层显式检查），不再需要跨库读。
--
-- 本文件不带 goose 的 Down 段：platform/database/migrate 把整个文件丢给一次 Exec、
-- 不识别 goose 指令，带上就会在同一个事务里建完表再删掉，而且不报错。

BEGIN;

-- ============================================================
-- 商户（租户）
-- ============================================================

CREATE TABLE merchants (
  id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  name          TEXT        NOT NULL,
  status        TEXT        NOT NULL DEFAULT 'pending',
  contact_name  TEXT,
  contact_phone TEXT,
  contact_email TEXT,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  merchants        IS '商户主体（即多租户体系的租户）';
COMMENT ON COLUMN merchants.status IS '商户状态：pending=待审核 active=正常 suspended=已暂停';

-- ============================================================
-- 自动更新 updated_at 的触发器函数
-- ============================================================

CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at = NOW();
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_merchants_updated_at
  BEFORE UPDATE ON merchants
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================
-- 品牌
-- ============================================================

CREATE TABLE brands (
  id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  merchant_id  UUID        NOT NULL REFERENCES merchants(id) ON DELETE CASCADE,
  name         TEXT        NOT NULL,
  logo         TEXT        NOT NULL DEFAULT '',
  banner       TEXT        NOT NULL DEFAULT '',
  description  TEXT        NOT NULL DEFAULT '',
  status       TEXT        NOT NULL DEFAULT 'active',
  audit_status TEXT        NOT NULL DEFAULT 'pending',
  audit_remark TEXT        NOT NULL DEFAULT '',
  audit_at     TIMESTAMPTZ,
  audit_by     TEXT        NOT NULL DEFAULT '',
  remark       TEXT        NOT NULL DEFAULT '',
  visible      BOOLEAN     NOT NULL DEFAULT TRUE,
  sort         INTEGER     NOT NULL DEFAULT 0,
  created_by   TEXT        NOT NULL DEFAULT '',
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (merchant_id, name)
);

COMMENT ON TABLE  brands              IS '品牌：隶属商户，门店挂在品牌下';
COMMENT ON COLUMN brands.status       IS '品牌状态：active=正常 disabled=禁用';
COMMENT ON COLUMN brands.audit_status IS '审核状态：pending=待审核 approved=已通过 rejected=已拒绝';
COMMENT ON COLUMN brands.audit_by     IS '审核人（平台管理员 ID 或名称）';
COMMENT ON COLUMN brands.remark       IS '后台备注，商户端不可见';
COMMENT ON COLUMN brands.visible      IS '是否在 C 端可见';
COMMENT ON COLUMN brands.created_by   IS '创建人（平台管理员或商户账号标识）';

CREATE INDEX idx_brands_merchant ON brands(merchant_id);
CREATE INDEX idx_brands_audit    ON brands(audit_status);

CREATE TRIGGER trg_brands_updated_at
  BEFORE UPDATE ON brands
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================
-- 门店
--
-- 后六列是两次后续追加，合并在这一份里，所以历史的「先建表再 ALTER」不再可见，
-- 但两批列各自的理由仍然成立，分别记在下面。
-- ============================================================

-- 【区划编码】province_code / city_code / district_code
--
-- 名字不是稳定的键。数据源一过期就会冒出「下城区」这种早已撤销的区名（NewCoffee
-- 自带的 pca-code.json 里就是这样），而库里只写 district='下城区' 时，将来没有任何
-- 确定性的办法把它迁到新的拱墅区；有 330103 至少能识别、能对照官方变更记录迁移——
-- 编码本身也会废止，但废止是可识别的，自由字符串不能识别、不能校验、不能迁移。
-- 全国区县还有 28 组重名，只有「省 + 市 + 区」三元组能定位；名字打错一个字，这行
-- 数据就永久无法修复。而且编码是事后算不出来的，名字反倒随时能从编码推出来。
--
-- 与名称并列而不是替代名称：列表和详情直接展示名字，回填和迁移用编码。
--
-- 允许为空，且**默认就是空**：只做过一次尽力而为的回填（cmd/backfill-region），
-- 历史自由文本（比如只写「北京」而不是「北京市」）匹配不上的留空；请求里没带编码
-- 时也保持空。不建字典表、不加外键：本轮唯一的消费者是后台的表单，等小程序改用
-- 同一份主数据时再考虑下发接口。
--
-- 【订货客户列】customer_code / dms_code / customer_type
--
-- 订货系统 xlsx 的「客户」页就是门店。它上面这四个字段——客户类型、点位名称、客户编码、
-- DMS 编码——前三个都不是门店已有的列，「点位名称」是 stores.name。所以加三列，换一张表
-- 是不划算的：一张独立的 customer 表会和 stores 一对一，两个地方可以改同一个事实，而
-- 「这家店叫什么」在任何一处改完都得同步另一处。
--
-- 这几列**只在本库**：拆出去的订货/库存服务曾经不复制它们（它的出库单、订货单上只存
-- store_id 值引用，要看客户名或客户编码就回本库读），而那个服务已从 V2 整体删掉。
-- xlsx 的出库、订货两页把客户四列都摊在单据上，那是一张 Excel 为了自洽，不是数据模型——
-- 将来单独拆出去的订货服务照这条办。
--
-- 客户类型**不做 CHECK 枚举**：xlsx 只给了列名没给取值，现在猜一组值写进约束，等运营
-- 报出真实分类时就得再开一刀去改约束。先用自由文本，等取值稳定了再收。

CREATE TABLE stores (
  id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  merchant_id    UUID        NOT NULL REFERENCES merchants(id) ON DELETE CASCADE,
  brand_id       UUID        NOT NULL REFERENCES brands(id) ON DELETE RESTRICT,
  name           TEXT        NOT NULL,
  logo           TEXT        NOT NULL DEFAULT '',
  photos         TEXT[]      NOT NULL DEFAULT '{}',
  province       TEXT        NOT NULL DEFAULT '',
  city           TEXT        NOT NULL DEFAULT '',
  district       TEXT        NOT NULL DEFAULT '',
  address        TEXT        NOT NULL DEFAULT '',
  longitude      DOUBLE PRECISION,
  latitude       DOUBLE PRECISION,
  phone          TEXT        NOT NULL DEFAULT '',
  contact_name   TEXT        NOT NULL DEFAULT '',
  contact_phone  TEXT        NOT NULL DEFAULT '',
  detail         TEXT        NOT NULL DEFAULT '',
  business_hours TEXT        NOT NULL DEFAULT '',
  status         TEXT        NOT NULL DEFAULT 'active',
  audit_status   TEXT        NOT NULL DEFAULT 'pending',
  audit_remark   TEXT        NOT NULL DEFAULT '',
  audit_at       TIMESTAMPTZ,
  audit_by       TEXT        NOT NULL DEFAULT '',
  remark         TEXT        NOT NULL DEFAULT '',
  visible        BOOLEAN     NOT NULL DEFAULT TRUE,
  created_by     TEXT        NOT NULL DEFAULT '',
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  province_code  TEXT        NOT NULL DEFAULT '',
  city_code      TEXT        NOT NULL DEFAULT '',
  district_code  TEXT        NOT NULL DEFAULT '',
  customer_code  TEXT        NOT NULL DEFAULT '',
  dms_code       TEXT        NOT NULL DEFAULT '',
  customer_type  TEXT        NOT NULL DEFAULT ''
);

COMMENT ON TABLE  stores                IS '门店：隶属商户与品牌';
COMMENT ON COLUMN stores.photos         IS '门店照片 URL 列表';
COMMENT ON COLUMN stores.longitude      IS '经度，未采集时 NULL';
COMMENT ON COLUMN stores.latitude       IS '纬度，未采集时 NULL';
COMMENT ON COLUMN stores.business_hours IS '营业时间，如 09:00-22:00';
COMMENT ON COLUMN stores.status         IS '门店状态：active=正常 disabled=禁用';
COMMENT ON COLUMN stores.audit_status   IS '审核状态：pending=待审核 approved=已通过 rejected=已拒绝';
COMMENT ON COLUMN stores.remark         IS '后台备注，商户端不可见';
COMMENT ON COLUMN stores.province_code IS '省级区划编码（GB/T 2260），与 province 名称并存；回填不到时为空';
COMMENT ON COLUMN stores.city_code IS '市级区划编码，与 city 名称并存；回填不到时为空';
COMMENT ON COLUMN stores.district_code IS '区县级区划编码，与 district 名称并存；回填不到时为空';
COMMENT ON COLUMN stores.customer_code IS '客户编码，订货系统对账用的业务键（xlsx「客户」页）；未编码的历史门店为空串';
COMMENT ON COLUMN stores.dms_code IS 'DMS 编码，供应商/经销商的系统号；未接入的门店为空串';
COMMENT ON COLUMN stores.customer_type IS '客户类型（xlsx「客户」页）；取值尚未定型，先存自由文本，稳定后再收成枚举';

CREATE INDEX idx_stores_merchant ON stores(merchant_id);
CREATE INDEX idx_stores_brand    ON stores(brand_id);
CREATE INDEX idx_stores_audit    ON stores(audit_status);

-- 编码唯一，但空串不参与：编码是对账的键，重复意味着两张单子指向同一个客户；
-- 而还没编码的门店有几百家，它们的空串必须能共存。
CREATE UNIQUE INDEX stores_customer_code_unique
    ON stores (customer_code) WHERE customer_code <> '';

CREATE UNIQUE INDEX stores_dms_code_unique
    ON stores (dms_code) WHERE dms_code <> '';

CREATE TRIGGER trg_stores_updated_at
  BEFORE UPDATE ON stores
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================
-- 审核记录：商户端提交创建/修改 → 平台审核
-- old_data/new_data 存提交快照，JSONB 便于差异展示
-- ============================================================

CREATE TABLE brand_audit_records (
  id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  brand_id      UUID        NOT NULL REFERENCES brands(id) ON DELETE CASCADE,
  type          TEXT        NOT NULL,
  status        TEXT        NOT NULL DEFAULT 'pending',
  old_data      JSONB,
  new_data      JSONB       NOT NULL,
  submit_remark TEXT        NOT NULL DEFAULT '',
  audit_remark  TEXT        NOT NULL DEFAULT '',
  audit_by      TEXT        NOT NULL DEFAULT '',
  audit_at      TIMESTAMPTZ,
  submitted_by  TEXT        NOT NULL DEFAULT '',
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  brand_audit_records              IS '品牌审核记录：一次提交一条';
COMMENT ON COLUMN brand_audit_records.type         IS '操作类型：create=创建 update=修改';
COMMENT ON COLUMN brand_audit_records.status       IS '审核状态：pending=待审核 approved=已通过 rejected=已拒绝';
COMMENT ON COLUMN brand_audit_records.old_data     IS '修改前快照，创建时为 NULL';
COMMENT ON COLUMN brand_audit_records.new_data     IS '提交的数据快照';
COMMENT ON COLUMN brand_audit_records.submitted_by IS '提交人（商户账号 ID）';

CREATE INDEX idx_brand_audit_brand  ON brand_audit_records(brand_id);
CREATE INDEX idx_brand_audit_status ON brand_audit_records(status);

CREATE TABLE store_audit_records (
  id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  store_id      UUID        NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
  type          TEXT        NOT NULL,
  status        TEXT        NOT NULL DEFAULT 'pending',
  old_data      JSONB,
  new_data      JSONB       NOT NULL,
  submit_remark TEXT        NOT NULL DEFAULT '',
  audit_remark  TEXT        NOT NULL DEFAULT '',
  audit_by      TEXT        NOT NULL DEFAULT '',
  audit_at      TIMESTAMPTZ,
  submitted_by  TEXT        NOT NULL DEFAULT '',
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  store_audit_records              IS '门店审核记录：一次提交一条';
COMMENT ON COLUMN store_audit_records.type         IS '操作类型：create=创建 update=修改';
COMMENT ON COLUMN store_audit_records.status       IS '审核状态：pending=待审核 approved=已通过 rejected=已拒绝';
COMMENT ON COLUMN store_audit_records.old_data     IS '修改前快照，创建时为 NULL';
COMMENT ON COLUMN store_audit_records.new_data     IS '提交的数据快照';
COMMENT ON COLUMN store_audit_records.submitted_by IS '提交人（商户账号 ID）';

CREATE INDEX idx_store_audit_store  ON store_audit_records(store_id);
CREATE INDEX idx_store_audit_status ON store_audit_records(status);

-- ============================================================
-- outbox / inbox 表（商户库）
-- ============================================================
--
-- 两个库各自需要一对 outbox / inbox，跨库写不共享表。
--
-- 顺序是硬要求：ALTER 必须先于 CREATE INDEX，否则对「由早期 schema 建出来、
-- 没有 lease 列」的表执行本文件时会因缺列而失败。这里保留 IF NOT EXISTS 与那八条
-- ALTER，正是因为 merchant 库可能是拆库前就存在的那一个。
--
-- 本段在十一个集里是同一份拷贝，但只有 identity 与 merchant 这两份带 ALTER 兜底
-- （它们的表先于租约列存在）；其余九个域直接写进 CREATE TABLE。列与索引的写法不要
-- 顺手调整，`migrations/migrations_test.go` 逐列比对，且逐字节比对这两份。

-- >>> message-tables:begin >>>

CREATE TABLE IF NOT EXISTS message_outbox (
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

CREATE TABLE IF NOT EXISTS message_inbox (
    event_id TEXT PRIMARY KEY CHECK (char_length(trim(event_id)) > 0),
    claimed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_owner TEXT,
    lease_token TEXT,
    lease_until TIMESTAMPTZ,
    completed_at TIMESTAMPTZ
);

-- Add columns before creating indexes so this migration also works on tables
-- created by the original schema, which did not include lease columns.
ALTER TABLE message_outbox ADD COLUMN IF NOT EXISTS lease_owner TEXT;
ALTER TABLE message_outbox ADD COLUMN IF NOT EXISTS lease_token TEXT;
ALTER TABLE message_outbox ADD COLUMN IF NOT EXISTS lease_until TIMESTAMPTZ;
ALTER TABLE message_outbox ADD COLUMN IF NOT EXISTS last_error TEXT;
ALTER TABLE message_inbox ADD COLUMN IF NOT EXISTS lease_owner TEXT;
ALTER TABLE message_inbox ADD COLUMN IF NOT EXISTS lease_token TEXT;
ALTER TABLE message_inbox ADD COLUMN IF NOT EXISTS lease_until TIMESTAMPTZ;
ALTER TABLE message_inbox ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS message_outbox_pending_idx
    ON message_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;

CREATE INDEX IF NOT EXISTS message_outbox_lease_idx
    ON message_outbox (lease_until)
    WHERE published_at IS NULL;

-- <<< message-tables:end <<<

COMMIT;
