-- payment/003：支付表和关键字段中文注释
--
-- 后台的支付/退款列表与详情直接读这些注释当列名口径，所以状态与词表要写全，
-- 与 order/002_order_comments.sql 的写法一致。

COMMENT ON TABLE payment_channels IS '支付渠道对接配置：一个渠道一套配置，沙箱与生产是两行；密钥不在本表';
COMMENT ON COLUMN payment_channels.legacy_id IS '老库支付方式 ID（ObjectID 十六进制），仅迁移过来的行有值';
COMMENT ON COLUMN payment_channels.code IS '渠道代码，如 wechat_miniapp、unionpay、fengxuan_wanlian、youlian，业务唯一';
COMMENT ON COLUMN payment_channels.provider IS '渠道适配器标识，取值是协议族（manual/form_md5/hmac_body/ums/wechat_v3）而不是渠道名：丰选万联、优联、首创饭卡、北方工业饭卡四家同为 form_md5，差别只在 config；族内加渠道是插一行，族外加才要改代码';
COMMENT ON COLUMN payment_channels.mode IS '运行模式：sandbox=沙箱，live=生产';
COMMENT ON COLUMN payment_channels.status IS '渠道状态：enabled=启用，disabled=停用，legacy_readonly=存量只读（不再发起新支付，但仍可退款与对账）';
COMMENT ON COLUMN payment_channels.config IS '渠道级非敏感对接参数（商户号、appid、回调地址、证书序列号等），不放任何密钥；方式级参数在 payment_methods.params，返回客户端时两层合并';
COMMENT ON COLUMN payment_channels.secret_ref IS '密钥在受控 Secret 管理系统里的键名或环境变量名，不是密钥本身；密钥由部署注入，不落库（方案 18.3）';

COMMENT ON TABLE payment_methods IS '支付方式：前台/设备上用户可选的支付方式；咖啡机服务的 device_payment_methods.payment_method_id 按值引用本表 id，某台设备可用哪几条在那边显式配置';
COMMENT ON COLUMN payment_methods.legacy_id IS '老库支付方式 ID（ObjectID 十六进制），仅迁移过来的行有值';
COMMENT ON COLUMN payment_methods.code IS '支付方式代码，如 points_xxx（积分）、meal_card_xxx（饭卡），业务唯一；给人看的标识，不做行为分派';
COMMENT ON COLUMN payment_methods.channel_id IS '对应的渠道配置；咖啡豆、福卡这类账户出资方式为空（由账户服务扣减）';
COMMENT ON COLUMN payment_methods.action IS '选中这条方式之后怎么起支付，客户端只认它：jump_miniapp=跳对方小程序，native_pay=小程序内 requestPayment，direct_pay=对接方直接扣款不跳转，qrcode=扫普通二维码，h5=跳 H5 收银台，account=走账户服务扣余额；同形态的新渠道插数据即可用，无需改客户端';
COMMENT ON COLUMN payment_methods.params IS '方式级启动参数（跳转 path、附加 query 等），不放密钥；与 payment_channels.config 合并后返回客户端';
COMMENT ON COLUMN payment_methods.funding_type IS '对应的出资类型，词表与 order_payment_lines.line_type 一致：wechat=微信，unionpay=银联，coffee_bean=咖啡豆，fortune_card=福卡，wallet=其他钱包，other=其他';
COMMENT ON COLUMN payment_methods.status IS '状态：enabled=启用，disabled=停用';

COMMENT ON TABLE payments IS '支付单：一次向用户收钱的尝试；一个订单可以有多个支付单（失败后可再发起），但只能有一张成功';
COMMENT ON COLUMN payments.payment_no IS '支付单号，业务唯一；写入 order 库 orders.payment_no 时仅作跨库值引用';
COMMENT ON COLUMN payments.legacy_id IS '老库支付单 ID（ObjectID 十六进制），仅迁移过来的历史数据有值';
COMMENT ON COLUMN payments.order_no IS '订单号，属于订单服务时仅作跨库值引用';
COMMENT ON COLUMN payments.user_id IS '付款用户 ID，属于身份服务时仅作跨库值引用';
COMMENT ON COLUMN payments.amount IS '应付总额，单位为分；等于各笔出资金额之和';
COMMENT ON COLUMN payments.funding_type IS '主出资类型，词表同 order_payment_lines.line_type；支付结果事件里的 paymentMethod 取它';
COMMENT ON COLUMN payments.channel_id IS '走哪套渠道配置；账户出资（纯咖啡豆/福卡）为空';
COMMENT ON COLUMN payments.payment_method_id IS '用户在哪个支付方式下发起，仅用于展示与统计；渠道侧支付可以为空';
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

