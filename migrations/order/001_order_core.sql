-- order/001：订单领域核心表结构
--
-- 本迁移属于 panda_order。用户、点位/门店、设备、饮品、优惠券、会员、账户、支付、
-- 履约等外部服务的 ID 只作为值引用保存；本库不得创建跨数据库外键。
--
-- 边界（方案 5.8 / 5.9 / 5.15）：Order 只拥有订单事实。支付单与退款单归 payment-service，
-- 出杯任务与提货归 fulfillment-service，券与用户券归 coupon-service，账户与账变归
-- account-service，会员资格归 membership-service，抽奖与奖品归 lottery-service。
-- 本库出现它们时一律只是「一个 ID / 一个单号」，状态由各自服务负责。
--
-- 订单的形状是「主表 + 行」，不是「一张订单一个商品」：
--
--   orders       任何类型都成立的事实（谁、多少钱、什么状态、付了没、退了多少、承诺发几张福卡）
--   order_lines  这一单实际买了哪几件东西，一件一行，带各自的商品快照、数量、价格与优惠
--
-- 为什么必须有行：原型里用户可以「咖啡 + 幸运杯套」一起买。杯套是加购活动（bonus campaign）
-- 而不是饮品，有自己的单价和数量，**券只优惠饮品、不抵扣杯套**，而且杯套每个还赠福卡——
-- 原型的结算口径就是这么算的（drinkPayableCents + bonusCents），确认订单页也是两件商品两行
-- 金额。把杯套挤进饮品的字段里，等于让「一杯一单」的假设污染整张表，改不动。
--
-- 加类型/加商品都不动主表：**订单类型不是主表的一个列，而是「这一单有哪些行」**。
-- 只买咖啡 = 饮品行（+ 加购行）；买会员 = 会员行；会员 + 咖啡的组合单 = 两种行都有。
-- 老库为组合单专门加过 EnableMembership 标志位，正是因为一个 order_type 列表达不了
-- 「既是会员订单又是咖啡订单」——V2 不再重复这个坑。将来出现新类型（比如订阅），加的
-- 是一个 line_type 值，主表结构、金额恒等式、退款范围都不用跟着动。
--
-- 分类查询就这么写（都有索引）：
--   会员订单    EXISTS (SELECT 1 FROM order_lines l WHERE l.order_id = o.id AND l.line_type = 'membership')
--   只有咖啡    NOT EXISTS (SELECT 1 FROM order_lines l WHERE l.order_id = o.id AND l.line_type = 'membership')
--
-- 金额单位一律是「分」（BIGINT，整数），与 coupon/005 之后的仓库约定一致：定点小数
-- 会在 JSON 解码之外还能被写进来，BIGINT 在类型层就挡掉了。
--
-- 本文件不带 goose 的 Down 段：platform/database/migrate 把整个文件丢给一次 Exec。

-- ============================================================
-- 订单主表
-- ============================================================

