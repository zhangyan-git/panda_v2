-- payment：支付领域（资金域）表结构、约束与中文注释
--
-- 本迁移属于 panda_payment，也是本集**唯一**一份：支付单、出资、退款、回调与调用流水、
-- 不可变资金流水、状态流水、支付幂等、委托代扣、渠道对账，以及并入本服务的分账与结算。
-- 订单、用户、账户（咖啡豆/福卡）、优惠券、会员、履约等外部服务的 ID 与单号只作为值引用保存；
-- 本库不得创建跨数据库外键。
--
-- 边界（方案 5.9 / 5.10 / 5.11）：
--   Payment   拥有支付单、支付回调、退款单、委托代扣、支付与退款流水、渠道对账、支付幂等
--   Order     拥有订单与订单状态（payment 只发布结果事件，不写订单表）
--   Account   拥有咖啡豆/福卡余额与账变（payment 的账户出资只记一个 account_entry_id 值引用）
--   Settlement 不单独建服务：分账、分润规则与结算单并入 payment-service（资金域），
--             见下面「分账与结算」一节。
--
-- 被别的服务固定、必须对齐的约定只有一条：
--
--   出资那一列（payment_fundings.line_type、payment_transactions.line_type、
--   payment_refund_fundings.line_type，以及 payments.payment_method）存的是**支付方式的
--   code**，也就是 payment-service 的 internal/catalog 里那几个常量（ums_h5_alipay /
--   ums_h5_wechat / ums_miniapp_wechat / coffee_bean …）。order-service 消费
--   payment.succeeded / payment.failed 时拿到的 paymentMethod 就是同一个 code，直接落它那份
--   出资分摊（backend/services/order-service/internal/dto/payment.go），两边不做翻译。
--   改 catalog 的 code 等于改跨服务契约，必须两边同时改（订单库那一半在同名的 order 集里）。
--
-- 渠道与支付方式**不进库**：本库不建渠道表，也不建支付方式表。四种支付方式、六个 code、
-- 一条银联商务渠道（provider = ums）都是 payment-service 的 internal/catalog 里的常量，
-- 库里只留下 payments.provider 与 payments.payment_method 两个值列。理由与两条仍然有效的
-- 判断写在下面「渠道与支付方式」一节。
--
-- 敏感数据不落库（方案 18.3 / 11.5）：渠道密钥、API Secret、签名原文一律不写进本库的列，
-- 凭据按名字读环境变量或挂载文件，由部署侧注入；payment_provider_calls 存的是脱敏摘要。
-- 这与 manufacturer_credentials 直接存密钥列的先例**有意不一致**——那张表先于 18.3 存在，
-- 本库是新建的，按 18.3 建。
--
-- 金额单位一律是「分」（BIGINT）；分成比例是 0~1 的 NUMERIC(20,6)。
--
-- # 上生产的注意：事务里建索引
--
-- 整份文件由 platform/database/migrate 丢给**一个事务**执行，所以**用不了
-- CREATE INDEX CONCURRENTLY**。本文件里几条建在会长的表上的索引——payments 的
-- payments_admin_list_idx / payments_pending_reconcile_idx / payments_overdue_account_funding_idx，
-- payment_refunds_processing_query_idx，payment_agreement_charges 的两条部分唯一索引，以及
-- settlement_tasks_rule_idx——在存量数据上会**短暂持写锁**。本仓库的支付库还很小，dev/prod
-- 都是秒级；真到几百万行那天，办法是：
--
--   1. 临时把要手工建的那条索引从本文件里去掉；
--   2. 跑 `panda-migrate -database "$PAYMENT_DATABASE_URL" apply payment`，其余部分照常建、
--      并记下 001 已应用；
--   3. 由 DBA 手工 `CREATE INDEX CONCURRENTLY` 建出那条索引；
--   4. 把语句补回本文件——已经跑过的库不会再执行它，新建的库照常拿到。
--
-- 合成本文件之后**不能再像从前那样单独 adopt 某一个编号**：adopt 的语义是把 THROUGH 那一份
-- **记为已应用、一条都不跑**（见 cmd/panda-migrate），而本集只剩 001 这一份，adopt 它等于把
-- 整集跳过。上面四步是同一件事在本集里的做法。
--
-- 本文件不带 goose 的 Down 段：platform/database/migrate 把整个文件丢给一次 Exec、不识别
-- goose 指令，带上就会在同一个事务里建完表再删掉，而且不报错。

BEGIN;

-- ============================================================
-- 渠道与支付方式：本库不建表
-- ============================================================
--
-- 这一节**没有 DDL**，只有一条边界：支付渠道与支付方式不是本库的数据。四种支付方式、
-- 六个 code、一条银联商务渠道都写在 payment-service 的 internal/catalog 里；库这一侧只留下
-- 两个值列（payments.provider 与 payments.payment_method，都在下面 payments 表上）。
--
-- 为什么可以这样（不是「暂时不用」）：
--   1. 渠道配置的用处是让运营去填 15–30 个协议键（密钥名、签名字段大小写、成功码、时间戳
--      格式），而今天只剩银联商务一条渠道、两条协议，那些键写死在适配器里（见 provider/ums）。
--   2. 凭据不进库。凭据按名字读环境变量，没有密文要解，连 PAYMENT_SECRET_KEY 都不再是一个
--      启动条件（方案 18.3 / 11.5）。
--   3. 支付单记的是「这一单走的哪条渠道、哪种方式」，而渠道名与方式 code 本身就够了。
--
-- 两条从这里继承下来、仍然有效的判断：
--
--   * provider 的取值是**协议族**，不是渠道名：manual / form_md5 / hmac_body / ums /
--     wechat_v3。丰选万联、优联、首创饭卡、北方工业饭卡四家的收单协议是同一套骨架，差别只在
--     几个配置项，所以「加一家渠道」分两种：族内加 = 在 catalog 里加一条；族外加 = 连适配器
--     一起加。写 provider 值时按这条判断，别照着渠道名填——catalog.Channel.Provider 沿用的
--     就是这套取值，而 payments.provider 存的是它。
--   * 分派认 action、不认 code：选中一条方式之后怎么起支付，取值是
--     jump_miniapp（跳对方小程序）/ native_pay（小程序内 requestPayment）/ direct_pay
--     （对接方直接扣款、不跳转）/ qrcode（扫普通二维码）/ h5（跳 H5 收银台）/ account
--     （走账户服务扣余额）六种，客户端只 switch 它。新接一家只要它的 action 属于上面已有
--     形态，加一条 catalog 记录就能用，客户端一行不改；出现全新形态才需要加一个取值。
--
-- 一条跨库的欠账，记在这里免得忘掉：coffee_machine 库的
-- device_payment_methods.payment_method_id 是 UUID 列，注释写着「支付方式 ID，属于支付
-- 服务」。支付方式今天已经没有 UUID 了——值是 catalog 里的 code——所以那一列的形状与词表对
-- 不上：设备域真要做「这台机器支持哪几种支付方式」时，第一件事是把那一列改成 TEXT 并改存
-- code，不是往里面写 uuid。那条迁移属于 coffee_machine 集，本文件不动它（那张表今天是空的、
-- 代码里也没有读写它的地方）。

-- ============================================================
-- 支付单与出资
-- ============================================================

-- 一次「向用户收这笔钱」的尝试。一个订单可以有多个支付单：第一次支付失败、超时被关，用户可以再发起
-- 一次，那是新的一行；但**成功只能有一张**（见下面的部分唯一索引），因为订单只能被收一次钱。
CREATE TABLE payments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    payment_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(payment_no)) > 0),
    legacy_id TEXT,
    -- 订单号（orders.order_no），值引用：支付手里只有下单时拿到的那张单，
    -- 两边各自维护对方的内部 ID 只会多一份对不上的可能。
    order_no TEXT NOT NULL CHECK (char_length(trim(order_no)) > 0),
    user_id UUID NOT NULL,
    -- 应付总额，单位为分；等于下面各笔出资之和（由应用在同一事务里写，跨表没法用 CHECK 表达）。
    amount BIGINT NOT NULL CHECK (amount > 0),
    -- created  = 已建单、还没向渠道发起
    -- pending  = 已向渠道发起（或已返回支付参数），等回调或轮询
    -- succeeded/failed = 终态
    -- closed   = 订单取消等主动关单；expired = 待支付超时
    status TEXT NOT NULL DEFAULT 'created' CHECK (status IN (
        'created', 'pending', 'succeeded', 'failed', 'closed', 'expired'
    )),
    -- 渠道收银台/账单上显示的商品描述。
    subject TEXT NOT NULL DEFAULT '',
    -- 渠道附加数据（小程序 openid、设备号等）。**不放密钥**，见文件头。
    attach JSONB NOT NULL DEFAULT '{}'::jsonb,
    provider_transaction_id TEXT NOT NULL DEFAULT '',
    failure_code TEXT NOT NULL DEFAULT '',
    failure_message TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ,
    paid_at TIMESTAMPTZ,
    closed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 账户出资扣豆的**那一刻**就落库（先于结算）。有值就表示「这笔钱确实动过」，
    -- 见下面「账户出资」两列的说明。
    account_entry_id UUID,
    -- 账户出资的成交时刻（账户域那笔账变的发生时刻）。
    account_funded_at TIMESTAMPTZ,
    -- 走哪条渠道，值是渠道**在代码里**的名字（catalog.Channel.Provider，今天只有 ums）。
    -- 账户出资（纯咖啡豆）是空串——那一列可空的语义，用空串表达（空串在 TEXT 上能直接比较，
    -- 不需要每个读它的地方都写一遍 NULLIF/COALESCE）。**没有外键可指**：本库不建渠道表。
    provider TEXT NOT NULL DEFAULT '',
    -- 用户是在哪一档支付方式下发起的，值是 catalog 里的 code（如 ums_miniapp_wechat /
    -- coffee_bean），不是某个 UUID。展示与统计用它；发起那次已经是按 code 分派的，
    -- 不回头读这一列。
    payment_method TEXT NOT NULL DEFAULT ''
);

-- 一个支付单下的逐笔出资：微信/银联是渠道出资，咖啡豆是账户余额出资。
-- 混合出资必须逐笔留行：退款要按来源分别冲正，对账要按渠道核对，只在支付单上存一个支付方式就丢了依据。
--
-- 这张表与 order 库的 order_payment_lines 是**同一事实的两个视角**（资金侧 / 订单侧），不是冗余：
-- 支付侧是「钱从哪儿来的」，订单侧是「这一单谁出的钱」，靠 payment.succeeded 事件对齐。
-- 两边都不许直接改对方的那份——发现不一致正是对账要报出来的差异。
CREATE TABLE payment_fundings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    payment_id UUID NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
    line_no INTEGER NOT NULL CHECK (line_no > 0),
    -- 支付方式的 code（catalog 里的 ums_h5_alipay / coffee_bean 等），与
    -- payments.payment_method 同一个值。**不比任何 SQL 里能写全的词表**：catalog 是代码里的
    -- 常量，把它的全集写进 CHECK 等于让「加一条 catalog 记录」重新变成一次迁移，所以这里
    -- 只守非空。跨库那一层一致性靠的是同一个 code，不是这条约束。
    line_type TEXT NOT NULL CHECK (line_type <> ''),
    amount BIGINT NOT NULL CHECK (amount > 0),
    -- 与 order_payment_lines.status 同词表：reserved=已预占（账户余额先冻结待扣），succeeded=已成功，
    -- failed=已失败，released=已释放（预占解开、不再扣），reversed=已冲正。
    status TEXT NOT NULL DEFAULT 'reserved' CHECK (status IN (
        'reserved', 'succeeded', 'failed', 'released', 'reversed'
    )),
    provider_transaction_id TEXT NOT NULL DEFAULT '',
    failure_code TEXT NOT NULL DEFAULT '',
    -- account-service 的账变 ID：账户出资扣的是余额，退款要按这笔账变冲正。
    account_entry_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    succeeded_at TIMESTAMPTZ,
    reversed_at TIMESTAMPTZ,
    UNIQUE (payment_id, line_no)
);

