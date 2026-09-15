-- account/005：咖啡豆账户（方案 5.6 的另一半）
--
-- 001 建的是福卡那一半，文件头里写着「咖啡豆账户等出货链路（payment 的 action=account）
-- 落地再建」。这一份就是那一天：payment-service 的账户出资今天起会真的扣减这里的余额。
--
-- ⚠️ 先说清楚这一半与 001 的**一处根本不同**：001 的头部写着「本库没有货币金额——福卡不是
-- 出资渠道」，那句话只对福卡成立。**咖啡豆是货币金额，单位是分**，与 orders.payable_amount
-- 同单位。所以这张表上没有「张数」，只有钱；payment_fundings.line_type='coffee_bean' 与
-- order_payment_lines.line_type='coffee_bean' 指的就是这里的账变。
--
-- ⚠️ 再说清楚与**另一个「咖啡余额」**的区别，这是本文件最容易被人搞混的地方：
-- panda_coffee_machine.devices.coffee_balance（注释「咖啡余额，单位为分」）配
-- device_balance_ledger，是**设备维度**的预付余额，按 device_id 记账、没有 user_id，老系统里
-- 是「每次打咖啡扣机器余额」。本文件建的是**用户维度**的账户，键是 user_id，两者没有任何
-- 关系、不共用代码、也不共用表名。表名一律 coffee_bean_*，不叫 account_*（payment 库的守卫
-- 禁止跨库 REFERENCES account_\w+，那是另一回事，但同样值得避开这个名字）。
--
-- 表形状与 fortune_card_accounts / fortune_card_entries 刻意保持同形（余额列 + 只追加流水 +
-- balance_after + 幂等号 + 冲正自引用），因为它们是同一类东西、由同一个服务拥有；差别只在
-- 币种、entry_type 词表和「豆没有冻结」。
--
-- **没有冻结**：福卡要冻，是因为退款申请到审核之间那几张卡还在用户手上、还能拿去抽奖；豆在
-- 支付的那一刻就已经从余额里扣走了，退款窗口里没有可保护的东西，驳回与撤销因此不需要任何
-- 动作。往这里加 frozen_balance 之前，先回答「冻的是哪一批还没被花掉的豆」——答不上来就是
-- 又抄了一遍福卡的表形状。
--
-- **没有过期**：豆永不过期（拍板）。不加 expires_at、不加到期 worker、不加兜底扫表。

-- ============================================================
-- 咖啡豆账户
-- ============================================================

