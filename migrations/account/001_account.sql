-- account：资产账户领域表结构与中文注释
--
-- 属于 panda_account（account-service，方案 5.6）。本库装一个用户手上的两种余额，各自配一条
-- 只追加流水，外加退款窗口里对福卡的冻结：
--   1. 福卡余额账户                     → fortune_card_accounts
--   2. 福卡收支流水（只允许追加、可冲正）  → fortune_card_entries
--   3. 退款期间的福卡冻结与追回           → fortune_card_freezes
--   4. 咖啡豆账户                       → coffee_bean_accounts
--   5. 咖啡豆收支流水（只允许追加、可冲正） → coffee_bean_entries
--
-- 用户、订单、抽奖活动、优惠券都不是本库的。外部服务的 ID 只作为值引用保存，本库不建
-- 跨数据库外键；本文件里的 REFERENCES 只指向本库自己的表。
--
-- 表形状不是新发明的：余额列 + 只追加流水 + balance_after + 幂等号 + 冲正自引用，
-- 与 coffee_machine 的 device_balance_ledger 是同一套东西（设备咖啡余额 vs 用户
-- 福卡余额）。那边踩过、这边就照着建。咖啡豆那一半与福卡刻意保持同形。
--
-- 福卡的张数是整数（INT 语义），福卡没有货币金额——福卡不是出资渠道（order 与
-- payment 两集已把两侧词表收窄），它只换抽奖参与次数。咖啡豆反过来，它是货币金额，
-- 单位是分。
--
-- 末尾另带平台样板的一对 message_outbox / message_inbox（各库自带一份）。

BEGIN;

-- ============================================================
-- 福卡账户
-- ============================================================

-- 一个用户一行，懒创建：第一次发放时才 INSERT，之后所有读写都先锁这一行。
--
-- balance 是流水求和的结果，两者在同一次事务里更新。存下来而不是每次 SUM(fortune_card_entries)，
-- 是因为余额要被「这一单的福卡到账了吗」这类只读查询反复读，而且 CHECK (balance >= 0) 是
-- 余额不会被扣穿的**最后一道**防线——只有一列能被约束，流水表上没法表达这个不变式。
-- 代价是它可能与流水漂移，所以有一把锁 + 一条集成测试盯着「SUM(amount) = balance」。
CREATE TABLE fortune_card_accounts (
    user_id UUID PRIMARY KEY,
    balance BIGINT NOT NULL DEFAULT 0 CHECK (balance >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 与 balance 一样是流水/冻结行求和的结果，在同一次事务里更新，同样是为了让只读查询不必
    -- 聚合，以及让「冻结不会被重复计数」有一个能被 CHECK 表达的地方。
    frozen_balance BIGINT NOT NULL DEFAULT 0 CHECK (frozen_balance >= 0),
    -- 跨列不变式：冻结额永远不超过余额。它是零成本的护栏——冻结重复计数、将来「退款成功
    -- 追回福卡」先扣余额后解冻写反了顺序，都会在这里当场炸出来，而不是变成一个可用余额为负、
    -- 对着账本也算不平的幽灵。
    --
    -- 它同时是追回那条路的顺序约束：必须先解冻（frozen_balance -= n）再冲正
    -- （balance -= n），反过来写这一步会失败。
    CONSTRAINT fortune_card_accounts_frozen_within_balance CHECK (frozen_balance <= balance)
);

-- ============================================================
-- 福卡流水
-- ============================================================

-- 只允许追加：冲正不是原地改数，是新增一条反向记录（同 device_balance_ledger）。
--
-- 「保留不可变收支明细」是方案 3.1 对福卡的原话，所以用触发器钉住，而不是靠「大家都
-- 只 INSERT」这句约定。
CREATE TABLE fortune_card_entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES fortune_card_accounts(user_id) ON DELETE RESTRICT,
    -- grant = 发放（订单完成赠送），draw = 抽奖扣减，reverse = 冲正。
    entry_type TEXT NOT NULL CHECK (entry_type IN ('grant', 'draw', 'reverse')),
    -- 有符号：发放为正，扣减为负，冲正与它冲掉的那笔相反。
    amount BIGINT NOT NULL CHECK (amount <> 0),
    -- 变动后余额。明细页显示的就是它；冲正重放时也靠它回答「当时是多少」。
    balance_after BIGINT NOT NULL CHECK (balance_after >= 0),
    -- 这一行在用户那边显示成什么（「订单完成赠送」「幸运杯套完成额外赠送」「参与抽奖」）。
    -- 文案由本服务拥有：调用方给的是起因，不是给用户看的字。
    title TEXT NOT NULL CHECK (char_length(trim(title)) > 0),
    -- 起因对象，都是值引用：order / draw / entry。
    -- 本列记的是「为什么余额变了」，与事件日志不是一回事。
    reference_type TEXT NOT NULL DEFAULT '',
    reference_id TEXT NOT NULL DEFAULT '',
    -- 给人看的号（订单号）。订单详情页按它查这一单的福卡流水。
    reference_no TEXT NOT NULL DEFAULT '',
    -- 幂等键，唯一索引见下。三种形状（都在服务侧拼，不落库约束）：
    --   order:{orderId}:base / order:{orderId}:bonus:{campaignId}  发放（原型口径）
    --   draw:{requestId}                                          扣减（调用方给的幂等号）
    --   reverse:{entryId}                                         冲正（一笔只能冲一次）
    entry_key TEXT NOT NULL,
    -- 冲正 = 新增一条反向记录，不原地改数。entry_key 的 reverse:{entryId} 已经保证了
    -- 一笔只能被冲正一次，这里不再叠一条部分唯一索引——同一条不变式两处表达，
    -- 迟早会有一处先改。
    reverses_entry_id UUID REFERENCES fortune_card_entries(id) ON DELETE RESTRICT,
    remark TEXT NOT NULL DEFAULT '',
    -- 业务发生时间。发放用订单的完成时间而不是 NOW()：补投或重放一条旧事件时，NOW()
    -- 会把「上周完成的那单」记成「刚才」，明细页上的顺序就乱了。
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((entry_type = 'reverse') = (reverses_entry_id IS NOT NULL))
);

