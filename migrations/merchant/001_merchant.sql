-- merchant/001: 商户库表结构
--
-- 单库时代商户域的建表在 001_init.sql（merchants）与 004_brands_stores.sql
-- （brands / stores / 两张审核表）。拆库后 merchant 库独占这一域，
-- 这里把它们整理成一份。
--
-- 库内三条外键原样保留，级联语义一概不变：
--   brands.merchant_id -> merchants  ON DELETE CASCADE
--   stores.merchant_id -> merchants  ON DELETE CASCADE
--   stores.brand_id    -> brands     ON DELETE RESTRICT
--
-- 库外指向这里的引用已经不存在：单库时代 merchant_users.merchant_id 与
-- admin_operation_logs.merchant_id 都指向 merchants，拆库时由
-- identity/004_split_cleanup.sql 去掉外键、只留列。
--
-- 门店删除前「品牌下还有没有门店」的判断由 merchant-service 在库内自己查
-- （stores.brand_id RESTRICT + service 层显式检查），不再需要跨库读。

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
-- ============================================================

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
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  stores                IS '门店：隶属商户与品牌';
COMMENT ON COLUMN stores.photos         IS '门店照片 URL 列表';
COMMENT ON COLUMN stores.longitude      IS '经度，未采集时 NULL';
COMMENT ON COLUMN stores.latitude       IS '纬度，未采集时 NULL';
COMMENT ON COLUMN stores.business_hours IS '营业时间，如 09:00-22:00';
COMMENT ON COLUMN stores.status         IS '门店状态：active=正常 disabled=禁用';
COMMENT ON COLUMN stores.audit_status   IS '审核状态：pending=待审核 approved=已通过 rejected=已拒绝';
COMMENT ON COLUMN stores.remark         IS '后台备注，商户端不可见';

CREATE INDEX idx_stores_merchant ON stores(merchant_id);
CREATE INDEX idx_stores_brand    ON stores(brand_id);
CREATE INDEX idx_stores_audit    ON stores(audit_status);

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

COMMIT;
