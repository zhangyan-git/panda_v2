-- membership/001：会员领域核心表结构
--
-- 本迁移属于 panda_membership。用户、订单、支付单、代扣协议、饮品价格等外部服务的 ID
-- 只作为值引用保存；本库不得创建跨数据库外键。
--
-- 边界（方案 5.4）：Membership 只拥有会员数据。三处容易越界的地方先说清楚：
--
--   * 「会员订单」不在本库。买会员是 order-service 里的一个订单行
--     （order_lines.line_type = 'membership'），订单事实、金额、支付与退款状态全在那边。
--     本库只在流水里存一个 order_id，用作「这次权益变更由哪一单触发」的线索。
--   * 会员价本身也不在本库。饮品上有原价/会员价/提货码价，价格归 coffee-machine-service；
--     本库只回答「这个用户此刻算不算会员价」。
--   * 微信委托代扣签约归 payment-service（payment_agreements）。本库存 agreement_id 与
--     商户协议号，只当值用：不解释含义、不校验、不复制协议状态。套餐上的 wechat_plan_id
--     是签约模板（微信商户平台的 plan_id），签约时原样透传，本库同样不解释它。
--
-- 范围只有一件事：**会员价权益**，而且它有两条来路（与老系统一致）：
--
--   auto   年度会员：会员本人自动享会员价。下单时看 memberships 在不在有效期内就够了。
--   coupon 连续包月（9.9）：会员本人**不**自动享会员价，靠会员价体验券。每期生效时由本
--          服务请 coupon-service 发一批券，用户下单时选中一张，核销才享会员价。
--
-- 所以本库没有「会员价次数」这种东西：次数就是手上还没花的券，券的张数、有效期与核销
-- 全归 coupon-service（user_coupons）。原型那句「本月会员价剩余 20 次」说的正是券的余额
-- ——老系统就是续费成功后一次性发 20 张（panda_serve subscription_service.go:1242），
-- 这里不重复记一份账。发行模板挂在套餐上（见 member_price_coupon_template_id）。
--
-- 老系统的 VIP 等级体系与线下会员卡批次、激活都不在 V2 范围内，这里不为它们建表。
-- 店铺码会员活动（StoreMembershipCampaign）只做了一半：活动配置与发放记录的表在 006，
-- 扫码领取那一半写在小程序端，尚未接入。
--
-- 四张表的分工：
--
--   membership_plans         卖了什么：套餐定义（价格、时长、是否自动续费、会员价从哪来）
--   memberships              谁现在是会员：一个用户一条，带有效期与成交快照
--   membership_subscriptions 连续包月怎么续：签约、扣款期次、失败计数、解约
--   membership_changes       发生过什么：不可变的变更流水
--
-- 本文件不带 goose 的 Down 段：platform/database/migrate 把整个文件丢给一次 Exec、
-- 不识别 goose 指令，带上就会在同一个事务里建完表再删掉，而且不报错。

-- ============================================================
-- 套餐
-- ============================================================