CREATE FUNCTION prevent_fortune_card_entry_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'fortune card ledger is append-only';
END;
$$;

CREATE TRIGGER fortune_card_entries_append_only
    BEFORE UPDATE OR DELETE ON fortune_card_entries
    FOR EACH ROW EXECUTE FUNCTION prevent_fortune_card_entry_change();

-- ============================================================
-- 退款期间的福卡冻结
-- ============================================================

-- 背景：一单的福卡发下去之后，用户申请退款，那些福卡今天照旧能拿去抽奖。等退款真的成功
-- 再去追回时，「已经抽过奖的福卡追不回来」——钱退了，卡也花了。所以从**用户提交退款申请
-- 的那一刻**起，这一单的福卡冻结，不可用于抽奖；申请被驳回或用户撤销后自动解冻。
--
-- 冻结**不是账变**：余额、流水、balance_after 一律不动。它改的是「可用」——
--   available = balance - frozen_balance
-- 所以这里没有新的 entry_type，fortune_card_entries 一个字节都不用改，扣减的判据则从
-- 「余额够不够」变成「可用够不够」。
--
-- 冻结粒度跟着退款范围走（scope=drink 冻基础赠送那张，scope=addon 冻加购加赠那张），
-- 所以冻结行上记的是**一组发放幂等键**而不是一个金额：金额要从流水里现算。
--
-- 冻结行本身**不是账本**，是可改的状态行（status/amount 会变），所以不加只追加触发器——
-- 那条触发器是给钱的。
--
-- 后来补上的后半段：**退款成功 ⇒ 解冻 + 冲正这几笔发放**。原来的「退款成功这一拍什么都
-- 不做」会让走到 refunded 的售后单永远停在 frozen，卡继续留在用户账上——钱退了，赠品
-- 没退。为什么不是把 status 直接写成 released：解冻与追回在账上是**相反**的两件事，
-- 解冻只是「这些卡又能抽了」，余额一分不动；追回是「这些卡收回去了」，余额真的少了。
-- 用同一个取值会让「已解冻」这一格在页面上同时指两件事，而这一格正是客服与审计照着回答
-- 问题的那个词。
--
-- 也顺手补上「退款失败 ⇒ 解冻」缺的那一半（原来那条路也漏着）：失败之后用户要重新申请，
-- 而重新申请时在途冻结已经把可用吃光，新冻结行只冻得到 0 张——第二次退款成功时一张卡都
-- 追不回来。那个洞不需要新的状态，走的就是 released。