CREATE TABLE orders (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(order_no)) > 0),
    -- 老库 orders._id（MongoDB ObjectID 的 24 位 hex）。只对迁移过来的历史订单有值，
    -- 新订单为空：它存在的唯一目的是让「这条 V2 订单对应老库哪一条」可查。
    legacy_id TEXT,
    -- 小程序用户 ID。只存值不留快照：手机号/昵称是身份库的资料，本库复制一份就会
    -- 在用户改资料后变成两份不一样的真相，需要时经 gRPC 现查。
    user_id UUID NOT NULL,
    -- 这里**没有** order_type：一次支付可以只买咖啡、只买会员，也可以两样一起买（老库
    -- 一张 orders 表 + 一个支付单号，组合单的钱只能挂在同一个订单上），一个列表达不了。
    -- 「这一单是什么类型」看它有哪些行：会员行 = 会员订单，饮品行 = 咖啡订单，两者都有
    -- 就是组合单。分类是派生出来的，不存冗余列，就不会出现「写着咖啡订单、里面却有会员行」
    -- 这种对不上的情况。注意方案 5.4 把「会员订单」写在 membership-service 名下：会员那边
    -- 拥有的是套餐、权益周期与续费，订单事实仍在 order-service 这里。
    --
    -- 两类下单来源（方案 5.8）：小程序直接下单、咖啡机屏幕选品后扫码下单。
    source TEXT NOT NULL CHECK (source IN ('miniapp', 'screen_qr')),
    status TEXT NOT NULL DEFAULT 'pending_payment' CHECK (status IN (
        'pending_payment', 'paid', 'completed', 'cancelled', 'expired', 'refunding', 'refunded'
    )),
    -- 履约状态汇总：制作中 / 待取杯 这一列是给用户和后台看的，事实由 fulfillment-service
    -- 的出杯任务产生，本库只接收事件更新这一个字段，不复制任务的中间状态。
    -- 一杯一单时它等价于饮品行的出杯状态；一单多行时只汇总饮品行——加购品（杯套在杯架上
    -- 自取）与会员行都不出杯，不参与。
    --
    -- none = 本单没有任何需要履约的行（纯会员订单）。没有这个值的话，会员订单会永远停在
    -- pending，后台的「待制作」列表和出杯队列里就会混进一堆永远做不完的会员单。
    -- 这一条由服务在下单时按行决定，跨表没法用 CHECK 表达；默认值仍是 pending。
    fulfillment_status TEXT NOT NULL DEFAULT 'pending' CHECK (fulfillment_status IN (
        'pending', 'making', 'ready', 'completed', 'failed', 'cancelled', 'none'
    )),
    -- 点位/门店与咖啡机：ID 是值引用，名字/编号是下单当时的快照——点位改名、设备换编号
    -- 之后，历史订单要能还原成用户当时看到的样子。一台咖啡机服务一次下单，所以设备在
    -- 订单这一层；具体哪一行是哪次出杯，看 order_lines 的 device_order_no。
    store_id UUID,
    store_name TEXT NOT NULL DEFAULT '',
    device_id UUID,
    device_no TEXT NOT NULL DEFAULT '',
    -- 屏幕选品的会话（scene/token）：只作值引用，会话本身由咖啡机侧维护。选品快照落在
    -- 饮品行上，因为它描述的是那一杯饮品和它的固定规格。
    scene_token TEXT NOT NULL DEFAULT '',
    -- 金额。主表的金额恒等于各行金额之和（行由应用在同一事务里写，跨表没法用 CHECK 表达），
    -- 这一点在下面各行的恒等式里各自成立。
    original_amount BIGINT NOT NULL CHECK (original_amount >= 0),
    discount_amount BIGINT NOT NULL DEFAULT 0 CHECK (discount_amount >= 0),
    payable_amount BIGINT NOT NULL CHECK (payable_amount >= 0),
    paid_amount BIGINT NOT NULL DEFAULT 0 CHECK (paid_amount >= 0),
    refunded_amount BIGINT NOT NULL DEFAULT 0 CHECK (refunded_amount >= 0),
    -- 券不在主表上：券抵扣的是某一件商品的价钱，所以挂在 order_lines 的那一行上（见那里的
    -- coupon_id / coupon_discount_amount）。主表不存券的汇总列，需要「这一单用了券没、抵了多少」
    -- 就按行求和——存一份汇总，就等于给「券挂在哪一行」和「主表抵了多少」留了两份真相。
    -- 下单时的会员资格快照（等级、有效期等），结构归 membership-service，本库只存一份
    -- 副本用于还原当时的会员价。会员价的具体优惠额不单独存列：它已经体现在
    -- original_amount 与 payable_amount 的差额里，再存一列就会有对不上的时候。
    membership_id UUID,
    membership_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- 这一单**承诺**发多少张福卡，以及这个数字是怎么来的（基础赠送 + 各加购活动的加赠）。
    -- 只存承诺快照，不存发放结果：福卡余额和发放流水归 account-service，发放时点与
    -- 退款追回规则在方案 3.1 里仍是待定项。存下来是因为规则会变，而「当时答应给用户几张」
    -- 是订单自己的事实——没有它，客服和退款追回都无从对账。
    -- 与行的关系：本列 = 基础赠送 + Σ(加购行 quantity × 该行 campaign_snapshot 里的 reward)。
    -- 行上是「这个活动每件赠几张」的规则快照，这里是算完的总数；规则改了，总数也不变。
    fortune_cards_expected INTEGER NOT NULL DEFAULT 0 CHECK (fortune_cards_expected >= 0),
    fortune_card_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- 主支付渠道与支付单号（payment-service 的值引用）。混合出资的逐笔金额在
    -- order_payment_lines 里，这里只留一个「主渠道」用于列表展示与对账归类。
    payment_method TEXT NOT NULL DEFAULT '',
    payment_no TEXT NOT NULL DEFAULT '',
    paid_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    cancelled_at TIMESTAMPTZ,
    cancellation_reason TEXT NOT NULL DEFAULT '',
    -- 待支付超时时间，关单扫描按它扫。
    expires_at TIMESTAMPTZ,
    remark TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 应付 = 原价 - 优惠 是定义，不是可以各自算的：写成约束，让「只有一边被改」编不进库。
    CONSTRAINT orders_payable_matches CHECK (payable_amount = original_amount - discount_amount),
    CONSTRAINT orders_paid_not_above_payable CHECK (paid_amount <= payable_amount),
    CONSTRAINT orders_refunded_not_above_paid CHECK (refunded_amount <= paid_amount)
);

