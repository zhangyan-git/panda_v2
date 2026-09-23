-- identity/037: 商户账号的数据范围从「单点」变成「一组目标」
-- 可重复执行。
--
-- 档位不变，还是 merchant / brand / store 三选一（scope_type 一列不动）；变的是
-- brand 与 store 两档不再限定**一个**目标：可以把几个品牌、或几家门店一起授权给
-- 同一个账号，展开出来的点位集合是它们的并集。
--
-- 这一列的身份是「值引用」而不是外键：scope_ids 指向 merchant 库的 brands / stores，
-- 跨库不建外键，归属由 user-service 在写入时逐个校验（每个目标都必须属于这个账号的
-- 商户），品牌/门店被删时由 merchant-service 发起、user-service 把那个 id 从数组里摘掉。
--
-- # 空数组不是 NULL
--
-- scope_type=merchant 时 scope_ids 是空数组，而不是 NULL，两者在展开处的读法完全相反：
--
--   * 空数组 → `= ANY('{}')` 恒假 → 这个账号一个点位都看不见；
--   * NULL   → 下游那几条判 nil 的谓词把它读成「调用方没传过滤条件」→ 看得见全部。
--
-- 所以列是 NOT NULL DEFAULT '{}'：任何一条写入路径都构造不出 NULL，这个区别就只剩
-- 「空数组」一种写法。同样的理由写在 platform/auth/store_scope.go 的 WithStoreScope 上
-- （nil → 空切片的归一化），两处是同一条不变式的两端。
--
-- # 旧值怎么搬
--
-- 一条旧行搬成一个单元素数组，语义与迁移前逐字相同：scope_id 为空的行（merchant 档）
-- 搬成空数组。搬完就把 scope_id 列整个删掉——留着它，库里就有了两份范围，而哪一份算数
-- 只能靠读代码才知道。

BEGIN;

ALTER TABLE merchant_users ADD COLUMN IF NOT EXISTS scope_ids UUID[] NOT NULL DEFAULT '{}';

UPDATE merchant_users
SET scope_ids = ARRAY[scope_id]
WHERE scope_id IS NOT NULL
  AND scope_type IN ('brand', 'store')
  AND cardinality(scope_ids) = 0;

-- 列都搬空了再删。索引跟着列走，单独换一个：ResetScopeByTarget 现在问的是
-- 「哪个账号的范围里还带着这个 id」，那是数组包含，B-tree 帮不上忙。
ALTER TABLE merchant_users DROP COLUMN IF EXISTS scope_id;
DROP INDEX IF EXISTS idx_merchant_users_scope;
CREATE INDEX IF NOT EXISTS idx_merchant_users_scope_ids ON merchant_users USING GIN (scope_ids);

COMMENT ON COLUMN merchant_users.scope_type  IS '数据范围档位：merchant=本商户全部 brand=指定品牌旗下 store=指定门店；目标见 scope_ids';
COMMENT ON COLUMN merchant_users.scope_ids   IS '范围目标 ID 数组（多态指向 merchant 库的 brands/stores，不建外键）：brand 档是品牌 ID，store 档是门店 ID，merchant 档为空数组。空数组=看不见任何点位，与 NULL 不是一回事';

COMMIT;
