-- identity/009: 小程序（C 端）用户身份域
--
-- 身份库此前只有两套 B 端账号：admin_users（平台管理员）和 merchant_users
-- （商户员工）。本文件建 C 端用户，是第三套，也是唯一一套面向小程序的。
--
-- 为什么落在 user-service / 身份库，而不是新起一个「用户服务」：计划 §5.2 把
-- 「小程序用户、微信 code2Session、openid/unionid 绑定、Access/Refresh Token
-- 与会话撤销」明确划归 user-service；计划的 §8.1 虽然列过一个 panda_user 库名，
-- 但实现里 user-service 的库就叫 panda_identity，照库名再拆一个身份源出来没有
-- 任何收益，只会让「谁是用户身份的唯一来源」这个问题变模糊。
--
-- 四张表：
--   users                   账号主体（手机号是账号属性，兼登录标识）
--   user_wechat_identities  微信身份绑定（openid 按应用独立，可多行）
--   user_sessions           Refresh Token 与会话撤销
--   user_login_events       登录安全事件审计
--
-- 表名没有写 miniapp_ 前缀：identity 库里没有同名的 C 端表，而 admin_/merchant_
-- 前缀的用意是区分「后台的管理员」与「商户的员工」，C 端用户不需要第三种前缀来
-- 和它们区分——它就是「用户」。coupon 库的 user_coupons.user_id 注释里写的是
-- 「持券用户 ID，属于身份服务时仅作跨库值引用」，指的就是这张 users.id，所以这里
-- 改表名会连带让那边的注释失准。主键是 UUID，与那列的类型对得上。
--
-- 微信身份为什么单独一张表、而不是在 users 上放 openid/unionid 两列：openid 是
-- 「按应用」的标识，同一个微信用户在公众号和两个小程序里各有一个 openid，共享的
-- 只有 unionid。放在 users 上意味着接入第二个应用时要改表并回填；独立成表则只是
-- 多一行。
--
-- 短信验证码原来打算放 Redis（platform/cache），后来改成也进身份库，见
-- 010_user_sms_codes.sql 的文件头：那个客户端只提供 Pub/Sub，没有 KV 接口，
-- 而它连不上 Redis 时会退化成 Noop 并且调用一律返回成功——验证码的校验不能
-- 建立在一个失败时静默放行的组件上。
--
-- 本迁移要同时服务两种库：全新的空库（这些表由本文件建），以及从 legacy 基线过来
-- 的库（那里这些表从没建过，本文件也是第一次建）。所以建表一律 IF NOT EXISTS、
-- 触发器一律先 DROP IF EXISTS、索引一律 IF NOT EXISTS——对任何库重跑都是空操作。
-- 没有写 Down 段：迁移 runner 把整份文件交给一次 Exec，不认 goose 指令，在同一个
-- 事务里建表再删表只会更难查。
--
-- 本库不得创建跨库外键：下面唯一的外键都在身份库内部（指向本文件的 users）。
-- 迁移集测试 TestSetsDoNotCrossTheDatabaseBoundary 会把关。

BEGIN;

-- ============================================================
-- C 端用户账号
-- ============================================================

-- phone 可空且唯一：三种登录方式里，「微信一键登录」拿到的只有 openid，用户可能
-- 一直没有授权手机号；而「手机号+短信验证码」是先有手机号、后有（或一直没有）
-- 微信身份。两边都得能建号。PG 的 UNIQUE 允许多个 NULL，所以「多行都没绑手机号」
-- 是合法的，不会互相冲突。
--
-- 刻意存 NULL 而不是空串：legacy 的 users 集合用 "" 表示「没有手机号」，迁移时
-- 必须归一化成 NULL，否则所有空串行会在唯一索引上互撞，迁移直接失败。
CREATE TABLE IF NOT EXISTS users (
  id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  legacy_id       TEXT        UNIQUE,
  phone           TEXT        UNIQUE,
  nickname        TEXT        NOT NULL DEFAULT '',
  avatar_url      TEXT        NOT NULL DEFAULT '',
  gender          TEXT        NOT NULL DEFAULT 'unknown',
  birthday        DATE,
  region_code     TEXT        NOT NULL DEFAULT '',
  region_name     TEXT        NOT NULL DEFAULT '',
  status          TEXT        NOT NULL DEFAULT 'active',
  register_source TEXT        NOT NULL DEFAULT 'wechat_miniapp',
  last_login_at   TIMESTAMPTZ,
  last_login_ip   TEXT        NOT NULL DEFAULT '',
  login_count     INTEGER     NOT NULL DEFAULT 0,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT users_status_check CHECK (status IN ('active', 'disabled', 'deleted')),
  CONSTRAINT users_gender_check CHECK (gender IN ('unknown', 'male', 'female'))
);