-- 账户出资（纯咖啡豆）那条路上，钱是**分两步**离开我们的，两步各自提交：
--
--   1. 在账户域扣豆（跨服务 RPC，账户域那边一次提交）
--   2. 在本地把支付单推到 succeeded（另一个事务，见 SettleAccountPayment）
--
-- 两步之间没有任何东西连着：进程被 kill、库抖动、或者这张单在这中间被超时关单收走，豆就已经
-- 真的扣走了，而本地**一个字段都没留下**——payment_fundings 那一行在没提交的那个事务里，
-- payments 上也没有任何列指向账户域那笔账变。事后只能靠人拿着订单号去账户库翻
-- coffee_bean_entries，可翻哪一张单、翻哪一笔，没有任何线索。
--
-- 现在那两条兜底各自补了一半：发起方拿同一个 request_id 重发会在账户域命中幂等键、回放原账变
-- （见 client.DeductRequest），但那只在「用户还会重试」时成立；一旦这张单到了超时点，它会被
-- 关成 expired，连重试都再也结不了。
--
-- 所以 payments 上有面那两列：account_entry_id 在扣豆返回的**那一刻**落库
-- （RecordAccountDeduction），account_funded_at 是豆真正动了的时刻。有值就表示「这笔钱确实
-- 动过」，这一条判据有两个用途，缺一不可：
--
--   * 超时关单不再碰它们（ExpireOverduePayments 的扫描条件排除了 account_entry_id 非空的
--     行）。关单会把 payment_fundings 里 reserved 的行标成 released——那是对「钱从来没动过」
--     的描述，而这里的钱已经动了，把它标成 released 就是一次记账上的撒谎。
--   * 补偿任务把它们结算掉（SettleOverdueAccountPayments）。豆已经扣了，这张单就必须成，
--     而不是等着一个人来发现。
--
-- 为什么不复用 payment_fundings.account_entry_id：那一行**只在结算成功的那个事务里**才存在，
-- 而这两列要覆盖的正是「结算没成功」的那个窗口——拿只在成功路径上出现的东西去描述失败路径，
-- 是做不到的。
--
-- 两列不需要 DEFAULT，也没有回填：它们的写入方只有上面那条路，渠道支付永远没有值，
-- NULL 的含义恰好就是「这张单没有账户出资的扣减」。

-- 补偿任务扫「到点未结、但豆已经扣了」的那些。条件是 status ∈ (created, pending) 且
-- expires_at 已过且 account_entry_id 非空——三条都在索引里，扫描不落回全表。
-- 与 payments_pending_expiry_idx 同一个形状、同一个理由，只是又多带了一列做过滤。
CREATE INDEX payments_overdue_account_funding_idx ON payments (status, expires_at)
    WHERE status IN ('created', 'pending') AND account_entry_id IS NOT NULL;

-- ============================================================
-- 退款
-- ============================================================

-- 一张退款单 = 把某一笔支付里的钱按售后结论退回去。售后申请本身归 order-service
-- （order_after_sales），本库只认 after_sale_no 这个值引用与它给出的金额、行。
--
-- 不复制 order_after_sales.scope（整单退/按行退）：范围是订单侧的权威事实，支付这边照它的结论执行，
-- 存一份副本只会多一份对不上的可能；需要时按 order_line_id 值引用回查。
CREATE TABLE payment_refunds (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    refund_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(refund_no)) > 0),
    -- 老库 refund_orders._id（ObjectID 的 24 位 hex），存量的退款单迁进来时有值。
    legacy_id TEXT,
    payment_id UUID NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
    payment_no TEXT NOT NULL CHECK (char_length(trim(payment_no)) > 0),
    order_no TEXT NOT NULL CHECK (char_length(trim(order_no)) > 0),
    -- order-service 的售后单号，值引用；一张售后单只能落一张退款单，所以它整表唯一。
    after_sale_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(after_sale_no)) > 0),
    -- 退的是哪一行（order_lines.id），值引用；整单退为空。金额与券的返还都落在这一行上。
    order_line_id UUID,
    user_id UUID NOT NULL,
    amount BIGINT NOT NULL CHECK (amount > 0),
    reason TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN (
        'pending', 'processing', 'succeeded', 'failed', 'cancelled'
    )),
    provider_refund_id TEXT NOT NULL DEFAULT '',
    failure_code TEXT NOT NULL DEFAULT '',
    failure_message TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    succeeded_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 退款也要逐笔：原支付是「微信 + 咖啡豆」混合出资时，钱要分别从渠道退回、从账户余额冲正，
-- 渠道退款单号与账户账变 ID 各自独立，合成一笔就再也说不清哪一半退成了。
CREATE TABLE payment_refund_fundings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    refund_id UUID NOT NULL REFERENCES payment_refunds(id) ON DELETE RESTRICT,
    -- 冲正的是哪一笔出资；存量迁移过来的退款可能找不到对应出资行，所以可空。
    funding_id UUID REFERENCES payment_fundings(id) ON DELETE RESTRICT,
    line_no INTEGER NOT NULL CHECK (line_no > 0),
    -- 被冲正的那笔出资的支付方式 code，与 payments.payment_method 同一个值；
    -- 词表与理由同 payment_fundings.line_type。
    line_type TEXT NOT NULL CHECK (line_type <> ''),
    amount BIGINT NOT NULL CHECK (amount > 0),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'succeeded', 'failed')),
    provider_refund_id TEXT NOT NULL DEFAULT '',
    failure_code TEXT NOT NULL DEFAULT '',
    -- 账户出资冲正产生的反向账变 ID，属于账户服务时仅作跨库值引用。
    account_entry_id UUID,
    succeeded_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (refund_id, line_no)
);

-- ============================================================
-- 渠道回调与调用流水
-- ============================================================

-- 渠道回来的每一条通知都先落在这里，再由处理器改支付/退款状态——回调本身是证据，不能只留在日志里。
--
-- 防重放靠 (provider, notification_id) 唯一：渠道重投、网络重放、我们自己点了两次，都会撞在这条上。
-- 验签失败的也落库（status='failed'）以便排查，但**绝不因此改支付状态**——这正是伪造回调想要的效果。
--
-- body 是渠道原始报文，只落在这一张表里：方案 11.5 禁止「未脱敏的第三方完整回调报文」进日志，
-- 所以日志侧只允许记 body_sha256 与摘要。
CREATE TABLE payment_notifications (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider TEXT NOT NULL CHECK (char_length(trim(provider)) > 0),
    -- 渠道侧的通知/流水唯一号（微信 resource.id、银联请求流水号、丰选通知序号）。
    notification_id TEXT NOT NULL CHECK (char_length(trim(notification_id)) > 0),
    event_type TEXT NOT NULL DEFAULT '',
    payment_no TEXT NOT NULL DEFAULT '',
    refund_no TEXT NOT NULL DEFAULT '',
    body BYTEA NOT NULL,
    body_sha256 TEXT NOT NULL CHECK (char_length(trim(body_sha256)) > 0),
    -- 只留定位用的请求头（请求号、时间戳、签名算法名）；签名原文与密钥不在这里。
    headers JSONB NOT NULL DEFAULT '{}'::jsonb,
    signature_verified BOOLEAN NOT NULL DEFAULT FALSE,
    status TEXT NOT NULL DEFAULT 'received' CHECK (status IN (
        'received', 'processed', 'ignored', 'failed'
    )),
    failure_reason TEXT NOT NULL DEFAULT '',
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ,
    UNIQUE (provider, notification_id)
);

-- 我们主动打给渠道的每一次调用（下单、查单、关单、退款、代扣、对账拉单、分账），方案 6 要求所有
-- 第三方适配器都有调用流水。超时与「结果未知」必须留痕：一笔状态不明的转账是运维要拿它去渠道查的线索。
--
-- request_summary / response_summary 是**脱敏后**的摘要：不放签名原文、密钥、完整卡号与身份证
-- （方案 11.5），只放能定位这一笔的键（商户单号、渠道单号、金额、返回码）。
CREATE TABLE payment_provider_calls (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider TEXT NOT NULL DEFAULT '',
    -- 词表按能力并集列。微信分账是四步（发起动账 / 查动账 / 完结 / 回退），银联商务是随支付
    -- 下发一步到底——某套渠道用不到哪几个，看它的实现就知道。
    operation TEXT NOT NULL CHECK (operation IN (
        'create', 'query', 'close', 'refund', 'query_refund',
        'agreement_sign', 'agreement_charge', 'agreement_terminate', 'reconcile',
        'division', 'query_division', 'finish_division', 'reverse_division'
    )),
    payment_no TEXT NOT NULL DEFAULT '',
    refund_no TEXT NOT NULL DEFAULT '',
    agreement_no TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    trace_id TEXT NOT NULL DEFAULT '',
    attempt_no INTEGER NOT NULL DEFAULT 1 CHECK (attempt_no > 0),
    request_summary JSONB NOT NULL DEFAULT '{}'::jsonb,
    response_summary JSONB NOT NULL DEFAULT '{}'::jsonb,
    http_status INTEGER,
    provider_code TEXT NOT NULL DEFAULT '',
    provider_message TEXT NOT NULL DEFAULT '',
    result TEXT NOT NULL DEFAULT 'unknown' CHECK (result IN ('success', 'failed', 'timeout', 'unknown')),
    duration_ms INTEGER,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 分账的每一次渠道调用也落这张表——老系统那套 settle_logs 在 V2 就是它，所以不另建表，
-- 也不新增列：分账调用用 payment_no 关联任务，回退调用用 refund_no 关联退款单，已有的两列够用。

-- 回调排查与补偿扫描：某一笔支付收到了哪些回调、哪些还没处理完。
CREATE INDEX payment_notifications_payment_idx ON payment_notifications (payment_no)
    WHERE payment_no <> '';
CREATE INDEX payment_notifications_unprocessed_idx ON payment_notifications (status, received_at)
    WHERE status IN ('received', 'failed');
CREATE INDEX payment_provider_calls_payment_idx ON payment_provider_calls (payment_no, created_at DESC)
    WHERE payment_no <> '';
CREATE INDEX payment_provider_calls_refund_idx ON payment_provider_calls (refund_no, created_at DESC)
    WHERE refund_no <> '';

-- ============================================================
-- 不可变资金流水
-- ============================================================

-- 支付与退款流水（方案 5.9）。操作表（payments / payment_refunds）记的是当前状态，会一直被改写；
-- 这一张只追加，记的是「什么时候真的进出过一笔钱」，对账（5.9、14.3）以它为基准。
--
-- 冲正不删不改原记录，新增一条反向记录（方案 8.2）——kind='reversal'，direction 与原记录相反，
-- 于是「这条流水现在还剩多少」永远是整表求和，而不是某一行被改成了什么。
--
-- 这一列里**没有渠道外键，也没有渠道 id**：流水是对账基准，渠道配置怎样都不该让已发生的流水
-- 对不上，所以它只记渠道名（provider）。
CREATE TABLE payment_transactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    legacy_id TEXT,
    -- payment=一笔出资成功入账，refund=一笔出资被退回，reversal=冲正（对账差异、人工纠正）
    kind TEXT NOT NULL CHECK (kind IN ('payment', 'refund', 'reversal')),
    -- 三个定位键都写成空串而不是 NULL：NULL 在唯一索引里互不相等，空串才能真的挡住重复记账。
    payment_no TEXT NOT NULL DEFAULT '',
    refund_no TEXT NOT NULL DEFAULT '',
    -- 出资序号，对应 payment_fundings.line_no 或 payment_refund_fundings.line_no；没有具体行时为 0。
    funding_line_no INTEGER NOT NULL DEFAULT 0 CHECK (funding_line_no >= 0),
    -- 支付方式的 code，与 payments.payment_method 同一个值；词表与理由同
    -- payment_fundings.line_type。
    line_type TEXT NOT NULL CHECK (line_type <> ''),
    direction TEXT NOT NULL CHECK (direction IN ('in', 'out')),
    amount BIGINT NOT NULL CHECK (amount > 0),
    provider_transaction_id TEXT NOT NULL DEFAULT '',
    account_entry_id UUID,
    -- 渠道给的成交时间，不是我们记账的时间。
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 这一行流水属于哪条渠道（渠道名，与 payments.provider 同词表）。
    provider TEXT NOT NULL DEFAULT '',
    UNIQUE (kind, payment_no, refund_no, funding_line_no)
);

-- 流水按单号与时间找：对账差异逐笔核对时用。
CREATE INDEX payment_transactions_payment_idx ON payment_transactions (payment_no, funding_line_no)
    WHERE payment_no <> '';
CREATE INDEX payment_transactions_refund_idx ON payment_transactions (refund_no, funding_line_no)
    WHERE refund_no <> '';
CREATE INDEX payment_transactions_occurred_idx ON payment_transactions (occurred_at);

-- ============================================================
-- 状态流水
-- ============================================================

