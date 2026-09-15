-- payment/002：委托代扣与渠道对账
--
-- 这两块的业务规则（续费周期怎么定、账单口径按什么切）在方案里还没冻结，所以本文件只落
-- 「谁、什么时候、对哪一笔做了什么」这些**不会随规则改的事实**，不去约束规则本身：
-- 不写「必须每月扣一次」，也不写「差异必须在几天内处理完」。
--
-- 归属（方案 5.9）：微信委托代扣的签约、扣款、解约归 payment-service；渠道对账也归它。
-- 会员怎么续、什么时候该扣，由 membership-service 决定并传进来（plan_code、biz_period、amount 都是
-- 业务方给的值），本库不存会员套餐，也不自己算期次。

-- ============================================================
-- 委托代扣
-- ============================================================

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
    -- 代扣只能走渠道（微信委托代扣、银联无感支付），所以渠道不能为空。
    channel_id UUID NOT NULL REFERENCES payment_channels(id) ON DELETE RESTRICT,
    payment_method_id UUID REFERENCES payment_methods(id) ON DELETE RESTRICT,
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
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
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
    -- 扣款成功时对应的支付单号（payments.payment_no），值引用。
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
    UNIQUE (agreement_id, biz_period)
);

-- ============================================================
-- 渠道对账
-- ============================================================

-- 一次对账 = 拉某一个渠道某一天的账单、与本库流水比一遍。微信的支付账单与退款账单是两份文件，
-- 所以 bill_type 要参与唯一性：同一渠道同一天可以有一个 payment 批次和一个 refund 批次。
CREATE TABLE reconciliation_batches (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    batch_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(batch_no)) > 0),
    channel_id UUID NOT NULL REFERENCES payment_channels(id) ON DELETE RESTRICT,
    provider TEXT NOT NULL DEFAULT '',
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
    UNIQUE (channel_id, biz_date, bill_type)
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

-- ============================================================
-- 索引
-- ============================================================

CREATE UNIQUE INDEX payment_agreements_legacy_id_key
    ON payment_agreements (legacy_id) WHERE legacy_id IS NOT NULL;

-- 用户看自己的签约、按状态筛。
CREATE INDEX payment_agreements_user_idx ON payment_agreements (user_id, status);
-- 扫描该扣款的人：生效中的签约，按 next_charge_at 到点。
CREATE INDEX payment_agreements_pending_charge_idx
    ON payment_agreements (status, next_charge_at)
    WHERE status = 'active';
CREATE INDEX payment_agreement_charges_status_idx ON payment_agreement_charges (status, next_retry_at);
CREATE INDEX payment_agreement_charges_payment_idx ON payment_agreement_charges (payment_no)
    WHERE payment_no <> '';

-- 对账批次的排队扫描与按渠道回看历史。
CREATE INDEX reconciliation_batches_status_idx ON reconciliation_batches (status, biz_date);
CREATE INDEX reconciliation_batches_channel_idx ON reconciliation_batches (channel_id, biz_date DESC);
-- 待处理差异：对账跑完只是开始，没处理完的差异才是要盯的。
CREATE INDEX reconciliation_records_open_idx ON reconciliation_records (batch_id, result)
    WHERE NOT handled;
CREATE INDEX reconciliation_records_local_idx ON reconciliation_records (local_kind, local_no)
    WHERE local_no <> '';