COMMENT ON TABLE  users                 IS '小程序（C 端）用户账号';
COMMENT ON COLUMN users.legacy_id       IS '老系统 users 集合的 ObjectID，迁移对账用；V2 自有账号为 NULL';
COMMENT ON COLUMN users.phone           IS '手机号，登录标识之一；NULL 表示尚未绑定手机号。存 NULL 不存空串';
COMMENT ON COLUMN users.nickname        IS '昵称，可空串（微信一键登录可能只给 openid，没给昵称）';
COMMENT ON COLUMN users.avatar_url      IS '头像地址';
COMMENT ON COLUMN users.gender          IS '性别：unknown=未填 male=男 female=女';
COMMENT ON COLUMN users.birthday        IS '生日，仅日期无时刻';
COMMENT ON COLUMN users.region_code     IS '地区编码（省市区），附近点位用；名称可从编码推出，反过来不行，所以两个都存';
COMMENT ON COLUMN users.region_name     IS '地区显示名，与 region_code 成对';
COMMENT ON COLUMN users.status          IS '账号状态：active=正常 disabled=禁用 deleted=已注销。鉴权必须校验本列';
COMMENT ON COLUMN users.register_source IS '注册来源：wechat_miniapp=微信小程序 wechat_phone=微信手机号快捷登录 sms_code=手机号验证码';
COMMENT ON COLUMN users.last_login_at   IS '最后登录时间';
COMMENT ON COLUMN users.last_login_ip   IS '最后登录 IP';
COMMENT ON COLUMN users.login_count     IS '累计登录次数';

-- status 带 deleted 是软删除：老系统没有任何注销能力，V2 补上，但注销后 user_coupons
-- 等跨库引用仍在，物理删除会让那些行变成悬空引用。鉴权时必须校验本列——老系统的
-- users.status 存在但登录链路根本不查，禁用用户照样能登录，这个债不能带过来。

-- ============================================================
-- 微信身份绑定
-- ============================================================

-- 唯一键是 (app_type, openid) 而不是单独 openid：openid 的空间按应用划分，同一个
-- 字符串在不同应用里可以指不同的人，单列唯一会把正常情况判成冲突。
-- unionid 可空：没绑定微信开放平台账号的小程序拿不到它。
CREATE TABLE IF NOT EXISTS user_wechat_identities (
  id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id       UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  app_type      TEXT        NOT NULL DEFAULT 'miniapp',
  openid        TEXT        NOT NULL,
  unionid       TEXT,
  last_login_at TIMESTAMPTZ,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT user_wechat_identities_app_type_check CHECK (app_type IN ('miniapp', 'official_account')),
  CONSTRAINT user_wechat_identities_app_openid_key UNIQUE (app_type, openid)
);

COMMENT ON TABLE  user_wechat_identities               IS '微信身份绑定：一个用户可以绑多个应用，一个应用一个 openid';
COMMENT ON COLUMN user_wechat_identities.user_id       IS '所属用户；账号注销（软删除）时本行保留，物理删除用户时级联清掉';
COMMENT ON COLUMN user_wechat_identities.app_type      IS '应用类型：miniapp=小程序 official_account=公众号';
COMMENT ON COLUMN user_wechat_identities.openid        IS '微信 openid，按 app_type 唯一';
COMMENT ON COLUMN user_wechat_identities.unionid       IS '微信 unionid，同开放平台账号下跨应用稳定；未绑定开放平台时为 NULL';
COMMENT ON COLUMN user_wechat_identities.last_login_at IS '本应用最后登录时间，与 users.last_login_at 的粒度不同';

CREATE INDEX IF NOT EXISTS user_wechat_identities_user_idx ON user_wechat_identities (user_id);

-- unionid 只建部分索引：它可空，而部分索引不收录 NULL 行，索引更小。补 unionid 的
-- 用途是把同一微信用户在多个应用下的账号认出来。
CREATE INDEX IF NOT EXISTS user_wechat_identities_unionid_idx
  ON user_wechat_identities (unionid) WHERE unionid IS NOT NULL;

-- ============================================================
-- 会话与 Refresh Token
-- ============================================================