-- 与 order_state_transitions 逐列同形：支付单、出资、退款、签约、扣款、对账都跨多个请求与回调，
-- 只看当前状态查不出「怎么走到这一步的」。这张表只追加，不改不删。
CREATE TABLE payment_state_transitions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type TEXT NOT NULL CHECK (aggregate_type IN (
        'payment', 'funding', 'refund', 'agreement', 'charge', 'reconciliation'
    )),
    aggregate_id UUID NOT NULL,
    from_status TEXT NOT NULL DEFAULT '',
    to_status TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    -- admin 的人工退款/关单同时会在身份库 admin_operation_logs 留一条后台操作审计，
    -- 这里记的是支付自己的口径。
    actor_type TEXT NOT NULL DEFAULT 'system' CHECK (actor_type IN ('user', 'merchant', 'admin', 'system')),
    actor_id UUID,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX payment_state_transitions_aggregate_idx
    ON payment_state_transitions (aggregate_type, aggregate_id, created_at);
CREATE INDEX payment_state_transitions_request_idx ON payment_state_transitions (request_id)
    WHERE request_id <> '';

CREATE FUNCTION prevent_payment_transaction_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'payment transactions are append-only';
END;
$$;

CREATE TRIGGER payment_transactions_append_only
    BEFORE UPDATE OR DELETE ON payment_transactions
    FOR EACH ROW EXECUTE FUNCTION prevent_payment_transaction_change();

CREATE FUNCTION prevent_payment_transition_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'payment state transitions are append-only';
END;
$$;

CREATE TRIGGER payment_state_transitions_append_only
    BEFORE UPDATE OR DELETE ON payment_state_transitions
    FOR EACH ROW EXECUTE FUNCTION prevent_payment_transition_change();

-- ============================================================
-- 支付幂等
-- ============================================================

-- 与 order_idempotency_keys、coupon_idempotency_keys 逐列同形：同一套幂等语义在仓库里只有一种表结构。
-- 发起支付、发起退款、代扣扣款都要用它：回调可能重投、客户端可能重试，扣两次钱不行。
CREATE TABLE payment_idempotency_keys (
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
-- 委托代扣与渠道对账
-- ============================================================

-- 这两块的业务规则（续费周期怎么定、账单口径按什么切）在方案里还没冻结，所以这一段只落
-- 「谁、什么时候、对哪一笔做了什么」这些**不会随规则改的事实**，不去约束规则本身：
-- 不写「必须每月扣一次」，也不写「差异必须在几天内处理完」。
--
-- 归属（方案 5.9）：微信委托代扣的签约、扣款、解约归 payment-service；渠道对账也归它。
-- 会员怎么续、什么时候该扣，由 membership-service 决定并传进来（plan_code、biz_period、amount 都是
-- 业务方给的值），本库不存会员套餐，也不自己算期次。
--
-- 两张表今天都是空的、也没有代码在读写：代扣与对账本轮没做。

-- 一次签约 = 用户授权我们从他的账户上按期扣钱。contract_no 是渠道侧的协议号，解约和解约后
-- 再查签约状态都得靠它。
--
-- next_charge_at 由业务方更新，payment 只按它扫描该扣谁——「什么时候该扣」是业务的决定，
-- 支付服务不该自己推算续费周期。max_charge_amount 是签约要素（渠道要求签约时就写明单次上限），
-- 超过它的扣款请求渠道会拒。
CREATE TABLE payment_agreements (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agreement_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(agreement_no)) > 0),
    -- 老库代扣/订阅签约记录的 ID（ObjectID 的 24 位 hex），存量迁过来时有值。
    legacy_id TEXT,
    user_id UUID NOT NULL,
    -- 渠道侧的协议号/签约号。
    contract_no TEXT NOT NULL DEFAULT '',
    -- 签约用途，如「会员自动续费」。
    subject TEXT NOT NULL DEFAULT '',
    -- 业务方给的签约计划标识（如会员套餐代码），本库不解释它的含义。
    plan_code TEXT NOT NULL DEFAULT '',
    -- 单次扣款上限，单位为分；0 表示签约未约定上限。
    max_charge_amount BIGINT NOT NULL DEFAULT 0 CHECK (max_charge_amount >= 0),
    -- 下一次该扣款的时间，由业务方更新；扫描待扣款按它走。
    next_charge_at TIMESTAMPTZ,
    -- pending=已发起签约待用户确认，signed=用户已确认，active=生效中，suspended=暂停扣款，
    -- terminated=已解约，expired=签约到期。
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN (
        'pending', 'signed', 'active', 'suspended', 'terminated', 'expired'
    )),
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    signed_at TIMESTAMPTZ,
    activated_at TIMESTAMPTZ,
    terminated_at TIMESTAMPTZ,
    terminate_reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 代扣只能走渠道（微信委托代扣、银联无感支付），所以它非空；存的是渠道名
    -- （与 payments.provider 同词表）。
    provider TEXT NOT NULL DEFAULT '',
    -- 签约时用户选的支付方式 code（catalog 里的常量），不是某个 UUID；
    -- 渠道侧的签约可以没有。
    payment_method TEXT NOT NULL DEFAULT ''
);

-- 一个签约、一个期次只扣一次钱——UNIQUE (agreement_id, biz_period) 就是代扣的幂等。
-- 重试是同一行把 attempt_count 加一、更新 next_retry_at，不是新开一行：多开一行就可能扣两次。
CREATE TABLE payment_agreement_charges (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agreement_id UUID NOT NULL REFERENCES payment_agreements(id) ON DELETE RESTRICT,
    agreement_no TEXT NOT NULL CHECK (char_length(trim(agreement_no)) > 0),
    -- 期次，由业务方给（如 2026-10、第 3 期）。本库不推算、不校验格式。
    biz_period TEXT NOT NULL CHECK (char_length(trim(biz_period)) > 0),
    amount BIGINT NOT NULL CHECK (amount > 0),
    -- 扣款落到 payments 时对应的支付单号，值引用。**代扣今天不建 payments 行**（代扣没有订单，
    -- 为它编一个订单号是往账本里掺假），所以这一列恒为空串——一期的账看本行的
    -- status / out_trade_no / charged_at。
    payment_no TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN (
        'pending', 'charging', 'succeeded', 'failed', 'skipped', 'cancelled'
    )),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_retry_at TIMESTAMPTZ,
    failure_code TEXT NOT NULL DEFAULT '',
    failure_message TEXT NOT NULL DEFAULT '',
    charged_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 这一期扣款在渠道那边的商户单号（微信报文里的 out_trade_no）。扣款的结果是**异步**回来
    -- 的：微信把「这一笔扣成没扣成」推到一个 notify_url 上，报文里带着它认这笔单的唯一凭据
    -- 就是它，而 agreement_no 是协议的号、biz_period 是期次，两个都不在渠道的报文里。
    -- 所以这一列不是「多存一个便于排查的字段」，它是那条回调能落地的**唯一**关联键：
    -- 老系统正是死在这里（它的续费扣款把 out_trade_no 取成交易记录的 _id，回调按它去查
    -- user_subscriptions 必然查不到 → 回 FAIL → 微信按重试策略反复推、永不收敛）。
    --
    -- **建行那一次生成，之后每一次重试都复用这一列的值**：重算一个新单号等于在微信侧开出
    -- 第二笔订单，而两笔单号不同，微信那边无法去重——用户会被扣两次钱。生成规则照
    -- service.paymentNo（PAY + YmdHis + 三位毫秒 + 六位随机），把前缀换成 CHG：26 个字符，
    -- 微信对 out_trade_no 的上限是 32；**不带协议号/期次**，微信只要求全局唯一。
    out_trade_no TEXT NOT NULL DEFAULT '',
    -- 这一期扣款在渠道那边的流水号（微信的 transaction_id）。out_trade_no 是**我们**发给渠道
    -- 的商户单号，而客服/财务在微信商户平台上看到的、用户在微信账单里看到的是渠道那一侧的号，
    -- 两者此前没有任何一列能对上。列名与 payments.provider_transaction_id 逐字相同，理由也
    -- 一样——它是渠道方流水号，不是我们的任何编号。
    provider_transaction_id TEXT NOT NULL DEFAULT '',
    UNIQUE (agreement_id, biz_period)
);

-- 用户看自己的签约、按状态筛。
CREATE INDEX payment_agreements_user_idx ON payment_agreements (user_id, status);
-- 扫描该扣款的人：生效中的签约，按 next_charge_at 到点。
CREATE INDEX payment_agreements_pending_charge_idx
    ON payment_agreements (status, next_charge_at)
    WHERE status = 'active';
CREATE INDEX payment_agreement_charges_status_idx ON payment_agreement_charges (status, next_retry_at);
CREATE INDEX payment_agreement_charges_payment_idx ON payment_agreement_charges (payment_no)
    WHERE payment_no <> '';

CREATE UNIQUE INDEX payment_agreements_legacy_id_key
    ON payment_agreements (legacy_id) WHERE legacy_id IS NOT NULL;

-- 存量行（本表今天还没有任何写路径）与将来的 cancelled/skipped 行都不该占号：空串不是单号。
-- 谓词 `out_trade_no <> ''` 与上面给 payment_no 建的那条同一个形状，理由也同一条——
-- 「没有单号」是一个合法状态，不该被唯一性约束当成一个重复的值。
-- 这条索引扫全表，所以在 CREATE TABLE 之后单独一句。
CREATE UNIQUE INDEX payment_agreement_charges_trade_no_unique
    ON payment_agreement_charges (out_trade_no)
    WHERE out_trade_no <> '';

-- 渠道的 transaction_id 在同一个商户号下全局唯一，所以「同一笔渠道流水挂在两行扣款上」在
-- 事实上不可能。而它一旦发生，含义非常具体：**同一笔钱被记成了两期**。那正是代扣这张表最怕的
-- 错误（比漏记严重得多——漏记只是没扣，重复记是账面上多收了用户一期的钱），所以这里用唯一
-- 约束把它挡在库这一层，而不是指望服务层每一处都判对。
CREATE UNIQUE INDEX payment_agreement_charges_transaction_unique
    ON payment_agreement_charges (provider_transaction_id)
    WHERE provider_transaction_id <> '';

-- 一次对账 = 拉某一个渠道某一天的账单、与本库流水比一遍。微信的支付账单与退款账单是两份文件，
-- 所以 bill_type 要参与唯一性：同一渠道同一天可以有一个 payment 批次和一个 refund 批次。
--
-- provider 这一列**非空且没有默认值**：建对账批次必须指名渠道。
CREATE TABLE reconciliation_batches (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    batch_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(batch_no)) > 0),
    provider TEXT NOT NULL,
    -- 对账的业务日（渠道账单上的那一天），不是拉单的那一天。
    biz_date DATE NOT NULL,
    bill_type TEXT NOT NULL DEFAULT 'all' CHECK (bill_type IN ('payment', 'refund', 'all')),
    -- 账单从哪来：渠道文件、渠道接口、人工上传。人工上传要留 source_ref 指向那份文件。
    source TEXT NOT NULL DEFAULT 'channel_file' CHECK (source IN (
        'channel_file', 'channel_api', 'manual_upload'
    )),
    source_ref TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN (
        'pending', 'fetching', 'comparing', 'completed', 'failed'
    )),
    total_count INTEGER NOT NULL DEFAULT 0 CHECK (total_count >= 0),
    total_amount BIGINT NOT NULL DEFAULT 0 CHECK (total_amount >= 0),
    matched_count INTEGER NOT NULL DEFAULT 0 CHECK (matched_count >= 0),
    difference_count INTEGER NOT NULL DEFAULT 0 CHECK (difference_count >= 0),
    failure_reason TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 「同一渠道同一天同一种账单只有一个批次」：唯一约束含渠道那一列，所以它写在建表里。
    UNIQUE (provider, biz_date, bill_type)
);