CREATE TABLE membership_plans (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- 稳定的业务标识（原型：monthly_auto / annual）。下单、签约、代扣都靠它对接，
    -- 与 coupon_types.code 同样不可修改：改编码等于换了一个产品，历史订单会对不上。
    code TEXT NOT NULL UNIQUE CHECK (char_length(trim(code)) > 0),
    name TEXT NOT NULL CHECK (char_length(trim(name)) > 0),
    description TEXT NOT NULL DEFAULT '',
    -- 权益文案列表，只用于展示（原型：「每月 20 次咖啡享会员价」）。真正的权益判定看
    -- period、period_count、auto_renew 和 member_price_mode，不解析这段文案。
    benefits JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- 单位是「分」，与 coupon/005 之后的仓库约定一致。
    price_cents BIGINT NOT NULL CHECK (price_cents >= 0),
    -- 每期时长：period_count 个 period（连续包月 = month/1，年度会员 = year/1）。
    -- 存「月/年 + 个数」而不是天数，因为续期是日历加法：1 月 31 日开通的包月，下一期是
    -- 2 月 28 日，按天数加 30 会漂成 3 月 2 日，续几年就漂几天。
    period TEXT NOT NULL CHECK (period IN ('month', 'year')),
    period_count INTEGER NOT NULL DEFAULT 1 CHECK (period_count > 0),
    -- 是否签署微信委托代扣、到期自动续费。连续包月为 TRUE，年度会员为 FALSE。
    auto_renew BOOLEAN NOT NULL DEFAULT FALSE,
    -- 微信支付委托代扣的签约模板 ID（微信商户平台的 plan_id，后台表单里那串 214488）。
    -- 签约时原样透传给 payment-service 换协议号，本库不解释它的含义。年度会员不签约，留空。
    wechat_plan_id TEXT NOT NULL DEFAULT '',
    -- 会员价怎么来：auto=会员自动享（年度会员），coupon=靠会员价体验券（连续包月）。
    -- 这一列是下单时 order-service 走哪个判定分支的依据，不能靠「是不是订阅制」去推。
    member_price_mode TEXT NOT NULL DEFAULT 'auto'
        CHECK (member_price_mode IN ('auto', 'coupon')),
    -- coupon 模式发券用的券模板 ID，仅作跨库值引用（coupon-service 的 coupon_templates）。
    -- 券张数、面额、适用范围、有效期全在模板上，本库不复制。
    member_price_coupon_template_id UUID,
    -- coupon 模式每期发几张。老系统是写死的常量 20（subscription_service.go:1242），
    -- 这里做成套餐可配：改张数不该要发一版服务。
    member_price_coupons_per_period INTEGER
        CHECK (member_price_coupons_per_period IS NULL OR member_price_coupons_per_period > 0),
    sort_order INTEGER NOT NULL DEFAULT 0,
    -- 只有 active 的套餐能被购买。draft 是还没配完，disabled 是下架：已购会员不受影响，
    -- 因为 memberships 存了成交快照。
    status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'active', 'disabled')),
    -- 老库会员等级迁移映射来的套餐才有的值，新套餐为空。
    legacy_id TEXT,
    created_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- coupon 模式必须配齐模板与张数，否则「该发券」会静默变成「什么都没发」，用户
    -- 开完包月发现会员价用不了；auto 模式留着这两列会让人以为它还有额外赠券。
    CHECK ((member_price_mode = 'coupon'
            AND member_price_coupon_template_id IS NOT NULL
            AND member_price_coupons_per_period IS NOT NULL)
        OR (member_price_mode = 'auto'
            AND member_price_coupon_template_id IS NULL
            AND member_price_coupons_per_period IS NULL)),
    -- 要签约就必须有模板 ID：没有它，payment-service 拿不到 plan_id，协议建不出来，
    -- 「开启了连续包月」这句话就是假的。老后台表单把它标成必填，这里再钉一层。
    CHECK (NOT auto_renew OR char_length(trim(wechat_plan_id)) > 0)
);

-- 类型编码是稳定的业务标识，只允许新增套餐或修改展示字段，不允许改编码。
-- 与 coupon_types 同一套写法：物理阻断，不靠服务层自觉。
CREATE FUNCTION prevent_membership_plan_code_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.code IS DISTINCT FROM OLD.code THEN
        RAISE EXCEPTION 'membership plan code is immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER membership_plans_code_immutable
    BEFORE UPDATE ON membership_plans
    FOR EACH ROW EXECUTE FUNCTION prevent_membership_plan_code_change();

-- ============================================================
-- 会员资格
-- ============================================================

