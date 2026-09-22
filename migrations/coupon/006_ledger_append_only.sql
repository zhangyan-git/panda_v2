-- coupon/006：给 coupon_state_transitions 与 coupon_inventory_ledger 补只增触发器
--
-- 背景：这两张表是优惠券域的账本。coupon_state_transitions 记的是「这张券/批次/模板
-- 从什么状态变成了什么状态、谁推的」；coupon_inventory_ledger 记的是「模板或批次的
-- 可用量为什么变了」。它们不是缓存，是事实来源——幂等重放、对账、审计都拿它们当依据。
--
-- 全仓另外 10 张同性质的账本都在建表时就带了 BEFORE UPDATE OR DELETE 的只增触发器：
-- device_balance_ledger、order_state_transitions、payment_transactions、
-- payment_state_transitions、stock_movements、fortune_card_entries、
-- coffee_bean_entries、lottery_draws、lottery_win_events、membership_changes。
-- coupon 这两张没有——本域此前唯一的触发器是 001 里那条 coupon_types_code_immutable。
--
-- 少了这条防线不是「少一层保险」，是账本可以被改写、被删空。本域没有 before-image、
-- 没有备份，删掉一条状态迁移就再也拼不回来；而它恰好是事后回答「这张券什么时候、
-- 被谁停掉的」唯一的地方。与那 10 张表同一套写法：物理阻断，不靠服务层自觉。
--
-- 加了什么：两条触发函数 + 两条触发器，一表一条。函数名与报错措辞照抄同类表的命名
-- 习惯（coupon_state_transitions 对 order_state_transitions / payment_state_transitions，
-- coupon_inventory_ledger 对 device_balance_ledger）：触发器叫 <表名>_append_only，
-- 函数叫 prevent_<表意>_change，报错是一句 "… is append-only"。
--
-- 现状是安全的：production 代码对这两张表只有 INSERT（postgres.go 的核销/作废/发券/
-- 后台调状态，template.go 的审核），没有一处 UPDATE 或 DELETE。
--
-- 为什么不改 001：001 已经在若干库上 apply 过（schema_migrations 里有记录），改一个
-- 已 apply 的文件的 DDL 会让「库里的形状」和「文件描述的库」对不上。新库从头跑
-- 001→006 拿到的结果与老库补跑 006 完全一致。
--
-- 回滚：本仓的迁移是单向的——runner 只 apply，不了解任何 goose 指令，连带 Down 段的
-- 文件都会当场拒收（migrations_test.go 的 TestSetsAreUsable 也在守这条）。所以这里
-- 没有配套的 down 文件。真要撤销只能手工执行：
--
--     DROP TRIGGER coupon_state_transitions_append_only ON coupon_state_transitions;
--     DROP TRIGGER coupon_inventory_ledger_append_only ON coupon_inventory_ledger;
--     DROP FUNCTION prevent_coupon_transition_change();
--     DROP FUNCTION prevent_coupon_inventory_ledger_change();
--
-- 但那是**放开**保护，只有在确认就是要改写账本时才做。
--
-- 连带改动：repository/postgres_integration_test.go 的 fixture 清理原本直接
-- `DELETE FROM coupon_state_transitions`，触发器建起来之后那句会被当场拒掉。那里已
-- 改成在事务里先 DISABLE TRIGGER 再删再 ENABLE（与 membership_changes 的测试清理同一
-- 个意图：删的只是用例自己刚写进去的行）。

-- coupon_state_transitions：券的状态迁移历史
CREATE FUNCTION prevent_coupon_transition_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'coupon state transitions are append-only';
END;
$$;

CREATE TRIGGER coupon_state_transitions_append_only
    BEFORE UPDATE OR DELETE ON coupon_state_transitions
    FOR EACH ROW EXECUTE FUNCTION prevent_coupon_transition_change();

-- coupon_inventory_ledger：库存变动流水
CREATE FUNCTION prevent_coupon_inventory_ledger_change()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'coupon inventory ledger is append-only';
END;
$$;

CREATE TRIGGER coupon_inventory_ledger_append_only
    BEFORE UPDATE OR DELETE ON coupon_inventory_ledger
    FOR EACH ROW EXECUTE FUNCTION prevent_coupon_inventory_ledger_change();
