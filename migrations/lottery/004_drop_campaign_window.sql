-- 抽奖不再有时间窗口：开奖只由「收满门槛」触发，没满就一直等着。
--
-- 001 给活动配了 start_at / end_at，给期次配了 starts_at / ends_at（后者一律等于活动的
-- end_at），并由此长出三条规则：期次到点由 worker 扫出来开奖、窗口过了活动自动置 ended、
-- 「拿一个窗口已过的活动去启用」报 409。这套东西运营侧看下来是多余的，而且和真实心理模型
-- 相反——说好「一期收满 10 次就开奖」，就该收满才开；设一个截止时间只会制造出「10 次没到
-- 也开了奖」和「窗口一过活动自己结束」这两种没人预期过的结果。
--
-- 拿掉之后，一期只有三条出路：收满门槛转 closed 并开奖、管理员人工开奖、管理员作废。
-- **期次可以一直开着是有意的**，不是兜底——所以模型层那段「到点必开，不流局」的注释也一并
-- 作废（见 model.Round.AwaitingDraw）。零人参与 / 长期没人参与的活动，出口是人工作废，
-- 作废会在同一事务里开出下一期（repository.CancelRound）。
--
-- 两处顺带：
--   * starts_at 原本写的就是 NOW()，与 created_at 同值，删它不丢信息；扫描改按 created_at
--     排序，所以索引也跟着换成 (status, created_at)。
--   * lottery_draws.trigger 里的 'deadline' 从此不会有新行产生。查过存量：
--     `SELECT count(*) FROM lottery_draws WHERE trigger='deadline'` = 0（全表 0 行），
--     所以直接把该取值从 CHECK 里收窄，而不是留一个永远不会出现的幽灵取值。
--
-- 执行前已把 lottery_campaigns / lottery_rounds / lottery_draws 的现存行整表 dump 到
-- /tmp/panda-run/lottery-window-drop/20260915-182059/（3 / 3 / 0 行）。DROP COLUMN 不可逆，
-- dev 库没有备份，这份 before-image 是唯一的退路。
--
-- 没有 down 段：本仓所有迁移都是单向下发（见 migrations/migrations.go 的包注释）。

-- CHECK (end_at > start_at) / CHECK (ends_at > starts_at) 会随列一起被 PostgreSQL 删掉，
-- 不需要单独 DROP CONSTRAINT。**索引同理**：lottery_rounds_sweep_idx 建在 (status, ends_at)
-- 上，删 ends_at 那一句就把它带走了，所以下面是先删列再按新列重建，中间没有 DROP INDEX——
-- 写了那一句反而会撞上「index does not exist」（这条迁移的第一版就是这么挂的）。
ALTER TABLE lottery_rounds DROP COLUMN ends_at, DROP COLUMN starts_at;
ALTER TABLE lottery_campaigns DROP COLUMN end_at, DROP COLUMN start_at;

-- 开奖 worker 的扫描索引：现在只有「已达门槛（closed）」一条路，排序改成按开期先后。
CREATE INDEX lottery_rounds_sweep_idx ON lottery_rounds (status, created_at);

COMMENT ON TABLE lottery_rounds IS '活动下面滚动开的一期一期（原型里的 roundNo）；收满门槛开奖，开奖后同一事务开下一期，一直滚下去';
COMMENT ON COLUMN lottery_rounds.created_at IS '开期时刻；期次原本还有一个 starts_at，写的就是这个值，已随窗口一起删除';
COMMENT ON COLUMN lottery_rounds.participant_target IS '开奖门槛，开期时从活动冻结；**数的是参与次数**（同一个人可以参与多次），达到它就转 closed';

COMMENT ON COLUMN lottery_campaigns.participant_target IS '新期次的默认开奖门槛（原型 threshold）；**数的是参与次数**，开期时冻结到期次上，之后改活动不影响正在跑的那一期';

-- 'deadline' 从此没有产生方，从闭集里删掉；存量已确认为 0 行。
ALTER TABLE lottery_draws DROP CONSTRAINT lottery_draws_trigger_check;
ALTER TABLE lottery_draws ADD CONSTRAINT lottery_draws_trigger_check
    CHECK (trigger IN ('threshold', 'manual'));
COMMENT ON COLUMN lottery_draws.trigger IS '触发条件：threshold=收满门槛，manual=人工；与 mode 一一对应';