-- legacy 完全没有 refresh token：配置里的 RefreshTokenExpiry 是个零实现的死键，
-- 30 天到期只能重新登录。V2 补上，而「能签发」的前提是「能撤销」，所以要有落库的
-- 会话记录。
--
-- 只存 refresh token 的哈希：这张表一旦泄露，明文 token 等于可以直接冒用登录态。
-- 校验时对请求里的 token 取哈希再等值查，原文任何时候都不落库、不回显。
CREATE TABLE IF NOT EXISTS user_sessions (
  id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id            UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
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

COMMENT ON TABLE  user_sessions                    IS '小程序登录会话，承载 refresh token 的签发与撤销';
COMMENT ON COLUMN user_sessions.refresh_token_hash IS 'refresh token 的哈希，原文不落库；唯一';
COMMENT ON COLUMN user_sessions.expires_at         IS '会话过期时间，签发时按配置算好写入，不在查询时动态计算';
COMMENT ON COLUMN user_sessions.revoked_at         IS '撤销时间；NULL=有效。改状态而不是删行，撤销后仍要能查';
COMMENT ON COLUMN user_sessions.revoke_reason      IS '撤销原因：logout=用户登出 rotated=刷新时轮换 admin=管理员强制下线';
COMMENT ON COLUMN user_sessions.last_used_at       IS '最后一次用本会话刷新 token 的时间';

-- 部分索引只覆盖有效会话：查「某用户当前有哪些会话」（列出登录设备 / 全部下线）
-- 以及清理过期行都只会碰有效行，已撤销的行越积越多也不拖慢它。
CREATE INDEX IF NOT EXISTS user_sessions_user_active_idx
  ON user_sessions (user_id) WHERE revoked_at IS NULL;

-- ============================================================
-- 登录安全事件
-- ============================================================

-- 刻意不对 users 建外键，理由与 admin_operation_logs 的目标列一致：账号注销后
-- 审计仍要可查，外键的 ON DELETE 动作（无论 CASCADE 还是 SET NULL）都会毁掉这段
-- 记录。user_id 在这里是软指针。
--
-- user_id 本身可空：登录失败时可能根本没匹配到账号（手机号不存在、openid 没绑过），
-- 这类事件恰恰是审计最想看的，不能因为「没有用户」就丢掉。
CREATE TABLE IF NOT EXISTS user_login_events (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     UUID,
  login_type  TEXT        NOT NULL,
  identifier  TEXT        NOT NULL DEFAULT '',
  success     BOOLEAN     NOT NULL,
  fail_reason TEXT        NOT NULL DEFAULT '',
  ip          TEXT        NOT NULL DEFAULT '',
  user_agent  TEXT        NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT user_login_events_login_type_check CHECK (login_type IN ('wechat_miniapp', 'wechat_phone', 'sms_code'))
);

COMMENT ON TABLE  user_login_events             IS '小程序登录安全事件，含失败尝试；不对 users 建外键，账号注销后仍需可查';
COMMENT ON COLUMN user_login_events.user_id     IS '软指针，指向 users.id；登录失败未匹配到账号时为 NULL';
COMMENT ON COLUMN user_login_events.login_type  IS '登录方式：wechat_miniapp=微信一键登录 wechat_phone=微信手机号快捷登录 sms_code=手机号验证码';
COMMENT ON COLUMN user_login_events.identifier  IS '登录标识快照：openid，或脱敏后的手机号（不存明文手机号）';
COMMENT ON COLUMN user_login_events.success     IS '是否登录成功';
COMMENT ON COLUMN user_login_events.fail_reason IS '失败原因，如 code 无效、手机号未注册、账号被禁用';

-- 审计查询按用户倒序时间线，所以是复合索引且带 DESC。
CREATE INDEX IF NOT EXISTS user_login_events_user_time_idx
  ON user_login_events (user_id, created_at DESC);

-- ============================================================
-- updated_at 触发器
-- ============================================================

-- set_updated_at() 由 identity/001 建好。这里先 DROP IF EXISTS 再建，让本文件可以
-- 重跑（PG 的 CREATE TRIGGER 没有 IF NOT EXISTS）。
DROP TRIGGER IF EXISTS trg_users_updated_at ON users;
CREATE TRIGGER trg_users_updated_at
  BEFORE UPDATE ON users
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_user_wechat_identities_updated_at ON user_wechat_identities;
CREATE TRIGGER trg_user_wechat_identities_updated_at
  BEFORE UPDATE ON user_wechat_identities
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_user_sessions_updated_at ON user_sessions;
CREATE TRIGGER trg_user_sessions_updated_at
  BEFORE UPDATE ON user_sessions
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- user_login_events 是只追加的事件表，没有 updated_at，不需要触发器。

COMMIT;