-- 一张售后申请一行。after_sale_no 是幂等键：同一条 applied 事件重投多少次，冻结只记一次。
--
-- 为什么按售后单而不是按流水行：冻结的起因、生命周期、解冻时点都由那张售后单决定，
-- 按它建行才能让「解冻」这件事有一个明确的、与服务调用顺序无关的落点。
CREATE TABLE fortune_card_freezes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- 冻结归属的账户。外键指向本库自己的账户行，所以这一单没承诺福卡也要先把账户行懒创建
    -- 出来——不然一个「从没收到过福卡、但申请了退款」的订单会把冻结挡在门外。
    user_id UUID NOT NULL REFERENCES fortune_card_accounts(user_id) ON DELETE RESTRICT,
    -- 售后单号，跨库值引用（售后单在 order 库）。幂等键。
    after_sale_no TEXT NOT NULL UNIQUE CHECK (char_length(trim(after_sale_no)) > 0),
    order_id TEXT NOT NULL,
    order_no TEXT NOT NULL,
    -- 被冻的那几笔发放的幂等键，形状与 fortune_card_entries.entry_key 一致：
    --   order:{orderId}:base / order:{orderId}:bonus:{campaignId}
    -- 记键而不是记流水 ID：申请可能早于发放（paid 状态就允许申请退款），那时流水还不存在，
    -- 但键已经定了——发放落库时回头按这些键把张数补进来。
    entry_keys TEXT[] NOT NULL,
    -- 实际冻住的张数，从流水里现算。允许为 0：申请早于发放、或者这张卡已经被抽掉了
    -- （那正是「追不回来」在余额上的样子）。金额保留在行上，解冻不改它，审计要看当初冻了多少。
    amount BIGINT NOT NULL DEFAULT 0 CHECK (amount >= 0),
    -- frozen = 冻结中，released = 已解冻（驳回或用户撤销），recovered = 已追回（退款成功）。
    -- 约束名是内联 CHECK 的自动命名（已在本库 pg_constraint 里核过），不是新起的名字。
    status TEXT NOT NULL CHECK (status IN ('frozen', 'released', 'recovered')),
    -- 冻结/解冻的原因文案，来自事件或服务侧渲染。
    reason TEXT NOT NULL DEFAULT '',
    -- 业务发生时间（申请时刻），用事件的 occurredAt 而不是 NOW()：补投旧事件时明细顺序不变。
    occurred_at TIMESTAMPTZ NOT NULL,
    released_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- 与 released_at 分成两列而不是共用一列：两件事发生在不同的时刻、由不同的事件驱动，
    -- 共用会让「解冻时间」这一格在追回的行上显示一个不是解冻的时间。
    recovered_at TIMESTAMPTZ
);

-- ============================================================
-- 咖啡豆账户（方案 5.6 的另一半）
-- ============================================================

-- 福卡那一半建表时留着一句「咖啡豆账户等出货链路（payment 的 action=account）落地再建」，
-- 咖啡豆这一半就是那一天：payment-service 的账户出资今天起会真的扣减这里的余额。
--
-- ⚠️ 先说清楚这一半与福卡的**一处根本不同**：福卡不是出资渠道、没有货币金额，那句话只对
-- 福卡成立。**咖啡豆是货币金额，单位是分**，与 orders.payable_amount 同单位。所以这张表
-- 上没有「张数」，只有钱；payment_fundings.line_type='coffee_bean' 与
-- order_payment_lines.line_type='coffee_bean' 指的就是这里的账变。
--
-- ⚠️ 再说清楚与**另一个「咖啡余额」**的区别，这是最容易被人搞混的地方：
-- panda_coffee_machine.devices.coffee_balance（注释「咖啡余额，单位为分」）配
-- device_balance_ledger，是**设备维度**的预付余额，按 device_id 记账、没有 user_id，老系统里
-- 是「每次打咖啡扣机器余额」。这里建的是**用户维度**的账户，键是 user_id，两者没有任何
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
    -- ⚠️ operator_name 恒为空串（令牌里没有用户名），别按它去 join；见下面这列的 COMMENT ON。
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
CREATE INDEX fortune_card_entries_user_idx
    ON fortune_card_entries (user_id, occurred_at DESC, id);

-- 订单详情那个「福卡」tab：按订单号一次取全。
CREATE INDEX fortune_card_entries_reference_no_idx
    ON fortune_card_entries (reference_no)
    WHERE reference_no <> '';

-- 幂等：同一个键只允许一条流水。重投一条 order.completed、重试一次扣减，都撞在这上面。
CREATE UNIQUE INDEX fortune_card_entries_key_unique
    ON fortune_card_entries (entry_key);

-- 订单详情那个「福卡」页签：按订单号取全这一单的冻结。
CREATE INDEX fortune_card_freezes_order_no_idx
    ON fortune_card_freezes (order_no);

