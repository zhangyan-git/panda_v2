-- account/001：资产账户领域基础表结构
--
-- 属于 panda_account（account-service，方案 5.6）。本轮只落福卡账户那一半：
--   1. 福卡余额账户                     → fortune_card_accounts
--   2. 福卡收支流水（只允许追加、可冲正）  → fortune_card_entries
--
-- 咖啡豆账户是方案 5.6 的另一半，等出货链路（payment 的 action=account）落地再建，
-- 本轮不在这个库里留一张空表——空表不会告诉后来的人任何事，注释会。
--
-- 用户、订单、抽奖活动、优惠券都不是本库的。外部服务的 ID 只作为值引用保存，本库不建
-- 跨数据库外键；本文件里的 REFERENCES 只指向本库自己的表。
--
-- 表形状不是新发明的：余额列 + 只追加流水 + balance_after + 幂等号 + 冲正自引用，
-- 与 coffee_machine/001 的 device_balance_ledger 是同一套东西（设备咖啡余额 vs 用户
-- 福卡余额）。那边踩过、这边就照着建。
--
-- 末尾另带平台样板的一对 message_outbox / message_inbox（各库自带一份）。
--
-- 张数是整数（INT 语义），本库没有货币金额——福卡不是出资渠道（order/003、
-- payment/004 已把两侧词表收窄），它只换抽奖参与次数。

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
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
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
-- 平台样板：消息 outbox / inbox
-- ============================================================

-- 与 identity/003、merchant/002、coupon/001、coffee_machine/001 里的同名两表逐列一致，
-- 各库自带一份，跨库不共享表。
--
-- outbox 对福卡发放不是可选件：state 变更除了写流水，还要把「谁在什么时候把余额调成了
-- 多少」留痕（方案 11.6 L893 的人工干预必审清单），而审计记录的写法（platform/audit）
-- 就是「在业务事务内往自己的 outbox 追加一条 admin.operation.logged」，由 relay 投到
-- RabbitMQ、再落到身份库的 admin_operation_logs。
--
-- 这里是全新库，lease 列直接建在表里，所以不需要 identity/003 那组
-- ALTER TABLE ADD COLUMN IF NOT EXISTS（那组是为了升级早期 schema 建出来的表）。
-- 列名和顺序保持不变，六份拷贝才能逐列对得起来。

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

-- relay 取待投递消息、以及续租，都只看未发布的行。
CREATE INDEX message_outbox_pending_idx
    ON message_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;

CREATE INDEX message_outbox_lease_idx
    ON message_outbox (lease_until)
    WHERE published_at IS NULL;
