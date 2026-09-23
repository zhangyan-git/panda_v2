-- order：订单领域表结构与中文注释
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
-- 金额单位一律是「分」（BIGINT，整数），与全仓其它库一致：定点小数会在 JSON 解码之外还能
-- 被写进来，BIGINT 在类型层就挡掉了。
--
-- 本文件不带 goose 的 Down 段：platform/database/migrate 把整个文件丢给一次 Exec。

BEGIN;

-- ============================================================
-- 订单主表
-- ============================================================

-- 四条下单路径共用一个形状，差别只在「谁发起、有没有用户、是不是直接落成已支付」：
--
--   miniapp    小程序直接下单。用户在小程序里点的那一下，先建单 → 待支付 → 支付回调 → 已支付。
--   screen_qr  咖啡机屏幕选品后扫码下单。与上一条同一条流水线，只是快照里带着选品会话。
--   device     线下刷卡机设备回调（方案 §四）。它与前两条是**反的**：钱已经在刷卡机上收过了，
--              建单即已支付。入口在 partner-service（它验完签），经 gRPC 调 order-service 的
--              CreateDeviceOrder。**这条路没有用户**——老系统写的是 primitive.NilObjectID，
--              一个「没有」的哨兵值；V2 用 NULL 表达同一件事：不是空串、不是某个系统用户、
--              也不是「还不知道」。空 uuid 与系统用户都会让「这一单属于谁」看起来有一个答案，
--              而这条路上真实的答案是「不属于任何人」。user_id 上没有外键，所以放不放开
--              NOT NULL 都不涉及任何级联。
--   renewal    会员续费代扣：连续包月的每一期代扣成功之后，membership-service 经 gRPC 建一张
--              已支付的订单（老系统就是这么做的，后台「订单管理」里业务阶段写着「自动续费」、
--              单号 SUB 开头的单子就是它）。**不复用 miniapp**：那条路的语义是「用户在小程序里
--              点了下单」，而续费没有这一下——没有人点，是到期自动扣的，后台的来源筛选、以及
--              按来源看的每一张报表都靠这一列，两者混在一起之后就再也分不开了。**也不复用
--              device**：设备单没有用户，续费单有——那一期的钱是某个人的会员费，两条路的建单
--              RPC 也不是同一条（见 order.proto 的 CreateRenewalOrder）。
--
-- 一条**没有**跟着放开的地方：order_after_sales.user_id 保持 NOT NULL。售后申请只有「用户本人
-- 发起」这一条路（后台只有审核与列表，见 routes.RegisterAdmin），而那条路在仓储里要求申请人
-- 的 user_id 与订单的 user_id 相等——设备单是 NULL，永远比不上，于是在锁内就被判成「订单不
-- 存在」。所以那张表不会出现 user_id 为空的行，也就不需要跟着放开。