-- 发放时的反查：「刚发的这条流水有没有被哪张售后单冻着」。发放是低频写路径，
-- 但它是每个订单完成都要走的一次查询，用 GIN 而不是全表扫。
CREATE INDEX fortune_card_freezes_entry_keys_idx
    ON fortune_card_freezes USING GIN (entry_keys);

-- 客服页：一个用户当前冻着哪些。部分索引只服务 frozen 那部分——解冻行只进审计，不进这里。
CREATE INDEX fortune_card_freezes_user_frozen_idx
    ON fortune_card_freezes (user_id, occurred_at DESC)
    WHERE status = 'frozen';

-- 明细页：一个用户的流水按时间倒序。含 id 是为了同毫秒的两笔也有稳定顺序。
CREATE INDEX coffee_bean_entries_user_idx
    ON coffee_bean_entries (user_id, occurred_at DESC, id);

-- 幂等：同一个键只允许一条流水。重发一次后台调整、重试一次扣减、重投一条
-- order.after_sale.reviewed，都撞在这上面。
CREATE UNIQUE INDEX coffee_bean_entries_key_unique
    ON coffee_bean_entries (entry_key);

-- ============================================================
-- 平台样板：消息 outbox / inbox
-- ============================================================

-- 与其它各集的同名两表逐列一致，各库自带一份，跨库不共享表。
--
-- outbox 对福卡发放不是可选件：state 变更除了写流水，还要把「谁在什么时候把余额调成了
-- 多少」留痕（方案 11.6 L893 的人工干预必审清单），而审计记录的写法（platform/audit）
-- 就是「在业务事务内往自己的 outbox 追加一条 admin.operation.logged」，由 relay 投到
-- RabbitMQ、再落到身份库的 admin_operation_logs。
--
-- 这里是全新库，lease 列直接建在表里，所以不需要 identity 那一集那组
-- ALTER TABLE ADD COLUMN IF NOT EXISTS（那组是为了升级早期 schema 建出来的表）。
-- 列名和顺序保持不变，各份拷贝才能逐列对得起来。

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

-- relay 取待投递消息、以及续租，都只看未发布的行。
CREATE INDEX message_outbox_pending_idx
    ON message_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;

CREATE INDEX message_outbox_lease_idx
    ON message_outbox (lease_until)
    WHERE published_at IS NULL;

-- <<< message-tables:end <<<

-- ============================================================
-- 表与关键字段的中文注释
--
-- 放在同一个文件里而不是单开一份 _comments：合并之后每个集只有这一个文件，注释和它描述
-- 的列不会再出现「改了列没改注释」这种两个文件各说各话的情况。
-- ============================================================

COMMENT ON TABLE fortune_card_accounts IS '福卡余额账户，一个用户一行，第一次发放时懒创建；余额与流水在同一次事务里更新';
COMMENT ON COLUMN fortune_card_accounts.user_id IS '小程序用户 ID，属于身份服务时仅作跨库值引用';
COMMENT ON COLUMN fortune_card_accounts.balance IS '可用福卡张数；等于该用户全部流水 amount 之和，存下来是为了只读查询与「余额不会被扣穿」这条 CHECK——只有一列能被约束，流水表上没法表达这个不变式';
COMMENT ON COLUMN fortune_card_accounts.created_at IS '账户创建时间（第一次发放到账的时间）';
COMMENT ON COLUMN fortune_card_accounts.updated_at IS '最近一次余额变动时间';
COMMENT ON COLUMN fortune_card_accounts.frozen_balance IS '退款冻结中的福卡张数；可用张数 = balance - frozen_balance，不落库。冻结不是账变，它改的是「可用」，balance 与流水都不动';