-- 对账的逐笔结论。matched 之外的四类都是差异，必须有人处理并留下结论（方案 14.3 要的是
-- 「差异被谁、什么时候、怎么消掉的」）——所以处理结果只追加在这张表上，**不去改流水**：
-- 账实不符时改本地数据让两边一致，等于把差异藏起来。
CREATE TABLE reconciliation_records (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    batch_id UUID NOT NULL REFERENCES reconciliation_batches(id) ON DELETE RESTRICT,
    -- 渠道侧的流水号；渠道多出来的那笔（本地没有）只有它。
    channel_trade_no TEXT NOT NULL CHECK (char_length(trim(channel_trade_no)) > 0),
    -- 本地这边对应的是支付单还是退款单（值引用 payment_no / refund_no 里的一个）。
    local_kind TEXT NOT NULL DEFAULT 'payment' CHECK (local_kind IN ('payment', 'refund')),
    local_no TEXT NOT NULL DEFAULT '',
    amount BIGINT NOT NULL DEFAULT 0 CHECK (amount >= 0),
    trade_at TIMESTAMPTZ,
    -- matched=两边一致，channel_only=渠道有本地没有，local_only=本地有渠道没有，
    -- amount_mismatch=金额不符，status_mismatch=状态不符。
    result TEXT NOT NULL CHECK (result IN (
        'matched', 'channel_only', 'local_only', 'amount_mismatch', 'status_mismatch'
    )),
    handled BOOLEAN NOT NULL DEFAULT FALSE,
    -- 处理人，属于身份服务时仅作跨库值引用。
    handled_by UUID,
    handled_at TIMESTAMPTZ,
    handle_remark TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (batch_id, channel_trade_no, local_kind)
);

-- 对账批次的排队扫描与按渠道回看历史。
CREATE INDEX reconciliation_batches_status_idx ON reconciliation_batches (status, biz_date);
CREATE INDEX reconciliation_batches_provider_idx ON reconciliation_batches (provider, biz_date DESC);
-- 待处理差异：对账跑完只是开始，没处理完的差异才是要盯的。
CREATE INDEX reconciliation_records_open_idx ON reconciliation_records (batch_id, result)
    WHERE NOT handled;
CREATE INDEX reconciliation_records_local_idx ON reconciliation_records (local_kind, local_no)
    WHERE local_no <> '';

-- ============================================================
-- 分账与结算
-- ============================================================
--
-- 本库装下「钱在各方之间怎么分」这件事。不单独建 settlement-service：渠道分账与结算单并入
-- payment-service，本服务的定位从「支付」扩成「资金域」（渠道执行 + 支付单 + 退款 + 对账 +
-- 分账 + 结算单）。
--
-- 三条边界，落地时不要越线：
--   1. 权限码必须拆开：settlement:read / settlement:manage（打款单独一条 settlement:payout）。
--      **不能并进 payment:manage**——分润比例是运营随手会改的东西，渠道密钥是另一回事，
--      把两个权限码合成一个，等于给「改比例」的人同时开了改密钥的门。
--   2. 结算侧只存值引用 + **冻结快照**：比例、金额、主体名、子商户号都是「当时的那个值」。
--      规则改了、门店改名了、账户停用了，历史明细一个字都不能跟着变。所以下面每张流水表
--      都另存了一份快照列，看起来冗余，但那是账，不是缓存。
--   3. 依赖单向：本库不反向读订单/门店/券的库；结算单不进任何别的域的表。
--
-- 老系统六个集合的落法（只读对照，不改老系统）：
--   settle_rules       → settlement_rules        业务分类与范围档位逐字保留
--   settle_rule_items  → settlement_rule_items   每个接收方一行，算法与比例保留
--   settle_accounts    → settlement_accounts     渠道侧子商户号（V2 多一列渠道名，见下）
--   settle_details     → settlement_receivers    「订单 × 接收方 × 金额」，形状一样
--   settle_logs        → **不建表**：它就是渠道调用日志，本库已有 payment_provider_calls
--   settle_roles       → **不建表**：纯标签，老系统自己的注释就写着「不参与计算」，
--                        改用 settlement_rule_items.party_type 的受控词表（下方逐个列出）
--
-- 老系统踩过、这里用约束钉住的六个坑：
--   1. 金额存 float64 —— 这里一律 BIGINT 分；比例 NUMERIC(20,6)（老系统也是 0~1 的比值）。
--   2. 六个集合**一个索引都没有**，幂等全靠应用「先查再插」——这里每条不变量都有唯一约束。
--   3. settle_details.order_id 没有唯一约束，同一笔订单重复分账拦不住。
--   4. status 列了 4 个值，实际只写过 0 和 2，另外两个从未赋值 —— 这里每个状态都写明**谁写它**。
--   5. 没有任何退款/取消的分账回退、解冻、冲正 —— 这里 settlement_reversals 是单独一层。
--   6. 没有结算单/结算批次/打款这一层 —— 这里 settlement_statements 是新的，老系统无从对照。
--
-- 分账基数就是**用户实付金额（券后）**，券成本不单独考虑（2026-09-18 定）。这意味着券的
-- 让利按分成比例在各方之间摊掉，而不是由发券方独担——
--
--     订单 100、券 30、门店分成 45%（无券时门店 45 / 平台 55）
--     实付 70 → 门店 70×45% = 31.5、平台差额 38.5
--     门店少拿 13.5、平台少拿 16.5，30 元按 45:55 摊掉了
--
-- 所以规则项上**不要**加「折扣承担方 / 券前券后基数」这类维度，也不要按发券方去分摊；
-- 这是有意不做的（真要做就得让一笔分账有两个基数，恒等式也要跟着改）。钱是平的，
-- 只是谁该出多少不按券的归属走——这台账认的就是这个口径。
--
-- 分账任务**在发起支付时就建好**，支付成功之后只剩推进状态（2026-09-18 定）。理由、
-- 那个「没付成」的出口（cancelled），以及它给扫描与结算带来的两处必须记住的过滤条件，
-- 都写在 settlement_tasks 与 settlement_statement_items 的头上。
--
-- **曾经的「缺口 A」已经补上了**（2026-09-18）：当时支付库里没有门店/设备维度，而任务在发起
-- 支付那一刻建，那些列必须在那一刻就有值——契约不补，这条链路就断在「安静地建不出任务」上
-- （没有报错，只是永远没有分账发生）。
--
-- 补法是 `CreatePaymentRequest` 追加三个字段（`store_id` / `device_id` / `biz_type`），
-- order-service 从订单与订单行推出来随发起支付一起送；payment-service 在建支付单的**同一个
-- 事务里**命中规则、算出各家金额，写出 settlement_tasks + settlement_receivers。三个维度的
-- 来路与 biz_type 的推法写在 service/settlement.go 的 buildSettlementPlan 与 order-service 的
-- pay.go 里。落点是下面 settlement_tasks 的 scope_* / store_ref 快照列。
--
-- 补上之后**仍然命中不了两档**，这是有意的、写在代码注释里的：
--   brand   订单库没有品牌（brands/stores 在商户域），要命中得先从商户域经 gRPC 取
--           「门店 → 品牌」；
--   product 一笔支付可能含多个商品，而这里是「一笔支付一条任务」，没有单一商品可指。
-- 命中函数的签名收全五档（device → store → brand → product → global），今天的调用方只给得出
-- 两档。**写全但暂时命不中**，等值来源出现时不用改命中逻辑。
--
-- 落到哪一步了：**建任务这一刀已经通了**（任务与接收方在发起支付的事务里建成 pending，支付单
-- 进 failed / expired 时两边一起转 cancelled）。**还没做**的是任务建好之后那半段——向渠道发起
-- 分账（pending → submitted → succeeded）、冲正、以及 settlement_statements 那两张结算单表，
-- 一行代码都还没有。
--
-- 金额恒等式：settlement_tasks.base_amount = platform_amount + Σ settlement_receivers.amount。
-- 跨表，CHECK 表达不了（与 payments.amount 同一处境），由应用在同一事务里写、由对账核。
-- 平台金额用**差额倒挤**（老系统的算法，也是唯一能吸收固定额与尾差的算法），保留。

-- ------------------------------------------------------------
-- 配置层：分润规则
-- ------------------------------------------------------------

-- 分账接收账户：一个主体在一个渠道上收钱用的那个号（微信服务商的 sub_mchid、银联商务的 mid）。
--
-- 老系统只有一列 sub_merchant_id，因为它的分账只走银联商务一套。V2 是多渠道的：同一个门店
-- 在微信和银联商务各有一个子商户号，所以这里必须有渠道名——它不是「多加的一列」，
-- 而是老系统那套结构在第二个渠道上必然要长出来的东西。
--
-- merchant_id / brand_id / store_id 是别的域的事实，只做值引用，不建跨库外键——**本表今天
-- 一列都不存它们**：登记账户时页面上的三个主体下拉是选填的，而全仓没有任何读路径按主体去找
-- 账户（命中规则挑账户只看 account_id 与 status、渠道名，金额计算不看它们），所以那三列连同
-- 由它们派生的 subject_ref 一并退场了。唯一性只由下面那条「一个渠道下的接收方号只能登记一次」
-- 护着——那才是真正护着渠道报文的那条。
--
-- account_no 与 receiver_name 两列也退场了：前者说好「结算单上写它」，而结算单根本没建、
-- 将来真做也是挂 account_id；后者从来没有随支付发出去过（渠道报文里只有子商户号与金额），
-- settlement_receivers 的快照里也没有它。主体名（下面 party_name）接手做这一行的身份，
-- 并且是必填。
CREATE TABLE settlement_accounts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- 老库 settle_accounts._id（ObjectID 的 24 位 hex），存量迁过来时有值。
    legacy_id TEXT,
    -- 主体名称快照：账户改名不改历史结算单（历史那份在流水表里另有快照，这里是当前值）。
    party_name TEXT NOT NULL DEFAULT '',
    -- 主体是谁，词表与 settlement_rule_items.party_type 一致。
    party_type TEXT NOT NULL CHECK (party_type IN (
        'partner', 'city_center', 'agent', 'member_store', 'platform'
    )),
    -- 渠道侧的接收方类型。微信服务商分账要它（MERCHANT_ID / PERSONAL_OPENID）；
    -- 银联商务按子商户号直接分，取不到这个概念的默认 MERCHANT_ID。
    receiver_type TEXT NOT NULL DEFAULT 'MERCHANT_ID' CHECK (receiver_type IN (
        'MERCHANT_ID', 'PERSONAL_OPENID'
    )),
    -- 渠道侧的子商户号本体。老系统叫 sub_merchant_id，这里与 receiver_type 配对。
    receiver_id TEXT NOT NULL CHECK (char_length(trim(receiver_id)) > 0),
    -- enabled = 可参与新分账；disabled = 停止使用。历史的用历史快照，与此处无关。
    status TEXT NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled', 'disabled')),
    remark TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 走哪条渠道（渠道名，与 payments.provider 同词表）。分账是渠道能力：没有渠道就没有可用的
    -- 接收方，所以这一列非空。本库不建渠道表，所以它没有可指的外键。
    provider TEXT NOT NULL,
    -- 主体名是这一行的身份，必填。列上的 DEFAULT '' 留着不动：它现在**永远满足不了**这条
    -- CHECK，所以一条漏写 party_name 的 INSERT 会以 23514 失败，而不是静默存进一个没有名字的
    -- 账户——那正是这里要的结果，不必再去摘那个默认值。
    CONSTRAINT settlement_accounts_party_name_check
        CHECK (char_length(TRIM(BOTH FROM party_name)) > 0)
);

-- 「一个渠道下的接收方号只能登记一次」，而且**启用才占位、停用即让位**：
-- 账户是删不掉的（三处 ON DELETE RESTRICT：规则项、历史接收方、结算单），唯一的退场动作就是
-- 停用。不带谓词的话会有这样一个局面——子商户号 R 原先挂在账户 A 上，A 已经停用了（换了主体，
-- 或者银联重发了号），运营要把 R 挂到新账户 B 上，而 B 建不出来：A 虽然停用了还占着 R，报的
-- 又是那句笼统的「这个账户号、子商户号或主体已经有一条记录」，用户看着自己刚停用的那条很难
-- 想到是它；A 也删不掉，R 就这么废了。放开之后 B 能建出来，A 继续停着不影响任何事。
--
-- 反面也照样成立：把一条停用的账户重新启用时，如果已经有一条启用中的账户占着同一个接收方号，
-- 那次启用会 23505 被拒——那正是这条索引要防的事（分账把钱打给同一个接收方两次）。
CREATE UNIQUE INDEX settlement_accounts_receiver_uniq
    ON settlement_accounts (provider, receiver_type, receiver_id)
    WHERE status = 'enabled';