CREATE TABLE orders (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(order_no)) > 0),
    -- 老库 orders._id（MongoDB ObjectID 的 24 位 hex）。只对迁移过来的历史订单有值，
    -- 新订单为空：它存在的唯一目的是让「这条 V2 订单对应老库哪一条」可查。
    legacy_id TEXT,
    -- 小程序用户 ID。只存值不留快照：手机号/昵称是身份库的资料，本库复制一份就会
    -- 在用户改资料后变成两份不一样的真相，需要时经 gRPC 现查。
    -- **没有 NOT NULL**：设备单（source=device）没有用户，那种单的 user_id 是 NULL，
    -- 「没有用户」就是 NULL 本身（见上面设备单那一段）。
    user_id UUID,
    -- 这里**没有** order_type：一次支付可以只买咖啡、只买会员，也可以两样一起买（老库
    -- 一张 orders 表 + 一个支付单号，组合单的钱只能挂在同一个订单上），一个列表达不了。
    -- 「这一单是什么类型」看它有哪些行：会员行 = 会员订单，饮品行 = 咖啡订单，两者都有
    -- 就是组合单。分类是派生出来的，不存冗余列，就不会出现「写着咖啡订单、里面却有会员行」
    -- 这种对不上的情况。注意方案 5.4 把「会员订单」写在 membership-service 名下：会员那边
    -- 拥有的是套餐、权益周期与续费，订单事实仍在 order-service 这里。
    --
    -- 下单来源（方案 5.8）：小程序直接下单、咖啡机屏幕选品后扫码下单、线下刷卡机设备回调
    -- （device）、会员续费代扣（renewal）。四条路的差别与另外三条的来历见上面那一段。
    source TEXT NOT NULL CHECK (source IN ('miniapp', 'screen_qr', 'device', 'renewal')),
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
    -- 用户选定的支付方式：payment-service 目录里的 code（ums_h5_alipay / ums_h5_wechat /
    -- ums_h5_miniapp_wechat / coffee_bean …），由支付结果事件的 paymentMethodCode 带过来。
    -- 逐笔出资在 order_payment_lines 里，那一列的 line_type 存的是**同一个值**。这里留一份
    -- 是为了列表展示与对账归类：照着 `WHERE payment_method = 'wechat'` 去捞会捞到空集——
    -- 出资渠道那套词表（wechat / unionpay / coffee_bean / wallet / other）已经没有了，
    -- 加一种支付方式只需要在 payment 的 catalog 里加一条。
    --
    -- 为什么不直接存出资渠道：支付宝那条路在旧词表里没有档位，只能落 other，于是后台把一笔
    -- 支付宝单显示成「其他」，看不出用户扫的是支付宝还是微信；而那个 code 也不能反过来塞进
    -- order_payment_lines.line_type——那会撞出资渠道的 CHECK，整条落单事务回滚，而钱在支付侧
    -- 已经收了。
    --
    -- 本列不改写历史行：本次改动之前的单子存的是出资渠道（other / wechat / coffee_bean），
    -- 因为那时事件里只有那一个值。「用户当时选的是哪一种方式」这个事实此前从来没落进订单库，
    -- 它在 panda_payment 的 payments.payment_method 上——回填要按支付单号从 payment 库导出后
    -- 精确订正（判据是「这一列的值等于该支付单的 funding_type」，只有老代码写下的行会命中），
    -- 跨库没有 dblink，不是一条迁移能做的事。
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
    -- 对方单号：设备回调里那个第三方订单号（刷卡机与取货码），或会员续费的渠道流水号
    -- （provider_transaction_id）。三条路共用一个单号空间，它是这几条路的幂等键。
    -- 空串表示「这一单没有对方单号」（小程序与屏幕扫码的单），不是「另一个空单号」——
    -- 与 orders_payment_no_key / orders_request_id_key 同一条规矩，空串排除在唯一性之外。
    third_party_order_no TEXT NOT NULL DEFAULT '',
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
    -- 取杯号：取杯口与屏幕上显示的短号（原型里的 C031），支付成功时生成，全局唯一。
    --
    -- 「取杯号」与「取杯码」是**同一个值**，不是两列：原型里本来就只有 order.pickup 一个
    -- 字段，:2142 一行里就同时用了两个名字（用户侧叫「取杯号」、取杯口屏幕上叫「取杯码」），
    -- 规划里从头到尾没有这两个词。它今天也**不再是凭据**：那个码本来就大字摆在取杯口屏幕上，
    -- 还配着「查看取杯码」按钮，从来不是秘密，所以它不禁进日志、审计、领域事件与后台列表
    -- ——用户本人与后台都看得到。
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
            AND fulfillment_task_no = '' AND pickup_code IS NULL
        )
    )
);

-- ============================================================
-- 订单出资分摊
-- ============================================================

