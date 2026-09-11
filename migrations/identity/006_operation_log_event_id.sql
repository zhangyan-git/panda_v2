-- identity/006: admin_operation_logs 记录来源事件 ID，让消费端可以真正幂等
--
-- 阶段 4 起这张表由 outbox 事件的消费者写入（此前它只有建表语句，没有任何写入方）。
-- 消费者本身的去重由 message_inbox 的租约保证，但「写库成功、标记 inbox 完成失败」
-- 这一小段窗口会让同一条消息被重新投递；没有唯一约束时它就会写成两行，
-- 审计日志出现重影。
--
-- 存一个独立的 event_id 列而不是复用主键 id：id 是日志自身的标识，让外地的事件 ID
-- 占用它会把「这条日志」和「那次投递」绑死。有了唯一索引，消费者用
-- INSERT ... ON CONFLICT (event_id) DO NOTHING 就能做到重投不重写。
--
-- 允许为空：消费者上线前写入的历史行、以及将来可能的非事件来源行都没有 event_id。
-- 唯一索引建成只覆盖非空值的部分索引，把「唯一」的适用范围写在索引定义里——
-- 约束的对象只是事件产生的行，历史行不在其内（普通唯一索引也不会让多行 NULL
-- 互相冲突，但那样索引里会白白带着一堆永远不回查的 NULL）。

BEGIN;

ALTER TABLE admin_operation_logs ADD COLUMN IF NOT EXISTS event_id TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS admin_operation_logs_event_id_key
  ON admin_operation_logs (event_id) WHERE event_id IS NOT NULL;

COMMENT ON COLUMN admin_operation_logs.event_id IS
  '产生这条日志的 message_outbox 事件 ID，用于消费端幂等去重；非事件来源的行为空';

COMMIT;
