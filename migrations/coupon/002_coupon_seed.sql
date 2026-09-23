-- coupon 种子数据：券类型字典
--
-- 为什么是数据迁移而不是纯 DDL：coupon_templates.coupon_type_id 是 NOT NULL
-- 且 ON DELETE RESTRICT，后台「新建模板」的「优惠券类型」又是必填下拉。全新库
-- 上没有任何类型可选，一张模板都建不出来。此前库里那两行（CASH / DISCOUNT）
-- 是手工插的演示数据，不在任何迁移里，所以新环境复现不出来。
--
-- 编码沿用 panda_serve/internal/models/coupon_type.go 的取值，而不是新造一套：
-- coupon_types.code 是不可修改的稳定标识（见 001 的 prevent_coupon_type_code_change
-- 触发器），将来导旧数据或与小程序对接时不用再写一层映射。
--
-- 只种这三类。旧系统的 FULL_DISCOUNT（商户满减券）、PERCENTAGE_DISCOUNT（商户折扣券）、
-- MERCHANT_EXCHANGE（商户兑换券）在 V2 没有对应业务：平台/商户的区别由
-- coupon_templates.merchant_id 是否为 NULL 表达，不再体现在类型码里。
--
-- 幂等：按 code 判存在，重复执行插 0 行。

BEGIN;

INSERT INTO coupon_types (code, name, description, status)
SELECT v.code, v.name, v.description, 'active'
FROM (VALUES
    ('COFFEE_CASH',                 '代金券',       '按面值抵扣，可设最低消费'),
    ('COFFEE_EXCHANGE',             '兑换券',       '兑换指定商品，不参与金额计算'),
    ('MEMBERSHIP_PRICE_EXPERIENCE', '会员价体验券', '按会员价结算的体验券')
) AS v(code, name, description)
WHERE NOT EXISTS (SELECT 1 FROM coupon_types t WHERE t.code = v.code);

COMMIT;