-- 一个用户一条：V2 的会员权益是全平台的，不分门店、不分点位——会员价在哪家店用都一样，
-- 老系统那条 UserMembership「按门店发会员」的做法不做了。但老系统记在它上面的 StoreID 有
-- 一半被留了下来：**归属门店**，即「这个人是谁拉来的」。它不是权益，只是一条留痕，在 004
-- 里补成 store_id 一列（不参与权益判定、不影响核销、不参与分账）。
--
-- 续费不改行，是把 expire_at 往后加：原型就是「在当前剩余天数上增加 N 天」。历史期次
-- 靠 membership_changes 的 from_expire_at / to_expire_at 还原，不另建期次表。
CREATE TABLE memberships (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    -- 老库 user_memberships._id。只对迁移过来的会员有值：它存在的唯一目的是让
    -- 「这条 V2 会员对应老库哪一条」可查。老库按 VIP 等级存了多条，映射到本表一个用户
    -- 只保留有效期最长的那条。
    legacy_id TEXT,
    plan_id UUID NOT NULL REFERENCES membership_plans(id) ON DELETE RESTRICT,
    -- 成交当时的套餐快照。套餐改名、改价、下架、改权益都不能改变已经卖出去的会员：
    -- 只靠 plan_id 现查套餐，等于让一次后台编辑改写所有历史会员的权益。
    -- 与订单行里的 membership_plan_snapshot 是同一条理由，那份副本在订单库，这份在本库。
    plan_code TEXT NOT NULL CHECK (char_length(trim(plan_code)) > 0),
    plan_name TEXT NOT NULL CHECK (char_length(trim(plan_name)) > 0),
    -- 会员价路径与发券配置的成交快照，取值同套餐。套餐改了模式、换了券模板或改了张数，
    -- 都不能追溯改变已经买了这个会员的人下一期收到什么——发券按这条记录发，不按套餐现查。
    member_price_mode TEXT NOT NULL DEFAULT 'auto'
        CHECK (member_price_mode IN ('auto', 'coupon')),
    member_price_coupon_template_id UUID,
    member_price_coupons_per_period INTEGER
        CHECK (member_price_coupons_per_period IS NULL OR member_price_coupons_per_period > 0),
    -- active=权益可用；frozen=权益暂停（争议/风控，到期时间不动，解冻后继续）；
    -- expired=已过期；revoked=已撤销，不再恢复。
    status TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'frozen', 'expired', 'revoked')),
    start_at TIMESTAMPTZ NOT NULL,
    expire_at TIMESTAMPTZ NOT NULL,
    -- 用户可关的自动续费开关。关掉只影响下一期，本期权益到 expire_at 为止。
    auto_renew BOOLEAN NOT NULL DEFAULT FALSE,
    -- 用户最后一次关掉自动续费的时间，留个痕；从没开过则为空。
    auto_renew_off_at TIMESTAMPTZ,
    -- 成功续费次数与最后一次续费时间。续费本身不新建行，所以累计值要落在这里。
    renewal_count INTEGER NOT NULL DEFAULT 0 CHECK (renewal_count >= 0),
    last_renewed_at TIMESTAMPTZ,
    frozen_at TIMESTAMPTZ,
    freeze_reason TEXT NOT NULL DEFAULT '',
    revoked_at TIMESTAMPTZ,
    revoke_reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (expire_at > start_at),
    CHECK (status <> 'frozen' OR frozen_at IS NOT NULL),
    CHECK (status <> 'revoked' OR revoked_at IS NOT NULL),
    CHECK ((member_price_mode = 'coupon'
            AND member_price_coupon_template_id IS NOT NULL
            AND member_price_coupons_per_period IS NOT NULL)
        OR (member_price_mode = 'auto'
            AND member_price_coupon_template_id IS NULL
            AND member_price_coupons_per_period IS NULL))
);

-- 一个用户只能有一条会员记录。两条并存意味着「以哪条算会员价资格」没有答案，
-- 而这正是老库按等级存多条的代价。
CREATE UNIQUE INDEX memberships_user_unique
    ON memberships (user_id);

-- ============================================================
-- 自动续费订阅
-- ============================================================

