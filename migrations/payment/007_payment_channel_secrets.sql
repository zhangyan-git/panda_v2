-- payment/007：渠道凭据自己落一列（密文）
--
-- 补 payment_channels.secrets JSONB。
--
-- # 为什么不再只存键名
--
-- 001 里这一列不存在，凭据的位置是 secret_ref，注释写着「密钥在受控 Secret 管理系统里的
-- 键名（或环境变量名），**不是密钥本身**」。那条约定挡住的东西是对的（config 列会原样
-- 出现在后台响应里，密钥塞进去等于每次打开那一页就把它发给浏览器），但代价是
-- **加一个渠道仍然要改部署配置 + 重启服务** —— 而「配置完直接能用」正是这批要解决的问题。
--
-- 真接微信还躲不过去：它要商户私钥（多行 PEM）与平台证书（同样多行、而且会轮换），
-- 这两样都不适合摆在环境变量里。
--
-- # 存的是什么
--
-- 槽名 → 信封，信封长这样（见 platform/secret 的 Envelope）：
--
--     {"signKey": {"kind": "text", "nonce": "<base64>", "ciphertext": "<base64>"}}
--
-- AES-256-GCM，一次一密，主密钥来自 PAYMENT_SECRET_KEY（**不在这张表里，也不在库里**）。
-- AAD 绑住槽名与 kind，所以密文换个槽放、或者把 kind 改掉，解密都会失败。
--
-- 三个字段全是 base64 或短枚举，所以这一列**可以直接给人看**：psql 里看到的不该是明文，
-- 也不该是一坨没法读的二进制。
--
-- # secret_ref 保留
--
-- 它没有删：解析顺序是**库里的密文 → secret_ref 指的环境变量 → 空串**（空串在适配器
-- 那边是「验不了签」，一律拒）。老渠道（manual 那类）继续走环境变量，一行都不用改；
-- 新渠道走密文，改配置不用重启。
--
-- # 为什么加那条 CHECK
--
-- 邻近的 config 列没有类型约束，这一列加一条是**刻意的不一致**：config 解不出来只是
-- 配置读错了，而 secrets 解不出来等于「这个渠道的每一笔支付都做不了」，而且报出来的
-- 是一句「解密失败」——排查要从「是不是有人改过库」开始，方向完全是错的。让它在写入
-- 那一刻就撞约束，比在支付那一刻变成一个密码学错误便宜得多。

BEGIN;

ALTER TABLE payment_channels
    ADD COLUMN secrets JSONB NOT NULL DEFAULT '{}'::jsonb;

-- 只接受对象：槽名 → 信封。数组、标量、null 都进不来。
ALTER TABLE payment_channels
    ADD CONSTRAINT payment_channels_secrets_is_object CHECK (jsonb_typeof(secrets) = 'object');

COMMENT ON COLUMN payment_channels.secrets IS
    '渠道凭据的密文槽：槽名 → {kind, nonce, ciphertext}（AES-256-GCM，见 platform/secret）。'
    '主密钥来自 PAYMENT_SECRET_KEY，不在库里。解析顺序：本列的密文 → secret_ref 指的环境变量 → 空串。';

COMMIT;
