-- payment/011：退款查询扫描的索引
--
-- 补一条 payment_refunds (updated_at) WHERE status = 'processing'。
--
-- # 它服务的是哪条查询
--
-- 退款查询任务（payment-service internal/worker/refund.go →
-- repository.ListStaleProcessingRefunds）每五分钟问一次库：
--
--     SELECT … FROM payment_refunds
--     WHERE status = 'processing' AND updated_at <= $1
--     ORDER BY updated_at
--     LIMIT $2
--
-- 形状与 010 那条逐字同源，只是换了一张表。两条索引不共用是因为它们扫的表不同，而
-- payments 上那条的前提（pending 是这张表里最少的那一段）在 payment_refunds 上要重新说
-- 一遍——见下面「为什么这条谓词能索引」。
--
-- # 排序键为什么是 updated_at 而不是 created_at
--
-- 与 010 同一条理由，而且在这里更硬：一笔「托管退款」在渠道那边可以挂几天，期间每一次查询
-- 都会把 updated_at 推到当下（见 repository.TouchRefund）。也就是说 processing 是一个**会
-- 主动重排**的状态。按 created_at 排的话，一笔挂了三天的退款每一轮都排在队首、被问一次、
-- 什么都没问出来、下一轮还在队首——后面那些刚进来、其实一次就能问出结果的退款永远轮不到。
-- 按 updated_at 排，问过一轮的自动沉到队尾，这正是那个 Touch 想要的排队效果。
--
-- # 为什么这条谓词能索引
--
-- 与 010 逐字相同：**部分索引**的谓词把索引本身缩小到只剩 processing 行，而 processing
-- 是这张表里最少的那一段——绝大多数退款单会在发起那一次应答里就落到 succeeded 或 failed，
-- 只有应答含糊（PROCESSING / UNKNOWN）或超时的那一小撮会停在这里。006 里那句「status 筛选
-- 有意不建索引」说的是全表索引，与部分索引不是一回事。
--
-- # 上生产的注意
--
-- 与 006 / 010 同一条：迁移在事务里跑（见 platform/database/migrate），用不了
-- CREATE INDEX CONCURRENTLY，这条语句在存量数据上会**短暂持写锁**。payment_refunds 比
-- payments 还小，dev/prod 都是秒级；真到几百万行那天，挑低峰窗口上，或者由 DBA 手工
-- CONCURRENTLY 建好再 adopt 过去（`panda-migrate adopt payment 011`）。

BEGIN;

CREATE INDEX payment_refunds_processing_query_idx ON payment_refunds (updated_at)
    WHERE status = 'processing';

COMMIT;
