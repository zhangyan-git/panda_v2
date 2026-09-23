-- membership：会员领域表结构与中文注释
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
-- 店铺码会员活动（StoreMembershipCampaign）只做了一半：活动配置与发放记录的表在下面
-- 「店铺码会员活动」一节里，扫码领取那一半写在小程序端，尚未接入。
--
-- 七张表的分工：
--
--   membership_plans              卖了什么：套餐定义（价格、时长、是否自动续费、会员价从哪来）
--   memberships                   谁现在是会员：一个用户一条，带有效期与成交快照
--   membership_subscriptions      连续包月怎么续：签约、扣款期次、失败计数、解约
--   membership_changes            发生过什么：不可变的变更流水
--   membership_campaigns          店铺码活动：一场活动送什么、送多少
--   membership_campaign_claims    店铺码活动：谁领到了，一行即一次发放完成
--   membership_charge_settlements 扣款成功但续费单没建上时的待办，落完即删
--
-- 本文件不带 goose 的 Down 段：platform/database/migrate 把整个文件丢给一次 Exec、
-- 不识别 goose 指令，带上就会在同一个事务里建完表再删掉，而且不报错。

BEGIN;

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
    -- 单位是「分」，与 coupon 集把金额统一到分之后的仓库约定一致。
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
-- 一半被留了下来：**归属门店**，即「这个人是谁拉来的」。它不是权益，只是一条留痕，就是本表
-- 末尾那一列 store_id（不参与权益判定、不影响核销、不参与分账）。
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
    -- 归属门店。权益那一半仍照上面那段：会员价在哪家店用都一样，这一列不参与任何权益判定、
    -- 不影响核销、不影响下单，更不参与分账——它只是一条留痕，回答「这个人是谁拉来的」。
    --
    -- 留痕本身是老系统一直在记的（users.member_store_id），而「不做店铺码活动」这句话在
    -- V2 也不再成立（见下面「店铺码会员活动」一节）。所以这一列按老系统的口径补回来，并沿用
    -- 同一条固化规则：
    --
    --   第一次成为会员那一刻固化；之后续费、升级**不覆盖**；只有会员**已经过期**之后重新
    --   开通，归属门店才可以变更——上一段会员关系结束了，这一次是新的拉新。
    --
    -- 判据落在服务层，且挂在一个已经存在的分叉上（renewMembership 里决定 base 取 occurredAt
    -- 还是 current.ExpireAt 的那个判断），不在这里用 CASE 再写一遍。
    --
    -- 可空、无外键、无索引：stores 在商户库，跨库建不了外键（与全仓一致，见下面 Outbox /
    -- Inbox 那一节里被 migrations_test.go 钉着的那段说明）；今天也没有「按门店筛会员」的查询。
    store_id UUID,
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
    -- 订阅「从哪来」的三个来源 id。
    --
    -- 老后台的订阅列表有两列表头——「签约场景」与「首月支付」——而老库上**这两列一个字段都
    -- 没有**：它们都是查询时算出来的。
    --
    --   签约场景 = coffee_order_id 非零 → 咖啡订单；否则 campaign_claim_id 非零 → 店铺码活动；
    --              否则 → 会员中心
    --   首月支付 = 拿 out_trade_no 回 orders 查（且只对「会员中心」那一种场景填）
    --
    -- V2 照抄这个口径：**派生值不落库**，只把三个来源 id 记下来，场景在服务层现算。
    -- 给派生值建列的话，两个字段迟早会有一个和 id 对不上，而那时候没有任何判据能说哪个是对
    -- 的。同理，那三个 id 也**不建外键**：orders 在订单库，campaign_claim 在本库，
    -- 跨库建不了（与全仓一致，见 memberships.store_id 那段说明）；coffee_order 与普通订单
    -- 在 V2 都落 orders。
    --
    -- 可空、无索引：今天没有「按来源筛订阅」的查询，三列只用来算上面那两个表头。
    --
    -- ⚠️ **今天没有任何代码写这三列**：能写它们的只有小程序端的签约（用户从店铺码活动页进来、
    -- 或从一杯咖啡的订单页进来签约），而小程序端这一轮不写。所以后台订阅列表上「签约场景」恒为
    -- 「会员中心」、首月支付恒为「待同步」——**那是数据还没来，不是算错了**。
    order_id UUID,
    coffee_order_id UUID,
    campaign_claim_id UUID,
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
-- 后台调整、撤销、代扣失败、订阅暂停。只增不改不删——与 stock_movements 同一套写法，
-- 物理阻断，不靠服务层自觉。
--
-- 这张表同时承担三个用途：小程序与后台的会员详情页时间线、退款/冻结这类人工操作的留痕、
-- 以及「这一单的会员权益到底改没改」的幂等凭据（见下面的唯一约束）。
--
-- change_type 里有两条是代扣这一刀专用的，**代扣失败与订阅暂停**，别把它们与 freeze 混了：
--
--   charge_failed  一期代扣没扣到（这一期还没成，到期日不动）
--   suspend        连续失败到上限，停掉自动续费；协议还在，不是解约
--
-- 不补这两条的话，事件消费会走到 CHECK 上被拒——表现是「渠道说这期钱没扣到，本地什么都没
-- 发生、日志里只有一句约束冲突」，而用户那边看到的仍是「自动续费：已开启」。这两种变更又
-- 恰恰是唯一会无限重复发生的两条，放不进流水就只能靠翻日志排查。
--
-- 扣款成功写的是**已有的 `renew`**，没有 `charge_succeeded` 这个值。一次成功的代扣与一次
-- 支付成功在会员身上是同一件事（有效期后移一个周期），对用户时间线而言也是同一句话。给代扣
-- 单开一个类型，只会让「这个月是怎么续上的」在时间线上分成两种长得一样的条目，而分不分得清
-- 靠的是 change_type 之外的东西（metadata 里的协议号）——那才是该去看的地方。
--
-- suspend 不是 freeze：`freeze` 是**权益暂停**（会员卡被冻住了，用户还能做什么照旧另说），
-- 而这里是**代扣被停掉**——连续几期扣不到钱之后，本服务不再自动发起下一期，会员本身照常到期
-- 失效。两者的操作人、可恢复方式、用户该看到的提示都不一样，所以不能共用一个值。
--
-- 与之相关的一条**有意为之**：暂停不是解约。渠道那边的协议仍然挂着，用户如果在暂停期间自己
-- 去微信里看，看到的是「已签约」。本服务不替用户撤授权（见 §六.4）——那是要用户自己点、
-- 或者运营在后台确认过的事。
--
-- 反过来它也**不是终态**：用户重新充上钱、运营在后台恢复，都能把订阅推回 active，此后每期
-- 又是一条 `renew`。流水是只增不改的，所以「这中间停过一段」只能靠这两条 suspend/续上的
-- 记录还原，不能靠订阅行的当前状态。
CREATE TABLE membership_changes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    membership_id UUID NOT NULL REFERENCES memberships(id) ON DELETE RESTRICT,
    user_id UUID NOT NULL,
    change_type TEXT NOT NULL CHECK (change_type IN (
        'activate',        -- 开通（首购生效）
        'renew',           -- 续费（有效期叠加）；**代扣成功也写这个**
        'expire',          -- 到期失效
        'freeze',          -- 权益暂停
        'unfreeze',        -- 暂停恢复
        'auto_renew_on',   -- 用户打开自动续费
        'auto_renew_off',  -- 用户关闭自动续费
        'subscribe',       -- 签约连续包月
        'unsubscribe',     -- 解约
        'refund_adjust',   -- 退款后按规则调整权益
        'admin_adjust',    -- 后台人工调整
        'revoke',          -- 撤销会员
        'charge_failed',   -- 一期代扣没扣到（这一期还没成，到期日不动）
        'suspend'          -- 连续失败到上限，停掉自动续费；协议还在，不是解约
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

-- 一个订单只能开通或续期一次，判据是「这个订单有没有产生过开通或续期」，与走的是哪一支无关。
--
-- 它**不是**建在 (order_id, change_type) 上的：那样挡的只是「同一条回调把同一种变更记两遍」，
-- 而重投走的**不是同一种变更**——一条 order.paid 第一次投递时这个人还不是会员，走开通分支记
-- 一条 activate；同一条消息再投一次时，memberships 上已经有那一行了，于是走续期分支记一条
-- renew。change_type 不一样，(order_id, change_type) 拦不住，第二次投递把有效期又叠了一段——
-- 一个只付了一次钱的人拿到了两期会员，而且这件事在流水上看起来完全正常（两条合法的变更记录）。
--
-- refund_adjust 这类挂在同一张订单上的其它变更不受影响：它们不是购买行为，一条订单可以有多条。
--
-- 这一条是**不变式**，所以钉在库上而不是写在服务层：靠服务层自觉拦截的话，下一个新增的
-- 购买入口（改价重推、补单、人工补录）就得自己记得查一遍，而漏掉的那一次没有任何东西会报错。
CREATE UNIQUE INDEX membership_changes_order_unique
    ON membership_changes (order_id)
    WHERE order_id IS NOT NULL AND change_type IN ('activate', 'renew');

COMMENT ON INDEX membership_changes_order_unique IS
    '一个订单只能开通或续期一次；支付成功回调重放不会把会员续两次';

-- 后台开通会员的幂等靠它兜底：操作员点一次提交、网络抖动后客户端重试，不能开出两条会员，也不能
-- 让重试那一次看到「这个人已经是会员了」——那在这种场景下是假警报，他刚亲手建的那条。
--
-- request_id 列的注释是「操作留痕串，与后台操作审计对得上」，而四个后台动作写的都是空串：
-- 部分索引的 WHERE 把它们排除在外，行为一个字不改。
--
-- 与上面那条 membership_changes_order_unique 同一条理由：幂等是**不变式**，钉在库上而不是
-- 只靠服务层先查一遍——漏掉的那一次没有任何东西会报错。
CREATE UNIQUE INDEX membership_changes_request_unique
    ON membership_changes (user_id, change_type, request_id)
    WHERE request_id <> '';

COMMENT ON INDEX membership_changes_request_unique IS
    '同一个用户在同一个变更类型下，一个 request_id 只对应一条流水；后台开通会员的重试靠它兜底';

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
-- 店铺码会员活动
-- ============================================================

-- 「扫码赠 VIP」——顾客扫店里的活动码，领 N 天会员，归属门店记成这家店。老系统
-- （panda_serve 的 store_membership_campaign）有两个 collection，这里照抄形状。
--
-- # 四处 V2 化改造，都不是随手改的
--
-- **一、`vip_level_id` → `plan_id`。** 老系统有一层「VIP 等级」，V2 没有，等价物是套餐。
-- 老系统那个等级门槛（`IsSubscription==true` 且签约模板非空）在这里译作
-- `membership_plans.auto_renew = true`——**注意它今天只是一个形状**：V2 的领取不签约
-- （见下面的「三」），所以这条门槛的实际作用只剩下「只能拿连续包月的套餐做活动」。
-- 另外，coupon 模式的套餐（包月就是）领到的会员，会员价要靠会员价券，而活动上可以配券
-- （见本表的 coupon_template_id / coupon_count，以及下面「五」）：本服务发
-- `membership.campaign.claimed`，coupon-service 消费它发券，所以挑 coupon 模式的套餐
-- 不是死路——会员价那一半由券服务按同一场活动发。
--
-- **二、`store_id` 是跨库值引用，无外键。** stores 在商户库，建不了外键——与
-- `membership_plans.member_price_coupon_template_id` 同一写法（那是本库现成的先例）。
-- campaigns 与 claims 两张表上的 store_id 都是这个性质。
--
-- **三、不建老系统的签约/预锁一族列。** 老系统那张 claims 上有 `subscription_id`、
-- `contract_code`、`coupon_stock_reserved`、`grant_lease_token`、`signing*`、`sign_*`
-- 十来个字段——它们全是为「先预扣券库存、再跳微信签约、回来确认、最后发放」那条两阶段
-- 链路服务的，两把分布式锁（5 分钟租约 + 租约围栏）也是。V2 这条链路**不签约**：
-- 「送 N 天会员」就是一次会员发放，「归属门店」是 memberships.store_id 的一个取值。
-- 一次事务做得完的事不需要租约，抄过来只会留下一堆永远为空的列。
--
-- **四、不建 `per_user_limit`。** 老系统那个字段的硬校验只接受 1，真正生效的是
-- `(campaign_id, user_id)` 唯一索引。抄过来会把「可配」这个假选项也一起抄来——
-- 一个能填 3 却按 1 执行的输入框，比没有这个输入框糟得多。这里直接只用唯一索引表达。
--
-- **五、券是活动上的配置（coupon_template_id / coupon_count）。** coupon-service 消费本服务
-- 的 `membership.campaign.claimed`，一次扫码领会员会同时送出 N 天会员（本服务自己发）与
-- M 张券（券服务发）。两边的账**仍然各自独立**：这里只记「这次承诺发多少」，实际发了几张去
-- 券库按 `user_coupons.campaign_claim_id` 数——本库不重复记一份券的账，与上面套餐那一节
-- 「券的余额全归 coupon-service」是同一条规矩。所以这两列是**承诺**，不是余额：它们不会随
-- 发券结果变，也没有「已发几张」这种列。
--
-- 券模板是**跨库值引用**（coupon-service 的 coupon_templates），UUID、无外键——与 store_id、
-- membership_plans.member_price_coupon_template_id 是同一写法。本服务没有任何办法校验那个 id
-- 存不存在：发券时券服务找不到模板会记一条日志并 ack（见那边的 service/consumer.go），不会
-- 回头改这里。
--
-- 两列**要么都有、要么都没有**：只配模板不配张数等于「该发券」静默变成「什么都不发」，而
-- 用户在小程序上看到的是活动写着送券。这与套餐那条 member_price_mode='coupon' 的 CHECK 是
-- 同一条理由——配置里的半截组合不该能存下去。
--
-- 同样不建的还有 `merchant_id`：老系统有，但 V2 归属门店能推出商户（商户域的
-- StoreClient 就在手上），存一份没有读取方的冗余副本不值当。
--
-- **领取记录上没有 `status`。** 老系统那五个状态（pending_sign / signed / granting /
-- completed / failed）是签约链路的产物；V2 的领取是一次幂等发放，**行的存在就是「发放
-- 完成」**。失败的那次整体回滚、不留行——留一行 failed 会被唯一索引挡住，那个人就再也
-- 领不了了，而失败的真正原因是可重试的。
CREATE TABLE membership_campaigns (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    -- 小程序码里带的 scene 参数，扫码进来靠它找回这场活动（老系统同一个口径）。
    -- 格式与老系统逐字相同：`smc_` 前缀 + URL 安全字符。
    scene TEXT NOT NULL CHECK (scene ~ '^smc_[A-Za-z0-9_-]+$'),
    store_id UUID NOT NULL,
    -- 「送的是哪个套餐的会员」。老系统的 vip_level_id；建表时要求该套餐 auto_renew = true，
    -- 那个校验在服务层（跨表 CHECK 写不了）。
    plan_id UUID NOT NULL REFERENCES membership_plans(id) ON DELETE RESTRICT,
    -- 送多少天。与套餐的 period/period_count 无关：这是一次赠送，按天数算。
    gift_days INTEGER NOT NULL CHECK (gift_days > 0),
    start_at TIMESTAMPTZ NOT NULL,
    end_at TIMESTAMPTZ NOT NULL,
    -- 老系统三个取值逐字照抄：draft=刚建好还不能扫，enabled=可在有效期内扫，
    -- disabled=下架（已经领过的人不受影响）。
    status TEXT NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'enabled', 'disabled')),
    -- 小程序码。**今天恒为空**：生成它要走微信的 wxacode.getUnlimited，而
    -- appid/secret 与 access_token 缓存都在 user-service（本服务没有任何微信配置）。
    -- 两列仍然建着——它们描述的是活动的事实，没有码的时候空着，语义清楚。
    qr_code_url TEXT NOT NULL DEFAULT '',
    qr_code_generated_at TIMESTAMPTZ,
    -- 操作人：身份库的 admin_users.id，仅作值引用（跨库，无外键）。
    created_by UUID,
    updated_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 券的两列是本次活动每领一次的**承诺**配置。见上面「五」。
    coupon_template_id UUID,
    coupon_count INTEGER CHECK (coupon_count IS NULL OR coupon_count > 0),
    CHECK (end_at > start_at),
    -- 券模板与张数同生共死（见上面「五」）。
    CONSTRAINT membership_campaigns_coupon_pair
        CHECK ((coupon_template_id IS NULL) = (coupon_count IS NULL))
);

