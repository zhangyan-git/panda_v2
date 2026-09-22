-- partner/001：开放平台（合作方接入）领域核心表结构
--
-- 本迁移属于 panda_partner。合作方的用户、订单、券、会员资格等外部服务的事实**一个都不
-- 落在本库**：这一域只拥有三样东西——「谁在用我们的开放接口」「他用哪把钥匙」「他调过
-- 什么」。业务本身一律经 gRPC 转给对应的服务（见 partner-service 的 ingress 包）。
--
-- 边界先说清楚，三处最容易越界的地方：
--
--   * 合作方**不是**商户。老系统把开放接口的密钥挂在 merchants 上
--     （panda_serve/internal/models/merchant.go 的 api_key / api_secret），所以我们
--     一度以为「合作方 = 商户」。实际用这套接口的是外部合作方（发券、查会员、查订单），
--     与「这家店归谁管」是两件事，共用一个实体只会让商户表长出一堆与门店无关的列。
--   * 老系统的 MerchantAPIAuth 这张表**从来没有被用过**（声明了结构、没有集合、没有
--     一条读写），真正在跑的是 merchants 上那几个 api_* 列。所以本文件不是它的移植，
--     是把它写在注释里、但只实现过一半的那套治理能力的补齐：老系统定义了
--     ApiWhiteList / ApiRateLimit，中间件里**一行校验都没有**（全仓 grep 只在 model
--     、admin DTO 与生成的 swagger 里出现），本库把它们真的接上（见 002 的注释）。
--   * 调用日志是**本域的**一张只增表。它与身份库的 admin_operation_logs 不是一回事：
--     那张记的是后台人工操作的审计，这张记的是合作方机器调用的流水。两者都不许合并
--     ——合并会逼出一张「有的行有操作人、有的行有 api_key」的表。
--
-- 三张表的分工：
--
--   partner_accounts   谁在用我们的开放接口（身份、联系人、总开关）
--   partner_api_keys   凭据与访问控制（密钥、密文签名密钥、启停、过期、IP 白名单、限流）
--   partner_call_logs  每一次调用（请求体、响应体、状态码、耗时）
--
-- 本文件不带 goose 的 Down 段：platform/database/migrate 把整个文件丢给一次 Exec、
-- 不识别 goose 指令，带上就会在同一个事务里建完表再删掉，而且不报错。

BEGIN;

-- ============================================================
-- 合作方账号
-- ============================================================

CREATE TABLE partner_accounts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- 稳定的业务标识，给人看的（"fengxuan"、"shouchuang"）。与 coupon_types.code 同样
    -- 不可修改：它会被写进日志、工单、对账邮件，改一次这些地方就对不上了。
    code TEXT NOT NULL UNIQUE CHECK (char_length(trim(code)) > 0),
    name TEXT NOT NULL CHECK (char_length(trim(name)) > 0),
    contact_name TEXT NOT NULL DEFAULT '',
    contact_phone TEXT NOT NULL DEFAULT '',
    contact_email TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    -- enabled / disabled。**禁用在这里是总开关**：它一旦不是 enabled，这个合作方名下
    -- **所有**密钥当场失效（中间件每次请求都真查，见 002）。
    --
    -- 与密钥自己的 status 分开是必须的：「这家合作方我们不想再合作了」与「这一把钥匙
    -- 泄了要换一把」是两次不同的操作，前者不该逼着运营挨个去停密钥——漏一把就是一个
    -- 还能继续调用我们的入口。
    status TEXT NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled', 'disabled')),
    -- 整个合作方的到期时间。NULL = 不过期。到点后名下所有密钥一起失效。
    --
    -- 与密钥级别的过期并存是有用的：一次签约一年，那把钥匙也跟着签一年；临时给一个
    -- 合作方开到月底做联调，写在这里比挨个改密钥省事。
    expires_at TIMESTAMPTZ,
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ============================================================
-- API 密钥
-- ============================================================