-- 分润规则：哪个范围（门店/设备/品牌…）上的哪类业务，按什么算法分。
--
-- scope_ref 是范围的值引用（device id / store id / brand id），global 档为空——两者的关系用
-- CHECK 钉住，免得出现「global 却指着一个门店」这种看不清规则到底命中谁的行。
--
-- 命中顺序按范围精度从具体到宽泛：device → store → brand → product → global。老系统也是这个
-- 语义（先按设备找，找不到再往上兼容），只是它没有任何约束保证同档不撞车——两条同档规则同时
-- 启用时，命中哪条取决于查询的返回顺序。这里用部分唯一索引把那种库拦在配置期。
CREATE TABLE settlement_rules (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    legacy_id TEXT,
    name TEXT NOT NULL CHECK (char_length(trim(name)) > 0),
    -- 业务分类，照老系统 settle_type 的词表逐字保留（存量迁过来不用做映射）。
    biz_type TEXT NOT NULL CHECK (biz_type IN (
        'coffee', 'membership', 'store_consume', 'addon_product'
    )),
    -- 范围档位，照老系统 scope_type 的词表逐字保留。
    scope_type TEXT NOT NULL CHECK (scope_type IN (
        'device', 'store', 'brand', 'product', 'global'
    )),
    scope_ref TEXT NOT NULL DEFAULT '',
    -- normal              = 各接收方按比例/固定额各拿各的，剩下的归平台
    -- fixed_then_remaining= 先扣掉固定额，剩余部分再按比例分（老系统的两档，算法见下）
    allocation_mode TEXT NOT NULL DEFAULT 'normal' CHECK (allocation_mode IN (
        'normal', 'fixed_then_remaining'
    )),
    -- enabled = 参与命中；disabled = 保留配置但不再命中（历史任务仍指着它，所以不删行）。
    status TEXT NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled', 'disabled')),
    remark TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- global 与 scope_ref 必须同步为空/非空。
    CHECK ((scope_type = 'global') = (scope_ref = ''))
);

-- 同一业务分类 + 同一档位 + 同一范围，同时只能有一条启用中的规则。
CREATE UNIQUE INDEX settlement_rules_scope_uniq
    ON settlement_rules (biz_type, scope_type, scope_ref) WHERE status = 'enabled';

-- 规则项：一条规则里，钱分给谁、分多少。一条规则的所有项构成完整的一次分配。
--
-- 三种算法各自要求哪些字段有值，用 CHECK 说清楚。老系统的固定额项也带着 ratio，于是
-- 「到底哪个数说了算」只存在于算法代码里，看数据看不出来。
--
-- 第三种 remainder 是给平台项的：平台拿的是**差额**（基数 - 其他各项），既不是比例也不是
-- 固定额。老系统里这一项也挂在 percent/fixed 底下、填一个算不出来的比例，等于让读数据的人
-- 去代码里找答案——所以这里给它一个自己的算法名，「平台自留靠差额倒挤」这件事就从算法
-- 搬进了数据。
--
-- 一条规则**通常**应在平台项上收口；没有平台项也合法，那时各接收方之和必须正好等于基数
-- （没有留给人吸收尾差的余地）。「必须有且只有一条平台项」里的「至少一条」表达不了
-- （要触发器），这里只钉住「至多一条」。
CREATE TABLE settlement_rule_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_id UUID NOT NULL REFERENCES settlement_rules(id) ON DELETE CASCADE,
    -- 收款主体类型，词表与 settlement_accounts.party_type 一致（原来在这里的是老系统的
    -- settle_roles：那份角色表不参与计算，只是一张可维护的标签表，收成受控词表更省事）。
    party_type TEXT NOT NULL CHECK (party_type IN (
        'partner', 'city_center', 'agent', 'member_store', 'platform'
    )),
    -- percent   = 按基数乘 ratio（其他接收方）
    -- fixed     = 拿固定额 fixed_amount，单位分（其他接收方；fixed_then_remaining 模式下先扣它）
    -- remainder = 差额自留（只有平台项能用，见上方说明）
    calc_type TEXT NOT NULL CHECK (calc_type IN ('percent', 'fixed', 'remainder')),
    -- 分成比例，0~1 的比值（0.45 = 45%），与老系统口径一致。
    ratio NUMERIC(20,6) NOT NULL DEFAULT 0 CHECK (ratio >= 0 AND ratio <= 1),
    -- 固定分账额，单位为分。
    fixed_amount BIGINT NOT NULL DEFAULT 0 CHECK (fixed_amount >= 0),
    -- 收款账户。平台项为空：平台是**差额自留**，不向渠道发起分账，永远没有接收方明细。
    account_id UUID REFERENCES settlement_accounts(id) ON DELETE RESTRICT,
    -- 展示顺序（老系统 settle_rule_items.sort_order）。
    sort_order INTEGER NOT NULL DEFAULT 0,
    remark TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 三种算法各自要求哪些字段有值，三选一，不留中间态；算法与主体也绑死
    -- （平台项不可能是比例项，别的项也不可能是 remainder——那会把差额倒挤挪到某个门店头上，
    -- 门店拿「剩下的」既算不清也说不通）。
    CHECK (
        (calc_type = 'remainder' AND party_type = 'platform'
            AND ratio = 0 AND fixed_amount = 0)
        OR (calc_type IN ('percent', 'fixed') AND party_type <> 'platform'
            AND ((calc_type = 'percent' AND ratio > 0 AND fixed_amount = 0)
              OR (calc_type = 'fixed' AND ratio = 0 AND fixed_amount > 0)))
    ),
    -- 平台项没有账户，其余项必须有账户——分账要发给谁，渠道那边得有个号。
    CHECK ((party_type = 'platform') = (account_id IS NULL))
);

-- 平台项一条规则只能有一个：平台拿的是「剩下的」，两条平台项就没有「剩下」可言了。
CREATE UNIQUE INDEX settlement_rule_items_platform_uniq
    ON settlement_rule_items (rule_id) WHERE party_type = 'platform';

-- 同一个账户在同一条规则里只能出现一次（同一笔钱分两次给同一个号 = 配置错了）。
CREATE UNIQUE INDEX settlement_rule_items_account_uniq
    ON settlement_rule_items (rule_id, account_id) WHERE account_id IS NOT NULL;

-- ------------------------------------------------------------
-- 执行层：一笔支付的分账，与逐笔接收方
-- ------------------------------------------------------------

-- 分账任务：一笔支付的分账计划，**在发起支付时就建好**，支付成功之后只剩推进状态。
--
-- 为什么提前建（2026-09-18 定）：发起支付的请求里正好带着这笔钱的全部上下文——订单、门店、
-- 设备、实付金额都由 order 权威给出，那一刻命中规则、算好各家金额最省事；等支付成功了再回头
-- 凑这些维度，要么跨服务反查，要么补一次契约，反而更绕。规则也因此在**发起支付那一刻冻结**：
-- 运营之后改了比例，不影响已经建好的这一笔。
--
-- 代价是要给「没付成」一个出口：支付单进 failed / expired / closed 时，这条任务连同它的
-- 接收方一起置 cancelled（见状态说明）。作废只会发生在 pending 上——那时还没向渠道发起过，
-- 渠道侧没有任何东西要收回。
--
-- 一笔支付只有一条任务（payment_id 唯一）：接收方一次全给完（老系统就是整单一次分完）。
-- 渠道侧临时失败就重试**同一条**，不新建——重试次数记在 attempts 上。
-- 万一将来真要「先分一部分、之后再补分」，把 payment_id 上的唯一约束换成 (payment_id, seq)
-- 即可，那时 seq 才有意义；今天不给没有第二行的东西预留序号。
--
-- 规则命中时用的维度（scope_* / store_ref）在这里存**快照**：规则后来改了、门店后来换了，
-- 这张任务单上记的仍是当初命中的那条规则和那个范围。rule_id 因此是 ON DELETE RESTRICT——
-- 有任务指着的规则删不掉，只能停用。
--
-- 下面那三列（merchant_ref / brand_ref / store_ref）是**订单侧推过来的归属维度**，有真读者：
-- 明细列表的筛选条件与详情页的「归属门店/品牌/商户」都读它们。这与 settlement_accounts 上
-- 曾经的三列不同——那边没有任何读路径，所以退场的是那边。
CREATE TABLE settlement_tasks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(task_no)) > 0),
    legacy_id TEXT,
    -- 同库，走外键（本库的约定：库内的归属关系用 UUID 外键，跨库的才退化成单号值引用）；
    -- 单号另存一份，供对账与排查直接读，与 payment_refunds 存 payment_no 是同一个做法。
    payment_id UUID NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
    payment_no TEXT NOT NULL CHECK (char_length(trim(payment_no)) > 0),
    -- 分账基数，单位为分：实付金额（券后）。老系统也是拿 PayAmount 当基数。
    base_amount BIGINT NOT NULL CHECK (base_amount > 0),
    -- 平台自留 = base_amount - Σ 各接收方金额。差额倒挤，吸收固定额与舍入尾差；
    -- 整单归平台时它等于 base_amount（老系统的兜底）。
    platform_amount BIGINT NOT NULL CHECK (platform_amount >= 0),
    -- 命中的规则。**为空是常态**：门店没配规则时这条任务照样建（整单归平台），
    -- 老系统也是这个兜底。停用/改名都不回改历史；被指着的规则删不掉。
    rule_id UUID REFERENCES settlement_rules(id) ON DELETE RESTRICT,
    -- 命中时的范围与维度快照。store_ref / brand_ref 来自订单侧推过来的值引用——
    -- store_ref 今天有值（订单推得出来），brand_ref 恒为空串：订单库里没有品牌，
    -- 见上面那两档「补上契约之后仍然命不中」的说明。建任务要用的正是它们
    -- （任务在发起支付时就建，见上方说明）。
    scope_type TEXT NOT NULL DEFAULT '' CHECK (scope_type IN (
        '', 'device', 'store', 'brand', 'product', 'global'
    )),
    scope_ref TEXT NOT NULL DEFAULT '',
    store_ref TEXT NOT NULL DEFAULT '',
    brand_ref TEXT NOT NULL DEFAULT '',
    merchant_ref TEXT NOT NULL DEFAULT '',
    -- pending   = 已建任务、还没向渠道发起。**发起支付时就写它**（那时这笔还等着付款）；
    --             支付成功之后才轮到 submitted。
    --             注意「等付款」和「等发起」在这里是同一个状态：任务提前建，但渠道侧的分账
    --             一律要等支付成功——钱先到账，才谈得上分。
    -- submitted = 请求已发出（或已返回），等渠道回执；**超时会停在这里**，靠查单收口，
    --             不能直接重发（渠道的 Provider 契约里那条不变量：钱可能已经分出去了）
    -- succeeded = 渠道确认分账成功（写它的人：回调或查单）
    -- failed    = 渠道明确拒绝（余额不足、接收方无效等），last_error 记原因
    -- returned  = 已全额回退（写它的人：settlement_reversals 全部回退完时；部分回退不改本列）
    -- cancelled = 这笔支付没成，任务作废（写它的人：支付单进 failed / expired / closed 时，
    --             连同接收方一起置）。**只能从 pending 进**：那时还没发往渠道，没有要收回的钱；
    --             已经发出去的任务没有作废这一说，只能走 settlement_reversals 回退。
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN (
        'pending', 'submitted', 'succeeded', 'failed', 'returned', 'cancelled'
    )),
    -- 渠道侧的分账单号（发起时我们给的那个）。
    provider_task_no TEXT NOT NULL DEFAULT '',
    -- 渠道侧的受理号/交易号。
    provider_transaction_id TEXT NOT NULL DEFAULT '',
    -- 渠道侧**完结**时间。微信分账要单独一步完结（动账 → 完结），一次下发的渠道（银联商务）
    -- 没有这一步，在 succeeded 时就一并置上——用一列而不是把一个状态劈成两半，
    -- 是因为「完结」是渠道相关的能力，不是本地状态机的必经台阶。
    finished_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 一笔支付一条任务（见上面的说明）。
CREATE UNIQUE INDEX settlement_tasks_payment_uniq ON settlement_tasks (payment_id);
-- 没做完的任务是重试与查单的输入。
--
-- ⚠️ 扫「待发起」时**必须连 payments 一起过滤**（只取 payments.status = 'succeeded' 的）：
-- 任务在发起支付时就建，pending 里混着大量还没付款的。不加这个条件，扫出来的东西会把一笔
-- **还没收到的钱**发去分账——渠道会拒（余额压根没到），但更坏的情况是它不拒。
-- 索引只用来缩小范围，那一句 join 才是对错所在。
CREATE INDEX settlement_tasks_unfinished_idx
    ON settlement_tasks (status, created_at) WHERE status IN ('pending', 'submitted');