-- 一个 scene 只能挂一场活动。扫码进来的请求手里只有 scene，撞了就没有任何判据能说是哪一场。
CREATE UNIQUE INDEX membership_campaigns_scene_unique
    ON membership_campaigns (scene);

COMMENT ON TABLE membership_campaigns IS
    '店铺码会员活动：扫码送 N 天会员并把归属门店记成活动门店';
COMMENT ON COLUMN membership_campaigns.store_id IS
    '活动门店 ID；跨库值引用（商户库），无外键。领取时作为会员的归属门店';
COMMENT ON COLUMN membership_campaigns.plan_id IS
    '送的是哪个套餐的会员；本库外键。建活动时要求该套餐 auto_renew = true（服务层校验）';
COMMENT ON COLUMN membership_campaigns.coupon_template_id IS
    '这场活动每领一次送几张什么券；跨库值引用（券服务的 coupon_templates），无外键。为空 = 只送会员天数';
COMMENT ON COLUMN membership_campaigns.coupon_count IS
    '每次领取送的券张数。与 coupon_template_id 同生共死（见上面那条 CHECK）；实际发了几张去券库按 campaign_claim_id 数';

-- 领取记录。一次领取一行，**行的存在即发放完成**（见上面「三」）。
--
-- 券的两列是**发放那一刻的快照**：活动后来改了券的配置，已经领过的那次不受影响。与同一个表上
-- 的 store_id / gift_days 是同一条规矩。
--
-- 券与领取记录的对应关系**不止这一处**：券库里 user_coupons.campaign_claim_id 也指着这条
-- 记录（老系统同一列、同一用途）。这边存的是「承诺给什么」，那边存的是「实际给了什么」，
-- 两条记录各归各的库，谁都不改对方。
CREATE TABLE membership_campaign_claims (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id UUID NOT NULL REFERENCES membership_campaigns(id) ON DELETE RESTRICT,
    user_id UUID NOT NULL,
    -- 快照：活动后来改了门店或天数，已经领过的那次不受影响。
    store_id UUID NOT NULL,
    plan_id UUID NOT NULL,
    gift_days INTEGER NOT NULL,
    -- 这次领取派生的那条会员。老系统把这个关系记在会员行上（campaign_claim_id），
    -- 这里反过来记——会员表今天没有那一列，而这边的方向查起来更直接
    -- （「这条领取给了什么」是后台领取记录页要回答的问题）。
    membership_id UUID NOT NULL REFERENCES memberships(id) ON DELETE RESTRICT,
    -- 发放那一刻算出来的到期时刻。会员行上也有 expire_at，但那条会随后台调整、
    -- 续费而变；这里记的是「这次领取当时送出到哪天」。
    membership_expire_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    coupon_template_id UUID,
    coupon_count INTEGER CHECK (coupon_count IS NULL OR coupon_count > 0),
    CONSTRAINT membership_campaign_claims_coupon_pair
        CHECK ((coupon_template_id IS NULL) = (coupon_count IS NULL))
);

