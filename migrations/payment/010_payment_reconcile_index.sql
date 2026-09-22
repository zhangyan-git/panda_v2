-- payment/010：主动查单扫描的索引
--
-- 补一条 payments (updated_at) WHERE status = 'pending'。
--
-- # 它服务的是哪条查询
--
-- 主动查单任务（payment-service internal/worker/reconcile.go →
-- repository.ListStalePendingPayments）每分钟问一次库：
--
--     SELECT … FROM payments
--     WHERE status = 'pending' AND provider <> '' AND updated_at <= $1
--     ORDER BY updated_at
--     LIMIT $2
--
-- 它要做的事和关单扫描正相反：关单找「到点该作废的」，它找「发起之后一直没结论的」。两者都从
-- pending 出发，但排序键不同，所以 001 的 payments_pending_expiry_idx (status, expires_at)
-- 帮不上忙——那条索引能给出行、给不了 updated_at 的顺序，于是这条查询会退化成「扫出全部
-- pending，再排序取前 20」。
--
-- # 为什么不直接复用 pending_expiry_idx
--
-- 因为排序键不是可以互相替代的东西。pending 是一个**会堆积**的状态：一笔支付发起之后没人付，
-- 它会一直待在那里直到超时关单（15 分钟）。钱收了、回调没进来的那一笔也一样待着，而它恰恰是
-- 这条查询要找的。用一个按 expires_at 排的索引去取「最老的 20 笔」，在积压时每次都从同一头开始
-- 扫——真正的老单反而永远排在后面取不到。
--
-- # status 筛选在这里可以索引，与 006 的判断不矛盾
--
-- 006 里写着「status 筛选有意不建索引」，理由是那一列分布高度倾斜、对选择性没有帮助。这里
-- 不同：**部分索引**的谓词把索引本身缩小到只剩 pending 行，而 pending 恰恰是这张表里最少的
-- 那一段（绝大多数支付单会走到 succeeded/failed/expired 三个终态之一）。倾斜在这里是好事，
-- 不是需要容忍的代价。
--
-- # 上生产的注意
--
-- 与 006 同一条：迁移在事务里跑（见 platform/database/migrate），用不了
-- CREATE INDEX CONCURRENTLY，这条语句在存量数据上会**短暂持写锁**。本仓库的支付库还很小，
-- dev/prod 都是秒级；真到几百万行那天，挑低峰窗口上，或者由 DBA 手工 CONCURRENTLY 建好再
-- adopt 过去（`panda-migrate adopt payment 010`）。

BEGIN;

CREATE INDEX payments_pending_reconcile_idx ON payments (updated_at)
    WHERE status = 'pending';

COMMIT;
