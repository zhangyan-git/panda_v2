-- partner/002：开放平台表与关键字段中文注释
--
-- 与其它域的 _comments 文件同一条约定：001 建结构，本文件写 COMMENT ON。分开是为了让
-- 「改一个字段的含义」与「改一个字段的类型」是两次可分辨的改动——001 已经 apply 过的
-- 环境不会再跑 001，只补注释要新开一个文件。

COMMENT ON TABLE partner_accounts IS '合作方账号：谁在用我们的开放接口（不是商户，见 001 的边界说明）';
COMMENT ON COLUMN partner_accounts.code IS '稳定的合作方编码（fengxuan / shouchuang），创建后不可修改';
COMMENT ON COLUMN partner_accounts.name IS '合作方名称';
COMMENT ON COLUMN partner_accounts.contact_name IS '联系人姓名';
COMMENT ON COLUMN partner_accounts.contact_phone IS '联系人电话';
COMMENT ON COLUMN partner_accounts.contact_email IS '联系人邮箱';
COMMENT ON COLUMN partner_accounts.description IS '备注，给运营看的';
COMMENT ON COLUMN partner_accounts.status IS '合作方总开关：enabled=启用，disabled=禁用（名下所有密钥当场失效）';
COMMENT ON COLUMN partner_accounts.expires_at IS '合作方到期时间，NULL=不过期；到点后名下所有密钥一起失效';
COMMENT ON COLUMN partner_accounts.created_by IS '创建人管理员 ID，仅作跨库值引用（身份库）';

COMMENT ON TABLE partner_api_keys IS 'API 密钥：凭据、签名密钥的密文与访问控制（启停 / 过期 / IP 白名单 / 限流）';
COMMENT ON COLUMN partner_api_keys.partner_id IS '所属合作方，引用本库 partner_accounts';
COMMENT ON COLUMN partner_api_keys.api_key IS 'X-API-Key 的值；公开标识，明文存储（见 001 的说明），不是密码';
COMMENT ON COLUMN partner_api_keys.secret_sealed IS '签名密钥的 AES-256-GCM 密文信封 {kind,nonce,ciphertext}，主密钥来自 PARTNER_SECRET_KEY（不在库里）';
COMMENT ON COLUMN partner_api_keys.api_key_mask IS 'api_key 的展示掩码（前 4 + … + 后 4），创建时算一次；读接口只回它';
COMMENT ON COLUMN partner_api_keys.secret_mask IS '签名密钥的展示掩码（前 4 + … + 后 4），创建时算一次；明文只在创建响应里出现一次';
COMMENT ON COLUMN partner_api_keys.name IS '备注：这把密钥是给谁的 / 什么时候发的 / 为什么换';
COMMENT ON COLUMN partner_api_keys.status IS '密钥状态：enabled=启用，disabled=禁用；与合作方总开关是「与」的关系，每次请求都真查';
COMMENT ON COLUMN partner_api_keys.expires_at IS '密钥到期时间，NULL=不过期；与账号的 expires_at 是「与」的关系';
COMMENT ON COLUMN partner_api_keys.ip_whitelist IS 'IP 白名单；空数组=不限制（不是全拒）。只在配了 PARTNER_TRUSTED_PROXY_CIDRS 时才按 X-Forwarded-For 判真实客户端，否则看到的是网关地址';
COMMENT ON COLUMN partner_api_keys.rate_limit_per_minute IS '每分钟允许的调用次数，按密钥计；超出回 429';
COMMENT ON COLUMN partner_api_keys.last_used_at IS '最后一次调用的时间（不论成功与失败，验签通过之后才更新）';
COMMENT ON COLUMN partner_api_keys.call_count IS '累计调用次数，与 last_used_at 一起更新';
COMMENT ON COLUMN partner_api_keys.created_by IS '签发人管理员 ID，仅作跨库值引用（身份库）';

COMMENT ON TABLE partner_call_logs IS '开放接口调用日志：每一次调用一条，只增不删；请求体与响应体都是截断后的副本';
COMMENT ON COLUMN partner_call_logs.partner_id IS '发起调用的合作方；仅作值引用，与账号表同库但日志不跟着删。认不出调用方时（X-API-Key 查无此键、或头都没带）写全零 UUID 00000000-0000-0000-0000-000000000000——这一条也要留痕，所以不能靠 NULL 表达「不知道」';
COMMENT ON COLUMN partner_call_logs.api_key_id IS '发起调用用的那一把密钥 ID；与 partner_id 同一条约定，认不出时写全零 UUID';
COMMENT ON COLUMN partner_call_logs.api_key_mask IS '该密钥的掩码，便于在日志里认出是哪一把；认不出时为空串（掩码本身不是凭据，空着不泄漏任何东西）';
COMMENT ON COLUMN partner_call_logs.query IS '原始查询串；签名头不在里面（它们在 HTTP 头里）';
COMMENT ON COLUMN partner_call_logs.request_ip IS '判定用的客户端地址（可信代理时是 XFF 解析出来的真实地址）';
COMMENT ON COLUMN partner_call_logs.request_body IS '请求体副本，截断到 8 KiB';
COMMENT ON COLUMN partner_call_logs.response_body IS '响应体副本，截断到 8 KiB；**不含密钥**（创建密钥那类接口不在本表记录的路径上）';
COMMENT ON COLUMN partner_call_logs.status_code IS 'HTTP 响应状态码';
COMMENT ON COLUMN partner_call_logs.duration_ms IS '从进入验签中间件到响应写完的耗时，毫秒';
COMMENT ON COLUMN partner_call_logs.error_code IS '内部失败原因，只进这张表不回给调用方（防枚举，见 001）';
COMMENT ON COLUMN partner_call_logs.created_at IS '调用时间；保留期清理的判据（清理任务尚未实现，见 001 的已知缺口）';