CREATE TABLE partner_api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- 同一库内的外键，允许（跨库的才禁止）。ON DELETE RESTRICT：密钥是调用日志的归属，
    -- 删掉账号会让历史日志变成查无此人的孤儿行。退出用 status，不用 DELETE。
    partner_id UUID NOT NULL REFERENCES partner_accounts (id) ON DELETE RESTRICT,

    -- X-API-Key 本身：**公开标识**，明文、唯一、有索引。
    --
    -- 它明文落库是有意的，不是偷懒：每一个请求都要拿它做一次等值查找，密文（或加盐
    -- 哈希）都查不到。而它也确实不是密码——它每次请求都写在 HTTP 头里，抓包、日志、
    -- 浏览器插件都看得见。真正当密码用的是下面那列 secret_sealed：**只有拿得到密钥的
    -- 人才能签出对得上的签名**，光有 api_key 一个字节都签不出来。
    --
    -- 想同时做到「标识不可读」与「可查找」需要一张确定性哈希列 + 一张密文列，代价是
    -- 多一列、多一次解密，而它挡住的是「能读库的人」——那个人本来就能读到密文列、
    -- 改 status、甚至改中间件。所以这里只要挡住「不能读库的人」，明文标识够用。
    api_key TEXT NOT NULL UNIQUE CHECK (char_length(trim(api_key)) > 0),

    -- 签名密钥的**密文**（AES-256-GCM 信封，见 platform/secret）：
    --     {"kind": "text", "nonce": "<base64>", "ciphertext": "<base64>"}
    -- 主密钥来自 PARTNER_SECRET_KEY，**不在库里**。AAD 绑住槽名与 kind，所以把这一列
    -- 的内容搬到另一个槽、或者改掉 kind，解密都会失败。
    --
    -- 为什么这一列必须是密文而 api_key 那列不必：这是两列里唯一**签得了名**的东西。
    -- psql 直接看到它不该是一句明文密钥（验收要拿这条当证据）。
    secret_sealed JSONB NOT NULL,
    -- 列表页显示用的掩码（前 4 + "…" + 后 4），创建时算一次就冻在这里。
    --
    -- 有它才不必为了渲染一行列表去解密：读路径上一次解密都不做，密钥就始终只在
    -- 「写请求进来的那一刻」与「验签的那一刻」以明文存在。两列的掩码各存一份而不是
    -- 现算，也让「读接口不回明文」是一条**结构上的**事实而不是一句保证。
    api_key_mask TEXT NOT NULL CHECK (char_length(trim(api_key_mask)) > 0),
    secret_mask TEXT NOT NULL CHECK (char_length(trim(secret_mask)) > 0),
    -- 备注：这把钥匙是给谁的、什么时候发的、为什么换。运营看到一堆掩码时唯一的线索。
    name TEXT NOT NULL DEFAULT '',

    -- 与账号那两列同义，只是范围缩到这一把钥匙。判定时**两者都要过**（见 002）。
    status TEXT NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled', 'disabled')),
    expires_at TIMESTAMPTZ,

    -- IP 白名单。**空数组 = 不限制**，不是「全拒」。
    --
    -- 选「空 = 不限制」是因为另一条路的代价更难看：新签发的密钥默认拒绝一切来源，运营
    -- 不填白名单就调不通，而报出来的是 403——他会先怀疑签名算错了，而真正的原因是一个
    -- 他根本不知道要填的字段。老系统的 ApiWhiteList 也是这个语义（虽然它从来没被读过）。
    --
    -- ⚠️ 白名单判的是**真实客户端地址**，而 partner-service 跑到网关后面时 RemoteAddr
    -- 是网关自己。所以它只在配了 PARTNER_TRUSTED_PROXY_CIDRS（可信代理网段）时才读
    -- X-Forwarded-For；没配时看到的是网关的地址，白名单非空就会**全拒**——失败关闭，
    -- 而且错在配置这一侧，比「白名单悄悄不生效」好查得多。见 internal/ingress/auth.go。
    ip_whitelist TEXT[] NOT NULL DEFAULT '{}',

    -- 每分钟允许的调用次数。按密钥算，不是按 IP：一个合作方可能开多条通道（生产一条、
    -- 联调一条），按 IP 会把同机房的全部算成一个人。
    rate_limit_per_minute INT NOT NULL DEFAULT 60 CHECK (rate_limit_per_minute > 0),

    -- 最后调用时间与总次数。老系统每次调用都 $inc 一次（UpdateMerchantApiLastActive），
    -- 这里保留：它是「这把钥匙到底还在不在用」的唯一答案，也是清理时的判据。
    --
    -- 更新放在**请求之后**、与该次调用的日志分开写（见 ingress/middleware.go 的说明）。
    last_used_at TIMESTAMPTZ,
    call_count BIGINT NOT NULL DEFAULT 0,

    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX partner_api_keys_partner_idx ON partner_api_keys (partner_id);

