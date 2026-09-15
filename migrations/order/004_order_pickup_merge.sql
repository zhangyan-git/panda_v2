-- order/004：取杯号与取杯码合并回一列
--
-- 原型里本来就只有**一个**字段 order.pickup（C031 这种四位串）。同一个值在用户侧叫
-- 「取杯号」、在取杯口屏幕上叫「取杯码」——原型 :2142 一行里就同时用了两个名字：
--   openModal('取杯码 ' + order.pickup, order.machine.name,
--             '...<span class="tiny muted">请核对取杯号</span>...')
-- 规划（PANDA_V2_REFACTOR_PLAN.md）里从头到尾没有「取杯号」「取杯码」这两个词。
--
-- 001 把一个原型字段拆成了两列：
--   pickup_no   「短号」，屏幕/取杯口显示，不建唯一约束（注释里说「是否按天或按机器复用
--               还没定，定了之后再补，取杯码才是唯一凭据」）
--   pickup_code 「取杯凭据」，唯一，支付成功时生成
-- 拆完之后 pickup_no **从来没有被任何代码写过**（全仓只有 DDL 与 SELECT 碰它），
-- dev 库 88 行订单行里 0 行有值。而「后台不得展示取杯码」那条规则（service 层把
-- PickupCode 置 nil）正是建立在「pickup_code 是凭据」这个拆分上——可原型里那个码本来就
-- 大字摆在取杯口屏幕上，还配了「查看取杯码」按钮，从来不是秘密。
--
-- 所以合并回一列：留 pickup_code（唯一、有值的那一列），删 pickup_no。
--
-- 收窄是安全的：pickup_no 现有值全是空串，没有数据要保。下面先断言一次——真有非空行的
-- 话整条迁移直接失败，那种行需要一个「这些号要不要留、留到哪一列」的处置决定，不该被
-- 这条迁移静默连列一起丢掉。

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM order_lines WHERE pickup_no <> '') THEN
        RAISE EXCEPTION 'order_lines.pickup_no 有非空值，合并前先决定它们的去留';
    END IF;
END $$;

-- 先显式删约束再删列：DROP COLUMN 确实会连带删掉引用它的表约束，但那是隐式的，
-- 而这条约束下面还要按新列集合重建一次。写出来，读的人才知道它换过。
ALTER TABLE order_lines DROP CONSTRAINT order_lines_drink_only_fields;

ALTER TABLE order_lines DROP COLUMN pickup_no;

ALTER TABLE order_lines ADD CONSTRAINT order_lines_drink_only_fields CHECK (
    line_type = 'drink' OR (
        device_id IS NULL AND device_order_no = ''
        AND fulfillment_task_no = '' AND pickup_code IS NULL
    )
);

-- 002 给这两列各留了一条注释（「取杯号，取杯口显示的短号」与「取杯码，取杯凭据；不得写入
-- 日志、审计、事件与后台列表」），合并后两条都不成立了，重发一遍让已经建好的库跟上
-- （新库会先跑 001/002 拿到旧文本，再由这条覆盖）。
COMMENT ON COLUMN order_lines.pickup_code IS '取杯号：取杯口与屏幕上显示的短号（原型里的 C031），支付成功时生成，全局唯一；用户侧与后台都看得到。屏幕上人们也叫它取杯码，是同一个东西';