-- 一个人一场活动只能领一次。老系统那五个状态的链路里这是发放的幂等凭据，V2 里它同时
-- 是「重复扫码」的答案：撞上就回原来那条，不再发第二次。
CREATE UNIQUE INDEX membership_campaign_claims_user_unique
    ON membership_campaign_claims (campaign_id, user_id);

-- 后台领取记录页按活动查，一页一查。没有它这条查询会随活动数增长扫全表。
CREATE INDEX membership_campaign_claims_campaign_idx
    ON membership_campaign_claims (campaign_id, created_at DESC);

COMMENT ON TABLE membership_campaign_claims IS
    '店铺码活动的领取记录：一行即一次发放完成（失败的那次整体回滚、不留行）';
COMMENT ON COLUMN membership_campaign_claims.store_id IS
    '领取时的活动门店快照，也就是这次送的会员的归属门店；跨库值引用（商户库），无外键';
COMMENT ON COLUMN membership_campaign_claims.coupon_template_id IS
    '这次领取承诺发的券模板（活动上那一份的快照）；为空 = 只送了会员天数';
COMMENT ON COLUMN membership_campaign_claims.coupon_count IS
    '这次领取承诺发的券张数；实际发了几张在券库的 user_coupons 里按 campaign_claim_id 数';

-- ============================================================
-- 扣款待办
-- ============================================================

