-- 009: 删除商户域中零引用的孤儿表与跨域触发器
--
-- 背景：001 按老项目（panda_serve MongoDB）的模型预留了一套「商户侧 RBAC」，
-- 即 merchant_roles / merchant_permissions / merchant_role_permissions /
-- merchant_user_role_bindings 四张表，配套一个跨租户校验触发器
-- check_merchant_user_role_same_tenant()。
--
-- 这套设计在 panda_v2 里从未被采用：全仓 Go 代码对四张表的引用数为 0
-- （casbin_rule 承担了全部授权存储），dev 库中四张表也都是 0 行。
-- 平台侧权限已统一走 admin_* + casbin_rule，商户侧权限由账号上的
-- scope_type / scope_id 数据范围表达，不再有「商户角色」这一层。
--
-- 保留一张不做删除：admin_operation_logs（006 建）。它同样暂无写入方，
-- 但 006 的注释写明了明确的设计意图（后台业务操作审计），后续由事件
-- 消费者补齐写入，属于「待接线」而不是「已废弃」。
--
-- 触发器函数必须显式 DROP：删除表会连带删除表上的触发器，但函数本身
-- 不会随之消失，会留下一个指向已删除表的孤儿函数。
--
-- 不改 001：已应用的迁移保持原样，新库按 001→009 顺序执行后同样收敛到
-- 「四张表不存在、触发器函数不存在」的同一状态。

BEGIN;

-- 先删带外键的关联表，再删被引用的主表
DROP TABLE IF EXISTS merchant_user_role_bindings;
DROP TABLE IF EXISTS merchant_role_permissions;
DROP TABLE IF EXISTS merchant_roles;
DROP TABLE IF EXISTS merchant_permissions;

-- 触发器随表一起消失，函数需要单独删除
DROP FUNCTION IF EXISTS check_merchant_user_role_same_tenant();

COMMIT;