-- ============================================================
-- 订单行
-- ============================================================

-- 这一单买了什么，一件一行。饮品行（drink）、加购品行（addon）、会员行（membership）
-- 在同一个表里，因为「这一单有哪些东西」永远是一张清单：拆成几张表之后，下单、详情页、
-- 退款、对账每一处都要 UNION 一次，而金额之和、序号、快照这些规则会在每张表上各写一遍。
--
-- 一台咖啡机一次下单 = 一个饮品行（原型与老库都是一次一杯，取杯号与出杯任务都是订单级
-- 唯一的）；加购品行 0 到 N 行，可以多件、可以多个不同活动；会员行 0 或 1 行。
--
-- 会员为什么是一行而不是主表上的一组列：买会员和买咖啡可以发生在同一笔支付里（老库的
-- 组合单），做成行之后「主表金额 = 各行金额之和」这条恒等式对三种订单同时成立，退款、
-- 对账、订单详情也只有一个形状要处理。
CREATE TABLE order_lines (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id UUID NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    line_no INTEGER NOT NULL CHECK (line_no > 0),
    -- 行类型：drink=饮品，addon=加购品（幸运杯套一类的活动商品），membership=会员（开的
    -- 是哪个套餐、多少钱、时长多少都在这一行里）。会员费也是「一行」，好处是主表金额恒等于
    -- 各行金额之和这条规则不用为组合单开口子，退款范围、对账口径也统一。
    line_type TEXT NOT NULL CHECK (line_type IN ('drink', 'addon', 'membership')),
    legacy_id TEXT,
    -- 商品快照：ID 是值引用（饮品是咖啡机服务的饮品目录，会员是会员服务的套餐），
    -- 编码/名称/图片是下单当时的副本。加购品可能只有活动、没有独立商品目录，那时 item_id
    -- 为空，身份看 campaign_id；会员行的 item_id 是套餐 ID（会员服务那边可能叫别的名字）。
    item_id UUID,
    item_code TEXT NOT NULL DEFAULT '',
    item_name TEXT NOT NULL DEFAULT '',
    item_image TEXT NOT NULL DEFAULT '',
    quantity INTEGER NOT NULL DEFAULT 1 CHECK (quantity > 0),
    -- 单价与金额，单位为分。两条恒等式（都由下面的 CHECK 保证）：
    --   payable_amount = original_unit_price * quantity - discount_amount
    --   discount_amount = price_discount_amount + coupon_discount_amount
    -- 算钱一律从 original_unit_price（目录价）出发；unit_price 只是「这一行当时标价多少」的
    -- 展示值（会员价/活动价之后的成交单价），**不参与任何恒等式**——否则同一笔会员价优惠
    -- 会在单价里少一次、又在优惠额里再算一次。
    original_unit_price BIGINT NOT NULL CHECK (original_unit_price >= 0),
    unit_price BIGINT NOT NULL CHECK (unit_price >= 0),
    -- 标价优惠：目录价与成交单价之间的差额（会员价、加购活动价、套餐活动价）。
    -- 原型结算页把会员价优惠（memberDiscountCents）和券优惠（couponDiscountCents）分开列，
    -- 这里就分开存：「这个会员价一共省了多少」= sum(price_discount_amount)，不用去猜 unit_price。
    price_discount_amount BIGINT NOT NULL DEFAULT 0 CHECK (price_discount_amount >= 0),
    discount_amount BIGINT NOT NULL DEFAULT 0 CHECK (discount_amount >= 0),
    payable_amount BIGINT NOT NULL CHECK (payable_amount >= 0),
    -- 用掉的用户券（coupon-service 的 user_coupons.id）与它在本行抵掉的金额。券挂在**行**上，
    -- 因为它抵的是某一件商品的价钱：原型确认订单页原话「咖啡券仅优惠饮品，不抵扣幸运杯套」。
    -- coupon_discount_amount 是本行 discount_amount（会员价+活动+券）里的券那部分，单列出来
    -- 是为了退款按行退、对账能区分「优惠里哪块是券」。券名/面值不在这里存快照：用户券本身就是
    -- coupon-service 的历史记录，模板改名不影响已发出的那张券。
    coupon_id UUID,
    coupon_discount_amount BIGINT NOT NULL DEFAULT 0 CHECK (coupon_discount_amount >= 0),
    -- 饮品规格（冷热/杯型/糖量等）与固定选品快照。结构归 coffee-machine-service，本库按
    -- 收到的样子原样存档，用于还原和排查，不参与查询条件。加购品与会员行这两列是空对象。
    specs JSONB NOT NULL DEFAULT '{}'::jsonb,
    selection_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- 加购活动（bonus campaign，原型里的「幸运杯套」）：活动 ID 是值引用，名称、活动价、
    -- 每件赠几张福卡是下单当时的快照。活动定义与福卡发放不归本库——本库只记「这一单
    -- 是按哪个活动、什么价、赠几张成交的」，活动改规则或下架都不能改变历史订单。
    campaign_id UUID,
    campaign_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- 会员套餐快照（套餐名称、时长/有效期、续费的目标会员 ID、权益摘要等），只有会员行有。
    -- 结构归 membership-service，本库只存成交当时的那一份副本：套餐改权益或下架都不能改变
    -- 历史订单。会员资格本身（等级、到期时间、权益周期）在 membership 那边，本库不复制。
    membership_plan_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- 出杯执行与取杯凭证，只有饮品行有。
    --
    -- device_id 是订单上那台机器的副本（V2 一台机器服务一次下单），冗余在这里是为了让
    -- (device_id, device_order_no) 能建唯一索引：厂商单号的唯一性作用域是「同一台机器」，
    -- 建成全局唯一会把两台机器各自的正常单号误判成冲突。
    device_id UUID,
    device_order_no TEXT NOT NULL DEFAULT '',
    -- 履约任务号（fulfillment-service 的值引用）。本库不存任务状态，只存这个号，
    -- 需要细节时拿它去查。
    fulfillment_task_no TEXT NOT NULL DEFAULT '',
    -- 取杯号：屏幕/取杯口上显示的短号（原型里的 C031）。不建唯一约束——是否按天或按机器
    -- 复用还没定，定了之后再补，取杯码才是唯一凭据。
    --
    -- 这一列由 004 删掉、与下面的 pickup_code 合并（原型里本来就只有 order.pickup 一个字段，
    -- 「取杯号」是用户侧叫法、「取杯码」是取杯口屏幕上叫法，是同一个值；拆出来的 pickup_no
    -- 从来没有被任何代码写过）。**但它必须在这里建出来**：下面那条 order_lines_drink_only_fields
    -- 引用了它，而迁移器是整文件一个事务——少这一列，001 整条失败并回滚，全新库连 orders 都
    -- 拿不到；老库按文件名记账、从不重跑，所以这个断链在已经建好的库上完全看不出来。
    pickup_no TEXT NOT NULL DEFAULT '',
    -- 取杯码：取杯凭据。属于凭据类字段，不许进日志、审计载荷、领域事件和后台列表接口，
    -- 只在「用户本人查自己的订单」时返回。——这条说法随 004 的合并一起作废，以 004 为准。
    pickup_code TEXT,
    remark TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (order_id, line_no),
    CONSTRAINT order_lines_payable_matches CHECK (payable_amount = original_unit_price * quantity - discount_amount),
    -- 本行优惠 = 标价优惠 + 券：写成约束，让「改了其中一块忘了另一块」编不进库。
    CONSTRAINT order_lines_discount_breakdown CHECK (
        discount_amount = price_discount_amount + coupon_discount_amount
    ),
    -- 券只优惠饮品（原型确认订单页原话：咖啡券仅优惠饮品，不抵扣幸运杯套），会员费同理。
    -- 将来真做会员券/加购券，删掉这一条即可，结构不用动。
    CONSTRAINT order_lines_coupon_only_on_drink CHECK (
        line_type = 'drink' OR coupon_discount_amount = 0
    ),
    CONSTRAINT order_lines_coupon_discount_needs_coupon CHECK (
        coupon_id IS NOT NULL OR coupon_discount_amount = 0
    ),
    -- 加购品必须来自一个活动：没有活动就没有加购价和赠卡数，这一行不成立。
    CONSTRAINT order_lines_addon_needs_campaign CHECK (line_type <> 'addon' OR campaign_id IS NOT NULL),
    -- 反过来，活动只有加购品能挂：饮品行与会员行不带 campaign_id。
    CONSTRAINT order_lines_campaign_only_on_addon CHECK (campaign_id IS NULL OR line_type = 'addon'),
    -- 会员套餐快照只有会员行有。
    CONSTRAINT order_lines_membership_plan_only_on_membership CHECK (
        line_type = 'membership' OR membership_plan_snapshot = '{}'::jsonb
    ),
    -- 出杯、履约与取杯字段只有饮品行有：杯套不出杯，会员也不出杯。
    CONSTRAINT order_lines_drink_only_fields CHECK (
        line_type = 'drink' OR (
            device_id IS NULL AND device_order_no = ''
            AND fulfillment_task_no = '' AND pickup_no = '' AND pickup_code IS NULL
        )
    )
);