-- 一个用户一行，懒创建：第一次调整或第一次扣减时才 INSERT，之后所有读写都先锁这一行。
--
-- balance 是流水求和的结果，两者在同一次事务里更新。存下来而不是每次
-- SUM(coffee_bean_entries)，理由与 fortune_card_accounts 逐条相同（被反复只读、且只有一列
-- 能被约束）。这里的 CHECK (balance >= 0) 同样是**最后一道**防线：后台调整填负数、扣减超过
-- 余额，都会在这里当场炸出来，而不是变成一个「用户欠平台钱」的幽灵。
CREATE TABLE coffee_bean_accounts (
    user_id UUID PRIMARY KEY,
    -- 单位为分，与 orders.payable_amount 同单位。整数，没有小数。
    balance BIGINT NOT NULL DEFAULT 0 CHECK (balance >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ============================================================
-- 咖啡豆流水
-- ============================================================

-- 只允许追加：冲正不是原地改数，是新增一条反向记录（同 fortune_card_entries、
-- device_balance_ledger）。
CREATE TABLE coffee_bean_entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES coffee_bean_accounts(user_id) ON DELETE RESTRICT,
    -- adjust = 后台人工调整（充值或纠错），consume = 纯豆出资扣减，reverse = 冲正。
    -- 调整只有一个词：列本身带符号，充值是正、纠错是负。分成 recharge/refund 两词会让人
    -- 以为 recharge 只能为正，而「把充错的豆调回来」正是要往负的方向走。
    entry_type TEXT NOT NULL CHECK (entry_type IN ('adjust', 'consume', 'reverse')),
    -- 有符号：充值为正，纠错为负，扣减为负，冲正与它冲掉的那笔相反。单位为分。
    amount BIGINT NOT NULL CHECK (amount <> 0),
    -- 变动后余额。明细页显示的就是它；冲正重放时也靠它回答「当时是多少」。
    balance_after BIGINT NOT NULL CHECK (balance_after >= 0),
    -- 这一行在用户与后台那边显示成什么（「后台充值」「后台调整」「咖啡豆支付」「退款冲正」）。
    -- 文案由本服务拥有：调用方给的是起因，不是给人看的字。
    title TEXT NOT NULL CHECK (char_length(trim(title)) > 0),
    -- 起因对象，都是值引用：manual（后台调整）/ order（扣减与它的冲正）/ after_sale（冲正）。
    -- 本列记的是「为什么余额变了」，与事件日志不是一回事。
    reference_type TEXT NOT NULL DEFAULT '',
    reference_id TEXT NOT NULL DEFAULT '',
    -- 给人看的号：调整是调用方给的幂等号，扣减与冲正是订单号 / 售后单号。后台明细按它检索。
    reference_no TEXT NOT NULL DEFAULT '',
    -- 幂等键，唯一索引见下。三种形状（都在服务侧拼，不落库约束）：
    --   adjust:{requestId}      后台调整（调用方给的幂等号，重发不会加两次）
    --   order:{orderId}         纯豆出资扣减（**用订单 ID 而不是支付单号**，见下）
    --   after_sale:{afterSaleNo} 冲正（一条售后只能冲一次）
    --
    -- 扣减为什么用订单 ID：冲正要从 order.after_sale.reviewed 反查「这单扣了多少豆」，而那条
    -- 事件只带 orderId。键里带上订单，账户域自己就能查到那笔扣减，不必让订单域把账变 ID 塞进
    -- 事件载荷。payments_one_succeeded_per_order 保证了一张订单只会有一条成功的豆扣减，
    -- 所以订单做键不会撞。
    entry_key TEXT NOT NULL,
    -- 冲正 = 新增一条反向记录，不原地改数。after_sale:{afterSaleNo} 的唯一索引保证了
    -- 「一条售后只冲一次」，这里不再叠一条部分唯一索引。
    reverses_entry_id UUID REFERENCES coffee_bean_entries(id) ON DELETE RESTRICT,
    -- 后台调整是谁动的。只对 entry_type='adjust' 有值，其余为空——与 device_balance_ledger
    -- 的 operator_id / operator_name 同形，也是审计之外的第二处留痕。
    -- ⚠️ operator_name 恒为空串（令牌里没有用户名），别按它去 join；见 006 的列注释。
    operator_id TEXT NOT NULL DEFAULT '',
    operator_name TEXT NOT NULL DEFAULT '',
    remark TEXT NOT NULL DEFAULT '',
    -- 业务发生时间。调整用请求时刻；扣减用支付时刻；冲正用审核时刻。补投或重放旧事件时
    -- NOW() 会把「上周那笔」记成「刚才」，明细顺序就乱了。
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((entry_type = 'reverse') = (reverses_entry_id IS NOT NULL))
);

CREATE FUNCTION prevent_coffee_bean_entry_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'coffee bean ledger is append-only';
END;
$$;

CREATE TRIGGER coffee_bean_entries_append_only
    BEFORE UPDATE OR DELETE ON coffee_bean_entries
    FOR EACH ROW EXECUTE FUNCTION prevent_coffee_bean_entry_change();

-- ============================================================
-- 索引
-- ============================================================

-- 明细页：一个用户的流水按时间倒序。含 id 是为了同毫秒的两笔也有稳定顺序。
CREATE INDEX coffee_bean_entries_user_idx
    ON coffee_bean_entries (user_id, occurred_at DESC, id);

-- 幂等：同一个键只允许一条流水。重发一次后台调整、重试一次扣减、重投一条
-- order.after_sale.reviewed，都撞在这上面。
CREATE UNIQUE INDEX coffee_bean_entries_key_unique
    ON coffee_bean_entries (entry_key);
