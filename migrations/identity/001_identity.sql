-- identity: 身份库表结构
--
-- 身份库承载三套彼此不重叠的账号，以及围绕它们的东西：
--
--   * admin_users    平台后台管理员；他们的登录会话是 admin_sessions
--   * merchant_users 商户员工账号。账号域（凭据、状态、数据范围）归身份库，
--                    写入方全部是 user-service：登录、/v1/admin/merchant-users/*、
--                    /v1/admin/merchants/{id}/users。merchant-service 只在删除商户前
--                    判断「商户下还有没有账号」，走 HasUsers RPC。
--   * users          小程序（C 端）用户，另有微信身份绑定、会话、登录事件与短信验证码
--
-- 另外两半：控制台 IAM（admin_roles / admin_permissions / admin_role_permissions /
-- admin_user_role_bindings / 菜单树）与平台审计 admin_operation_logs。
-- 权限码、内置角色与菜单树的**内容**不在这里，在 002_identity_seed.sql。
--
-- # 拆库后的两条边界
--
-- 一、不建商户域的表。merchants / brands / stores 等属于 merchant 库；单库时代由 009
-- 删掉的那四张孤儿表（merchant_roles / merchant_permissions / merchant_role_permissions /
-- merchant_user_role_bindings）干脆不建。
--
-- 二、指向商户域的列只留值，不建外键：
--     merchant_users.merchant_id      列、NOT NULL 与 (merchant_id, username) 唯一约束
--                                     全部保留，归属改由 merchant-service 经 gRPC 校验
--                                     （阶段 1 的 GetBrandMerchant/GetStoreMerchant）
--     admin_operation_logs.merchant_id 本意就是「商户删除后置空、日志仍可查」，
--                                     拆库后连置空都不需要，只留 ID 用于筛选
--     本库内唯一的外键都在身份库内部（user_wechat_identities/user_sessions → users，
--     admin_sessions → admin_users，几张绑定表 → admin_roles/admin_permissions）。
--     迁移集测试 TestSetsDoNotCrossTheDatabaseBoundary 会把关。
--
-- 全新身份库跑完本文件之后的表结构，与「单库时代 001→009 之后」的身份域部分逐列一致：
-- 跨库外键与孤儿表在那边是被删掉的，在这里是从来没建过。
--
-- 本文件不带 goose 的 Down 段：platform/database/migrate 把整个文件丢给一次 Exec、
-- 不识别 goose 指令，带上就会在同一个事务里建完表再删掉，而且不报错。

BEGIN;

-- ============================================================
-- 自动更新 updated_at 的触发器函数
--
-- 本库有 updated_at 的表都挂这一条，触发器在各表建好之后就地创建。
-- ============================================================

CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at = NOW();
  RETURN NEW;
END;
$$;

-- ============================================================
-- 平台管理员
-- ============================================================

CREATE TABLE admin_users (
  id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  username      TEXT        NOT NULL UNIQUE,
  password_hash TEXT        NOT NULL,
  name          TEXT        NOT NULL,
  email         TEXT        NOT NULL UNIQUE,
  status        TEXT        NOT NULL DEFAULT 'active',
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  admin_users          IS '平台后台管理员账号';
COMMENT ON COLUMN admin_users.username IS '登录用户名，全局唯一';
COMMENT ON COLUMN admin_users.status   IS '账号状态：active=正常 disabled=禁用';

CREATE TRIGGER trg_admin_users_updated_at
  BEFORE UPDATE ON admin_users
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================
-- 商户账号
-- ============================================================

CREATE TABLE merchant_users (
  id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  merchant_id   UUID        NOT NULL,
  username      TEXT        NOT NULL UNIQUE,
  password_hash TEXT        NOT NULL,
  name          TEXT        NOT NULL,
  email         TEXT,
  phone         TEXT,
  status        TEXT        NOT NULL DEFAULT 'active',
  is_admin      BOOLEAN     NOT NULL DEFAULT FALSE,
  scope_type    TEXT        NOT NULL DEFAULT 'merchant',
  avatar        TEXT        NOT NULL DEFAULT '',
  last_login_at TIMESTAMPTZ,
  last_login_ip TEXT        NOT NULL DEFAULT '',
  login_count   INTEGER     NOT NULL DEFAULT 0,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  scope_ids     UUID[]      NOT NULL DEFAULT '{}',
  UNIQUE (merchant_id, username)
);

COMMENT ON TABLE  merchant_users                 IS '商户员工账号，username 全局唯一';
COMMENT ON COLUMN merchant_users.merchant_id     IS '所属商户 ID；拆库后是软指针，不再有外键，归属由 merchant-service 校验';
COMMENT ON COLUMN merchant_users.username        IS '登录用户名，全局唯一，登录只需 username+password';
COMMENT ON COLUMN merchant_users.status          IS '账号状态：active=正常 disabled=禁用';
COMMENT ON COLUMN merchant_users.is_admin        IS '是否商户管理员';
COMMENT ON COLUMN merchant_users.scope_type      IS '数据范围档位：merchant=本商户全部 brand=指定品牌旗下 store=指定门店；目标见 scope_ids';
COMMENT ON COLUMN merchant_users.scope_ids       IS '范围目标 ID 数组（多态指向 merchant 库的 brands/stores，不建外键）：brand 档是品牌 ID，store 档是门店 ID，merchant 档为空数组。空数组=看不见任何点位，与 NULL 不是一回事';
COMMENT ON COLUMN merchant_users.last_login_at   IS '最后登录时间';
COMMENT ON COLUMN merchant_users.login_count     IS '登录次数';

-- 数据范围是一组目标而不是一个：brand 与 store 两档可以把几个品牌、或几家门店一起授权
-- 给同一个账号，展开出来的点位集合是它们的并集。
--
-- 空数组不是 NULL，两者在展开处的读法完全相反：
--   空数组 → `= ANY('{}')` 恒假 → 这个账号一个点位都看不见；
--   NULL   → 下游那几条判 nil 的谓词把它读成「调用方没传过滤条件」→ 看得见全部。
-- 所以列是 NOT NULL DEFAULT '{}'：任何一条写入路径都构造不出 NULL，这个区别就只剩
-- 「空数组」一种写法。同一条不变式写在 platform/auth/store_scope.go 的 WithStoreScope 上
-- （nil → 空切片的归一化），两处是一件事的两端。
--
-- scope_ids 指向 merchant 库的 brands / stores，跨库不建外键，归属由 user-service 在写入
-- 时逐个校验（每个目标都必须属于这个账号的商户）；品牌/门店被删时由 merchant-service 发起、
-- user-service 把那个 id 从数组里摘掉。索引是 GIN：ResetScopeByTarget 问的是「哪个账号的范围
-- 里还带着这个 id」，那是数组包含，B-tree 帮不上忙。
CREATE INDEX idx_merchant_users_merchant  ON merchant_users(merchant_id);

CREATE INDEX idx_merchant_users_scope_ids ON merchant_users USING GIN (scope_ids);

CREATE TRIGGER trg_merchant_users_updated_at
  BEFORE UPDATE ON merchant_users
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================
-- 平台侧权限
-- ============================================================

CREATE TABLE admin_roles (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  code        TEXT        NOT NULL UNIQUE,
  name        TEXT        NOT NULL,
  description TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  admin_roles             IS '平台管理员角色定义表';
COMMENT ON COLUMN admin_roles.code        IS '角色唯一标识码，用于 Casbin subject，如 super_admin / operator';
COMMENT ON COLUMN admin_roles.name        IS '角色显示名，如 超级管理员、运营人员';
COMMENT ON COLUMN admin_roles.description IS '角色职责说明';

CREATE TRIGGER trg_admin_roles_updated_at
  BEFORE UPDATE ON admin_roles
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE admin_permissions (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  code        TEXT        NOT NULL UNIQUE,
  perm_group  TEXT        NOT NULL DEFAULT '',
  name        TEXT        NOT NULL,
  description TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  admin_permissions             IS '平台权限定义表；code 是唯一标识，也是鉴权与绑定使用的键';
COMMENT ON COLUMN admin_permissions.code        IS '权限唯一标识码，如 admin:roles:view；语义由 code 自身承载';
COMMENT ON COLUMN admin_permissions.perm_group  IS '前端分组展示用（中文显示名），如 商户管理、订单管理、设备管理';

CREATE TABLE admin_role_permissions (
  role_id       UUID NOT NULL REFERENCES admin_roles(id) ON DELETE CASCADE,
  permission_id UUID NOT NULL REFERENCES admin_permissions(id) ON DELETE CASCADE,
  PRIMARY KEY (role_id, permission_id)
);

COMMENT ON TABLE admin_role_permissions IS '平台角色与权限的绑定关系，多对多';

CREATE TABLE admin_user_role_bindings (
  admin_user_id UUID        NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
  role_id       UUID        NOT NULL REFERENCES admin_roles(id) ON DELETE CASCADE,
  granted_by    UUID        REFERENCES admin_users(id),
  granted_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (admin_user_id, role_id)
);

COMMENT ON TABLE  admin_user_role_bindings            IS '平台管理员与角色的绑定关系，带授权人和时间记录';
COMMENT ON COLUMN admin_user_role_bindings.granted_by IS '执行授权操作的管理员ID，NULL 表示系统初始化写入';
COMMENT ON COLUMN admin_user_role_bindings.granted_at IS '角色被授予的时间';

-- admin_permissions 上**没有** resource / action 两列：那是把 code 拆成「资源 + 动作」的
-- 旧结构，全仓没有任何读取方（Go 模型、前端权限页、Casbin 策略都只用 code），而
-- AdminPermissionService.Create 从来不写它们，于是 POST /v1/admin/permissions 恒 500
-- （400 分支之前的那一步就炸）。修法不是让服务层去补两列：现有数据的 resource 与 code
-- 之间没有可推导的规则（admin:users:view -> admin_users，但 admin:menus:view -> menus），
-- 补出来的值必然是编的。既然无人读，就整个不要它，语义完全由 code 承载。

CREATE INDEX idx_admin_role_perms_role_id ON admin_role_permissions(role_id);
CREATE INDEX idx_admin_role_perms_perm_id ON admin_role_permissions(permission_id);
CREATE INDEX idx_admin_user_role_user_id  ON admin_user_role_bindings(admin_user_id);
CREATE INDEX idx_admin_user_role_role_id  ON admin_user_role_bindings(role_id);
CREATE INDEX idx_admin_perms_group        ON admin_permissions(perm_group);

-- ============================================================
-- 后台菜单树
--
-- 侧边栏是从这张表来的（前端 menuDataRender 整个替换掉静态路由里的菜单项），所以
-- 「页面删了但菜单行还在」的表现不是 404 而是空白页。内容在 002_identity_seed.sql。
-- ============================================================

CREATE TABLE admin_menus (
  id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  parent_id  UUID        REFERENCES admin_menus(id) ON DELETE CASCADE,
  name       TEXT        NOT NULL,
  path       TEXT        NOT NULL DEFAULT '',
  icon       TEXT        NOT NULL DEFAULT '',
  sort       INTEGER     NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  admin_menus            IS '后台菜单树：目录与菜单；path 为空的节点仅作为分组目录';
COMMENT ON COLUMN admin_menus.parent_id  IS '父菜单 ID，NULL 表示顶级';
COMMENT ON COLUMN admin_menus.path       IS '前端路由路径；目录节点为空字符串';
COMMENT ON COLUMN admin_menus.icon       IS '图标名（Ant Design Icons 组件名）';

CREATE INDEX idx_admin_menus_parent ON admin_menus(parent_id);
CREATE INDEX idx_admin_menus_sort   ON admin_menus(sort, created_at);

CREATE TABLE admin_role_menus (
  role_id UUID NOT NULL REFERENCES admin_roles(id) ON DELETE CASCADE,
  menu_id UUID NOT NULL REFERENCES admin_menus(id) ON DELETE CASCADE,
  PRIMARY KEY (role_id, menu_id)
);

COMMENT ON TABLE admin_role_menus IS '角色-菜单绑定：决定角色可见的侧栏菜单';

-- ============================================================
-- 后台业务操作日志
--
-- merchant_id 原本外键指向 merchants。拆库后商户表在 merchant 库，该列降级为软指针：
-- 只保留 ID 用于按商户筛选历史日志，不做任何归属校验。
-- 日志自身仍不对目标对象建外键，目标删除后日志照常可查。
-- ============================================================

CREATE TABLE admin_operation_logs (
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
  merchant_id         UUID,

  -- 操作结果
  result              TEXT NOT NULL DEFAULT 'success',
  error_code          TEXT NOT NULL DEFAULT '',
  error_message       TEXT NOT NULL DEFAULT '',

  -- 经过脱敏的业务数据快照
  before_data         JSONB,
  after_data          JSONB,

  occurred_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),

  -- 产生这条日志的 message_outbox 事件 ID，用于消费端幂等去重；非事件来源的行为空
  event_id            TEXT
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
  '关联商户 ID；跨库软指针，不加外键，用于按商户筛选历史日志';
COMMENT ON COLUMN admin_operation_logs.result IS
  '操作结果：success=成功，failure=失败';
COMMENT ON COLUMN admin_operation_logs.before_data IS
  '变更前快照，仅保存经过脱敏的业务字段';
COMMENT ON COLUMN admin_operation_logs.after_data IS
  '变更后快照，仅保存经过脱敏的业务字段';
COMMENT ON COLUMN admin_operation_logs.event_id IS
  '产生这条日志的 message_outbox 事件 ID，用于消费端幂等去重；非事件来源的行为空';

CREATE INDEX idx_admin_operation_logs_occurred_at
  ON admin_operation_logs (occurred_at DESC);
CREATE INDEX idx_admin_operation_logs_admin_time
  ON admin_operation_logs (admin_user_id, occurred_at DESC);
CREATE INDEX idx_admin_operation_logs_module_time
  ON admin_operation_logs (module, occurred_at DESC);
CREATE INDEX idx_admin_operation_logs_action_time
  ON admin_operation_logs (action, occurred_at DESC);
CREATE INDEX idx_admin_operation_logs_target
  ON admin_operation_logs (target_type, target_id, occurred_at DESC)
  WHERE target_id IS NOT NULL;
CREATE INDEX idx_admin_operation_logs_merchant_time
  ON admin_operation_logs (merchant_id, occurred_at DESC)
  WHERE merchant_id IS NOT NULL;
CREATE INDEX idx_admin_operation_logs_result_time
  ON admin_operation_logs (result, occurred_at DESC);

-- 这张表由 outbox 事件的消费者写入（此前它只有建表语句，没有任何写入方）。消费者本身的
-- 去重由 message_inbox 的租约保证，但「写库成功、标记 inbox 完成失败」这一小段窗口会让
-- 同一条消息被重新投递；没有唯一约束时它就会写成两行，审计日志出现重影。
--
-- 存一个独立的 event_id 列而不是复用主键 id：id 是日志自身的标识，让外地的事件 ID 占用它
-- 会把「这条日志」和「那次投递」绑死。有了唯一索引，消费者用
-- INSERT ... ON CONFLICT (event_id) WHERE event_id IS NOT NULL DO NOTHING 就能做到
-- 重投不重写——谓词不能省：冲突目标必须与索引定义逐字相同（索引带 WHERE），少了它
-- PostgreSQL 找不到可用的唯一索引，语句直接报错（见 user-service 的
-- internal/repository/operation_log.go）。
--
-- 允许为空：消费者上线前写入的历史行、以及将来可能的非事件来源行都没有 event_id。
-- 唯一索引建成只覆盖非空值的部分索引，把「唯一」的适用范围写在索引定义里——约束的对象
-- 只是事件产生的行，历史行不在其内（普通唯一索引也不会让多行 NULL 互相冲突，但那样索引
-- 里会白白带着一堆永远不回查的 NULL）。
CREATE UNIQUE INDEX admin_operation_logs_event_id_key
  ON admin_operation_logs (event_id) WHERE event_id IS NOT NULL;

-- ============================================================
-- Casbin policy 存储
--
-- 表名 casbin_rule（单数）与 casbin-go PostgreSQL adapter 默认一致，但**它既不是读的
-- 来源、也不再被任何代码写入**：user-service 的 adapter 从角色-权限/用户-角色两张关系
-- 表派生策略，SavePolicy 一系全是空实现（见 internal/casbin/enforcer.go）。留着它是因为
-- 对方 adapter 假定表存在，历史行也不再参与判定。禁止业务代码直接操作。
-- ============================================================

CREATE TABLE casbin_rule (
  id    BIGSERIAL   PRIMARY KEY,
  ptype TEXT        NOT NULL,
  v0    TEXT        NOT NULL DEFAULT '',
  v1    TEXT        NOT NULL DEFAULT '',
  v2    TEXT        NOT NULL DEFAULT '',
  v3    TEXT        NOT NULL DEFAULT '',
  v4    TEXT        NOT NULL DEFAULT '',
  v5    TEXT        NOT NULL DEFAULT ''
);

COMMENT ON TABLE  casbin_rule       IS 'Casbin policy 存储表，由 casbin-go adapter 管理，禁止业务代码直接操作';
COMMENT ON COLUMN casbin_rule.ptype IS '规则类型：p=权限策略 g=用户角色分组';
COMMENT ON COLUMN casbin_rule.v0    IS 'ptype=p: 角色标识(subject)；ptype=g: 用户ID';
COMMENT ON COLUMN casbin_rule.v1    IS 'ptype=p: 保留为空；ptype=g: 角色code';
COMMENT ON COLUMN casbin_rule.v2    IS 'ptype=p: 权限码(action)；ptype=g: 保留为空';
COMMENT ON COLUMN casbin_rule.v3    IS 'ptype=p: 作用域(domain)，平台域为 *；ptype=g: 保留为空';

CREATE UNIQUE INDEX idx_casbin_rule_unique
  ON casbin_rule (ptype, v0, v1, v2, v3, v4, v5)
  NULLS NOT DISTINCT;

CREATE INDEX idx_casbin_ptype_v0 ON casbin_rule(ptype, v0);
CREATE INDEX idx_casbin_ptype_v2 ON casbin_rule(ptype, v2);
CREATE INDEX idx_casbin_ptype_v3 ON casbin_rule(ptype, v3);

-- ============================================================
-- C 端（小程序）用户
--
-- 身份库此前只有两套 B 端账号，这第三套是唯一一套面向小程序的。
--
-- 为什么落在 user-service / 身份库，而不是新起一个「用户服务」：计划 §5.2 把
-- 「小程序用户、微信 code2Session、openid/unionid 绑定、Access/Refresh Token
-- 与会话撤销」明确划归 user-service；计划的 §8.1 虽然列过一个 panda_user 库名，
-- 但实现里 user-service 的库就叫 panda_identity，照库名再拆一个身份源出来没有
-- 任何收益，只会让「谁是用户身份的唯一来源」这个问题变模糊。
--
--    users                   账号主体（手机号是账号属性，兼登录标识）
--    user_wechat_identities  微信身份绑定（openid 按应用独立，可多行）
--    user_sessions           Refresh Token 与会话撤销
--    user_login_events       登录安全事件审计
--    user_sms_codes          短信验证码（见下面那一节）
--
-- 表名没有写 miniapp_ 前缀：identity 库里没有同名的 C 端表，而 admin_/merchant_
-- 前缀的用意是区分「后台的管理员」与「商户的员工」，C 端用户不需要第三种前缀来
-- 和它们区分——它就是「用户」。coupon 库的 user_coupons.user_id 注释里写的是
-- 「持券用户 ID，属于身份服务时仅作跨库值引用」，指的就是这张 users.id，所以这里
-- 改表名会连带让那边的注释失准。主键是 UUID，与那列的类型对得上。
-- ============================================================

-- phone 可空且唯一：三种登录方式里，「微信一键登录」拿到的只有 openid，用户可能
-- 一直没有授权手机号；而「手机号+短信验证码」是先有手机号、后有（或一直没有）
-- 微信身份。两边都得能建号。PG 的 UNIQUE 允许多个 NULL，所以「多行都没绑手机号」
-- 是合法的，不会互相冲突。
--
-- 刻意存 NULL 而不是空串：legacy 的 users 集合用 "" 表示「没有手机号」，迁移时
-- 必须归一化成 NULL，否则所有空串行会在唯一索引上互撞，迁移直接失败。
CREATE TABLE users (
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
CREATE TRIGGER trg_users_updated_at
  BEFORE UPDATE ON users
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================
-- 微信身份绑定
-- ============================================================

-- 唯一键是 (app_type, openid) 而不是单独 openid：openid 的空间按应用划分，同一个
-- 字符串在不同应用里可以指不同的人，单列唯一会把正常情况判成冲突。
-- unionid 可空：没绑定微信开放平台账号的小程序拿不到它。
--
-- 为什么单独一张表、而不是在 users 上放 openid/unionid 两列：openid 是「按应用」的
-- 标识，同一个微信用户在公众号和两个小程序里各有一个 openid，共享的只有 unionid。
-- 放在 users 上意味着接入第二个应用时要改表并回填；独立成表则只是多一行。
CREATE TABLE user_wechat_identities (
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

CREATE INDEX user_wechat_identities_user_idx ON user_wechat_identities (user_id);

-- unionid 只建部分索引：它可空，而部分索引不收录 NULL 行，索引更小。补 unionid 的
-- 用途是把同一微信用户在多个应用下的账号认出来。
CREATE INDEX user_wechat_identities_unionid_idx
  ON user_wechat_identities (unionid) WHERE unionid IS NOT NULL;

CREATE TRIGGER trg_user_wechat_identities_updated_at
  BEFORE UPDATE ON user_wechat_identities
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================
-- 会话与 Refresh Token
-- ============================================================

-- legacy 完全没有 refresh token：配置里的 RefreshTokenExpiry 是个零实现的死键，
-- 30 天到期只能重新登录。V2 补上，而「能签发」的前提是「能撤销」，所以要有落库的
-- 会话记录。
--
-- 只存 refresh token 的哈希：这张表一旦泄露，明文 token 等于可以直接冒用登录态。
-- 校验时对请求里的 token 取哈希再等值查，原文任何时候都不落库、不回显。
--
-- admin_sessions 与本表**故意同形**（只差外键指向 admin_users），是为了让 Go 那边能用
-- 同一份实现（repository 的 sessionStore），而不是把同一段 SQL 抄两遍——那种抄法一定
-- 会漂移。
CREATE TABLE user_sessions (
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
CREATE INDEX user_sessions_user_active_idx
  ON user_sessions (user_id) WHERE revoked_at IS NULL;

CREATE TRIGGER trg_user_sessions_updated_at
  BEFORE UPDATE ON user_sessions
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================
-- 登录安全事件
-- ============================================================

-- 刻意不对 users 建外键，理由与 admin_operation_logs 的目标列一致：账号注销后
-- 审计仍要可查，外键的 ON DELETE 动作（无论 CASCADE 还是 SET NULL）都会毁掉这段
-- 记录。user_id 在这里是软指针。
--
-- user_id 本身可空：登录失败时可能根本没匹配到账号（手机号不存在、openid 没绑过），
-- 这类事件恰恰是审计最想看的，不能因为「没有用户」就丢掉。
CREATE TABLE user_login_events (
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
CREATE INDEX user_login_events_user_time_idx
  ON user_login_events (user_id, created_at DESC);

-- user_login_events 是只追加的事件表，没有 updated_at，不需要触发器。

-- ============================================================
-- 小程序短信验证码
--
-- 手机号 + 短信验证码是 C 端三种登录方式之一，也是唯一一种需要服务端暂存一个秘密的。
--
-- 为什么进 Postgres 而不是 Redis：这个决定改过一次，原来是打算放 Redis 的，因为验证码
-- 是 5 分钟 TTL 的典型短命数据。问题出在 platform/cache 这个客户端上——它的接口只有
-- Ping/Close/Publish/Subscribe，没有 Get/Set 一类的 KV 操作，也就是说当前根本没有可用
-- 的验证码存储。而它更危险的一面是：REDIS_ADDR 为空时构造出来的是 Noop，Noop.Publish
-- 返回 nil，即调用方看到的是「成功」。把验证码校验建在一个连不上时静默放行的组件上，
-- 等于给了一个「Redis 挂掉期间任意验证码都通过」的窗口（如果将来补上 KV 也照抄这个失败
-- 语义的话）。落在 Postgres 里，校验依赖的是和 users 表同一个连接、同一个事务，失败就是
-- 明确的错误。
-- ============================================================

-- phone 直接做主键，一个手机号同时只有一个有效验证码：
--   1. 重发就是覆盖（INSERT ... ON CONFLICT (phone) DO UPDATE），不需要额外的「失效旧码」
--      逻辑，也就不会出现旧码忘了失效而被继续使用的情况；
--   2. 同一个号不可能攒出多个同时有效的码——否则反复请求就能把猜中的概率乘上去。
-- 代价是「登录」和「绑手机」两个用途不能同时持有验证码，先要的那个会被顶掉。这在产品上
-- 不是问题：这两条流程不会并行发生，而少一个并发维度就少一类漏洞。
--
-- code_hash 存哈希不存原文，理由与 user_sessions.refresh_token_hash 相同：这两张表一旦
-- 被导出，里面的行就等于可以直接使用的凭据。验证码只有 6 位、空间很小，所以哈希算法必须
-- 是带盐的慢哈希（bcrypt），不能用裸 SHA——否则对着一张导出表把 10^6 个候选全算一遍是
-- 瞬间的事。哈希的选择与验证都在 service 层。
--
-- attempts 是校验失败次数，由 service 层累加并在达到上限时作废整条记录。列在这里是因为它
-- 必须和验证码同生命周期、同事务地读改写：放在进程内存里会在多副本下每个副本各算各的，
-- 等于把上限乘以副本数。
--
-- created_at 兼两个用途：这条码的签发时间，以及重发频控的基准（service 层据此拒绝过于频繁
-- 的重发）。expires_at 单独一列而不是 created_at + 固定 TTL，是为了让 TTL 将来可按用途
-- 调整而不必改数据。
--
-- 不对 users 建外键：验证码可以在「还没有这个账号」时请求（注册流程），那时 users 里没有
-- 对应行。这一点和 user_login_events 不对 users 建外键是同一个理由。
--
-- 过期行的清理：成功校验后 service 层直接删掉该行，所以常态下这张表的大小约等于「当前正在
-- 验证中的手机号数量」。只有「要了码又不回来」的号会留下过期行，而它们会在同一个号下次请求
-- 时被覆盖，或由将来的清理任务按 expires_at 扫掉——下面的索引就是为后者准备的。表的行数
-- 上界因此是「历史上请求过验证码的手机号数」，不是请求次数。
CREATE TABLE user_sms_codes (
  phone      TEXT        PRIMARY KEY,
  purpose    TEXT        NOT NULL,
  code_hash  TEXT        NOT NULL,
  attempts   INTEGER     NOT NULL DEFAULT 0,
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT user_sms_codes_purpose_check CHECK (purpose IN ('login', 'bind_phone'))
);

CREATE INDEX user_sms_codes_expires_idx ON user_sms_codes (expires_at);

-- ============================================================
-- 后台管理员的会话
--
-- user_sessions 是**小程序**登录会话（它的外键指向 users），而平台管理员住在
-- admin_users 里，两套账号体系没有任何一行是重叠的。在 admin_sessions 出现之前，
-- 后台登录**根本不落会话**，于是：
--
--   * 后台的「退出登录」是个空操作——令牌还在，谁拿着都能继续换新的；
--   * 某个管理员账号被停用后，他手上那枚 refresh token 照样能换出一对新令牌；
--   * refresh 只验签名、不查会话，所以**任何**本服务签发的 refresh token（包括 C 端顾客、
--     商户员工的那两种）都能打到后台接口上换出后台令牌。
--
-- 这三件事只能一起解决，而它们的共同前提是「撤销要有地方记」。撤销状态必须落库，所以
-- 需要这张表；不能借用 user_sessions，因为外键会把管理员 id 挡在 users 之外。
--
-- 结构、列注释、索引口径全部与 user_sessions 保持一致，理由不再重复。唯一实质的区别就是
-- 外键指向 admin_users，级联删除跟着账号走：管理员账号被删，他的会话没有留着的道理。
-- ============================================================

CREATE TABLE admin_sessions (
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
CREATE INDEX admin_sessions_user_active_idx
  ON admin_sessions (user_id) WHERE revoked_at IS NULL;

CREATE TRIGGER trg_admin_sessions_updated_at
  BEFORE UPDATE ON admin_sessions
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ============================================================
-- outbox / inbox 表（身份库）
-- ============================================================
--
-- 两个库各自需要一对 outbox / inbox，跨库写不共享表。
--
-- 顺序是硬要求：ALTER 必须先于 CREATE INDEX，否则对「由早期 schema 建出来、没有 lease
-- 列」的表执行本文件时会因缺列而失败。这里保留 IF NOT EXISTS 与那八条 ALTER，正是因为
-- identity 库可能是拆库前就存在的那一个。
--
-- 本段在十一个集里是同一份拷贝，但只有 identity 与 merchant 这两份带 ALTER 兜底（它们的
-- 表先于租约列存在）；其余九个域直接写进 CREATE TABLE。列与索引的写法不要顺手调整，
-- `migrations/migrations_test.go` 逐列比对，且逐字节比对这两份。

-- >>> message-tables:begin >>>

CREATE TABLE IF NOT EXISTS message_outbox (
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

CREATE TABLE IF NOT EXISTS message_inbox (
    event_id TEXT PRIMARY KEY CHECK (char_length(trim(event_id)) > 0),
    claimed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_owner TEXT,
    lease_token TEXT,
    lease_until TIMESTAMPTZ,
    completed_at TIMESTAMPTZ
);

-- Add columns before creating indexes so this migration also works on tables
-- created by the original schema, which did not include lease columns.
ALTER TABLE message_outbox ADD COLUMN IF NOT EXISTS lease_owner TEXT;
ALTER TABLE message_outbox ADD COLUMN IF NOT EXISTS lease_token TEXT;
ALTER TABLE message_outbox ADD COLUMN IF NOT EXISTS lease_until TIMESTAMPTZ;
ALTER TABLE message_outbox ADD COLUMN IF NOT EXISTS last_error TEXT;
ALTER TABLE message_inbox ADD COLUMN IF NOT EXISTS lease_owner TEXT;
ALTER TABLE message_inbox ADD COLUMN IF NOT EXISTS lease_token TEXT;
ALTER TABLE message_inbox ADD COLUMN IF NOT EXISTS lease_until TIMESTAMPTZ;
ALTER TABLE message_inbox ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS message_outbox_pending_idx
    ON message_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;

CREATE INDEX IF NOT EXISTS message_outbox_lease_idx
    ON message_outbox (lease_until)
    WHERE published_at IS NULL;

-- <<< message-tables:end <<<

COMMIT;