-- 连续包月的签约与扣款期次。签约协议本体在 payment-service，本表只记
-- 「这一期什么时候该扣、扣了没、失败几次、什么时候解约」。
-- 方案 7.3 的注释反过来也成立：扣多少、什么时候扣由本服务决定并传给 payment。
CREATE TABLE membership_subscriptions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    membership_id UUID NOT NULL REFERENCES memberships(id) ON DELETE RESTRICT,
    user_id UUID NOT NULL,
    plan_id UUID NOT NULL REFERENCES membership_plans(id) ON DELETE RESTRICT,
    -- pending_sign=已下单、等签约结果；active=签约成功、按期扣款中；
    -- suspended=连续扣款失败已暂停，等待下一轮重试；cancelled=用户解约；
    -- expired=会员到期且不再续（正常走完的终点）。
    status TEXT NOT NULL DEFAULT 'pending_sign'
        CHECK (status IN ('pending_sign', 'active', 'suspended', 'cancelled', 'expired')),
    -- payment-service 的 payment_agreements.id，仅作值引用。
    agreement_id UUID,
    -- 商户协议号（签约时由 payment 返回并写回）。对账时要拿它去渠道查，所以本库留一份副本。
    contract_code TEXT NOT NULL DEFAULT '',
    -- 签约时的每期扣款金额快照。套餐调价不影响已签约用户：代扣金额是签约时约定死的。
    price_cents BIGINT NOT NULL CHECK (price_cents >= 0),
    -- 签约时的签约模板 ID 快照，取值同套餐 wechat_plan_id。签约后套餐换了模板，
    -- 已签的协议仍然挂在旧模板上，得拿旧的那个去查协议、去扣款。
    wechat_plan_id TEXT NOT NULL DEFAULT '',
    -- 每期叠加的时长（连续包月 = month/1）。与套餐同义，同样存「月/年 + 个数」，
    -- 续费按日历加，不按天数加。
    period TEXT NOT NULL CHECK (period IN ('month', 'year')),
    period_count INTEGER NOT NULL DEFAULT 1 CHECK (period_count > 0),
    -- 下一次应扣款时间。续费扫描按它取任务，所以 active 必须有值（见下面的 CHECK）。
    next_charge_at TIMESTAMPTZ,
    last_charge_at TIMESTAMPTZ,
    charge_count INTEGER NOT NULL DEFAULT 0 CHECK (charge_count >= 0),
    -- 累计失败次数用于统计，连续失败次数用于触发暂停：连续失败到阈值就转 suspended，
    -- 一次成功两者都清零。
    failed_count INTEGER NOT NULL DEFAULT 0 CHECK (failed_count >= 0),
    consecutive_failed_count INTEGER NOT NULL DEFAULT 0 CHECK (consecutive_failed_count >= 0),
    suspended_at TIMESTAMPTZ,
    cancel_at TIMESTAMPTZ,
    cancel_reason TEXT NOT NULL DEFAULT '',
    -- 解约发起人：用户自己在小程序点的，或者是后台运营/风控操作。
    cancelled_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (status <> 'suspended' OR suspended_at IS NOT NULL),
    CHECK (status <> 'cancelled' OR cancel_at IS NOT NULL),
    -- active 却没有下次扣款时间的订阅，永远不会被扫描到，也不会有人发现。
    CHECK (status <> 'active' OR next_charge_at IS NOT NULL)
);

-- 一个会员同时只能有一条活着的订阅。pending_sign 也算活着：否则用户连点两次「开通连续
-- 包月」就会签出两份协议，扣两次钱。
CREATE UNIQUE INDEX membership_subscriptions_live_unique
    ON membership_subscriptions (membership_id)
    WHERE status IN ('pending_sign', 'active', 'suspended');

-- 一份代扣协议只对应一条订阅。重复绑定意味着钱可能扣两遍。
CREATE UNIQUE INDEX membership_subscriptions_agreement_unique
    ON membership_subscriptions (agreement_id)
    WHERE agreement_id IS NOT NULL;

-- ============================================================
-- 变更记录
-- ============================================================