-- 分账接收方明细：一条任务发给一个主体的一笔钱。这是「账」的最小单位。
--
-- 与任务一起**在发起支付时**写好（金额当场算得出来，见 settlement_tasks 的说明），
-- 之后只推进状态。
--
-- 主体名、子商户号、比例全部另存快照：settlement_accounts 那一行后来改了名或停用了，
-- 这里记的仍必须是**当初发给渠道的那个号**——不然退款回退时会对不上渠道。这里的
-- merchant_ref / brand_ref / store_ref 同样是快照：它们是历史留痕，今天没有读路径
-- （账户侧那三列已经不存了），要删是单独一条迁移的事。
--
-- 老系统 settle_details 的 settle_target_id 一列混装「账户 ID」和「子商户号」，查账时要靠
-- 上下文猜是哪一种。这里 account_id（值引用）与 receiver_id（渠道号）分开。
CREATE TABLE settlement_receivers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- 老库 settle_details._id，存量迁过来时有值。
    legacy_id TEXT,
    task_id UUID NOT NULL REFERENCES settlement_tasks(id) ON DELETE CASCADE,
    account_id UUID NOT NULL REFERENCES settlement_accounts(id) ON DELETE RESTRICT,
    party_type TEXT NOT NULL CHECK (party_type IN (
        'partner', 'city_center', 'agent', 'member_store', 'platform'
    )),
    -- 主体值引用与名称快照。
    merchant_ref TEXT NOT NULL DEFAULT '',
    brand_ref TEXT NOT NULL DEFAULT '',
    store_ref TEXT NOT NULL DEFAULT '',
    party_name TEXT NOT NULL DEFAULT '',
    -- 渠道侧接收方快照（当初实际发出去的那个号）。
    receiver_type TEXT NOT NULL DEFAULT 'MERCHANT_ID',
    receiver_id TEXT NOT NULL CHECK (char_length(trim(receiver_id)) > 0),
    -- 当初生效的比例/固定额快照，单位与规则项一致（比例 0~1；固定额记在 amount 上，
    -- 这里留 0）。规则改了不影响这一行。
    ratio NUMERIC(20,6) NOT NULL DEFAULT 0 CHECK (ratio >= 0 AND ratio <= 1),
    -- 实际分给这一方的金额，单位为分。老系统允许「不足 1 分不分账」，那种接收方**不建行**
    -- （没有金额的行会让「Σ 明细 = base - 平台」这条恒等式变得要打折），而不是建一行 0。
    amount BIGINT NOT NULL CHECK (amount > 0),
    -- 渠道侧的分账明细号（微信的 detail_id）。回退时要按它指回这一笔。
    provider_detail_no TEXT NOT NULL DEFAULT '',
    -- 已回退金额（回退成功时累加）。放在这一行上，是为了让「部分回退不能超过原额」这条
    -- 能写成行内 CHECK——放到 settlement_reversals 里就要跨表求和才判得出来。
    reversed_amount BIGINT NOT NULL DEFAULT 0 CHECK (reversed_amount >= 0),
    -- pending   = 随任务一起写入（还等着付款），之后等渠道回执
    -- succeeded = 渠道确认这一笔分出去了
    -- failed    = 这一笔被渠道拒了（整单可能仍部分成功）
    -- returned  = 已全额回退（reversed_amount = amount）
    -- cancelled = 这笔支付没成，跟着任务一起作废（写它的人：任务置 cancelled 时）
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN (
        'pending', 'succeeded', 'failed', 'returned', 'cancelled'
    )),
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 回退不能超过原额。
    CHECK (reversed_amount <= amount),
    -- returned 状态与金额必须一致，免得出现「标了全额回退、金额只回了一半」。
    CHECK ((status = 'returned') = (reversed_amount = amount))
);

-- 一条任务里同一个账户只有一行。
CREATE UNIQUE INDEX settlement_receivers_task_account_uniq
    ON settlement_receivers (task_id, account_id);
-- 结算要按主体+时间捞（结算单明细从这里来）。
CREATE INDEX settlement_receivers_party_idx
    ON settlement_receivers (account_id, created_at);

-- 分账回退：退款发生时，把已经分出去的那部分从接收方收回来。
--
-- **老系统完全没有这一层**：`panda_serve` 的分账是随支付请求一次性下发的，退款路径上没有任何
-- 分账回退、解冻、冲正，已分出去的钱在系统里就此与订单脱钩。V2 必须有——退款是常态。
--
-- 一次退款可以对应多条回退（一笔分账分给了三个主体，退款按各自的比例往回退）。
-- (refund_id, receiver_id) 唯一：同一笔退款对同一个明细只能回退一次。
-- 回退金额的上限（不得超过该明细未回退的余额）跨行，由应用保证，对账核。
CREATE TABLE settlement_reversals (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    reversal_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(reversal_no)) > 0),
    receiver_id UUID NOT NULL REFERENCES settlement_receivers(id) ON DELETE RESTRICT,
    -- 退款单，同库走外键（同 settlement_tasks.payment_id）；单号另存一份供排查。
    refund_id UUID NOT NULL REFERENCES payment_refunds(id) ON DELETE RESTRICT,
    refund_no TEXT NOT NULL CHECK (char_length(trim(refund_no)) > 0),
    -- 回退金额，单位为分，恒为正；方向由「这是回退」这件事本身表达。
    amount BIGINT NOT NULL CHECK (amount > 0),
    -- pending = 已建、待发起；submitted = 已发往渠道；succeeded = 渠道确认收回；
    -- failed  = 渠道拒绝（如原分账已结算完），last_error 记原因
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN (
        'pending', 'submitted', 'succeeded', 'failed'
    )),
    provider_reversal_no TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '',
    succeeded_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX settlement_reversals_refund_receiver_uniq
    ON settlement_reversals (refund_id, receiver_id);
CREATE INDEX settlement_reversals_unfinished_idx
    ON settlement_reversals (status, created_at) WHERE status IN ('pending', 'submitted');

-- ------------------------------------------------------------
-- 结算层：周期汇总与打款
-- ------------------------------------------------------------

-- 结算单：某主体在一段周期内应收/应付的钱，以及打款。
--
-- 这一层**老系统没有**（它只做到「渠道把每笔分账的结果写回 settle_details」，没有批次、
-- 没有打款、没有对账口径），所以这张表与它的明细表都是新设计的，没有对照物。
-- 方案 5.10 要求结算侧有「不可变的结算流水」，这里的选择是：**不另设一本 ledger**——
-- settlement_receivers / settlement_reversals 本身就是不可变的流水（只追加、金额不改、
-- 状态单向推进），再记一本账等于两本账互相对；结算单只记「哪几条明细被算进了这张单」。
--
-- 金额恒等式写在行内（CHECK 可表达，照 orders_payable_matches 的先例）：
--   payable = opening + settled - reversed + adjustment
-- payable_amount 允许为负：本期回退大于本期收入时它是负数，含义是「下期冲抵」。
CREATE TABLE settlement_statements (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    statement_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(statement_no)) > 0),
    -- 结算给哪个账户（收款主体）。
    account_id UUID NOT NULL REFERENCES settlement_accounts(id) ON DELETE RESTRICT,
    -- 结算周期，左闭右开 [period_start, period_end)。跨期调整走 adjustment_amount，
    -- 不允许把一条明细同时算进两张单（见明细表的唯一索引）。
    period_start DATE NOT NULL,
    period_end DATE NOT NULL,
    -- 期初未结（上期结转），单位为分。
    opening_amount BIGINT NOT NULL DEFAULT 0,
    -- 本期分账收入合计（正数）。
    settled_amount BIGINT NOT NULL DEFAULT 0,
    -- 本期回退合计，以**正数**存，语义是减项。
    reversed_amount BIGINT NOT NULL DEFAULT 0 CHECK (reversed_amount >= 0),
    -- 人工调整（对账差异、抹零），可正可负；谁调的看 remark 与身份库的审计。
    adjustment_amount BIGINT NOT NULL DEFAULT 0,
    -- 本期应付。正数 = 该打给这个主体；负数 = 下期冲抵。
    payable_amount BIGINT NOT NULL,
    -- draft     = 系统生成、还没确认（此状态下明细可增删）
    -- confirmed = 财务确认、金额冻结（此后只读，改动走新的调整单）
    -- paid      = 已打款（paid_at / paid_by / payout_* 在此刻写）
    -- void      = 作废（明细保留，不进任何统计）
    status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN (
        'draft', 'confirmed', 'paid', 'void'
    )),
    -- 打款方式与凭证：offline = 线下转账（凭据号在 payout_ref），channel = 走渠道打款，
    -- 空 = 还没打。**不把打款做成支付单**：打款是结算自己的事，混进 payments 会让
    -- 「支付单 = 用户付给平台的钱」这条口径不成立。
    payout_method TEXT NOT NULL DEFAULT '' CHECK (payout_method IN ('', 'offline', 'channel')),
    payout_ref TEXT NOT NULL DEFAULT '',
    payout_amount BIGINT NOT NULL DEFAULT 0,
    paid_at TIMESTAMPTZ,
    -- 操作者（身份库的用户 ID，值引用）。
    paid_by TEXT NOT NULL DEFAULT '',
    remark TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (period_end > period_start),
    CHECK (payable_amount = opening_amount + settled_amount - reversed_amount + adjustment_amount),
    -- 打了款才有方式与时间，没打款就不该有。
    CHECK ((status = 'paid') = (paid_at IS NOT NULL))
);

-- 同一账户的同一周期只能有一张有效单（作废的重开不受限）。
CREATE UNIQUE INDEX settlement_statements_period_uniq
    ON settlement_statements (account_id, period_start, period_end) WHERE status <> 'void';

-- 结算单明细：这张单里算了哪些分账明细、哪些回退。
--
-- 「一条分账明细只能被结算一次」是这张表存在的全部理由——老系统没有这层，同一笔钱重复
-- 打款没有任何东西拦得住。两个部分唯一索引就是那道闸。
-- 金额带符号：分账收入为正，回退为负，与单头的等式相加方向一致。
--
-- 只有 succeeded 的接收方（与 succeeded 的回退）才进这张表：failed 与 cancelled 的一律不进。
-- 任务提前建的代价就是这里——库里躺着注定不会成功的明细，归集时漏一句过滤，结算单上就会
-- 多出一笔没付成的钱。
CREATE TABLE settlement_statement_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    statement_id UUID NOT NULL REFERENCES settlement_statements(id) ON DELETE RESTRICT,
    -- 来源二选一：一条分账明细，或一条回退。
    receiver_id UUID REFERENCES settlement_receivers(id) ON DELETE RESTRICT,
    reversal_id UUID REFERENCES settlement_reversals(id) ON DELETE RESTRICT,
    -- 带符号金额，单位为分。
    amount BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 恰好一个来源。
    CHECK ((receiver_id IS NULL) <> (reversal_id IS NULL)),
    -- 来源与符号必须匹配：分账为正、回退为负。
    CHECK ((receiver_id IS NOT NULL AND amount > 0)
        OR (reversal_id IS NOT NULL AND amount < 0))
);

CREATE UNIQUE INDEX settlement_statement_items_receiver_uniq
    ON settlement_statement_items (receiver_id) WHERE receiver_id IS NOT NULL;
CREATE UNIQUE INDEX settlement_statement_items_reversal_uniq
    ON settlement_statement_items (reversal_id) WHERE reversal_id IS NOT NULL;

-- 五条外键的索引。PostgreSQL **不会为外键自动建索引**。缺索引的代价不在写入（这些父行几乎
-- 不删），在**删父行**那一侧：RESTRICT / CASCADE 要在子表上确认一遍，而另外三张表上已有的
-- 部分索引**用不上**——外键检查那句 `WHERE col = $1` 推不出 `account_id IS NOT NULL` /
-- `status <> 'void'` 这类谓词，规划器只能退回去扫全表（dev 库上 EXPLAIN 过，
-- settlement_rule_items 与 settlement_statements 都给的是 Seq Scan，**即便把 seqscan 判成
-- 禁止**）；settlement_reversals 那条虽然 receiver_id 在键里，但不是**前导列**，规划器只能
-- 把整条索引从头扫一遍再用它过滤。
--
-- 所以下面这几条不是「已经有了、不用加」，将来也不要把它们当成重复索引删掉。五条都建成
-- **不带谓词**的普通索引：让规划器去证明谓词蕴含关系是个没必要的依赖，而这五张表都不大，
-- 多存下来的那几行空值不值一提。
CREATE INDEX settlement_tasks_rule_idx ON settlement_tasks (rule_id);
CREATE INDEX settlement_rule_items_rule_idx ON settlement_rule_items (rule_id);
CREATE INDEX settlement_reversals_receiver_idx ON settlement_reversals (receiver_id);
CREATE INDEX settlement_statement_items_statement_idx ON settlement_statement_items (statement_id);
CREATE INDEX settlement_statements_account_idx ON settlement_statements (account_id);

