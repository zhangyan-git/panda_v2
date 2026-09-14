-- identity/010: 小程序短信验证码
--
-- 手机号 + 短信验证码是 C 端三种登录方式之一（另外两种是微信一键登录和微信
-- 手机号快捷登录），也是唯一一种需要服务端暂存一个秘密的。本文件建那张表。
--
-- 为什么进 Postgres 而不是 Redis：这个决定改过一次，原来是打算放 Redis 的，
-- 因为验证码是 5 分钟 TTL 的典型短命数据。问题出在 platform/cache 这个客户端
-- 上——它的接口只有 Ping/Close/Publish/Subscribe，没有 Get/Set 一类的 KV 操作，
-- 也就是说当前根本没有可用的验证码存储。而它更危险的一面是：REDIS_ADDR 为空时
-- 构造出来的是 Noop，Noop.Publish 返回 nil，即调用方看到的是「成功」。把验证码
-- 校验建在一个连不上时静默放行的组件上，等于给了一个「Redis 挂掉期间任意验证码
-- 都通过」的窗口（如果将来补上 KV 也照抄这个失败语义的话）。落在 Postgres 里，
-- 校验依赖的是和 users 表同一个连接、同一个事务，失败就是明确的错误。
--
-- phone 直接做主键，一个手机号同时只有一个有效验证码：
--   1. 重发就是覆盖（INSERT ... ON CONFLICT (phone) DO UPDATE），不需要额外
--      的「失效旧码」逻辑，也就不会出现旧码忘了失效而被继续使用的情况；
--   2. 同一个号不可能攒出多个同时有效的码——否则反复请求就能把猜中的概率乘上去。
-- 代价是「登录」和「绑手机」两个用途不能同时持有验证码，先要的那个会被顶掉。
-- 这在产品上不是问题：这两条流程不会并行发生，而少一个并发维度就少一类漏洞。
--
-- code_hash 存哈希不存原文，理由与 user_sessions.refresh_token_hash 相同：这两张
-- 表一旦被导出，里面的行就等于可以直接使用的凭据。验证码只有 6 位、空间很小，
-- 所以哈希算法必须是带盐的慢哈希（bcrypt），不能用裸 SHA——否则对着一张导出表
-- 把 10^6 个候选全算一遍是瞬间的事。哈希的选择与验证都在 service 层。
--
-- attempts 是校验失败次数，由 service 层累加并在达到上限时作废整条记录。列在这里
-- 是因为它必须和验证码同生命周期、同事务地读改写：放在进程内存里会在多副本下
-- 每个副本各算各的，等于把上限乘以副本数。
--
-- created_at 兼两个用途：这条码的签发时间，以及重发频控的基准（service 层据此
-- 拒绝过于频繁的重发）。expires_at 单独一列而不是 created_at + 固定 TTL，是为了
-- 让 TTL 将来可按用途调整而不必改数据。
--
-- 不对 users 建外键：验证码可以在「还没有这个账号」时请求（注册流程），那时
-- users 里没有对应行。这一点和 user_login_events 不对 users 建外键是同一个理由。
--
-- 过期行的清理：成功校验后 service 层直接删掉该行，所以常态下这张表的大小约等于
-- 「当前正在验证中的手机号数量」。只有「要了码又不回来」的号会留下过期行，而它们
-- 会在同一个号下次请求时被覆盖，或由将来的清理任务按 expires_at 扫掉——下面的
-- 索引就是为后者准备的。表的行数上界因此是「历史上请求过验证码的手机号数」，不是
-- 请求次数。
--
-- 可重跑：CREATE TABLE / CREATE INDEX 都是 IF NOT EXISTS，对任何库重跑都是空操作。
-- 没有 Down 段，理由同 009。

BEGIN;

CREATE TABLE IF NOT EXISTS user_sms_codes (
  phone      TEXT        PRIMARY KEY,
  purpose    TEXT        NOT NULL,
  code_hash  TEXT        NOT NULL,
  attempts   INTEGER     NOT NULL DEFAULT 0,
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT user_sms_codes_purpose_check CHECK (purpose IN ('login', 'bind_phone'))
);

CREATE INDEX IF NOT EXISTS user_sms_codes_expires_idx ON user_sms_codes (expires_at);

COMMIT;
