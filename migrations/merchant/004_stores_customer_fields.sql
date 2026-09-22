-- merchant/004: stores 增加订货系统要的客户列（客户编码 / DMS 编码 / 客户类型）
--
-- 订货系统 xlsx 的「客户」页就是门店。它上面这四个字段——客户类型、点位名称、客户编码、
-- DMS 编码——前三个都不是门店已有的列，「点位名称」是 stores.name。所以加三列，换一张表
-- 是不划算的：一张独立的 customer 表会和 stores 一对一，两个地方可以改同一个事实，而
-- 「这家店叫什么」在任何一处改完都得同步另一处。
--
-- 这几列**只在本库**：拆出去的订货/库存服务曾经不复制它们（它的出库单、订货单上只存
-- store_id 值引用，要看客户名或客户编码就回本库读），而那个服务已从 V2 删掉（identity/036）。
-- xlsx 的出库、订货两页把客户四列都摊在单据上，那是一张 Excel 为了自洽，不是数据模型——
-- 将来单独拆出去的订货服务照这条办。
--
-- 两个编码都加**部分唯一索引**（空值不参与）：编码是拿来跟供应商、DMS 对账的键，重复
-- 意味着两张单子指向同一个客户；但历史门店还没有编码，全空串必须能共存。
--
-- 客户类型**不做 CHECK 枚举**：xlsx 只给了列名没给取值，现在猜一组值写进约束，等运营
-- 报出真实分类时就得再开一刀去改约束。先用自由文本，等取值稳定了再收。

BEGIN;

ALTER TABLE stores ADD COLUMN IF NOT EXISTS customer_code TEXT NOT NULL DEFAULT '';
ALTER TABLE stores ADD COLUMN IF NOT EXISTS dms_code      TEXT NOT NULL DEFAULT '';
ALTER TABLE stores ADD COLUMN IF NOT EXISTS customer_type TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN stores.customer_code IS '客户编码，订货系统对账用的业务键（xlsx「客户」页）；未编码的历史门店为空串';
COMMENT ON COLUMN stores.dms_code IS 'DMS 编码，供应商/经销商的系统号；未接入的门店为空串';
COMMENT ON COLUMN stores.customer_type IS '客户类型（xlsx「客户」页）；取值尚未定型，先存自由文本，稳定后再收成枚举';

-- 编码唯一，但空串不参与：编码是对账的键，重复意味着两张单子指向同一个客户；
-- 而还没编码的门店有几百家，它们的空串必须能共存。
CREATE UNIQUE INDEX IF NOT EXISTS stores_customer_code_unique
    ON stores (customer_code) WHERE customer_code <> '';

CREATE UNIQUE INDEX IF NOT EXISTS stores_dms_code_unique
    ON stores (dms_code) WHERE dms_code <> '';

COMMIT;
