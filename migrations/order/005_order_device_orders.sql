-- order/005：线下刷卡机订单——把既成事实记下来
--
-- 线下刷卡机（方案 §四）与另外两条下单路是**反的**：小程序与屏幕扫码是「先建单 → 待支付 →
-- 支付回调 → 已支付」，刷卡机是「钱已经在机器上收过了 → 建一张已支付」。入口在
-- partner-service（它验完签），经 gRPC 调 order-service 的 CreateDeviceOrder。
--
-- 这条路的形状变化全部落在这一个文件里：
--
--   1. orders.user_id 去掉 NOT NULL。刷卡机订单**没有用户**——老系统写的是
--      primitive.NilObjectID，一个「没有」的哨兵值；V2 用 NULL 表达同一件事：
--      不是空串、不是某个系统用户、也不是「还不知道」。空 uuid 与系统用户都会让
--      「这一单属于谁」看起来有一个答案，而这条路上真实的答案是「不属于任何人」。
--      user_id 上没有外键（见 001），所以去掉 NOT NULL 不涉及任何级联。
--   2. 新增 orders.third_party_order_no：对方单号，设备回调的幂等键。空串排除在唯一性
--      之外——空串表示「这一单没有对方单号」（小程序与屏幕扫码的单），不是「另一个空
--      单号」，与 orders_payment_no_key / orders_request_id_key 同一条规矩。
--   3. orders.source 多一个取值：device=设备回调（线下刷卡机）。
--
-- 顺带说明一件**没有**跟着改的事：order_after_sales.user_id 保持 NOT NULL。售后申请只有
-- 「用户本人发起」这一条路（后台只有审核与列表，见 routes.RegisterAdmin），而那条路在仓储
-- 里要求申请人的 user_id 与订单的 user_id 相等——设备单是 NULL，永远比不上，于是在锁内就
-- 被判成「订单不存在」。所以那张表不会出现 user_id 为空的行，也就不需要跟着放开。
--
-- 收窄/放宽都是安全的：dev 库与老库都没有设备单，存量行的 user_id 全部有值、source 只有
-- 001 的两个取值、third_party_order_no 一律为空串。去掉 NOT NULL 不改任何一行的值，加列带
-- 默认值也不改；真要有存量行违反新约束，下面的 ALTER 会直接失败而不是静默改写数据。

-- 1. 设备单没有用户。
ALTER TABLE orders ALTER COLUMN user_id DROP NOT NULL;

-- 2. 对方单号 + 幂等用的部分唯一索引。
ALTER TABLE orders ADD COLUMN third_party_order_no TEXT NOT NULL DEFAULT '';

-- 同一个对方单号只能落一张订单。空串表示「没有对方单号」，所以排除在唯一性之外——照
-- orders_payment_no_key 的写法。它同时是并发下的最后一道锁：两个同号回调同时到达时，
-- 先查后插两边都会说「没有」，只有这条索引能挡住第二张。
CREATE UNIQUE INDEX orders_third_party_order_no_key
    ON orders (third_party_order_no) WHERE third_party_order_no <> '';

-- 3. source 多一个取值。先显式删约束再加：DROP 之后重建是必要的，而写出来读的人才看得见
--    词表换过（与 order/003、order/004 同一个写法）。
ALTER TABLE orders DROP CONSTRAINT orders_source_check;
ALTER TABLE orders ADD CONSTRAINT orders_source_check
    CHECK (source IN ('miniapp', 'screen_qr', 'device'));

-- 001 里 source 那一列上方的行内注释写着「两类下单来源（方案 5.8）」，加了第三类之后那句
-- 话不成立了，一并改掉（注释不进库，改的是源码里的那份说明）。002 那边已经落进库的列注释
-- 同样失效，下面重发一遍让已经建好的库跟上（新库会先跑 002 拿到旧文本，再由这条覆盖）——
-- 与 order/003 / order/004 里重发注释的做法一致。
COMMENT ON COLUMN orders.user_id IS '下单用户 ID，属于身份服务时仅作跨库值引用；**设备单（source=device）为 NULL**：钱在刷卡机上收过了，这一单不属于任何用户。NULL 就是「没有用户」，不是空 uuid、也不是某个系统用户';
COMMENT ON COLUMN orders.source IS '下单来源：miniapp=小程序直接下单，screen_qr=咖啡机屏幕选品后扫码下单，device=线下刷卡机设备回调（partner-service 验签后经 gRPC 建单，直接落成已支付）';
COMMENT ON COLUMN orders.third_party_order_no IS '对方单号（设备回调里那个第三方订单号），设备单的幂等键；同一单号只能落一张订单，空串表示这一单没有对方单号（小程序与屏幕扫码的单）且不参与唯一性';
