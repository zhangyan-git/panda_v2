-- membership 种子数据：会员套餐字典
--
-- 为什么必须有：`membership_plans` 是运营在后台**改**的东西，但两个基础套餐是产品定义，
-- 不是内容。新库起来一个套餐都没有时，后台的会员套餐页是空的、小程序的下单页也选不到
-- 任何会员商品——和 coupon 集不种 `coupon_types` 就一张模板都建不出来是同一回事。
--
-- 编码沿用原型里的取值：`membership_plans.code` 是不可修改的稳定业务标识
-- （建表时就写着「与 coupon_types.code 同样不可修改」，下单、签约、代扣都靠它对接），
-- 将来导旧数据或与小程序对接时不用再写一层映射。
--
-- # 两个套餐的会员价都是 `auto`，连续包月不发体验券
--
-- 与老系统一致：连续包月会员**直接享受会员价**，不发放「会员价体验券」。
--
-- 所以 `member_price_mode` 两个都是 `auto`，两条 CHECK 原样满足，模板与张数都留空。
-- 这一条同时解掉一个跨库问题：`member_price_coupon_template_id` 指向的是 **coupon 库**的
-- `coupon_templates` 行，membership 集根本种不出它（迁移不跨库建引用，也不该跨库写数据）。
-- 之前的 dev 库里 `monthly_auto` 是 `mode='coupon'` 且挂着一张 coupon 库的折扣券模板
-- （已发 53 张），那是历次验证时挂错的，不是产品定义。
--
-- `coupon` 模式**不删**：它仍接在 schema、DTO、成交快照、续期代扣与后台表单上
-- （`model.MemberPriceModeCoupon`），只是这两个种子套餐都不用它。
--
-- # 两个都不签约，`wechat_plan_id` 留空
--
-- `auto_renew=false` + `wechat_plan_id=''` 是有意的：微信商户平台的签约模板 ID 是**真实
-- 配置值**，不该进仓库（CLAUDE.md 禁止提交真实密钥/生产配置）。拿到新的模板号之后，由运营
-- 在后台把连续包月开起来——`SetPlanStatus` 与 `UpdatePlan` 都在，改完这一个字段即生效。
-- 建表时那条 `CHECK (NOT auto_renew OR char_length(trim(wechat_plan_id)) > 0)` 因此在这里
-- 是自然满足的，不需要放宽任何约束。
--
-- # 两点不要照抄 dev
--
--   * `annual.benefits` 在 dev 里是 `["啊"]`，历次验证留下的垃圾；这里写 `[]`。
--   * `sort_order` 在 dev 里两行都是 0（列表排序因此不确定）；这里给 1 / 2 定死顺序。
--
-- 幂等：按 `code` 判存在，重复执行插 0 行。

BEGIN;

INSERT INTO membership_plans (
    code, name, description, benefits,
    price_cents, period, period_count,
    auto_renew, wechat_plan_id,
    member_price_mode, member_price_coupon_template_id, member_price_coupons_per_period,
    sort_order, status
)
SELECT v.code, v.name, '', '[]'::jsonb,
       v.price_cents, v.period, 1,
       FALSE, '',
       'auto', NULL, NULL,
       v.sort_order, 'active'
FROM (VALUES
    ('monthly_auto', '连续包月',  990::bigint,  'month', 1),
    ('annual',       '年度会员', 12800::bigint, 'year',  2)
) AS v(code, name, price_cents, period, sort_order)
WHERE NOT EXISTS (SELECT 1 FROM membership_plans p WHERE p.code = v.code);

COMMIT;