-- ============================================================
-- 订单出资分摊
-- ============================================================

-- 一个订单可能同时由微信、咖啡豆、家园消费金出资。福卡不在出资方之列：规划里只把它写成
-- 「订单完成发放、用于参与抽奖」（§3.1），它的余额归 Account（§5.6），见 003。退款要按来源分别
-- 冲正、分账要按来源拆分，所以逐笔留行，而不是在主表存一个支付方式了事。
--
-- 券不在这里：券是权益抵扣、不是资金出资。它抵掉的钱记在它作用的那一行
-- （order_lines.coupon_discount_amount），核销事实在 coupon-service 的核销流水里。
CREATE TABLE order_payment_lines (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id UUID NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    line_no INTEGER NOT NULL CHECK (line_no > 0),
    line_type TEXT NOT NULL CHECK (line_type IN (
        'wechat', 'unionpay', 'coffee_bean', 'fortune_card', 'wallet', 'other'
    )),
    amount BIGINT NOT NULL CHECK (amount > 0),
    status TEXT NOT NULL DEFAULT 'reserved' CHECK (status IN (
        'reserved', 'succeeded', 'failed', 'released', 'reversed'
    )),
    -- payment-service 的支付单号；渠道支付的第三方流水号与失败码一并留档。
    payment_no TEXT NOT NULL DEFAULT '',
    provider_transaction_id TEXT NOT NULL DEFAULT '',
    failure_code TEXT NOT NULL DEFAULT '',
    -- account-service 的账变 ID：咖啡豆出资扣的是账户余额，退款要按这笔账变冲正。
    account_entry_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    succeeded_at TIMESTAMPTZ,
    reversed_at TIMESTAMPTZ,
    UNIQUE (order_id, line_no)
);

