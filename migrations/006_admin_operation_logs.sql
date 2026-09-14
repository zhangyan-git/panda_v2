-- 006: 平台后台业务操作日志
--
-- 仅记录后台管理员执行的业务操作；接口全链路请求日志由独立的日志系统负责。

BEGIN;

CREATE TABLE IF NOT EXISTS admin_operation_logs (
  id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),

  -- 操作人快照
  admin_user_id       UUID REFERENCES admin_users(id) ON DELETE SET NULL,
  admin_username      TEXT NOT NULL DEFAULT '',
  admin_name          TEXT NOT NULL DEFAULT '',

  -- 业务操作
  module              TEXT NOT NULL DEFAULT '',
  action              TEXT NOT NULL DEFAULT '',
  operation           TEXT NOT NULL DEFAULT '',

  -- 被操作对象；不对目标对象建立外键，保证目标删除后日志仍可查询
  target_type         TEXT NOT NULL DEFAULT '',
  target_id           UUID,
  target_name         TEXT NOT NULL DEFAULT '',
  merchant_id         UUID REFERENCES merchants(id) ON DELETE SET NULL,

  -- 操作结果
  result              TEXT NOT NULL DEFAULT 'success',
  error_code          TEXT NOT NULL DEFAULT '',
  error_message       TEXT NOT NULL DEFAULT '',

  -- 经过脱敏的业务数据快照
  before_data         JSONB,
  after_data          JSONB,

  occurred_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE admin_operation_logs IS
  '平台后台业务操作日志，不记录接口全链路访问日志';
COMMENT ON COLUMN admin_operation_logs.admin_user_id IS
  '执行操作的平台管理员 ID；管理员删除后置空，保留日志';
COMMENT ON COLUMN admin_operation_logs.admin_username IS
  '操作发生时的平台管理员用户名快照';
COMMENT ON COLUMN admin_operation_logs.admin_name IS
  '操作发生时的平台管理员名称快照';
COMMENT ON COLUMN admin_operation_logs.module IS
  '业务模块，例如 merchants、brands、stores、roles';
COMMENT ON COLUMN admin_operation_logs.action IS
  '标准动作，例如 create、update、delete、audit、enable、disable';
COMMENT ON COLUMN admin_operation_logs.operation IS
  '面向后台展示的操作描述，例如 创建商户、审核品牌';
COMMENT ON COLUMN admin_operation_logs.target_type IS
  '目标类型，例如 merchant、brand、store、admin_user、role';
COMMENT ON COLUMN admin_operation_logs.target_id IS
  '目标对象 ID；不加外键，目标删除后仍保留日志';
COMMENT ON COLUMN admin_operation_logs.target_name IS
  '操作发生时的目标名称快照';
COMMENT ON COLUMN admin_operation_logs.merchant_id IS
  '关联商户 ID；商户删除后置空，用于按商户筛选历史日志';
COMMENT ON COLUMN admin_operation_logs.result IS
  '操作结果：success=成功，failure=失败';
COMMENT ON COLUMN admin_operation_logs.before_data IS
  '变更前快照，仅保存经过脱敏的业务字段';
COMMENT ON COLUMN admin_operation_logs.after_data IS
  '变更后快照，仅保存经过脱敏的业务字段';

CREATE INDEX IF NOT EXISTS idx_admin_operation_logs_occurred_at
  ON admin_operation_logs (occurred_at DESC);

CREATE INDEX IF NOT EXISTS idx_admin_operation_logs_admin_time
  ON admin_operation_logs (admin_user_id, occurred_at DESC);

CREATE INDEX IF NOT EXISTS idx_admin_operation_logs_module_time
  ON admin_operation_logs (module, occurred_at DESC);

CREATE INDEX IF NOT EXISTS idx_admin_operation_logs_action_time
  ON admin_operation_logs (action, occurred_at DESC);

CREATE INDEX IF NOT EXISTS idx_admin_operation_logs_target
  ON admin_operation_logs (target_type, target_id, occurred_at DESC)
  WHERE target_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_admin_operation_logs_merchant_time
  ON admin_operation_logs (merchant_id, occurred_at DESC)
  WHERE merchant_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_admin_operation_logs_result_time
  ON admin_operation_logs (result, occurred_at DESC);

COMMIT;
