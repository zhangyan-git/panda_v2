-- coupon/003：发券幂等缓存响应的键名由 snake_case 改为 camelCase
--
-- 背景：IssueCouponsResponse 的 json tag 统一到 camelCase（后台接口风格统一）。
-- coupon_idempotency_keys.response 存的是首次处理结果的 JSON 快照，重放时会被
-- 原样反序列化回 IssueCouponsResponse。旧行的键不改写的话，改名后全部对不上，
-- 重放会静默返回一个 batchId 为空串、issuedQuantity 为 0 的空对象——不报错，
-- 但调用方拿到的是坏数据。所以这里把旧行原地改写成新键名。
--
-- 只处理 scope='admin.coupons.issue'：另外两个幂等作用域（coupon.redeem、
-- coupon.revoke）缓存的是 model.UserCoupon，该结构体没有 json tag，序列化出来
-- 是 Go 字段名（PascalCase），不受本次改名影响。
--
-- 幂等：改写后 'batch_id' 键不再存在，WHERE 里的 response ? 'batch_id' 自然
-- 不再命中，因此重复执行是安全空操作。
--
-- 注意 request_hash 不在这里迁移、也迁移不了：表里只存了摘要、没存原始请求体，
-- 算不回去。哈希的一致性改由代码保证——service 包里的 issueHashPayload 冻结了
-- 旧 tag 渲染，改名前后产出的哈希逐字节相同（有黄金摘要测试钉住）。

UPDATE coupon_idempotency_keys
SET response = jsonb_build_object(
        'batchId',        response -> 'batch_id',
        'issuedQuantity', response -> 'issued_quantity',
        'userCouponIds',  response -> 'user_coupon_ids'
    )
WHERE scope = 'admin.coupons.issue'
  AND response ? 'batch_id';