-- # 这张表补的是哪一格
--
-- 扣款成功那条链上有两步，顺序不能反（见 service/renewal_order.go 的文件头）：先在订单域记一张
-- 续费单，再把会员续一期。第一步失败时的旧处置是「返回错误让平台重投」——而平台的重投是**毫秒级
-- 的 5 次**：rabbitmq.go 的 republishRetry 不带任何延迟（没有 TTL、没有延迟队列），第 5 次之后
-- d.Reject(false) 进 panda.events.dlq，而那个队列今天既没有消费者也没有监控。
--
-- 所以订单域抖一下（或者重启几分钟），结果不是「晚几分钟续上」，而是**这笔钱在会员域与订单域
-- 两边都没有痕迹**：渠道那边钱动了，用户这边会员没续、订单管理里查不到这一笔，事后对账的人手上
-- 没有任何东西能解释它。这一格与「后台人工开通/调整」那种「至少有人点过」的缺失不同，它是全自动
-- 的，没有目击者。
--
-- 这张表把那一次失败**存下来**：一行 = 一笔已经收到成功结论、而账没落成的渠道流水。worker 定时
-- 重试那两步，落完就把行删掉。
--
-- # 它是工作队列，不是业务数据
--
-- 所以它**没有历史、没有审计**：行删掉之后，「这个月有哪几笔补过」只能去翻日志；而这一期最终落成
-- 的东西（membership_changes 上那条 renew 流水 + 订单域那张单）才是事实。想留一份「补过哪些」的
-- 台账，等于给这张表再养一条只增的流水，而它的读者只有一个——排查时的人，那时候日志够用。
--
-- 同样地，它**不回填**：上线之前已经进了死信的那几笔不会自己回来，dlq 里那些行要人去看。这张表
-- 从第一行新数据开始。
--
-- # 主键为什么是渠道流水号
--
-- 与结算的幂等键是**同一个身份**（见 repository.SettleCharge：那笔渠道流水号写进
-- membership_changes.request_id，撞那条唯一索引即「这一期已经续过了」）。同一笔钱只可能有一条
-- 待办——同一份事件投两次而两次都失败时，第二次是 DO NOTHING（见 ParkChargeSettlement）。换成
-- 自增 id 或 event_id 做键，一条事件投两次就落两行，而后面那行重试时建的是同一张续费单、结的是
-- 同一期会员，白跑一遍。
--
-- 反过来，渠道流水号的唯一性也是**渠道那边给的**：一次扣款一个号，这是重试安全的前提（见
-- dto.AgreementChargeEventPayload 与 recordRenewalOrder 里关于空流水号为什么是硬错误的那段）。
--
-- # 没有 lease 列
--
-- 认领靠把 next_attempt_at 往后推（见 ClaimDueChargeSettlements）：一行被某个副本拿走之后，在
-- 租期内不会再被别的副本拿到。message_outbox 上有 lease_owner/lease_token/lease_until 三列，因为
-- 它的投递**可能成功**、要按 token 精确地标记「这一条是我的、我投完了」；这里只有「做完就删」与
-- 「没做完、推后再来」两种收场，没有需要按 token 认领的写。
CREATE TABLE membership_charge_settlements (
    -- 渠道流水号。见上面「主键为什么是渠道流水号」。
    provider_transaction_id TEXT PRIMARY KEY
        CHECK (btrim(provider_transaction_id) <> ''),
    -- 命中的那份代扣协议（membership_subscriptions.agreement_id 上的值）。事件体里没有订阅 id，
    -- 本域其余地方也都是按协议号定位（库上那份部分唯一索引保证一份协议最多一行订阅）。
    agreement_id UUID NOT NULL,
    -- 事件里说的签约人。**不参与定位**，只用来核对（与 ChargeSettleParams.UserID 同一个用途）：
    -- 支付域记的签约人与本域记的不是同一个人时，那一期照常落账，但要留一条要人看的错。
    user_id      UUID NOT NULL,
    -- 这一期的结论，取值同 dto.ChargeStatus*。**今天只会是 succeeded**：失败那一期压根不建单
    -- （见 recordRenewalOrderFor），也就没有待办可言。留着这一列是为了让这一行自己说得清「待办的
    -- 是哪一种结论」，而不是靠读代码推断。
    target       TEXT NOT NULL CHECK (target IN ('succeeded', 'failed')),
    -- 期次与金额。只进流水与订单，是对账时手里拿的那个号与那笔钱（与 ChargeSettleParams 上同名
    -- 两列同义）。
    biz_period   TEXT NOT NULL CHECK (btrim(biz_period) <> ''),
    amount       BIGINT NOT NULL,
    -- **事件到达的时刻**，不是重试的时刻。它是续期的起点候选之一（这个人的会员已经过期时，就从
    -- 这一刻重新起算，见 renewMembership）：用重试时刻的话，「这一期从哪天开始」会随一次订单域
    -- 抖动而漂，而那是用户看得见的东西（会员中心的有效期）。
    occurred_at  TIMESTAMPTZ NOT NULL,
    trace_id     TEXT NOT NULL DEFAULT '',

    -- 认领时加一，与 message_outbox 同一个口径。退避由 worker 按它算（见 settlementBackoff），
    -- 表上只存结论。
    attempts        INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 上一次为什么没成。**只给人看**：这一列不参与任何判定，重试是无条件的。一张永远修不好的待办
    -- 会一直 ERROR 下去，直到有人把根因修好或者手动删掉它——这**是有意的**，因为另一种收场
    -- （试满几次就放弃）等于把「这笔钱没有落账」这件事悄悄删掉，而它是一条对不上账的钱。
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 认领那条查询的索引：取到点的那几行，按 next_attempt_at 排。
CREATE INDEX membership_charge_settlements_due_idx
    ON membership_charge_settlements (next_attempt_at);

COMMENT ON TABLE membership_charge_settlements IS
    '扣款成功但续费单没建上时的待办：worker 重试「建单 + 结算」，落完即删（见上面「扣款待办」那一节）';
COMMENT ON COLUMN membership_charge_settlements.occurred_at IS
    '事件到达的时刻，重试时原样复用，不要让续期起点随重试漂移';

-- ============================================================
-- Outbox / Inbox
-- ============================================================

-- 每个库都要有自己的一对：outbox 与业务行同一个事务写入，所以不可能是共享表。
-- 列必须与其它库逐列一致（migrations 包的 TestMessageTablesStayInSyncAcrossSets 会比对）。
--
-- identity 与 merchant 两集的服务先于租约列存在，它们的那两份另带一段 ADD COLUMN 兜底；
-- 本库的库从来就是新建的，所以租约列直接写在 CREATE TABLE 里，两种写法都正确。要改必须
-- 十一个集一起改。

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

-- <<< message-tables:end <<<

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

-- ============================================================
-- 表与关键字段的中文注释
--
-- 放在同一个文件里而不是单开一份 _comments：合并之后每个集只有这一个文件，注释和它描述
-- 的列不会再出现「改了列没改注释」这种两个文件各说各话的情况。
-- ============================================================

COMMENT ON TABLE membership_plans IS '会员套餐定义：连续包月与年度会员；权益只有会员价一项';
COMMENT ON COLUMN membership_plans.code IS '稳定的套餐编码（monthly_auto / annual），创建后不可修改';
COMMENT ON COLUMN membership_plans.description IS '套餐说明文案';
COMMENT ON COLUMN membership_plans.benefits IS '权益文案列表（JSON 数组），仅用于展示，不参与权益判定';
COMMENT ON COLUMN membership_plans.price_cents IS '套餐售价，单位为分';
COMMENT ON COLUMN membership_plans.period IS '每期时长的单位：month=月度，year=年度；续期按日历加，不按天数加';
COMMENT ON COLUMN membership_plans.period_count IS '每期几个单位：连续包月与年度会员都是 1';
COMMENT ON COLUMN membership_plans.auto_renew IS '是否自动续费：TRUE=签署微信委托代扣并按期扣款';
COMMENT ON COLUMN membership_plans.wechat_plan_id IS '微信支付委托代扣的签约模板 ID（微信商户平台的 plan_id）；自动续费的套餐必填';
COMMENT ON COLUMN membership_plans.member_price_mode IS '会员价路径：auto=会员自动享，coupon=靠会员价体验券';
COMMENT ON COLUMN membership_plans.member_price_coupon_template_id IS 'coupon 模式发券用的券模板 ID（券类型 MEMBERSHIP_PRICE_EXPERIENCE）；仅作跨库值引用（券服务）';
COMMENT ON COLUMN membership_plans.member_price_coupons_per_period IS 'coupon 模式每期扣款成功后发放的券张数';
COMMENT ON COLUMN membership_plans.sort_order IS '展示排序，数值越小越靠前';
COMMENT ON COLUMN membership_plans.status IS '套餐状态：draft=草稿，active=在售，disabled=下架（不影响已购会员）';
COMMENT ON COLUMN membership_plans.legacy_id IS '老库会员等级 ID；仅迁移映射来的套餐有值';
COMMENT ON COLUMN membership_plans.created_by IS '创建人管理员 ID，仅作跨库值引用（身份库）';

COMMENT ON TABLE memberships IS '用户会员资格：一个用户一条，续费叠加有效期而不新增行';
COMMENT ON COLUMN memberships.user_id IS '小程序用户 ID；仅作跨库值引用';
COMMENT ON COLUMN memberships.legacy_id IS '老库 user_memberships ID；仅迁移过来的会员有值';
COMMENT ON COLUMN memberships.plan_id IS '当前套餐 ID，引用本库 membership_plans';
COMMENT ON COLUMN memberships.plan_code IS '成交时的套餐编码快照';
COMMENT ON COLUMN memberships.plan_name IS '成交时的套餐名称快照';
COMMENT ON COLUMN memberships.member_price_mode IS '会员价路径的成交快照：auto=自动享，coupon=靠会员价体验券';
COMMENT ON COLUMN memberships.member_price_coupon_template_id IS '发券券模板 ID 的成交快照；仅作跨库值引用';
COMMENT ON COLUMN memberships.member_price_coupons_per_period IS '每期发券张数的成交快照';
COMMENT ON COLUMN memberships.status IS '会员状态：active=生效，frozen=权益暂停，expired=已过期，revoked=已撤销';
COMMENT ON COLUMN memberships.start_at IS '会员生效时间';
COMMENT ON COLUMN memberships.expire_at IS '会员到期时间；续费在此之上叠加';
COMMENT ON COLUMN memberships.auto_renew IS '自动续费开关：用户可在小程序关闭或恢复';
COMMENT ON COLUMN memberships.auto_renew_off_at IS '用户最后一次关闭自动续费的时间；从未开启则为空';
COMMENT ON COLUMN memberships.renewal_count IS '累计成功续费次数';
COMMENT ON COLUMN memberships.last_renewed_at IS '最后一次续费成功时间';
COMMENT ON COLUMN memberships.frozen_at IS '权益暂停时间';
COMMENT ON COLUMN memberships.freeze_reason IS '权益暂停原因';
COMMENT ON COLUMN memberships.revoked_at IS '会员撤销时间';
COMMENT ON COLUMN memberships.revoke_reason IS '会员撤销原因';
COMMENT ON COLUMN memberships.store_id IS '归属门店 ID：第一次成为会员时固化，续费与升级不覆盖，会员过期后重新开通才可变；仅作跨库值引用（商户库），不参与权益判定与分账';

COMMENT ON TABLE membership_subscriptions IS '连续包月的签约与扣款期次；代扣协议本体在支付服务';
COMMENT ON COLUMN membership_subscriptions.membership_id IS '所属会员资格 ID，引用本库 memberships';
COMMENT ON COLUMN membership_subscriptions.user_id IS '小程序用户 ID；仅作跨库值引用';
COMMENT ON COLUMN membership_subscriptions.plan_id IS '订阅的套餐 ID，引用本库 membership_plans';
COMMENT ON COLUMN membership_subscriptions.status IS '订阅状态：pending_sign=待签约，active=扣款中，suspended=连续失败已暂停，cancelled=已解约，expired=已到期不再续';
COMMENT ON COLUMN membership_subscriptions.agreement_id IS '支付服务的委托代扣协议 ID（payment_agreements.id）；仅作跨库值引用';
COMMENT ON COLUMN membership_subscriptions.contract_code IS '商户协议号，签约时由支付服务返回，用于向渠道对账';
COMMENT ON COLUMN membership_subscriptions.price_cents IS '每期扣款金额快照，单位为分；套餐调价不影响已签约用户';
COMMENT ON COLUMN membership_subscriptions.wechat_plan_id IS '签约模板 ID 快照，取值同套餐；已签协议挂在签约当时那个模板上';
COMMENT ON COLUMN membership_subscriptions.period IS '每期时长的单位快照：month=月度，year=年度';
COMMENT ON COLUMN membership_subscriptions.period_count IS '每期几个单位的快照';
COMMENT ON COLUMN membership_subscriptions.next_charge_at IS '下次应扣款时间；续费扫描按它取任务';
COMMENT ON COLUMN membership_subscriptions.last_charge_at IS '上次扣款时间';
COMMENT ON COLUMN membership_subscriptions.charge_count IS '累计成功扣款次数';
COMMENT ON COLUMN membership_subscriptions.failed_count IS '累计扣款失败次数';
COMMENT ON COLUMN membership_subscriptions.consecutive_failed_count IS '连续扣款失败次数；达到阈值转 suspended，成功一次即清零';
COMMENT ON COLUMN membership_subscriptions.suspended_at IS '因连续扣款失败暂停的时间';
COMMENT ON COLUMN membership_subscriptions.cancel_at IS '解约时间';
COMMENT ON COLUMN membership_subscriptions.cancel_reason IS '解约原因';
COMMENT ON COLUMN membership_subscriptions.cancelled_by IS '解约发起人 ID：用户或后台操作人';
COMMENT ON COLUMN membership_subscriptions.order_id IS '首月那张订单（会员中心支付并签约）；跨库值引用（订单库），无外键';
COMMENT ON COLUMN membership_subscriptions.coffee_order_id IS '咖啡订单仅签约；跨库值引用（订单库），无外键';
COMMENT ON COLUMN membership_subscriptions.campaign_claim_id IS '店铺码活动签约对应的那条领取记录（本库 membership_campaign_claims.id），无外键';

COMMENT ON TABLE membership_changes IS '会员变更流水：只增不改不删，同时作为权益变更的幂等凭据';
COMMENT ON COLUMN membership_changes.membership_id IS '所属会员资格 ID，引用本库 memberships';
COMMENT ON COLUMN membership_changes.user_id IS '小程序用户 ID；仅作跨库值引用';
COMMENT ON COLUMN membership_changes.change_type IS '变更类型：activate=开通，renew=续费，expire=到期，freeze=暂停，unfreeze=恢复，auto_renew_on/off=自动续费开关，subscribe=签约，unsubscribe=解约，refund_adjust=退款调整，admin_adjust=后台调整，revoke=撤销';
COMMENT ON COLUMN membership_changes.from_status IS '变更前状态';
COMMENT ON COLUMN membership_changes.to_status IS '变更后状态';
COMMENT ON COLUMN membership_changes.from_expire_at IS '变更前到期时间';
COMMENT ON COLUMN membership_changes.to_expire_at IS '变更后到期时间';
COMMENT ON COLUMN membership_changes.plan_id IS '变更涉及的套餐 ID，引用本库 membership_plans';
COMMENT ON COLUMN membership_changes.order_id IS '触发本次变更的订单 ID；仅作跨库值引用';
COMMENT ON COLUMN membership_changes.operator_type IS '操作人类型：user=用户，admin=后台，system=系统，worker=后台任务';
COMMENT ON COLUMN membership_changes.operator_id IS '操作人 ID；系统与任务为空';
COMMENT ON COLUMN membership_changes.reason IS '变更原因（枚举化的短语）';
COMMENT ON COLUMN membership_changes.remark IS '补充说明';
COMMENT ON COLUMN membership_changes.metadata IS '变更的扩展明细，低频、不参与查询';
COMMENT ON COLUMN membership_changes.request_id IS '操作留痕串，与后台操作审计对得上';
COMMENT ON COLUMN membership_changes.occurred_at IS '业务发生时间';

COMMENT ON TABLE message_outbox IS '事务性发件箱：与业务行同事务写入，由 relay 投递到消息队列';
COMMENT ON COLUMN message_outbox.event_id IS '事件 ID，消费端幂等依据';
COMMENT ON COLUMN message_outbox.event_type IS '事件类型';
COMMENT ON COLUMN message_outbox.event_version IS '事件版本';
COMMENT ON COLUMN message_outbox.trace_id IS '链路追踪 ID';
COMMENT ON COLUMN message_outbox.payload IS '事件载荷';
COMMENT ON COLUMN message_outbox.attempts IS '已投递尝试次数';
COMMENT ON COLUMN message_outbox.next_attempt_at IS '下次投递时间，重试退避用';
COMMENT ON COLUMN message_outbox.published_at IS '投递成功时间；为空表示待投递';
COMMENT ON COLUMN message_outbox.lease_owner IS '投递租约持有者';
COMMENT ON COLUMN message_outbox.lease_token IS '投递租约令牌，防止过期持有者回写';
COMMENT ON COLUMN message_outbox.lease_until IS '消息处理租约截止时间';
COMMENT ON COLUMN message_outbox.last_error IS '最后一次投递失败原因';
COMMENT ON COLUMN message_outbox.created_at IS '事件写入时间';

COMMENT ON TABLE message_inbox IS '事务性收件箱：消费幂等，与业务更新同事务写入';
COMMENT ON COLUMN message_inbox.event_id IS '事件 ID，唯一约束即消费幂等';
COMMENT ON COLUMN message_inbox.claimed_at IS '认领时间';
COMMENT ON COLUMN message_inbox.lease_owner IS '消费租约持有者';
COMMENT ON COLUMN message_inbox.lease_token IS '消费租约令牌';
COMMENT ON COLUMN message_inbox.lease_until IS '消息消费租约截止时间';
COMMENT ON COLUMN message_inbox.completed_at IS '处理完成时间；为空表示仍在处理';

COMMIT;