-- ============================================================
-- 支付单上的索引
-- ============================================================

-- 历史映射：只有迁移过来的行才有 legacy_id。
CREATE UNIQUE INDEX payments_legacy_id_key ON payments (legacy_id) WHERE legacy_id IS NOT NULL;
CREATE UNIQUE INDEX payment_refunds_legacy_id_key ON payment_refunds (legacy_id) WHERE legacy_id IS NOT NULL;
CREATE UNIQUE INDEX payment_transactions_legacy_id_key ON payment_transactions (legacy_id) WHERE legacy_id IS NOT NULL;

-- 一个订单只能有一张成功的支付单：重试可以开新单，但成功只能是那一次。
-- 与 order_payment_lines_one_live_success 同一个思路，只是作用在支付单上。
CREATE UNIQUE INDEX payments_one_succeeded_per_order
    ON payments (order_no) WHERE status = 'succeeded';

-- 同一支付单、同一出资类型只允许有一条成功线（重复回调不能变成扣两次钱）。
CREATE UNIQUE INDEX payment_fundings_one_live_success
    ON payment_fundings (payment_id, line_type) WHERE status = 'succeeded';

-- 方案 8.2：外部单号必须唯一。空串表示「还没有」，不是「另一个空单号」，所以空串排除在唯一性之外。
CREATE UNIQUE INDEX payments_request_id_key ON payments (request_id) WHERE request_id <> '';
CREATE UNIQUE INDEX payment_refunds_request_id_key ON payment_refunds (request_id) WHERE request_id <> '';
CREATE UNIQUE INDEX payments_provider_transaction_id_key
    ON payments (provider_transaction_id) WHERE provider_transaction_id <> '';
CREATE UNIQUE INDEX payment_refunds_provider_refund_id_key
    ON payment_refunds (provider_refund_id) WHERE provider_refund_id <> '';

-- 订单/用户视角的查询：后台列表、客服按单号查、用户看自己的支付记录。
CREATE INDEX payments_order_idx ON payments (order_no, created_at DESC);
CREATE INDEX payments_user_idx ON payments (user_id, created_at DESC);
-- 关单扫描：待支付且已过期的支付单。
CREATE INDEX payments_pending_expiry_idx ON payments (status, expires_at)
    WHERE status IN ('created', 'pending');
-- 出资线与冲正线都不另建 (父表, line_no) 索引：上面的 UNIQUE 约束已经建了同一个 btree，
-- 再建一份只是多付写放大。
CREATE INDEX payment_fundings_account_entry_idx ON payment_fundings (account_entry_id)
    WHERE account_entry_id IS NOT NULL;
CREATE INDEX payment_refunds_order_idx ON payment_refunds (order_no, created_at DESC);
CREATE INDEX payment_refunds_payment_idx ON payment_refunds (payment_id);
CREATE INDEX payment_refunds_status_idx ON payment_refunds (status, created_at);

-- 后台支付单列表的默认视图是**不带任何筛选**的「最近发生了什么」：它按 created_at 倒序取第一页，
-- 而上面三条索引里没有一条的前导列是 created_at（order_idx 与 user_idx 的前导列分别是 order_no
-- 与 user_id，pending_expiry_idx 是 status 且带部分条件）。没有这一条，那条查询会走全表扫描 +
-- Top-N 排序。带 id 是为了同一毫秒的两行也有稳定顺序：不含它的分页会在边界上重复或跳过行，
-- 而支付单的 created_at 精度是微秒，同批写入撞在一起是常态不是巧合。
--
-- 列表上按 status 的筛选**有意不建索引**（没有 (status, created_at DESC)）：status 只有 6 个
-- 取值、分布又高度倾斜（绝大多数是 succeeded），这种列上的二级索引对选择性几乎没有帮助，
-- 而 payments 是只增表，多一个索引就是给每一笔支付的成功路径多付一次写放大。哪天真慢了，
-- 先看 created_at 这一条能不能撑住，再谈要不要建部分索引。
CREATE INDEX payments_admin_list_idx ON payments (created_at DESC, id DESC);

-- 主动查单任务（internal/worker/reconcile.go → repository.ListStalePendingPayments）每分钟问
-- 一次库：`WHERE status = 'pending' AND provider <> '' AND updated_at <= $1 ORDER BY updated_at`。
-- 它要做的事和关单扫描正相反：关单找「到点该作废的」，它找「发起之后一直没结论的」。
--
-- 不复用 payments_pending_expiry_idx 是因为排序键不是可以互相替代的东西：pending 是一个
-- **会堆积**的状态（一笔支付发起之后没人付，它会一直待在那里直到超时关单），用一个按
-- expires_at 排的索引去取「最老的 20 笔」，在积压时每次都从同一头开始扫——真正的老单反而
-- 永远排在后面取不到。
--
-- 这里 status 筛选反而可以索引，与上面那句「status 筛选有意不建索引」不矛盾：**部分索引**的
-- 谓词把索引本身缩小到只剩 pending 行，而 pending 恰恰是这张表里最少的那一段。倾斜在这里是
-- 好事，不是需要容忍的代价。
CREATE INDEX payments_pending_reconcile_idx ON payments (updated_at)
    WHERE status = 'pending';

-- 退款查询任务（internal/worker/refund.go → repository.ListStaleProcessingRefunds）每五分钟问
-- 一次库：`WHERE status = 'processing' AND updated_at <= $1 ORDER BY updated_at`。形状与上面那条
-- 逐字同源，只是换了一张表。
--
-- 排序键为什么是 updated_at 而不是 created_at：一笔「托管退款」在渠道那边可以挂几天，期间每
-- 一次查询都会把 updated_at 推到当下（见 repository.TouchRefund）——processing 是一个**会主动
-- 重排**的状态。按 created_at 排的话，一笔挂了三天的退款每一轮都排在队首、被问一次、什么都没
-- 问出来、下一轮还在队首；后面那些刚进来、其实一次就能问出结果的退款永远轮不到。按 updated_at
-- 排，问过一轮的自动沉到队尾，这正是那个 Touch 想要的排队效果。
--
-- 谓词能索引的理由与上面那条相同：部分索引只剩 processing 行，而 processing 是这张表里最少
-- 的那一段（绝大多数退款单会在发起那一次应答里就落到 succeeded 或 failed，只有应答含糊
-- （PROCESSING / UNKNOWN）或超时的那一小撮会停在这里）。
CREATE INDEX payment_refunds_processing_query_idx ON payment_refunds (updated_at)
    WHERE status = 'processing';

-- ============================================================
-- 平台样板：消息 outbox / inbox
-- ============================================================

-- 与 identity、merchant、coupon、coffee_machine、order、account、lottery、membership、partner
-- 各集里的同名两表逐列一致，各库自带一份，跨库不共享表。
--
-- 本服务的 outbox 不是可选件：支付结果、退款结果与代扣结果都要以领域事件发出去（方案 7.3 / 7.4），
-- 而审计记录的写法（platform/audit）就是「在业务事务内往自己的 outbox 追加一条
-- admin.operation.logged」。没有这张表，后台的人工退款与关单就没有留痕的地方。

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
--
-- 后台的支付/退款列表与详情直接读这些注释当列名口径，所以状态与词表要写全，
-- 与 order 集的写法一致。放在同一个文件里而不是单开一份 _comments：合并之后每个集只有
-- 这一个文件，注释和它描述的列不会再出现「改了列没改注释」这种两个文件各说各话的情况。
-- ============================================================

COMMENT ON TABLE payments IS '支付单：一次向用户收钱的尝试；一个订单可以有多个支付单（失败后可再发起），但只能有一张成功';
COMMENT ON COLUMN payments.payment_no IS '支付单号，业务唯一；写入 order 库 orders.payment_no 时仅作跨库值引用';
COMMENT ON COLUMN payments.legacy_id IS '老库支付单 ID（ObjectID 十六进制），仅迁移过来的历史数据有值';
COMMENT ON COLUMN payments.order_no IS '订单号，属于订单服务时仅作跨库值引用';
COMMENT ON COLUMN payments.user_id IS '付款用户 ID，属于身份服务时仅作跨库值引用';
COMMENT ON COLUMN payments.amount IS '应付总额，单位为分；等于各笔出资金额之和';
COMMENT ON COLUMN payments.status IS '支付单状态：created=已建单，pending=已发起待结果，succeeded=已成功，failed=已失败，closed=已关单，expired=待支付超时';
COMMENT ON COLUMN payments.subject IS '渠道收银台与账单上显示的商品描述';
COMMENT ON COLUMN payments.attach IS '渠道附加数据（小程序 openid、设备号等）；不放密钥与签名原文';
COMMENT ON COLUMN payments.provider_transaction_id IS '渠道方流水号，非空时唯一';
COMMENT ON COLUMN payments.failure_code IS '失败码';
COMMENT ON COLUMN payments.failure_message IS '失败原因（渠道返回的文案）';
COMMENT ON COLUMN payments.request_id IS '发起支付请求的幂等号，非空时唯一';
COMMENT ON COLUMN payments.expires_at IS '待支付超时时间，超时关单扫描依据';
COMMENT ON COLUMN payments.paid_at IS '渠道确认支付成功的时间';
COMMENT ON COLUMN payments.closed_at IS '主动关单时间';
COMMENT ON COLUMN payments.account_entry_id IS '账户出资扣豆的账变 ID（coffee_bean_entries.id），扣豆返回时即落库、先于结算；非空表示这笔钱确实动过，超时关单不会碰它，由补偿任务结算';
COMMENT ON COLUMN payments.account_funded_at IS '账户出资的成交时刻（账户域那笔账变的发生时刻）；渠道支付没有这一列的值，它由渠道给的 paid_at 描述';
COMMENT ON COLUMN payments.provider IS '走哪条渠道，值是渠道**在代码里**的名字（payment-service 的 catalog.Channel.Provider，今天只有 ums）。账户出资（纯咖啡豆）是空串——那一列可空的语义，用空串表达。这条渠道的事实与回调 URL 里那一段是同名的，回调据此与支付单对一遍（见 repository.notificationChannelMatches）。⚠️ 存量行上这一列可能是**协议族**（manual / form_md5 / hmac_body / ums / wechat_v3），不是渠道名——它从前抄自渠道配置行，而那一列装的就是协议族。今天有数据的库上它只可能是 ums——能收到回调的渠道只有 catalog 里那一条，而老库里属于别的族的支付单，回调今天连渠道都解析不到（/v1/payments/callback/<那一段> 不在 catalog 里）。留着原族名只是给排查留一条线索，不是一条能用的路由。';
COMMENT ON COLUMN payments.payment_method IS '用户是在哪一档支付方式下发起的，值是 catalog 里的 code（如 ums_miniapp_wechat / coffee_bean），不是某个 UUID：本库不建支付方式表（见文件头）。展示与统计用它；发起那次已经是按 code 分派的，不回头读这一列。';

COMMENT ON TABLE payment_fundings IS '支付出资分摊：一个支付单下逐笔出资来源与金额；与订单库的 order_payment_lines 是同一事实的资金侧视角';
COMMENT ON COLUMN payment_fundings.payment_id IS '支付单 ID';
COMMENT ON COLUMN payment_fundings.line_no IS '支付单内出资序号';
COMMENT ON COLUMN payment_fundings.line_type IS '这笔出资的支付方式 code（catalog 里的 ums_h5_alipay / coffee_bean 等），与 payments.payment_method 同一个值；本库没有出资渠道词表这一档';
COMMENT ON COLUMN payment_fundings.amount IS '该笔出资金额，单位为分';
COMMENT ON COLUMN payment_fundings.status IS '出资状态，与 order_payment_lines.status 逐字一致：reserved=已预占，succeeded=已成功，failed=已失败，released=已释放，reversed=已冲正';
COMMENT ON COLUMN payment_fundings.provider_transaction_id IS '渠道方流水号；账户出资为空';
COMMENT ON COLUMN payment_fundings.account_entry_id IS '账户出资对应的账变 ID，属于账户服务时仅作跨库值引用';