-- 一个订单可以拆成多笔出资。福卡不在出资方之列：规划里只把它写成「订单完成发放、用于参与
-- 抽奖」（§3.1），它的余额归 Account（§5.6）。退款要按笔分别冲正、分账要按笔拆分，所以逐笔
-- 留行，而不是在主表存一个支付方式了事。
--
-- line_type 存的是**用户选定的那一种支付方式**，也就是 payment-service 目录里的 code
-- （ums_h5_alipay / ums_h5_wechat / ums_miniapp_wechat / coffee_bean …），与同一张订单上
-- orders.payment_method 是同一个值。这里**没有**一套自己的「出资渠道」词表（曾经有过
-- wechat / unionpay / coffee_bean / wallet / other，与 payment 库那份逐字对应，后来退场了）：
-- 加一种支付方式本来只该在 catalog 里加一条，两套词表并存时还要在出资渠道里给它找一档，找不到
-- 就得改两个库的 DDL——支付宝就卡在这里，它在出资渠道里没有档位，只能落 other，于是后台把一笔
-- 支付宝单显示成「其他」。
--
-- 于是约束只剩 `line_type <> ''`。这比一套词表弱得多，而且跨库一致性那层保护换不回来——order
-- 库看不见 payment 的目录，谁也没法在这里写出一份「今天的合法 code 表」。能守住的只剩「非空」：
-- 一条出资行说不出自己从哪来，是比它从一个陌生 code 来更坏的事。反过来说那层保护本来也是假的：
-- 旧词表拦不住 payment 侧新增一种方式，只会让它在订单侧落成 other。
--
-- 历史行不在库里回填（那是单独从 payment 库导出再按支付单号精确订正的一次性作业）。要在库里
-- 分辨老行，看 orders.payment_method：今天它与本列是同一个值。
--
-- 券不在这里：券是权益抵扣、不是资金出资。它抵掉的钱记在它作用的那一行
-- （order_lines.coupon_discount_amount），核销事实在 coupon-service 的核销流水里。
CREATE TABLE order_payment_lines (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id UUID NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    line_no INTEGER NOT NULL CHECK (line_no > 0),
    line_type TEXT NOT NULL CHECK (line_type <> ''),
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

-- 同一订单、同一支付方式只允许有一条成功线：重试或重复回调不能变成扣两次钱。
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
    -- 失败码与失败文案成对，而且**每次写结论时两个一起写**（AdvanceRefund 的 UPDATE 都取事件里
    -- 的值）：成功那条路上事件带的正是两个空串，所以两列永远描述同一个结论。一行只落得下一个
    -- 结论——退款失败之后要再退是**重新申请**（新售后单、新退款单号），不是在原行上重来。
    failure_code TEXT NOT NULL DEFAULT '',
    reviewed_by UUID,
    reviewed_at TIMESTAMPTZ,
    review_remark TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    refunded_at TIMESTAMPTZ,
    -- 退款失败原因：渠道返回的文案。失败这条路上最该给客服看的就是这句——`ACQ.TRADE_NOT_EXIST`
    -- 谁也读不懂，渠道回的原文（「原交易不存在」）才说明白发生了什么、要不要找用户重来一次。
    -- 这句文案本来就在链路上，只是走到订单域被丢掉了（UMS refundResponse.failureMessage →
    -- payment_refunds.failure_message → 事件体 payment.refund.failed 的 failureMessage 字段
    -- → 收口时只写了 failure_code）。
    --
    -- 落在**这一张表**而不是让客服去支付域查：后台看退款失败就是看这一行（routes.RegisterAdmin
    -- 的售后列表），而 order_after_sales 是这条链在订单侧的落点——与 refund_no、refunded_at
    -- 同一个归属。payment_refunds 那边留着自己那份，两边各自完整，不做跨库读取。
    failure_message TEXT NOT NULL DEFAULT '',
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
-- 同一个对方单号只能落一张订单。它同时是并发下的最后一道锁：两个同号回调同时到达时，
-- 先查后插两边都会说「没有」，只有这条索引能挡住第二张。
CREATE UNIQUE INDEX orders_third_party_order_no_key
    ON orders (third_party_order_no) WHERE third_party_order_no <> '';

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

-- 与其它九个库里的同名两表逐列一致，各库自带一份，跨库不共享表
-- （migrations_test.go 比对这十一份 DDL 的列集合）。
--
-- 本服务的 outbox 不是可选件：订单状态变更、支付结果与售后退款都要以领域事件发出去
-- （方案 7.3 / 7.4），而审计记录的写法（platform/audit）就是「在业务事务内往自己的
-- outbox 追加一条 admin.operation.logged」，由 relay 投到 RabbitMQ、再落到身份库的
-- admin_operation_logs。没有这张表，后台对订单的手工干预就没有留痕的地方。

-- >>> message-tables:begin >>>

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

-- <<< message-tables:end <<<

-- ============================================================
-- 表与关键字段的中文注释
-- ============================================================

COMMENT ON TABLE orders IS '订单主表：一次下单的事实，金额单位为分；买了哪几件东西见 order_lines。订单类型不存列，由行的 line_type 派生：有 membership 行的是会员订单，有 drink 行的是咖啡订单';
COMMENT ON COLUMN orders.order_no IS '订单号，业务唯一';
COMMENT ON COLUMN orders.legacy_id IS '老库订单 ID（ObjectID 十六进制），仅迁移过来的历史订单有值';
COMMENT ON COLUMN orders.user_id IS '下单用户 ID，属于身份服务时仅作跨库值引用；**设备单（source=device）为 NULL**：钱在刷卡机上收过了，这一单不属于任何用户。NULL 就是「没有用户」，不是空 uuid、也不是某个系统用户';
COMMENT ON COLUMN orders.source IS '下单来源：miniapp=小程序直接下单，screen_qr=咖啡机屏幕选品后扫码下单，device=线下刷卡机设备回调（partner-service 验签后经 gRPC 建单，没有用户、直接落成已支付），renewal=会员续费代扣（membership-service 在扣款成功之后经 gRPC 建单，有用户、纯会员行、直接落成已支付）';
COMMENT ON COLUMN orders.status IS '订单状态：pending_payment=待支付，paid=已支付，completed=已完成，cancelled=已取消，expired=支付超时，refunding=退款中，refunded=已退款';
COMMENT ON COLUMN orders.fulfillment_status IS '履约状态汇总（只由饮品行驱动）：pending=待制作，making=制作中，ready=待取杯，completed=已取杯，failed=出杯失败，cancelled=已取消，none=本单无需履约（纯会员订单）；事实由履约服务产生';
COMMENT ON COLUMN orders.store_id IS '点位或门店 ID，属于商户服务时仅作跨库值引用';
COMMENT ON COLUMN orders.store_name IS '下单时的点位名称快照';
COMMENT ON COLUMN orders.device_id IS '咖啡机 ID，属于咖啡机服务时仅作跨库值引用';
COMMENT ON COLUMN orders.device_no IS '下单时的咖啡机编号快照';
COMMENT ON COLUMN orders.scene_token IS '屏幕选品会话标识，属于咖啡机侧时仅作跨库值引用';
COMMENT ON COLUMN orders.original_amount IS '订单原价，单位为分；等于各行原价之和';
COMMENT ON COLUMN orders.discount_amount IS '订单优惠合计（会员价、活动、券等），单位为分；等于各行优惠之和';
COMMENT ON COLUMN orders.payable_amount IS '应付金额，单位为分；等于原价减优惠合计，也等于各行应付之和';
COMMENT ON COLUMN orders.paid_amount IS '已支付金额，单位为分';
COMMENT ON COLUMN orders.refunded_amount IS '已退款金额，单位为分';
COMMENT ON COLUMN orders.membership_id IS '下单时的会员资格 ID（会员价的计算依据），属于会员服务时仅作跨库值引用；买会员套餐这件事本身是 order_lines 里的会员行';
COMMENT ON COLUMN orders.membership_snapshot IS '下单时的会员资格快照（等级、有效期等），结构归会员服务，本库只存副本；与 order_lines 的会员套餐快照是两回事：这里是「买的时候已经是会员」，那里是「这一单买了会员」';
COMMENT ON COLUMN orders.fortune_cards_expected IS '本单承诺发放的福卡张数，包含基础赠送与加购活动加赠';
COMMENT ON COLUMN orders.fortune_card_snapshot IS '承诺发放福卡的构成快照（基础赠送、各活动加赠）；发放结果与余额归账户服务';
COMMENT ON COLUMN orders.payment_method IS '用户选定的支付方式（payment-service 目录里的 code，如 ums_h5_alipay）；与 order_payment_lines.line_type 同一个值。历史行已按支付单回填订正，全库只有这一种值';
COMMENT ON COLUMN orders.payment_no IS '支付单号，属于支付服务时仅作跨库值引用，非空时唯一';
COMMENT ON COLUMN orders.third_party_order_no IS '对方单号：设备回调里那个第三方订单号（刷卡机与取货码），或会员续费的渠道流水号（provider_transaction_id）。三条路共用一个单号空间，它是幂等键：同一单号只能落一张订单；空串表示这一单没有对方单号（小程序与屏幕扫码的单）且不参与唯一性';
COMMENT ON COLUMN orders.paid_at IS '支付完成时间';
COMMENT ON COLUMN orders.finished_at IS '订单完成时间';
COMMENT ON COLUMN orders.cancelled_at IS '订单取消时间';
COMMENT ON COLUMN orders.cancellation_reason IS '取消原因';
COMMENT ON COLUMN orders.expires_at IS '待支付订单的超时时间，超时关单扫描依据';
COMMENT ON COLUMN orders.remark IS '订单备注';
COMMENT ON COLUMN orders.request_id IS '下单请求幂等号，非空时唯一';

COMMENT ON TABLE order_lines IS '订单行：这一单买了哪几件东西，一件一行，带商品快照、数量、价格与优惠';
COMMENT ON COLUMN order_lines.order_id IS '订单 ID';
COMMENT ON COLUMN order_lines.line_no IS '订单内行号';
COMMENT ON COLUMN order_lines.line_type IS '行类型：drink=饮品（一次一杯），addon=加购品（幸运杯套一类的活动商品），membership=会员（开通或续费的套餐）';
COMMENT ON COLUMN order_lines.legacy_id IS '老库对应记录 ID（ObjectID 十六进制），仅迁移过来的历史数据有值';
COMMENT ON COLUMN order_lines.item_id IS '商品 ID，属于咖啡机服务时仅作跨库值引用（会员行是会员服务的套餐 ID）；加购品可能只有活动、没有独立商品，此时为空';
COMMENT ON COLUMN order_lines.item_code IS '下单时的商品编码快照';
COMMENT ON COLUMN order_lines.item_name IS '下单时的商品名称快照';
COMMENT ON COLUMN order_lines.item_image IS '下单时的商品图片快照';
COMMENT ON COLUMN order_lines.quantity IS '数量；饮品与会员恒为 1，加购品可以多件';
COMMENT ON COLUMN order_lines.original_unit_price IS '下单时的商品目录单价，单位为分';
COMMENT ON COLUMN order_lines.unit_price IS '成交单价，单位为分，仅用于展示与对账；不参与任何金额恒等式，不是「本行应付除以数量」';
COMMENT ON COLUMN order_lines.price_discount_amount IS '标价优惠金额，单位为分：目录价与成交单价之间的差额，例如会员价、活动价（原型里的 memberDiscountCents）；只有饮品行与加购行可能有，会员费恒为 0';
COMMENT ON COLUMN order_lines.discount_amount IS '本行优惠合计，单位为分；等于标价优惠加券抵扣；加购品的活动价也走标价优惠这一格';
COMMENT ON COLUMN order_lines.payable_amount IS '本行应付金额，单位为分；等于商品目录单价乘数量减本行优惠合计';
COMMENT ON COLUMN order_lines.coupon_id IS '本行使用的用户券 ID，属于优惠券服务时仅作跨库值引用；券挂在行上（咖啡券只优惠饮品，不抵扣幸运杯套），一张券只能落一行';
COMMENT ON COLUMN order_lines.coupon_discount_amount IS '本行由用户券抵扣的金额，单位为分；是本行优惠合计里的券那部分，只有饮品行可以为非零';
COMMENT ON COLUMN order_lines.specs IS '饮品规格快照（冷热、杯型、糖量等），结构归咖啡机服务；加购品与会员行为空对象';
COMMENT ON COLUMN order_lines.selection_snapshot IS '固定选品原始快照，用于还原与排查，不参与查询；加购品与会员行为空对象';
COMMENT ON COLUMN order_lines.campaign_id IS '加购活动 ID，属于活动归属服务时仅作跨库值引用；只有加购行有';
COMMENT ON COLUMN order_lines.campaign_snapshot IS '成交时的加购活动快照（活动名称、活动价、每件赠卡数）；活动改规则或下架不影响历史订单';
COMMENT ON COLUMN order_lines.membership_plan_snapshot IS '成交时的会员套餐快照（套餐名称、时长、续费目标会员、权益摘要）；结构归会员服务，本库只存副本；只有会员行有值';
COMMENT ON COLUMN order_lines.device_id IS '出杯咖啡机 ID，与所属订单的 device_id 一致，冗余用于厂商单号的唯一性作用域；只有饮品行有';
COMMENT ON COLUMN order_lines.device_order_no IS '咖啡机侧订单号，外部单号，同一台机器内唯一；只有饮品行有';
COMMENT ON COLUMN order_lines.fulfillment_task_no IS '履约任务号，属于履约服务时仅作跨库值引用；只有饮品行有';
COMMENT ON COLUMN order_lines.pickup_code IS '取杯号：取杯口与屏幕上显示的短号（原型里的 C031），支付成功时生成，全局唯一；用户侧与后台都看得到。屏幕上人们也叫它取杯码，是同一个东西';
COMMENT ON COLUMN order_lines.remark IS '单行备注（例如加购品的口味或备注）';

COMMENT ON TABLE order_payment_lines IS '订单出资分摊：一个订单下逐笔出资来源与金额';
COMMENT ON COLUMN order_payment_lines.order_id IS '订单 ID';
COMMENT ON COLUMN order_payment_lines.line_no IS '订单内出资序号';
COMMENT ON COLUMN order_payment_lines.line_type IS '出资来源：用户选定的支付方式 code（payment-service 目录里的，如 ums_h5_alipay / coffee_bean），与 orders.payment_method 同一个值；出资渠道那套词表（wechat/unionpay/wallet/other）已退场';
COMMENT ON COLUMN order_payment_lines.amount IS '该笔出资金额，单位为分';
COMMENT ON COLUMN order_payment_lines.status IS '出资状态：reserved=已预占，succeeded=已成功，failed=已失败，released=已释放，reversed=已冲正';
COMMENT ON COLUMN order_payment_lines.payment_no IS '支付单号，属于支付服务时仅作跨库值引用';
COMMENT ON COLUMN order_payment_lines.provider_transaction_id IS '渠道方流水号';
COMMENT ON COLUMN order_payment_lines.failure_code IS '失败码';
COMMENT ON COLUMN order_payment_lines.account_entry_id IS '账户账变 ID，属于账户服务时仅作跨库值引用';
COMMENT ON COLUMN order_payment_lines.succeeded_at IS '出资成功时间';
COMMENT ON COLUMN order_payment_lines.reversed_at IS '出资冲正时间';

COMMENT ON TABLE order_state_transitions IS '订单领域状态变更审计记录，只追加';
COMMENT ON COLUMN order_state_transitions.aggregate_type IS '聚合类型：order=订单，order_line=订单行，after_sale=售后单，payment_line=出资';
COMMENT ON COLUMN order_state_transitions.aggregate_id IS '发生状态变化的聚合 ID';
COMMENT ON COLUMN order_state_transitions.from_status IS '变更前状态';
COMMENT ON COLUMN order_state_transitions.to_status IS '变更后状态';
COMMENT ON COLUMN order_state_transitions.reason IS '状态变更原因';
COMMENT ON COLUMN order_state_transitions.request_id IS '触发状态变化的请求幂等号';
COMMENT ON COLUMN order_state_transitions.actor_type IS '操作者类型：user=用户，merchant=商户，admin=后台，system=系统';
COMMENT ON COLUMN order_state_transitions.actor_id IS '操作人 ID，属于身份服务时仅作跨库值引用';
COMMENT ON COLUMN order_state_transitions.metadata IS '状态变化附加信息';

COMMENT ON TABLE order_after_sales IS '订单售后申请与处理结果，不保存退款单本身';
COMMENT ON COLUMN order_after_sales.legacy_id IS '老库退款单 ID（ObjectID 十六进制），仅迁移过来的历史记录有值';
COMMENT ON COLUMN order_after_sales.after_sale_no IS '售后单号，业务唯一';
COMMENT ON COLUMN order_after_sales.order_id IS '订单 ID';
COMMENT ON COLUMN order_after_sales.order_no IS '订单号快照，供客服与对账直接检索';
COMMENT ON COLUMN order_after_sales.user_id IS '申请用户 ID，属于身份服务时仅作跨库值引用';
COMMENT ON COLUMN order_after_sales.type IS '售后类型，当前只有 refund=退款';
COMMENT ON COLUMN order_after_sales.scope IS '退款范围：all=整单，drink=只退饮品行，addon=只退加购行，membership=只退会员套餐；除整单外都必须指明具体是哪一行';
COMMENT ON COLUMN order_after_sales.order_line_id IS '退的那一行，指向 order_lines；scope=all 时为空，其余范围必填（同一单有两件加购品时，只有它能说清退的是哪件）；「这一行属于本售后单的订单」由服务校验，外键看不到别的行';
COMMENT ON COLUMN order_after_sales.status IS '售后状态：pending=待处理，approved=已通过，rejected=已驳回，refunding=退款中，refunded=已退款，failed=退款失败，cancelled=已撤销';
COMMENT ON COLUMN order_after_sales.reason IS '申请原因';
COMMENT ON COLUMN order_after_sales.images IS '申请凭证图片 URL 列表';
COMMENT ON COLUMN order_after_sales.refund_amount IS '退款金额，单位为分';
COMMENT ON COLUMN order_after_sales.refund_no IS '退款单号，属于支付服务时仅作跨库值引用';
COMMENT ON COLUMN order_after_sales.failure_code IS '退款失败码';
COMMENT ON COLUMN order_after_sales.failure_message IS '退款失败原因：渠道返回的文案，与 failure_code 成对（成功时两者都是空串）';
COMMENT ON COLUMN order_after_sales.reviewed_by IS '审核人 ID，属于身份服务时仅作跨库值引用';
COMMENT ON COLUMN order_after_sales.reviewed_at IS '审核时间';
COMMENT ON COLUMN order_after_sales.review_remark IS '审核备注';
COMMENT ON COLUMN order_after_sales.refunded_at IS '退款完成时间';

COMMENT ON TABLE order_idempotency_keys IS '订单操作幂等请求记录';
COMMENT ON COLUMN order_idempotency_keys.scope IS '幂等作用域，例如 create、cancel、pay';
COMMENT ON COLUMN order_idempotency_keys.idempotency_key IS '调用方提供的幂等键';
COMMENT ON COLUMN order_idempotency_keys.request_hash IS '请求内容摘要，用于检测同一幂等键复用不同请求';
COMMENT ON COLUMN order_idempotency_keys.resource_type IS '幂等请求创建或操作的资源类型';
COMMENT ON COLUMN order_idempotency_keys.resource_id IS '幂等请求关联的资源 ID';
COMMENT ON COLUMN order_idempotency_keys.response IS '首次处理结果快照';
COMMENT ON COLUMN order_idempotency_keys.status IS '处理状态：processing=处理中，succeeded=成功，failed=失败';

COMMENT ON TABLE message_outbox IS '订单服务待发布消息事件';
COMMENT ON COLUMN message_outbox.event_id IS '事件唯一 ID';
COMMENT ON COLUMN message_outbox.event_type IS '事件类型';
COMMENT ON COLUMN message_outbox.event_version IS '事件版本';
COMMENT ON COLUMN message_outbox.payload IS '事件内容二进制数据';
COMMENT ON COLUMN message_outbox.attempts IS '发布尝试次数';
COMMENT ON COLUMN message_outbox.published_at IS '成功发布时间';
COMMENT ON COLUMN message_outbox.lease_until IS '消息处理租约截止时间';

COMMENT ON TABLE message_inbox IS '订单服务已接收消息事件及消费租约';
COMMENT ON COLUMN message_inbox.event_id IS '已接收事件唯一 ID，用于消费去重';
COMMENT ON COLUMN message_inbox.claimed_at IS '首次领取消费时间';
COMMENT ON COLUMN message_inbox.lease_until IS '消息消费租约截止时间';
COMMENT ON COLUMN message_inbox.completed_at IS '成功完成消费时间';

COMMIT;
