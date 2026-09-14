-- coupon/005：金额单位由「元」统一为「分」（BIGINT），并删除折扣比例相关字段
--
-- 背景：V2 里有钱的地方原本只有优惠券这一处，用 numeric(18,2) 存「元」、接口上传
-- 定点字符串。现在统一成「分 + BIGINT」，与 panda_serve 已有的约定一致
-- （membership_card.price 分、user_balance.balance 分），顺带消掉小数解析这条
-- —— 它本身就是 500 的来源：请求里的垃圾串会一路进到 numeric 列，Postgres 抛
-- 22P02，最后被兜成 INTERNAL_ERROR。改成 BIGINT 后，类型不对在 JSON 解码阶段就
-- 是 400，压根到不了数据库。
--
-- 同时删除 discount_rate / discount_cap：产品确认优惠券不做折扣比例（券类型只有
-- 代金券/兑换券/会员价体验券，没有按比例打折这一类）。discount_cap 是「折扣金额
-- 上限」，没有折扣比例它约束不了任何东西，一并不留。
--
-- 换算 ×100 天生不幂等：重复执行会把 1000 分再乘成 100000 分。runner 靠
-- schema_migrations 保证只跑一次，但这里仍然按列的当前类型做守卫，让这个文件
-- 被手工重放时是空操作，而不是静默把金额放大 100 倍。
--
-- 只动 coupon 库：全仓其余服务（user / merchant / gateway / platform）没有任何
-- 金额列，没有第二处要迁。

-- 1. 金额列：numeric(18,2) 元 → bigint 分
--
-- 「先删 CHECK 再改类型再补 CHECK」是刻意的：CHECK 表达式里带 (0)::numeric 这样的
-- 常量，改类型时由 Postgres 自行重解析虽然能过，但重解析的规则（尤其常量这一侧的
-- 类型）在不同版本间不保证一致。显式重建的结果是确定的：新约束就是 bigint >= 0。
DO $$
DECLARE
    target   record;
    col_type text;
BEGIN
    FOR target IN
        SELECT * FROM (VALUES
            ('coupon_templates',   'face_value'),
            ('coupon_templates',   'min_purchase_amount'),
            ('coupon_templates',   'purchase_price'),
            ('user_coupons',       'face_value'),
            ('user_coupons',       'min_purchase_amount'),
            ('coupon_redemptions', 'amount_before'),
            ('coupon_redemptions', 'discount_amount'),
            ('coupon_redemptions', 'amount_after')
        ) AS t(tbl, col)
    LOOP
        SELECT format_type(a.atttypid, a.atttypmod) INTO col_type
        FROM pg_attribute a
        WHERE a.attrelid = target.tbl::regclass
          AND a.attname = target.col
          AND NOT a.attisdropped;

        -- 已经是 bigint（= 本文件跑过了），或列压根不存在：不动。
        CONTINUE WHEN col_type IS NULL OR col_type <> 'numeric(18,2)';

        EXECUTE format('ALTER TABLE %I DROP CONSTRAINT IF EXISTS %I',
                       target.tbl, target.tbl || '_' || target.col || '_check');
        EXECUTE format('ALTER TABLE %I ALTER COLUMN %I TYPE BIGINT USING (%I * 100)::BIGINT',
                       target.tbl, target.col, target.col);
        EXECUTE format('ALTER TABLE %I ADD CONSTRAINT %I CHECK (%I >= 0)',
                       target.tbl, target.tbl || '_' || target.col || '_check', target.col);
    END LOOP;
END $$;

-- 2. 折扣比例与折扣上限：整块删掉
--
-- 删列会连带删掉这两列自己的 CHECK。coupon_templates 的 discount_rate 是
-- numeric(5,2)、discount_cap 是 numeric(18,2)，两者都没有索引、没有被视图引用。
ALTER TABLE coupon_templates DROP COLUMN IF EXISTS discount_rate;
ALTER TABLE coupon_templates DROP COLUMN IF EXISTS discount_cap;
ALTER TABLE user_coupons     DROP COLUMN IF EXISTS discount_rate;
ALTER TABLE user_coupons     DROP COLUMN IF EXISTS discount_cap;

-- 3. 幂等缓存里的金额快照
--
-- coupon.redeem / coupon.revoke 两个作用域缓存的是 model.UserCoupon 的 JSON
-- （该结构体没有 json tag，键是 Go 字段名，金额是字符串）。改类型后旧行的
-- "FaceValue": "25.00" 再也反序列化不进 int64，重放会以 500 收场——不是返回
-- 坏数据，是直接报错，且只在重放同一个 Idempotency-Key 时才出现。
--
-- FaceValue / MinPurchaseAmount 换算成数字，DiscountRate / DiscountCap 直接摘掉。
-- 摘不摘其实都不报错（重放走的是裸 json.Unmarshal，多余的键会被静默忽略），
-- 但结构体里已经没有这两个字段了，留着只是垃圾。
--
-- 空串要按 0 处理：库里确实存在 "MinPurchaseAmount": "" 这种行（早期代码路径没
-- 填这个字段），直接 ::numeric 会抛 22P02。
--
-- 幂等：改写后 FaceValue 变成 number，jsonb_typeof 不再等于 'string'，重复执行
-- 命中 0 行。这正是它的守卫。
UPDATE coupon_idempotency_keys
SET response = (response - 'DiscountRate' - 'DiscountCap') || jsonb_build_object(
        'FaceValue',         (COALESCE(NULLIF(response ->> 'FaceValue', ''), '0')::numeric * 100)::bigint,
        'MinPurchaseAmount', (COALESCE(NULLIF(response ->> 'MinPurchaseAmount', ''), '0')::numeric * 100)::bigint
    )
WHERE scope IN ('coupon.redeem', 'coupon.revoke')
  AND jsonb_typeof(response -> 'FaceValue') = 'string';

-- 4. 注释跟着单位改（002 里写的是「单位为元」）
COMMENT ON COLUMN coupon_templates.face_value          IS '优惠券面值，单位为分';
COMMENT ON COLUMN coupon_templates.min_purchase_amount IS '使用优惠券所需的最低消费金额，单位为分';
COMMENT ON COLUMN coupon_templates.purchase_price      IS '购买价格，单位为分；0 表示免费领取';
COMMENT ON COLUMN user_coupons.face_value              IS '领取时面值快照，单位为分';
COMMENT ON COLUMN user_coupons.min_purchase_amount     IS '领取时最低消费快照，单位为分';
COMMENT ON COLUMN coupon_redemptions.amount_before     IS '核销前订单金额，单位为分';
COMMENT ON COLUMN coupon_redemptions.discount_amount   IS '本次优惠金额，单位为分';
COMMENT ON COLUMN coupon_redemptions.amount_after      IS '核销后应付金额，单位为分';