-- 同一订单、同一出资方只允许有一条成功线：重试或重复回调不能变成扣两次钱。
CREATE UNIQUE INDEX order_payment_lines_one_live_success
    ON order_payment_lines (order_id, line_type)
    WHERE status = 'succeeded';

-- ============================================================
-- 状态流水（订单与售后）
-- ============================================================

-- 订单跨越多个请求、事件和补偿任务（方案 11.3），只靠主表上的当前状态查不出「怎么走到
-- 这一步的」。这张表只追加，不改不删。
CREATE TABLE order_state_transitions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type TEXT NOT NULL CHECK (aggregate_type IN ('order', 'order_line', 'after_sale', 'payment_line')),
    aggregate_id UUID NOT NULL,
    from_status TEXT NOT NULL DEFAULT '',
    to_status TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    -- 谁推动了这次变更。admin/merchant 的操作同时会在身份库 admin_operation_logs 留一条
    -- 后台操作审计，这里记的是订单自己的口径。
    actor_type TEXT NOT NULL DEFAULT 'system' CHECK (actor_type IN ('user', 'merchant', 'admin', 'system')),
    actor_id UUID,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE FUNCTION prevent_order_transition_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'order state transitions are append-only';
END;
$$;

CREATE TRIGGER order_state_transitions_append_only
    BEFORE UPDATE OR DELETE ON order_state_transitions
    FOR EACH ROW EXECUTE FUNCTION prevent_order_transition_change();