COMMENT ON TABLE fortune_card_entries IS '福卡收支流水，只允许追加：冲正是新增反向记录，不原地改数';
COMMENT ON COLUMN fortune_card_entries.user_id IS '账户归属用户，指向本库的 fortune_card_accounts';
COMMENT ON COLUMN fortune_card_entries.entry_type IS '账变类型：grant=发放（订单完成赠送），draw=抽奖扣减，reverse=冲正';
COMMENT ON COLUMN fortune_card_entries.amount IS '变动张数，有符号：发放为正，扣减为负，冲正与它冲掉的那笔相反';
COMMENT ON COLUMN fortune_card_entries.balance_after IS '变动后余额；明细页显示的就是它，冲正重放时也靠它回答「当时是多少」';
COMMENT ON COLUMN fortune_card_entries.title IS '这一行在用户那边显示成什么（订单完成赠送、参与抽奖等）；文案由账户服务拥有，调用方给的是起因不是文案';
COMMENT ON COLUMN fortune_card_entries.reference_type IS '账变起因的对象类型：order=订单，draw=抽奖参与，entry=冲正指向的流水；本列记的是「为什么余额变了」，与事件日志不是一回事';
COMMENT ON COLUMN fortune_card_entries.reference_id IS '账变起因的对象 ID，仅作跨库值引用';
COMMENT ON COLUMN fortune_card_entries.reference_no IS '账变起因的对外单号（订单号），供客服与订单详情页直接检索';
COMMENT ON COLUMN fortune_card_entries.entry_key IS '幂等键，全局唯一；三种形状：order:{orderId}:base 与 order:{orderId}:bonus:{campaignId}（发放）、draw:{requestId}（扣减）、reverse:{entryId}（冲正）';
COMMENT ON COLUMN fortune_card_entries.reverses_entry_id IS '冲正对应的原始流水 ID，指向本表；非空当且仅当 entry_type=reverse';
COMMENT ON COLUMN fortune_card_entries.remark IS '备注';
COMMENT ON COLUMN fortune_card_entries.occurred_at IS '业务发生时间；发放用订单的完成时间而不是 NOW()，补投或重放旧事件才不会把上周的单记成刚才';
COMMENT ON COLUMN fortune_card_entries.created_at IS '入库时间';

COMMENT ON TABLE fortune_card_freezes IS '退款申请期间的福卡冻结，一张售后单一行；冻的是「可用」不是余额。三种结局：驳回或用户撤销 ⇒ released（解冻，余额不动）、退款失败 ⇒ released（解冻，余额不动）、退款成功 ⇒ recovered（先解冻再冲正那几笔发放，余额真的少了）';
COMMENT ON COLUMN fortune_card_freezes.user_id IS '冻结归属的账户，指向本库的 fortune_card_accounts；账户行在该用户第一次收到福卡或第一次被冻结时懒创建';
COMMENT ON COLUMN fortune_card_freezes.after_sale_no IS '售后单号，跨库值引用（售后单在 order 库）；幂等键，同一条申请事件重投多少次都只记一行';
COMMENT ON COLUMN fortune_card_freezes.order_id IS '订单 ID，跨库值引用';
COMMENT ON COLUMN fortune_card_freezes.order_no IS '订单号，供客服与订单详情页直接检索';
COMMENT ON COLUMN fortune_card_freezes.entry_keys IS '被冻的那几笔发放的幂等键（order:{orderId}:base / order:{orderId}:bonus:{campaignId}），由订单域按退款范围拆好；记键而不是记流水 ID，因为申请可能早于发放';
COMMENT ON COLUMN fortune_card_freezes.amount IS '实际冻住的张数，从流水现算；允许为 0（申请早于发放，或这张卡已经被抽掉）。解冻与追回都不改它——追回冲正的正是这个数，审计要在一行里看见「当初冻了多少」';
COMMENT ON COLUMN fortune_card_freezes.status IS '冻结状态：frozen=冻结中，released=已解冻（申请被驳回、用户撤销、或退款失败），recovered=已追回（退款成功，那几笔发放已被冲正、钱收了回去）';
COMMENT ON COLUMN fortune_card_freezes.reason IS '冻结或解冻的原因文案';
COMMENT ON COLUMN fortune_card_freezes.occurred_at IS '业务发生时间（申请时刻）；用事件的时刻而不是 NOW()，补投旧事件时明细顺序才不变';
COMMENT ON COLUMN fortune_card_freezes.released_at IS '解冻时间（驳回 / 用户撤销 / 退款失败）；未解冻为空';
COMMENT ON COLUMN fortune_card_freezes.created_at IS '入库时间';
COMMENT ON COLUMN fortune_card_freezes.updated_at IS '最近一次状态变更时间（发放联动补冻也会改它）';
COMMENT ON COLUMN fortune_card_freezes.recovered_at IS '追回时间（退款成功）；未追回为空。与 released_at 互斥：一条冻结行只会走到两者之一';

