-- coupon/002：优惠券表和关键字段中文注释

COMMENT ON TABLE coupon_types IS '优惠券业务类型字典';
COMMENT ON COLUMN coupon_types.code IS '稳定的优惠券类型编码，创建后不可修改';
COMMENT ON COLUMN coupon_types.status IS '类型状态：active=启用，disabled=停用';

COMMENT ON TABLE coupon_templates IS '优惠券模板及发行、领取、核销规则';
COMMENT ON COLUMN coupon_templates.coupon_type_id IS '优惠券类型 ID，引用本库 coupon_types';
COMMENT ON COLUMN coupon_templates.merchant_id IS '商户 ID，NULL 表示平台券；仅作跨库值引用';
COMMENT ON COLUMN coupon_templates.face_value IS '优惠券面值，单位为元';
COMMENT ON COLUMN coupon_templates.min_purchase_amount IS '使用优惠券所需的最低消费金额，单位为元';
COMMENT ON COLUMN coupon_templates.purchase_price IS '购买价格，单位为元；零表示免费领取';
COMMENT ON COLUMN coupon_templates.discount_rate IS '折扣比例，百分比数值，例如 80 表示八折';
COMMENT ON COLUMN coupon_templates.discount_cap IS '折扣金额上限，单位为元；NULL 表示无上限';
COMMENT ON COLUMN coupon_templates.total_quantity IS '模板总发行数量';
COMMENT ON COLUMN coupon_templates.issued_quantity IS '模板已发行数量；由事务内操作维护';
COMMENT ON COLUMN coupon_templates.reserved_quantity IS '模板已预留但尚未完成发行的数量';
COMMENT ON COLUMN coupon_templates.validity_mode IS '有效期模式：fixed=固定日期，relative=领取后相对天数';
COMMENT ON COLUMN coupon_templates.valid_from IS '固定有效期开始时间';
COMMENT ON COLUMN coupon_templates.valid_to IS '固定有效期结束时间';
COMMENT ON COLUMN coupon_templates.valid_days IS '相对有效期天数';
COMMENT ON COLUMN coupon_templates.claim_limit_mode IS '领取限制：once_ever=永久一次，unlimited_after_use=使用后可再领，periodic=周期限制';
COMMENT ON COLUMN coupon_templates.redemption_type IS '核销方式：platform=平台核销，external_code=外部券码，show_qr=展示二维码';
COMMENT ON COLUMN coupon_templates.audit_status IS '审核状态：pending=待审核，approved=已通过，rejected=已拒绝';
COMMENT ON COLUMN coupon_templates.status IS '模板状态：draft=草稿，active=启用，disabled=停用，closed=关闭';

COMMENT ON TABLE coupon_template_scopes IS '优惠券模板适用范围';
COMMENT ON COLUMN coupon_template_scopes.scope_type IS '范围类型：brand=品牌，store=门店，device=设备，category=分类';
COMMENT ON COLUMN coupon_template_scopes.scope_id IS '范围对象 ID，属于对应主数据服务时仅作跨库值引用';

COMMENT ON TABLE coupon_batches IS '优惠券发行批次及批次库存';
COMMENT ON COLUMN coupon_batches.batch_no IS '发行批次号，业务唯一';
COMMENT ON COLUMN coupon_batches.source IS '批次来源：platform=平台，admin=后台，merchant=商户，event=活动，purchase=购买';
COMMENT ON COLUMN coupon_batches.total_quantity IS '批次总数量';
COMMENT ON COLUMN coupon_batches.reserved_quantity IS '批次已预留数量';
COMMENT ON COLUMN coupon_batches.issued_quantity IS '批次已发行数量';
COMMENT ON COLUMN coupon_batches.released_quantity IS '批次已释放数量';
COMMENT ON COLUMN coupon_batches.order_id IS '关联订单 ID，属于订单服务时仅作跨库值引用';
COMMENT ON COLUMN coupon_batches.user_id IS '关联用户 ID，属于身份服务时仅作跨库值引用';
COMMENT ON COLUMN coupon_batches.request_id IS '创建批次或发券操作的幂等请求号';

COMMENT ON TABLE user_coupons IS '用户持有的优惠券实例及领取时规则快照';
COMMENT ON COLUMN user_coupons.template_id IS '来源优惠券模板 ID';
COMMENT ON COLUMN user_coupons.batch_id IS '来源发行批次 ID';
COMMENT ON COLUMN user_coupons.user_id IS '持券用户 ID，属于身份服务时仅作跨库值引用';
COMMENT ON COLUMN user_coupons.coupon_type_code IS '领取时的优惠券类型编码快照';
COMMENT ON COLUMN user_coupons.claim_type IS '发券来源：主动领取、后台发放、每日赠送、活动奖励、领取码或购买';
COMMENT ON COLUMN user_coupons.status IS '用户券状态：claimed=已领取，held=已预占，redeemed=已核销，expired=已过期，refunded=已退款，invalidated=已作废';
COMMENT ON COLUMN user_coupons.redemption_code_digest IS '核销码摘要，不保存明文核销码';
COMMENT ON COLUMN user_coupons.face_value IS '领取时面值快照，单位为元';
COMMENT ON COLUMN user_coupons.min_purchase_amount IS '领取时最低消费快照，单位为元';
COMMENT ON COLUMN user_coupons.discount_rate IS '领取时折扣比例快照';
COMMENT ON COLUMN user_coupons.discount_cap IS '领取时折扣上限快照，单位为元';
COMMENT ON COLUMN user_coupons.valid_from IS '用户券实际生效时间快照';
COMMENT ON COLUMN user_coupons.expired_at IS '用户券实际过期时间快照';

