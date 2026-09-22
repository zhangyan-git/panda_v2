-- order/008：出资渠道词表退场——order_payment_lines.line_type 改存支付方式的 code
--
-- 003 给 order_payment_lines.line_type 立了一套「出资渠道」词表
-- （wechat / unionpay / coffee_bean / wallet / other），与 payment 库那份逐字对应。这一条
-- 把它退掉：line_type 今天存的是**用户选的那一种支付方式**，也就是 payment-service 目录里的
-- code（ums_h5_alipay / ums_h5_wechat / ums_miniapp_wechat / coffee_bean …），与同一张订单上
-- orders.payment_method 是**同一个值**。
--
-- 为什么退：加一种支付方式本来只该在 catalog 里加一条，但两套词表并存时还要在出资渠道里给它
-- 找一档——找不到就得改两个库的 DDL。支付宝就卡在这里：它在出资渠道里没有档位，只能落 `other`，
-- 于是后台把一笔支付宝单显示成「其他」。003 的注释里写着「词表在 payment 库有一份逐字对应的」，
-- 那是当时的**约束**，不是今天的形状。payment/012 是这一条在 payment 库的另一半，两个文件
-- 一起上。
--
-- 换成什么约束：`CHECK (line_type <> '')`。这比原来弱得多，而且**跨库一致性这层保护换不回来**
-- ——order 库看不见 payment 的目录，谁也没法在这里写出一份「今天的合法 code 表」。能守住的只剩
-- 「非空」：一条出资行说不出自己从哪来，是比它从一个陌生 code 来更坏的事。反过来说，这层保护
-- 本来也是假的：003 的词表拦不住 payment 侧新增一种方式，只会让它在订单侧落成 other。
--
-- 历史行怎么办：这一条**不在库里回填**。要订正的两列（orders.payment_method 与
-- order_payment_lines.line_type）存的是出资渠道，而「用户当时选的是哪一种方式」那个事实在
-- panda_payment 的 payments.payment_method 上——跨库没有 dblink，这不是一条迁移能做的事。
-- 回填单独做，从 payment 库导出 (payment_no, funding_type, payment_method) 再在 order 库按
-- 支付单号精确改，判据是「这一行/这一列的值等于该支付单的 funding_type」——只有老代码写下的
-- 行会命中，咖啡豆单（值本来就等于 code）、设备单（没有支付单号）、没付过款的空值行都不受影响。
--
-- 顺序：**apply 本文件 → 回填脚本 → apply payment/012**（012 自己回填 payment 库同库那三列
-- 并删掉 funding_type）。
--
-- 本文件必须排在回填**之前**，理由就是上面那条 CHECK：回填写进去的是 code，而旧 CHECK 只认
-- 出资渠道那五个值，先回填会被它整条顶回来。（写这条迁移时真踩过：脚本先跑，报的正是
-- order_payment_lines_line_type_check。）
--
-- 007 的说法到此作废：它写着这一列「两种值并存」（老单是出资渠道、新单是支付方式 code）。
-- 回填之后不再并存——全库只剩一种值。

ALTER TABLE order_payment_lines
    DROP CONSTRAINT order_payment_lines_line_type_check,
    ADD CONSTRAINT order_payment_lines_line_type_check CHECK (line_type <> '');

-- 002 与 003 都写过这条注释，这里重发一遍让已经建好的库跟上（新库会先跑那两条拿到旧文本，
-- 再由这条覆盖）。
COMMENT ON COLUMN order_payment_lines.line_type IS '出资来源：用户选定的支付方式 code（payment-service 目录里的，如 ums_h5_alipay / coffee_bean），与 orders.payment_method 同一个值；出资渠道那套词表（wechat/unionpay/wallet/other）已退场';

-- 007 写的注释里「两种值并存」与「出资渠道逐笔见 order_payment_lines.line_type」两句都不再成立，
-- 一并重发。
COMMENT ON COLUMN orders.payment_method IS '用户选定的支付方式（payment-service 目录里的 code，如 ums_h5_alipay）；与 order_payment_lines.line_type 同一个值。历史行已按支付单回填订正，全库只有这一种值';