-- ============================================================
-- 售后申请
-- ============================================================

-- 只装「申请与处理结果」。真正的退款单、渠道调用、退款流水在 payment-service，本库只留
-- 一个 refund_no 值引用——同一个退款在两边各存一份状态，迟早会不一致。
CREATE TABLE order_after_sales (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    legacy_id TEXT,
    after_sale_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(after_sale_no)) > 0),
    order_id UUID NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    -- 冗余订单号：客服/对账都是拿着单号来找，不为了显示一个号再回表 join 一次。
    order_no TEXT NOT NULL CHECK (char_length(trim(order_no)) > 0),
    user_id UUID NOT NULL,
    -- 类型暂时只有退款。加新类型（换杯、补做）时扩这个 CHECK，不新开表。
    type TEXT NOT NULL DEFAULT 'refund' CHECK (type IN ('refund')),
    -- 退款范围：all=整单，drink=只退饮品行，addon=只退加购行，membership=只退会员套餐
    -- （开了会员又要退的场景：权益是否收回、怎么算已用天数，规则还在会员服务那边）。
    -- 券只优惠饮品、杯套与会员费不打折，所以退款必须能分开算，不能永远整单退。
    scope TEXT NOT NULL DEFAULT 'all' CHECK (scope IN ('all', 'drink', 'addon', 'membership')),
    -- 退的是**哪一行**。scope 只说到「哪一类」，说不出一单里两个不同加购活动的杯套分别是哪个；
    -- 而退款金额、券的退回、赠卡追回都得落在具体那一行上，所以这里必须能指到行。
    -- all=整单退时为空（没有具体行），其余三种范围必须指行——见下面的 CHECK。
    -- 一次要退多行（比如两个加购品都要退）就开两条售后单：金额、审核、退款单号本来就是
    -- 各自独立的事实，合成一条反而要在里面再挂一张子表。
    order_line_id UUID REFERENCES order_lines(id) ON DELETE RESTRICT,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN (
        'pending', 'approved', 'rejected', 'refunding', 'refunded', 'failed', 'cancelled'
    )),
    reason TEXT NOT NULL DEFAULT '',
    -- 用户上传的凭证图片 URL 列表。
    images JSONB NOT NULL DEFAULT '[]'::jsonb,
    refund_amount BIGINT NOT NULL DEFAULT 0 CHECK (refund_amount >= 0),
    refund_no TEXT NOT NULL DEFAULT '',
    failure_code TEXT NOT NULL DEFAULT '',
    reviewed_by UUID,
    reviewed_at TIMESTAMPTZ,
    review_remark TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    refunded_at TIMESTAMPTZ,
    -- 整单退没有具体行，按行退必须指到行：两头都不能含糊，所以写成等价关系而不是两条 CHECK。
    CONSTRAINT order_after_sales_scope_matches_line CHECK (
        (scope = 'all') = (order_line_id IS NULL)
    )
);