-- 会员身上发生过什么：开通、续费、过期、冻结、解冻、开关自动续费、签约、解约、退款调整、
-- 后台调整、撤销。只增不改不删——与 stock_movements 同一套写法，物理阻断，不靠服务层自觉。
--
-- 这张表同时承担三个用途：小程序与后台的会员详情页时间线、退款/冻结这类人工操作的留痕、
-- 以及「这一单的会员权益到底改没改」的幂等凭据（见下面的唯一约束）。
CREATE TABLE membership_changes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    membership_id UUID NOT NULL REFERENCES memberships(id) ON DELETE RESTRICT,
    user_id UUID NOT NULL,
    change_type TEXT NOT NULL CHECK (change_type IN (
        'activate',        -- 开通（首购生效）
        'renew',           -- 续费（有效期叠加）
        'expire',          -- 到期失效
        'freeze',          -- 权益暂停
        'unfreeze',        -- 暂停恢复
        'auto_renew_on',   -- 用户打开自动续费
        'auto_renew_off',  -- 用户关闭自动续费
        'subscribe',       -- 签约连续包月
        'unsubscribe',     -- 解约
        'refund_adjust',   -- 退款后按规则调整权益
        'admin_adjust',    -- 后台人工调整
        'revoke'           -- 撤销会员
    )),
    from_status TEXT,
    to_status TEXT,
    from_expire_at TIMESTAMPTZ,
    to_expire_at TIMESTAMPTZ,
    plan_id UUID,
    order_id UUID,
    operator_type TEXT NOT NULL CHECK (operator_type IN ('user', 'admin', 'system', 'worker')),
    operator_id UUID,
    reason TEXT NOT NULL DEFAULT '',
    remark TEXT NOT NULL DEFAULT '',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- 操作留痕串：与后台操作审计（方案 11）对得上。
    request_id TEXT NOT NULL DEFAULT '',
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 同一个订单的同一种变更只记一次。支付成功回调重放、人工重试都不会把会员续两次、
-- 记两笔流水——这是一条不变式，不是服务层的自觉。
CREATE UNIQUE INDEX membership_changes_order_unique
    ON membership_changes (order_id, change_type)
    WHERE order_id IS NOT NULL;

CREATE FUNCTION prevent_membership_change_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'membership change ledger is append-only';
END;
$$;

CREATE TRIGGER membership_changes_append_only
    BEFORE UPDATE OR DELETE ON membership_changes
    FOR EACH ROW EXECUTE FUNCTION prevent_membership_change_change();

-- ============================================================
-- Outbox / Inbox
-- ============================================================

-- 每个库都要有自己的一对：outbox 与业务行同一个事务写入，所以不可能是共享表。
-- 列必须与其它库逐列一致（migrations 包的 TestMessageTablesStayInSyncAcrossSets 会比对）。

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

-- ============================================================
-- 索引
-- ============================================================

-- 后台套餐列表：按状态筛、按排序展示。
CREATE INDEX membership_plans_listing_idx
    ON membership_plans (status, sort_order);

-- 小程序进入会员中心、下单校验会员价资格：都是按 user_id 取那唯一一条，走 memberships_user_unique。

-- 到期扫描：把 status 还是 active 但已经过了 expire_at 的行标成 expired。
CREATE INDEX memberships_expiry_idx
    ON memberships (expire_at)
    WHERE status = 'active';

-- 自动续费扫描：到期前该扣款的那批。
CREATE INDEX memberships_renewal_idx
    ON memberships (expire_at)
    WHERE auto_renew AND status = 'active';

-- 后台会员列表：按状态 + 到期时间排。
CREATE INDEX memberships_status_expiry_idx
    ON memberships (status, expire_at);

CREATE INDEX membership_subscriptions_user_idx
    ON membership_subscriptions (user_id, created_at DESC);

-- 续费任务扫描：到点该扣的活跃订阅。
CREATE INDEX membership_subscriptions_charge_idx
    ON membership_subscriptions (next_charge_at)
    WHERE status = 'active';

-- 暂停后的重试扫描。
CREATE INDEX membership_subscriptions_suspended_idx
    ON membership_subscriptions (suspended_at)
    WHERE status = 'suspended';

-- 会员详情页时间线：按会员倒序取。
CREATE INDEX membership_changes_membership_idx
    ON membership_changes (membership_id, occurred_at DESC);

-- 后台按用户查变更。
CREATE INDEX membership_changes_user_idx
    ON membership_changes (user_id, occurred_at DESC);

-- 按变更类型统计（今天开了多少会员、解约多少）。
CREATE INDEX membership_changes_type_idx
    ON membership_changes (change_type, occurred_at DESC);

CREATE INDEX message_outbox_pending_idx
    ON message_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;

CREATE INDEX message_outbox_lease_idx
    ON message_outbox (lease_until)
    WHERE published_at IS NULL;
