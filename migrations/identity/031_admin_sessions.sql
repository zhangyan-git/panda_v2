-- identity/031: 后台管理员的会话表
-- 可重复执行（IF NOT EXISTS + DROP TRIGGER IF EXISTS）。
--
-- 为什么要有这张表：009 建的 user_sessions 是**小程序**登录会话（它建表时就写着，
-- 外键是 user_id → users(id)），而平台管理员住在 admin_users 里，两套账号体系没有
-- 任何一行是重叠的。在 031 之前，后台登录**根本不落会话**，于是：
--
--   * 后台的「退出登录」是个空操作——令牌还在，谁拿着都能继续换新的；
--   * 某个管理员账号被停用后，他手上那枚 refresh token 照样能换出一对新令牌；
--   * refresh 只验签名，不查会话，所以**任何**本服务签发的 refresh token
--     （包括 C 端顾客、商户员工的那两种）都能打到后台接口上换出后台令牌。
--
-- 这三件事只能一起解决，而它们的共同前提是「撤销要有地方记」。撤销状态必须落库，
-- 所以需要这张表；不能借用 user_sessions，因为外键会把管理员 id 挡在 users 之外
-- （dev 库上 admin_users 有 3 行、users 有 0 行，写进去就是 23503）。
--
-- 结构、列注释、索引口径全部与 user_sessions 保持一致，理由不再重复（见 009）。
-- 两张表故意同形，是为了让 Go 那边能用同一份实现（repository 的 sessionStore），
-- 而不是把同一段 SQL 抄两遍——那种抄法一定会漂移。
--
-- 与 user_sessions 唯一实质的区别就是外键指向 admin_users，级联删除跟着账号走：
-- 管理员账号被删，他的会话没有留着的道理。

BEGIN;

CREATE TABLE IF NOT EXISTS admin_sessions (
  id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id            UUID        NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
  refresh_token_hash TEXT        NOT NULL UNIQUE,
  issued_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  expires_at         TIMESTAMPTZ NOT NULL,
  revoked_at         TIMESTAMPTZ,
  revoke_reason      TEXT        NOT NULL DEFAULT '',
  last_used_at       TIMESTAMPTZ,
  ip                 TEXT        NOT NULL DEFAULT '',
  user_agent         TEXT        NOT NULL DEFAULT '',
  created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  admin_sessions                    IS '平台后台管理员登录会话，承载 refresh token 的签发与撤销';
COMMENT ON COLUMN admin_sessions.user_id            IS 'admin_users.id。管理员与小程序顾客是两套账号，这张表不存后者';
COMMENT ON COLUMN admin_sessions.refresh_token_hash IS 'refresh token 的哈希，原文不落库；唯一';
COMMENT ON COLUMN admin_sessions.expires_at         IS '会话过期时间，签发时按配置算好写入，不在查询时动态计算';
COMMENT ON COLUMN admin_sessions.revoked_at         IS '撤销时间；NULL=有效。改状态而不是删行，撤销后仍要能查';
COMMENT ON COLUMN admin_sessions.revoke_reason      IS '撤销原因：logout=用户登出 rotated=刷新时轮换 reuse_detected=疑似盗用 disabled=账号被停用';
COMMENT ON COLUMN admin_sessions.last_used_at       IS '最后一次用本会话刷新 token 的时间';

-- 与 user_sessions_user_active_idx 同口径：只覆盖有效会话。后台目前没有「列出某人的
-- 登录设备」的界面，但「停用账号时撤销其全部会话」会按 user_id 扫，走的正是这条。
CREATE INDEX IF NOT EXISTS admin_sessions_user_active_idx
  ON admin_sessions (user_id) WHERE revoked_at IS NULL;

-- set_updated_at() 由 identity/001 建好；先 DROP 再建让本文件可以重跑。
DROP TRIGGER IF EXISTS trg_admin_sessions_updated_at ON admin_sessions;
CREATE TRIGGER trg_admin_sessions_updated_at
  BEFORE UPDATE ON admin_sessions
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMIT;