-- ============================================================
-- 下单幂等
-- ============================================================

-- 与 coupon_idempotency_keys 逐列同形：同一套幂等语义在仓库里只有一种表结构，
-- 换服务时不用重新理解一遍。
CREATE TABLE order_idempotency_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope TEXT NOT NULL CHECK (char_length(trim(scope)) > 0),
    idempotency_key TEXT NOT NULL CHECK (char_length(trim(idempotency_key)) > 0),
    request_hash TEXT NOT NULL CHECK (char_length(trim(request_hash)) > 0),
    resource_type TEXT NOT NULL,
    resource_id UUID,
    response JSONB NOT NULL DEFAULT '{}'::jsonb,
    status TEXT NOT NULL CHECK (status IN ('processing', 'succeeded', 'failed')),
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (scope, idempotency_key)
);

-- ============================================================
-- 索引
-- ============================================================

-- 历史订单迁移映射：只有迁移过来的行才有 legacy_id。
CREATE UNIQUE INDEX orders_legacy_id_key ON orders (legacy_id) WHERE legacy_id IS NOT NULL;
CREATE UNIQUE INDEX order_lines_legacy_id_key ON order_lines (legacy_id) WHERE legacy_id IS NOT NULL;
CREATE UNIQUE INDEX order_after_sales_legacy_id_key ON order_after_sales (legacy_id) WHERE legacy_id IS NOT NULL;

-- 方案 8.2：外部单号必须唯一。空串表示「还没有」，不是「另一个空单号」，所以空串要排除在
-- 唯一性之外。
CREATE UNIQUE INDEX orders_payment_no_key
    ON orders (payment_no) WHERE payment_no <> '';
-- 同一个下单请求只能落一个订单，幂等之外再加一道。
CREATE UNIQUE INDEX orders_request_id_key ON orders (request_id) WHERE request_id <> '';

CREATE INDEX orders_user_created_idx ON orders (user_id, created_at DESC);
CREATE INDEX orders_status_created_idx ON orders (status, created_at DESC);
CREATE INDEX orders_device_created_idx ON orders (device_id, created_at DESC);
CREATE INDEX orders_store_created_idx ON orders (store_id, created_at DESC);
CREATE INDEX orders_fulfillment_status_idx ON orders (fulfillment_status, created_at)
    WHERE status = 'paid';
-- 超时关单扫描：只关心还在等支付的订单。
CREATE INDEX orders_pending_expiry_idx ON orders (expires_at)
    WHERE status = 'pending_payment';

CREATE INDEX order_lines_order_idx ON order_lines (order_id, line_no);
CREATE INDEX order_lines_item_idx ON order_lines (item_id) WHERE item_id IS NOT NULL;
-- 一张用户券只能落在**一行**上：券是一次性核销的权益，写进两个行/两个订单就是重复核销。
-- 这是本库自己的兜底——券的核销事实在 coupon-service，它那边才是权威，但这里也不该放行。
-- 将来真要支持「满减券跨多行分摊」，删掉这条索引改成按行分摊即可。
CREATE UNIQUE INDEX order_lines_coupon_key ON order_lines (coupon_id) WHERE coupon_id IS NOT NULL;
-- 「这个加购活动卖了多少件」按活动查，含已下架活动。
CREATE INDEX order_lines_campaign_idx ON order_lines (campaign_id) WHERE campaign_id IS NOT NULL;
CREATE INDEX order_lines_fulfillment_task_idx ON order_lines (fulfillment_task_no)
    WHERE fulfillment_task_no <> '';

