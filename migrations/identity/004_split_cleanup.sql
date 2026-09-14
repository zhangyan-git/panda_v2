-- identity/004: 拆库收尾——把商户域从身份库摘掉
--
-- 只对「单库时代建出来的身份库」有实际动作：
--
--   1. 去掉两条跨库外键。两列都保留，改成软指针：
--        merchant_users.merchant_id      -> merchants   （列、NOT NULL、
--          (merchant_id, username) 唯一约束全部保留，归属改由 merchant-service
--          经 gRPC 校验）
--        admin_operation_logs.merchant_id -> merchants  （006 建，本意就是
--          「商户删除后置空、日志仍可查」，拆库后连置空都不需要，只留 ID 用于筛选）
--   2. 删除商户域五张表：merchants / brands / stores / brand_audit_records /
--      store_audit_records。数据在跑本文件之前已经由 psql COPY 搬到 merchant 库
--      并核对过行数。
--
-- 对全新身份库是空操作：merchant 那套迁移里才有这些表，identity/001 里也没有
-- 那两条外键。所以本文件可以无条件留在迁移集里。
--
-- 执行时机是硬约束：必须在 merchant 库建好、数据搬完核对过、merchant-service
-- 已经指过去之后。在那之前回滚只是把连接串翻回去，零风险；跑完本文件就没有
-- 回头路，所以执行前先 pg_dump 身份库留档。
--
-- 删表顺序由外键决定：审核表 -> stores（引用 brands）-> brands（引用 merchants）
-- -> merchants。

BEGIN;

ALTER TABLE merchant_users
  DROP CONSTRAINT IF EXISTS merchant_users_merchant_id_fkey;

ALTER TABLE admin_operation_logs
  DROP CONSTRAINT IF EXISTS admin_operation_logs_merchant_id_fkey;

DROP TABLE IF EXISTS store_audit_records;
DROP TABLE IF EXISTS brand_audit_records;
DROP TABLE IF EXISTS stores;
DROP TABLE IF EXISTS brands;
DROP TABLE IF EXISTS merchants;

COMMIT;
