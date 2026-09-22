-- partner/003：本库的 outbox（审计事件出口）
--
-- 这一域**没有 inbox**：partner-service 不消费任何事件。它只有一条出向的、写在业务事务里的
-- 追加 —— 后台每一次治理动作（建合作方、签发密钥、启停、改 IP 白名单与限流）都要落一条审计。
--
-- 为什么审计要从**本库的 outbox** 走、而不是直接往身份库的 admin_operation_logs 插一行：
-- 那两件事（「这次改动发生了」与「记下来」）必须同一个事务。直接写身份库意味着要么开一条到
-- 另一个库的连接、要么在提交之后补写——前者把一次后台操作变成一个跨库的分布式事务，后者
-- 会在「业务写成功、进程崩在补写之前」时留下一次没有任何痕迹的密钥签发。这个判断与
-- platform/audit 的包注释、以及 payment / membership 各域的同一份实现是同一个。
--
-- 两处刻意的不一致，都是继承来的，不是本域的选择：
--
--   * inbox 也一起建了，尽管本服务今天一行都不写它。**表必须与其它九份逐字相同**：
--     migrations 的 TestMessageTablesStayInSyncAcrossSets 逐列比对十一个域的这一对表，
--     少建一张就红。将来真接消费者时它是现成的。
--   * 列与索引的写法照抄 membership/001 的那一段，**不要顺手调整**：那张比对测试是按
--     CREATE TABLE 的列清单（含顺序）比字符串的，注释可以不同，列不能。
--
-- 与 001 同样的理由，本文件不带 goose 的 Down 段。

BEGIN;

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

CREATE INDEX message_outbox_pending_idx
    ON message_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;

CREATE INDEX message_outbox_lease_idx
    ON message_outbox (lease_until)
    WHERE published_at IS NULL;

COMMIT;