-- 一杯一单：一个订单一杯饮品。老库的咖啡订单表就是「一次只买一杯，无 items」，出杯侧的
-- 取杯号和出杯任务也都是订单级唯一的。要买两杯就是两单——将来真要放开，删掉这条索引，
-- 出杯字段本来就已经在行上了，不用改表。
CREATE UNIQUE INDEX order_lines_one_drink_per_order
    ON order_lines (order_id) WHERE line_type = 'drink';
-- 一单一个会员套餐：续费就是新的一单，不是同一单里第二行。同一个套餐买两次、或者一单里
-- 同时开两个套餐，语义上都不成立。
CREATE UNIQUE INDEX order_lines_one_membership_per_order
    ON order_lines (order_id) WHERE line_type = 'membership';
-- 「会员订单列表」「这一单是不是会员单」按行类型查，不靠主表上的冗余类型列。
CREATE INDEX order_lines_type_created_idx ON order_lines (line_type, created_at DESC);
-- 厂商单号在「同一台机器」内唯一。
CREATE UNIQUE INDEX order_lines_device_order_no_key
    ON order_lines (device_id, device_order_no) WHERE device_order_no <> '';
-- 取杯号（屏幕上叫取杯码，同一个值）全局唯一，一个号只能对应一个订单行。
CREATE UNIQUE INDEX order_lines_pickup_code_key
    ON order_lines (pickup_code) WHERE pickup_code IS NOT NULL;

CREATE INDEX order_payment_lines_order_idx ON order_payment_lines (order_id, line_no);
CREATE INDEX order_payment_lines_account_entry_idx ON order_payment_lines (account_entry_id)
    WHERE account_entry_id IS NOT NULL;

CREATE INDEX order_state_transitions_aggregate_idx
    ON order_state_transitions (aggregate_type, aggregate_id, created_at);
-- 按请求号回放一次下单/回调期间的所有状态变更（方案 11.3 的业务关联）。
CREATE INDEX order_state_transitions_request_idx ON order_state_transitions (request_id)
    WHERE request_id <> '';

CREATE INDEX order_after_sales_order_idx ON order_after_sales (order_id, created_at DESC);
-- 「这一行有没有正在处理的退款」——重复申请同一行时要能查出来。
CREATE INDEX order_after_sales_line_idx ON order_after_sales (order_line_id)
    WHERE order_line_id IS NOT NULL;
CREATE INDEX order_after_sales_status_idx ON order_after_sales (status, created_at);
CREATE INDEX order_after_sales_user_idx ON order_after_sales (user_id, created_at DESC);

-- ============================================================
-- 平台样板：消息 outbox / inbox
-- ============================================================

-- 与 identity/003、merchant/002、coupon/001、coffee_machine/001 里的同名两表逐列一致，
-- 各库自带一份，跨库不共享表。
--
-- 本服务的 outbox 不是可选件：订单状态变更、支付结果与售后退款都要以领域事件发出去
-- （方案 7.3 / 7.4），而审计记录的写法（platform/audit）就是「在业务事务内往自己的
-- outbox 追加一条 admin.operation.logged」，由 relay 投到 RabbitMQ、再落到身份库的
-- admin_operation_logs。没有这张表，后台对订单的手工干预就没有留痕的地方。

CREATE TABLE message_outbox (
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

CREATE TABLE message_inbox (
    event_id TEXT PRIMARY KEY CHECK (char_length(trim(event_id)) > 0),
    claimed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_owner TEXT,
    lease_token TEXT,
    lease_until TIMESTAMPTZ,
    completed_at TIMESTAMPTZ
);

CREATE INDEX message_outbox_pending_idx
    ON message_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;
CREATE INDEX message_outbox_lease_idx
    ON message_outbox (lease_until)
    WHERE published_at IS NULL;
CREATE INDEX message_inbox_lease_idx
    ON message_inbox (lease_until)
    WHERE completed_at IS NULL;