COMMENT ON TABLE user_coupon_scopes IS '用户券领取时的适用范围快照';
COMMENT ON COLUMN user_coupon_scopes.user_coupon_id IS '用户券 ID';
COMMENT ON COLUMN user_coupon_scopes.scope_type IS '范围类型：brand=品牌，store=门店，device=设备，category=分类';
COMMENT ON COLUMN user_coupon_scopes.scope_id IS '领取时适用范围对象 ID';

COMMENT ON TABLE coupon_redemptions IS '优惠券核销记录及核销金额快照';
COMMENT ON COLUMN coupon_redemptions.user_coupon_id IS '被核销的用户券 ID';
COMMENT ON COLUMN coupon_redemptions.request_id IS '核销请求幂等号';
COMMENT ON COLUMN coupon_redemptions.redemption_method IS '核销方式：platform=平台，employee=员工，qr=二维码，external_code=外部券码';
COMMENT ON COLUMN coupon_redemptions.store_id IS '核销门店 ID，属于商户服务时仅作跨库值引用';
COMMENT ON COLUMN coupon_redemptions.employee_id IS '核销员工 ID，属于身份服务时仅作跨库值引用';
COMMENT ON COLUMN coupon_redemptions.order_id IS '关联订单 ID，属于订单服务时仅作跨库值引用';
COMMENT ON COLUMN coupon_redemptions.amount_before IS '核销前订单金额，单位为元';
COMMENT ON COLUMN coupon_redemptions.discount_amount IS '本次优惠金额，单位为元';
COMMENT ON COLUMN coupon_redemptions.amount_after IS '核销后应付金额，单位为元';
COMMENT ON COLUMN coupon_redemptions.status IS '核销状态：succeeded=成功，rejected=拒绝，reversed=已反转';

COMMENT ON TABLE coupon_state_transitions IS '优惠券领域状态变更审计记录';
COMMENT ON COLUMN coupon_state_transitions.aggregate_type IS '聚合类型：template=模板，batch=批次，user_coupon=用户券，redemption=核销';
COMMENT ON COLUMN coupon_state_transitions.aggregate_id IS '发生状态变化的聚合 ID';
COMMENT ON COLUMN coupon_state_transitions.from_status IS '变更前状态';
COMMENT ON COLUMN coupon_state_transitions.to_status IS '变更后状态';
COMMENT ON COLUMN coupon_state_transitions.request_id IS '触发状态变化的请求幂等号';
COMMENT ON COLUMN coupon_state_transitions.actor_id IS '操作人 ID，属于身份服务时仅作跨库值引用';
COMMENT ON COLUMN coupon_state_transitions.metadata IS '状态变化附加信息';

COMMENT ON TABLE coupon_inventory_ledger IS '优惠券库存变动流水及对账记录';
COMMENT ON COLUMN coupon_inventory_ledger.template_id IS '优惠券模板 ID';
COMMENT ON COLUMN coupon_inventory_ledger.batch_id IS '发行批次 ID';
COMMENT ON COLUMN coupon_inventory_ledger.reference_type IS '关联业务类型，例如 claim、redeem、refund、adjust';
COMMENT ON COLUMN coupon_inventory_ledger.reference_id IS '关联业务记录 ID';
COMMENT ON COLUMN coupon_inventory_ledger.quantity IS '库存变动数量，正负号表示增加或减少';
COMMENT ON COLUMN coupon_inventory_ledger.operation IS '库存操作：reserve=预留，issue=发行，release=释放，expire=过期，adjust=调整';
COMMENT ON COLUMN coupon_inventory_ledger.request_id IS '库存变更请求幂等号';

COMMENT ON TABLE coupon_idempotency_keys IS '优惠券操作幂等请求记录';
COMMENT ON COLUMN coupon_idempotency_keys.scope IS '幂等作用域，例如 claim、redeem、issue';
COMMENT ON COLUMN coupon_idempotency_keys.idempotency_key IS '调用方提供的幂等键';
COMMENT ON COLUMN coupon_idempotency_keys.request_hash IS '请求内容摘要，用于检测同一幂等键复用不同请求';
COMMENT ON COLUMN coupon_idempotency_keys.resource_type IS '幂等请求创建或操作的资源类型';
COMMENT ON COLUMN coupon_idempotency_keys.resource_id IS '幂等请求关联的资源 ID';
COMMENT ON COLUMN coupon_idempotency_keys.response IS '首次处理结果快照';
COMMENT ON COLUMN coupon_idempotency_keys.status IS '处理状态：processing=处理中，succeeded=成功，failed=失败';

COMMENT ON TABLE message_outbox IS '优惠券服务待发布消息事件';
COMMENT ON COLUMN message_outbox.event_id IS '事件唯一 ID';
COMMENT ON COLUMN message_outbox.event_type IS '事件类型';
COMMENT ON COLUMN message_outbox.event_version IS '事件版本';
COMMENT ON COLUMN message_outbox.payload IS '事件内容二进制数据';
COMMENT ON COLUMN message_outbox.attempts IS '发布尝试次数';
COMMENT ON COLUMN message_outbox.published_at IS '成功发布时间';
COMMENT ON COLUMN message_outbox.lease_until IS '消息处理租约截止时间';

COMMENT ON TABLE message_inbox IS '优惠券服务已接收消息事件及消费租约';
COMMENT ON COLUMN message_inbox.event_id IS '已接收事件唯一 ID，用于消费去重';
COMMENT ON COLUMN message_inbox.claimed_at IS '首次领取消费时间';
COMMENT ON COLUMN message_inbox.lease_until IS '消息消费租约截止时间';
COMMENT ON COLUMN message_inbox.completed_at IS '成功完成消费时间';
