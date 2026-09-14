-- 004: 品牌管理 / 门店管理 / 商户账号数据范围（库表设计）
--
-- 对齐老项目（panda_serve MongoDB 模型 brand.go / store.go / merchant_account.go）
-- 的核心字段，落到 panda_v2 的关系模型约定：UUID 主键、TEXT 状态枚举、
-- set_updated_at() 触发器、外键级联策略显式声明。
--
-- 本轮只建表，不含权限码/菜单（随页面实现一起进迁移），不含接口与页面。
--
-- 范围取舍（老项目有、本轮不做）：
--   - 分类管理（category_ids）：独立菜单，未在本次截图范围；届时补 categories 表与关联表
--   - 门店自动赠送 VIP（auto_gift_vip 等）、小程序码、领券 H5、dffl 对接：属会员/小程序模块
--   - 商户 API 对接字段（api_key 等）：属开放平台模块

BEGIN;

-- ============================================================
-- 品牌
-- ============================================================
CREATE TABLE IF NOT EXISTS brands (
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

CREATE INDEX IF NOT EXISTS idx_brands_merchant ON brands(merchant_id);
CREATE INDEX IF NOT EXISTS idx_brands_audit    ON brands(audit_status);

DROP TRIGGER IF EXISTS trg_brands_updated_at ON brands;
CREATE TRIGGER trg_brands_updated_at
  BEFORE UPDATE ON brands
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================
-- 门店
-- ============================================================
CREATE TABLE IF NOT EXISTS stores (
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

COMMENT ON TABLE  stores               IS '门店：隶属商户与品牌';
COMMENT ON COLUMN stores.photos        IS '门店照片 URL 列表';
COMMENT ON COLUMN stores.longitude     IS '经度，未采集时 NULL';
COMMENT ON COLUMN stores.latitude      IS '纬度，未采集时 NULL';
COMMENT ON COLUMN stores.business_hours IS '营业时间，如 09:00-22:00';
COMMENT ON COLUMN stores.status        IS '门店状态：active=正常 disabled=禁用';
COMMENT ON COLUMN stores.audit_status  IS '审核状态：pending=待审核 approved=已通过 rejected=已拒绝';
COMMENT ON COLUMN stores.remark        IS '后台备注，商户端不可见';

CREATE INDEX IF NOT EXISTS idx_stores_merchant ON stores(merchant_id);
CREATE INDEX IF NOT EXISTS idx_stores_brand    ON stores(brand_id);
CREATE INDEX IF NOT EXISTS idx_stores_audit    ON stores(audit_status);

DROP TRIGGER IF EXISTS trg_stores_updated_at ON stores;
CREATE TRIGGER trg_stores_updated_at
  BEFORE UPDATE ON stores
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================
-- 审核记录：商户端提交创建/修改 → 平台审核（老项目 BrandAuditRecord/StoreAuditRecord）
-- old_data/new_data 存提交快照，JSONB 便于差异展示
-- ============================================================
CREATE TABLE IF NOT EXISTS brand_audit_records (
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

COMMENT ON TABLE  brand_audit_records           IS '品牌审核记录：一次提交一条';
COMMENT ON COLUMN brand_audit_records.type      IS '操作类型：create=创建 update=修改';
COMMENT ON COLUMN brand_audit_records.status    IS '审核状态：pending=待审核 approved=已通过 rejected=已拒绝';
COMMENT ON COLUMN brand_audit_records.old_data  IS '修改前快照，创建时为 NULL';
COMMENT ON COLUMN brand_audit_records.new_data  IS '提交的数据快照';
COMMENT ON COLUMN brand_audit_records.submitted_by IS '提交人（商户账号 ID）';

CREATE INDEX IF NOT EXISTS idx_brand_audit_brand  ON brand_audit_records(brand_id);
CREATE INDEX IF NOT EXISTS idx_brand_audit_status ON brand_audit_records(status);

CREATE TABLE IF NOT EXISTS store_audit_records (
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

COMMENT ON TABLE  store_audit_records           IS '门店审核记录：一次提交一条';
COMMENT ON COLUMN store_audit_records.type      IS '操作类型：create=创建 update=修改';
COMMENT ON COLUMN store_audit_records.status    IS '审核状态：pending=待审核 approved=已通过 rejected=已拒绝';
COMMENT ON COLUMN store_audit_records.old_data  IS '修改前快照，创建时为 NULL';
COMMENT ON COLUMN store_audit_records.new_data  IS '提交的数据快照';
COMMENT ON COLUMN store_audit_records.submitted_by IS '提交人（商户账号 ID）';

CREATE INDEX IF NOT EXISTS idx_store_audit_store  ON store_audit_records(store_id);
CREATE INDEX IF NOT EXISTS idx_store_audit_status ON store_audit_records(status);

-- ============================================================
-- 商户账号扩展：管理员标记 + 数据范围
--
-- 范围模型（产品确认）：账号单点关联 商户/品牌/门店 三选一，
-- 关联哪一级就看该级旗下所有数据——
--   scope_type=merchant：看本商户旗下全部品牌与门店（scope_id 为 NULL）
--   scope_type=brand   ：看 scope_id 所指品牌及其下全部门店
--   scope_type=store   ：只看 scope_id 所指门店
-- scope_id 不做外键（多态指向 brands/stores 两张表），写入时由服务层
-- 校验归属（品牌/门店必须属于该账号的商户）；删除品牌/门店时服务层把
-- 指向它的账号回收为 scope_type=merchant。
-- ============================================================
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.columns
                 WHERE table_name = 'merchant_users' AND column_name = 'is_admin') THEN
    ALTER TABLE merchant_users
      ADD COLUMN is_admin      BOOLEAN     NOT NULL DEFAULT FALSE,
      ADD COLUMN scope_type    TEXT        NOT NULL DEFAULT 'merchant',
      ADD COLUMN scope_id      UUID,
      ADD COLUMN avatar        TEXT        NOT NULL DEFAULT '',
      ADD COLUMN last_login_at TIMESTAMPTZ,
      ADD COLUMN last_login_ip TEXT        NOT NULL DEFAULT '',
      ADD COLUMN login_count   INTEGER     NOT NULL DEFAULT 0;
  ELSE
    -- 收敛 004 早期草稿：permission_type + 范围关联表 → scope_type/scope_id 单点
    ALTER TABLE merchant_users DROP COLUMN IF EXISTS permission_type;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns
                   WHERE table_name = 'merchant_users' AND column_name = 'scope_type') THEN
      ALTER TABLE merchant_users ADD COLUMN scope_type TEXT NOT NULL DEFAULT 'merchant';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns
                   WHERE table_name = 'merchant_users' AND column_name = 'scope_id') THEN
      ALTER TABLE merchant_users ADD COLUMN scope_id UUID;
    END IF;
  END IF;
END $$;

-- 早期草稿的范围关联表作废
DROP TABLE IF EXISTS merchant_user_store_scope;
DROP TABLE IF EXISTS merchant_user_brand_scope;

COMMENT ON COLUMN merchant_users.is_admin      IS '是否商户管理员';
COMMENT ON COLUMN merchant_users.scope_type    IS '数据范围：merchant=本商户全部 brand=指定品牌旗下 store=指定门店';
COMMENT ON COLUMN merchant_users.scope_id      IS '范围目标 ID：scope_type=brand 时为品牌 ID，store 时为门店 ID，merchant 时为 NULL';
COMMENT ON COLUMN merchant_users.last_login_at IS '最后登录时间';
COMMENT ON COLUMN merchant_users.login_count   IS '登录次数';

CREATE INDEX IF NOT EXISTS idx_merchant_users_scope ON merchant_users(scope_type, scope_id);

COMMIT;