-- ============================================================
-- 调用日志
-- ============================================================

-- 每一次调用一条。**它是一张只增表**，量级按「合作方数 × 每方每分钟限流」估：一个合作
-- 方按 600/min 跑满就是一天 86 万行，所以下面三件事一件都不能省：
--
--   1. 主键用 BIGINT IDENTITY，不用 UUID：UUID 是 16 字节且随机，在亿级行上光是索引就
--      比数据大，而这张表的写入路径在请求上，插入开销直接变成合作方看到的延迟。
--   2. 请求体与响应体**截断后**才落库（见 002 的 request_body / response_body）。
--   3. 按 (partner_id, created_at DESC) 与 created_at 各建一条索引：前者是「查某个
--      合作方某段时间调了什么」唯一的走法，后者是将来保留期清理脚本要用的。
--
-- ⚠️ **已知缺口：没有保留期清理**。这张表今天只增不删，也没有分区。要补的是一条按
-- created_at 删除的定时任务（或用 pg_partman 按月分区 + 丢老分区），本轮没做——它不是
-- 「配一下就有」的东西，得先定清楚留多久（30 天？90 天？要不要冷存到对象存储），而那
-- 是运营与合规的决定，不是实现的决定。上面那条 created_at 索引就是给它留的入口。
CREATE TABLE partner_call_logs (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    partner_id UUID NOT NULL,
    api_key_id UUID NOT NULL,
    -- 冗余存一份掩码后的标识。日志表**不存 api_key 明文**：它是公开标识，但把一堆
    -- 明文标识连同样例摊在一张人人都能查的表里，没有必要。掩码够定位「是哪把钥匙」。
    api_key_mask TEXT NOT NULL,
    method TEXT NOT NULL,
    path TEXT NOT NULL,
    -- 原始查询串。参数里可能有合作方自己的订单号之类的信息，但它就是请求的一部分，
    -- 排查「他到底带了什么参数」只能靠它。**签名头不在里面**（它们在 HTTP 头里）。
    query TEXT NOT NULL DEFAULT '',
    request_ip TEXT NOT NULL DEFAULT '',
    -- 请求体与响应体：截断到 8 KiB（见 002）。截断而不是整存，是因为一个合作方发一次
    -- 10MB 的报文就是 10MB 的一个 TOAST 值，而这张表按上面的量级增长。
    request_body TEXT NOT NULL DEFAULT '',
    response_body TEXT NOT NULL DEFAULT '',
    status_code INT NOT NULL,
    duration_ms BIGINT NOT NULL,
    -- 内部失败原因（"signature does not match"、"nonce replayed"…）。**只进这张表，
    -- 不回给调用方**：老系统把 "API密钥无效" / "签名验证失败" 原样发回去，等于送攻击者
    -- 一台枚举机，告诉他猜到了哪一步。这里是同一个信息，但只有我们看得到。
    error_code TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX partner_call_logs_partner_time_idx ON partner_call_logs (partner_id, created_at DESC);
CREATE INDEX partner_call_logs_time_idx ON partner_call_logs (created_at);
CREATE INDEX partner_call_logs_api_key_time_idx ON partner_call_logs (api_key_id, created_at DESC);

COMMIT;
