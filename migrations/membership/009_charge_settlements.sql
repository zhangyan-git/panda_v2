-- membership/009：扣款成功但续费单没建上时落的**待办**
--
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

BEGIN;

CREATE TABLE membership_charge_settlements (
    -- 渠道流水号。见文件头「主键为什么是渠道流水号」。
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
    '扣款成功但续费单没建上时的待办：worker 重试「建单 + 结算」，落完即删（见 009 文件头）';
COMMENT ON COLUMN membership_charge_settlements.occurred_at IS
    '事件到达的时刻，重试时原样复用，不要让续期起点随重试漂移';

COMMIT;