COMMENT ON TABLE payment_refunds IS '退款单：把某笔支付里的钱按售后结论退回去；售后申请本身归订单服务';
COMMENT ON COLUMN payment_refunds.refund_no IS '退款单号，业务唯一；写入 order 库 order_after_sales.refund_no 时仅作跨库值引用';
COMMENT ON COLUMN payment_refunds.legacy_id IS '老库退款单 ID（ObjectID 十六进制），仅迁移过来的历史退款有值';
COMMENT ON COLUMN payment_refunds.payment_id IS '退的是哪张支付单';
COMMENT ON COLUMN payment_refunds.payment_no IS '支付单号快照，供对账直接检索';
COMMENT ON COLUMN payment_refunds.order_no IS '订单号，属于订单服务时仅作跨库值引用';
COMMENT ON COLUMN payment_refunds.after_sale_no IS '售后单号（order_after_sales.after_sale_no），仅作跨库值引用；一张售后单只对应一张退款单，故唯一';
COMMENT ON COLUMN payment_refunds.order_line_id IS '退的是哪一行（order_lines.id），仅作跨库值引用；整单退为空；不存 scope，退款范围以订单侧的售后单为准';
COMMENT ON COLUMN payment_refunds.amount IS '退款金额，单位为分';
COMMENT ON COLUMN payment_refunds.status IS '退款单状态：pending=待处理，processing=退款中，succeeded=已退款，failed=退款失败，cancelled=已取消';
COMMENT ON COLUMN payment_refunds.provider_refund_id IS '渠道方退款流水号，非空时唯一';
COMMENT ON COLUMN payment_refunds.failure_code IS '退款失败码';

COMMENT ON TABLE payment_refund_fundings IS '退款出资冲正：一次退款按原出资来源逐笔退回或冲正';
COMMENT ON COLUMN payment_refund_fundings.refund_id IS '退款单 ID';
COMMENT ON COLUMN payment_refund_fundings.funding_id IS '冲正的原出资笔；存量迁移的退款可能为空';
COMMENT ON COLUMN payment_refund_fundings.line_no IS '退款单内出资序号';
COMMENT ON COLUMN payment_refund_fundings.line_type IS '被冲正的那笔出资的支付方式 code，与 payments.payment_method 同一个值；本库没有出资渠道词表这一档';
COMMENT ON COLUMN payment_refund_fundings.status IS '冲正状态：pending=待处理，succeeded=已成功，failed=已失败';
COMMENT ON COLUMN payment_refund_fundings.account_entry_id IS '账户出资冲正产生的反向账变 ID，属于账户服务时仅作跨库值引用';

COMMENT ON TABLE payment_notifications IS '渠道回调原始记录：验签与防重放的依据，回调本身是证据';
COMMENT ON COLUMN payment_notifications.provider IS '渠道适配器标识';
COMMENT ON COLUMN payment_notifications.notification_id IS '渠道侧通知或流水唯一号，(provider, notification_id) 唯一，重投与重放撞在这一条上';
COMMENT ON COLUMN payment_notifications.event_type IS '渠道事件类型（如交易成功、退款成功）';
COMMENT ON COLUMN payment_notifications.payment_no IS '回调指向的支付单号';
COMMENT ON COLUMN payment_notifications.refund_no IS '回调指向的退款单号';
COMMENT ON COLUMN payment_notifications.body IS '渠道原始报文；只允许落在本表，不得写入日志（方案 11.5）';
COMMENT ON COLUMN payment_notifications.body_sha256 IS '原始报文摘要，日志与排查时只能引用它';
COMMENT ON COLUMN payment_notifications.headers IS '脱敏后的关键请求头（请求号、时间戳、签名算法名），不含签名原文';
COMMENT ON COLUMN payment_notifications.signature_verified IS '验签是否通过；未通过的回调只留档，绝不改支付状态';
COMMENT ON COLUMN payment_notifications.status IS '处理状态：received=已接收待处理，processed=已处理，ignored=已忽略（不认识的事件），failed=处理或验签失败';

COMMENT ON TABLE payment_provider_calls IS '渠道调用流水：每一次主动调用渠道的请求与响应摘要（已脱敏）';
COMMENT ON COLUMN payment_provider_calls.operation IS '渠道动作：create/query/close=支付单，refund/query_refund=退款，agreement_*=委托代扣，reconcile=对账，division/query_division/finish_division/reverse_division=分账（发起/查/完结/回退）';
COMMENT ON COLUMN payment_provider_calls.attempt_no IS '第几次尝试，重试会递增';
COMMENT ON COLUMN payment_provider_calls.request_summary IS '脱敏后的请求摘要；不放签名原文、密钥、完整卡号与身份证（方案 11.5）';
COMMENT ON COLUMN payment_provider_calls.response_summary IS '脱敏后的响应摘要';
COMMENT ON COLUMN payment_provider_calls.result IS '调用结果：success=成功，failed=失败，timeout=超时，unknown=结果未知（必须人工或对账确认）';

COMMENT ON TABLE payment_transactions IS '支付与退款流水：只追加，不改不删；对账以此表为基准';
COMMENT ON COLUMN payment_transactions.kind IS '流水类型：payment=一笔出资成功入账，refund=一笔出资被退回，reversal=冲正（新增一条反向记录，不改原记录）';
COMMENT ON COLUMN payment_transactions.payment_no IS '支付单号；没有则为空串';
COMMENT ON COLUMN payment_transactions.refund_no IS '退款单号；没有则为空串';
COMMENT ON COLUMN payment_transactions.funding_line_no IS '出资序号，对应出资表或退款出资表的 line_no；没有具体行时为 0';
COMMENT ON COLUMN payment_transactions.line_type IS '这条流水对应的支付方式 code，与 payments.payment_method 同一个值；本库没有出资渠道词表这一档';
COMMENT ON COLUMN payment_transactions.direction IS '资金方向：in=入账，out=出账';
COMMENT ON COLUMN payment_transactions.amount IS '流水金额，单位为分，恒为正数；方向看 direction';
COMMENT ON COLUMN payment_transactions.provider_transaction_id IS '渠道方流水号';
COMMENT ON COLUMN payment_transactions.account_entry_id IS '账户出资对应的账变 ID，属于账户服务时仅作跨库值引用';
COMMENT ON COLUMN payment_transactions.occurred_at IS '渠道给出的成交时间，不是记账时间';
COMMENT ON COLUMN payment_transactions.provider IS '这一行流水属于哪条渠道（渠道名，与 payments.provider 同词表）。本库不建渠道表（渠道写在 payment-service 的 internal/catalog），所以它只有名字，没有可指的外键。';

COMMENT ON TABLE payment_state_transitions IS '支付领域状态变更审计记录，只追加';
COMMENT ON COLUMN payment_state_transitions.aggregate_type IS '聚合类型：payment=支付单，funding=出资，refund=退款单，agreement=代扣签约，charge=代扣扣款，reconciliation=对账批次';
COMMENT ON COLUMN payment_state_transitions.actor_type IS '操作者类型：user=用户，merchant=商户，admin=后台，system=系统（回调与任务）';

COMMENT ON TABLE payment_idempotency_keys IS '支付操作幂等请求记录';
COMMENT ON COLUMN payment_idempotency_keys.scope IS '幂等作用域，例如 create_payment、create_refund、agreement_charge';
COMMENT ON COLUMN payment_idempotency_keys.status IS '处理状态：processing=处理中，succeeded=成功，failed=失败';

COMMENT ON TABLE payment_agreements IS '委托代扣签约：用户授权按期扣款，什么时候该扣由业务方给出';
COMMENT ON COLUMN payment_agreements.agreement_no IS '签约单号，业务唯一';
COMMENT ON COLUMN payment_agreements.contract_no IS '渠道侧协议号或签约号，解约与查签约状态靠它';
COMMENT ON COLUMN payment_agreements.subject IS '签约用途，如「会员自动续费」';
COMMENT ON COLUMN payment_agreements.plan_code IS '业务方给的签约计划标识（如会员套餐代码），本库不解释其含义';
COMMENT ON COLUMN payment_agreements.max_charge_amount IS '单次扣款上限，单位为分；0 表示签约未约定上限';
COMMENT ON COLUMN payment_agreements.next_charge_at IS '下一次该扣款的时间，由业务方更新；代扣扫描按它走';
COMMENT ON COLUMN payment_agreements.status IS '签约状态：pending=待用户确认，signed=已确认，active=生效中，suspended=暂停扣款，terminated=已解约，expired=已到期';
COMMENT ON COLUMN payment_agreements.provider IS '走哪条渠道（渠道名）。代扣只能走渠道（微信委托代扣、银联无感支付），所以它非空。';
COMMENT ON COLUMN payment_agreements.payment_method IS '签约时用户选的支付方式 code（catalog 里的常量），不是某个 UUID；渠道侧的签约可以没有。';

COMMENT ON TABLE payment_agreement_charges IS '代扣扣款：一个签约一个期次只扣一次，重试加 attempt_count 而不新开行';
COMMENT ON COLUMN payment_agreement_charges.biz_period IS '期次，由业务方给出（如 2026-10），本库不推算不校验格式';
COMMENT ON COLUMN payment_agreement_charges.payment_no IS '扣款落到 payments 时对应的支付单号，值引用；代扣今天**不建 payments 行**（代扣没有订单），所以这一列恒为空串，一期的账看本行的 status / out_trade_no / charged_at';
COMMENT ON COLUMN payment_agreement_charges.status IS '扣款状态：pending=待扣，charging=扣款中，succeeded=已扣成功，failed=扣款失败，skipped=本期跳过，cancelled=已取消';
COMMENT ON COLUMN payment_agreement_charges.out_trade_no IS '这一期扣款在渠道那边的商户单号（微信报文里的 out_trade_no），也是扣款结果通知回来时唯一的关联键；建行时生成一次，重试复用同一个值，绝不重算';
COMMENT ON COLUMN payment_agreement_charges.provider_transaction_id IS '这一期扣款在渠道那边的流水号（微信的 transaction_id），由扣款结果通知写进来；渠道同一商户号下全局唯一，所以「同一笔渠道流水挂在两期上」被唯一索引挡住——那意味着同一笔钱被记成了两期';

COMMENT ON TABLE reconciliation_batches IS '渠道对账批次：一个渠道一天一份账单一次对账';
COMMENT ON COLUMN reconciliation_batches.biz_date IS '对账业务日（渠道账单上的那一天），不是拉单的那一天';
COMMENT ON COLUMN reconciliation_batches.bill_type IS '账单类型：payment=支付账单，refund=退款账单，all=合并账单';
COMMENT ON COLUMN reconciliation_batches.source IS '账单来源：channel_file=渠道文件，channel_api=渠道接口，manual_upload=人工上传';
COMMENT ON COLUMN reconciliation_batches.difference_count IS '差异笔数，对账跑完要看的就是它';

COMMENT ON TABLE reconciliation_records IS '对账明细与差异：差异处理只追加结论，不改流水';
COMMENT ON COLUMN reconciliation_records.channel_trade_no IS '渠道侧流水号；(batch, 流水号, 本地类型) 唯一';
COMMENT ON COLUMN reconciliation_records.local_kind IS '本地对应的单据类型：payment=支付单，refund=退款单';
COMMENT ON COLUMN reconciliation_records.local_no IS '本地单号（支付单号或退款单号）';
COMMENT ON COLUMN reconciliation_records.result IS '对账结论：matched=一致，channel_only=渠道有本地无，local_only=本地有渠道无，amount_mismatch=金额不符，status_mismatch=状态不符';
COMMENT ON COLUMN reconciliation_records.handled IS '差异是否已处理；未处理的差异是资金对不上的部分';
COMMENT ON COLUMN reconciliation_records.handled_by IS '处理人 ID，属于身份服务时仅作跨库值引用';

COMMENT ON COLUMN settlement_accounts.provider IS '走哪条渠道（渠道名，与 payments.provider 同词表）。分账是渠道能力：没有渠道就没有可用的接收方，所以这一列非空。本库不建渠道表（渠道写在 payment-service 的 internal/catalog），所以它只有名字，没有可指的外键。';

COMMENT ON TABLE message_outbox IS '支付服务待发布消息事件';
COMMENT ON COLUMN message_outbox.event_id IS '事件唯一 ID';
COMMENT ON COLUMN message_outbox.payload IS '事件内容二进制数据';
COMMENT ON COLUMN message_outbox.published_at IS '成功发布时间';

COMMENT ON TABLE message_inbox IS '支付服务已接收消息事件及消费租约';
COMMENT ON COLUMN message_inbox.event_id IS '已接收事件唯一 ID，用于消费去重';
COMMENT ON COLUMN message_inbox.completed_at IS '成功完成消费时间';

COMMIT;