COMMENT ON TABLE coffee_bean_accounts IS '用户维度的咖啡豆账户（方案 5.6 的账户余额那一半），一个用户一行、懒创建；单位为分。与 panda_coffee_machine.devices.coffee_balance（设备维度的预付余额）无关';
COMMENT ON COLUMN coffee_bean_accounts.user_id IS '账户归属的用户，跨库值引用（用户在身份库）；本库不建跨库外键';
COMMENT ON COLUMN coffee_bean_accounts.balance IS '咖啡豆余额，单位为分，与 orders.payable_amount 同单位；是流水求和的结果，与前一条流水在同一次事务里更新。CHECK (balance >= 0) 是扣减与负数调整的最后一道防线';
COMMENT ON COLUMN coffee_bean_accounts.created_at IS '账户行创建时间（第一次调整或第一次扣减时）';
COMMENT ON COLUMN coffee_bean_accounts.updated_at IS '最近一次余额变动时间';

COMMENT ON TABLE coffee_bean_entries IS '咖啡豆账变流水，只允许追加；冲正是新增一条反向记录而不是原地改数。没有冻结、没有过期';
COMMENT ON COLUMN coffee_bean_entries.entry_type IS '账变类型：adjust=后台人工调整（充值或纠错），consume=纯豆出资扣减，reverse=冲正';
COMMENT ON COLUMN coffee_bean_entries.amount IS '变动额，单位分，有符号：充值为正、纠错为负、扣减为负、冲正与它冲掉的那笔相反';
COMMENT ON COLUMN coffee_bean_entries.balance_after IS '变动后余额，明细页显示的就是它';
COMMENT ON COLUMN coffee_bean_entries.title IS '这一行在人那边显示成什么（后台充值 / 咖啡豆支付 / 退款冲正），文案由本服务拥有';
COMMENT ON COLUMN coffee_bean_entries.reference_type IS '起因对象的类型：manual=后台调整，order=纯豆出资扣减，after_sale=退款冲正';
COMMENT ON COLUMN coffee_bean_entries.reference_id IS '起因对象的 ID，跨库值引用（订单 ID / 售后单 ID）';
COMMENT ON COLUMN coffee_bean_entries.reference_no IS '给人看的号：调整的幂等号、订单号、售后单号';
COMMENT ON COLUMN coffee_bean_entries.entry_key IS '幂等键，唯一索引 coffee_bean_entries_key_unique。三种形状：adjust:{requestId}（后台调整）、order:{orderId}（纯豆扣减，用订单 ID 是因为冲正要按订单反查）、after_sale:{afterSaleNo}（冲正，一条售后只冲一次）';
COMMENT ON COLUMN coffee_bean_entries.reverses_entry_id IS '被冲正的那条流水，只有 entry_type=reverse 有值；与 amount 的符号一起构成「冲正不原地改数」';
COMMENT ON COLUMN coffee_bean_entries.operator_id IS '后台调整的操作人 ID，只对 entry_type=adjust 有值；是审计之外的第二处留痕';
COMMENT ON COLUMN coffee_bean_entries.operator_name IS '后台调整的操作人显示名。今天恒为空串，与 device_balance_ledger.operator_name 一样：令牌里没有用户名，展示名由读侧按 operator_id 去身份库解析';
COMMENT ON COLUMN coffee_bean_entries.remark IS '备注；后台调整时是操作人填的理由';
COMMENT ON COLUMN coffee_bean_entries.occurred_at IS '业务发生时间：调整用请求时刻，扣减用支付时刻，冲正用审核时刻；用事件时刻而不是 NOW()，补投旧事件时明细顺序才不变';
COMMENT ON COLUMN coffee_bean_entries.created_at IS '入库时间';

COMMENT ON TABLE message_outbox IS '账户服务待发布消息事件';
COMMENT ON COLUMN message_outbox.event_id IS '事件唯一 ID';
COMMENT ON COLUMN message_outbox.event_type IS '事件类型';
COMMENT ON COLUMN message_outbox.event_version IS '事件版本';
COMMENT ON COLUMN message_outbox.payload IS '事件内容二进制数据';
COMMENT ON COLUMN message_outbox.attempts IS '发布尝试次数';
COMMENT ON COLUMN message_outbox.published_at IS '成功发布时间';
COMMENT ON COLUMN message_outbox.lease_until IS '消息处理租约截止时间';

COMMENT ON TABLE message_inbox IS '账户服务已接收消息事件及消费租约';
COMMENT ON COLUMN message_inbox.event_id IS '已接收事件唯一 ID，用于消费去重';
COMMENT ON COLUMN message_inbox.claimed_at IS '首次领取消费时间';
COMMENT ON COLUMN message_inbox.lease_until IS '消息消费租约截止时间';
COMMENT ON COLUMN message_inbox.completed_at IS '成功完成消费时间';

COMMIT;
