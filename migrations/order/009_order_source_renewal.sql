-- order/009：会员续费单——orders.source 多一个取值 renewal
--
-- 背景：连续包月的每一期代扣成功之后，要留下**一张订单**。老系统就是这么做的
-- （panda_serve 的 subscription_service.go:685 createRenewalOrder，后台「订单管理」里那些
-- 业务阶段写着「自动续费」、单号 SUB 开头的单子就是它）。这一刀之前 V2 一张都不建——
-- payment_agreement_charges 里记着「这一期收了 9.9」，订单域没有任何东西，后台看不到这笔钱，
-- 而下游发会员价券又要求事件里带着订单号（见 membership 那边同批的改动）。
--
-- 这条迁移只做一件事：把 source 的词表放开一格，外加把两列注释重发一遍。
--
--   **为什么不复用 miniapp**：那条路的语义是「用户在小程序里点了下单」，而续费没有这一下
--   ——没有人点，是到期自动扣的。后台列表的来源筛选、以及按来源看的每一张报表都靠这一列，
--   两者混在一起之后就再也分不开了。
--
--   **为什么不复用 device**：设备单没有用户（user_id 为 NULL），续费单有——那一期的钱是
--   某个人的会员费。两条路的建单 RPC 也不是同一条（见 order.proto 的 CreateRenewalOrder）。
--
-- 放宽词表是安全的：存量行的 source 只有 001 与 005 那三个取值，没有一行会违反新约束。
-- 真要有，下面的 ALTER 会直接失败而不是静默改写数据。

-- 1. source 多一个取值。先显式删约束再加：DROP 之后重建是必要的，而写出来读的人才看得见
--    词表换过（与 order/003、order/004、order/005 同一个写法）。
ALTER TABLE orders DROP CONSTRAINT orders_source_check;
ALTER TABLE orders ADD CONSTRAINT orders_source_check
    CHECK (source IN ('miniapp', 'screen_qr', 'device', 'renewal'));

-- 001 里 source 那一列上方的行内注释、以及 005 重发过一次的列注释都被这一条取代了。
-- 注释不进库，改的是源码里的那份说明；而已经建好的库要跟上，靠的是下面这一句重发
-- （新库会先跑 002 拿到旧文本，再由这条覆盖）——与 order/003 / 004 / 005 里重发的做法一致。
COMMENT ON COLUMN orders.source IS '下单来源：miniapp=小程序直接下单，screen_qr=咖啡机屏幕选品后扫码下单，device=线下刷卡机设备回调（partner-service 验签后经 gRPC 建单，没有用户、直接落成已支付），renewal=会员续费代扣（membership-service 在扣款成功之后经 gRPC 建单，有用户、纯会员行、直接落成已支付）';

-- 2. 续费单也走 third_party_order_no（传的是渠道流水号 provider_transaction_id），幂等靠
--    005 建的那条 orders_third_party_order_no_key。这里**不新建索引**，只把注释补全——
--    那一列今天服务三条路（刷卡机 / 取货码 / 续费），005 的注释里只写了设备回调，会让后来
--    读的人以为续费单没走这条路。
COMMENT ON COLUMN orders.third_party_order_no IS '对方单号：设备回调里那个第三方订单号（刷卡机与取货码），或会员续费的渠道流水号（provider_transaction_id）。三条路共用一个单号空间，它是幂等键：同一单号只能落一张订单；空串表示这一单没有对方单号（小程序与屏幕扫码的单）且不参与唯一性';