COMMENT ON TABLE payment_fundings IS '支付出资分摊：一个支付单下逐笔出资来源与金额；与订单库的 order_payment_lines 是同一事实的资金侧视角';
COMMENT ON COLUMN payment_fundings.payment_id IS '支付单 ID';
COMMENT ON COLUMN payment_fundings.line_no IS '支付单内出资序号';
COMMENT ON COLUMN payment_fundings.line_type IS '出资来源，词表与 order_payment_lines.line_type 逐字一致（见 payment_methods.funding_type）';
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
COMMENT ON COLUMN payment_refund_fundings.line_type IS '出资来源，词表同 payment_fundings.line_type';
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
COMMENT ON COLUMN payment_provider_calls.operation IS '调用类型：create=下单，query=查单，close=关单，refund=退款，query_refund=查退款，agreement_sign=签约，agreement_charge=代扣，agreement_terminate=解约，reconcile=对账拉单';
COMMENT ON COLUMN payment_provider_calls.attempt_no IS '第几次尝试，重试会递增';
COMMENT ON COLUMN payment_provider_calls.request_summary IS '脱敏后的请求摘要；不放签名原文、密钥、完整卡号与身份证（方案 11.5）';
COMMENT ON COLUMN payment_provider_calls.response_summary IS '脱敏后的响应摘要';
COMMENT ON COLUMN payment_provider_calls.result IS '调用结果：success=成功，failed=失败，timeout=超时，unknown=结果未知（必须人工或对账确认）';

COMMENT ON TABLE payment_transactions IS '支付与退款流水：只追加，不改不删；对账以此表为基准';
COMMENT ON COLUMN payment_transactions.kind IS '流水类型：payment=一笔出资成功入账，refund=一笔出资被退回，reversal=冲正（新增一条反向记录，不改原记录）';
COMMENT ON COLUMN payment_transactions.payment_no IS '支付单号；没有则为空串';
COMMENT ON COLUMN payment_transactions.refund_no IS '退款单号；没有则为空串';
COMMENT ON COLUMN payment_transactions.funding_line_no IS '出资序号，对应出资表或退款出资表的 line_no；没有具体行时为 0';
COMMENT ON COLUMN payment_transactions.direction IS '资金方向：in=入账，out=出账';
COMMENT ON COLUMN payment_transactions.amount IS '流水金额，单位为分，恒为正数；方向看 direction';
COMMENT ON COLUMN payment_transactions.provider_transaction_id IS '渠道方流水号';
COMMENT ON COLUMN payment_transactions.account_entry_id IS '账户出资对应的账变 ID，属于账户服务时仅作跨库值引用';
COMMENT ON COLUMN payment_transactions.occurred_at IS '渠道给出的成交时间，不是记账时间';

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

COMMENT ON TABLE payment_agreement_charges IS '代扣扣款：一个签约一个期次只扣一次，重试加 attempt_count 而不新开行';
COMMENT ON COLUMN payment_agreement_charges.biz_period IS '期次，由业务方给出（如 2026-10），本库不推算不校验格式';
COMMENT ON COLUMN payment_agreement_charges.payment_no IS '扣款成功对应的支付单号，值引用';
COMMENT ON COLUMN payment_agreement_charges.status IS '扣款状态：pending=待扣，charging=扣款中，succeeded=已扣成功，failed=扣款失败，skipped=本期跳过，cancelled=已取消';

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

COMMENT ON TABLE message_outbox IS '支付服务待发布消息事件';
COMMENT ON COLUMN message_outbox.event_id IS '事件唯一 ID';
COMMENT ON COLUMN message_outbox.payload IS '事件内容二进制数据';
COMMENT ON COLUMN message_outbox.published_at IS '成功发布时间';

COMMENT ON TABLE message_inbox IS '支付服务已接收消息事件及消费租约';
COMMENT ON COLUMN message_inbox.event_id IS '已接收事件唯一 ID，用于消费去重';
COMMENT ON COLUMN message_inbox.completed_at IS '成功完成消费时间';
