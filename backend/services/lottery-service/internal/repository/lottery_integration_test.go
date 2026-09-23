package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/draw"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
)

// 这一份打的是真实的 dev PG，量与方案验证第 3 条对齐：并发确认不丢更新、一期只开一次奖、
// 种子 + 参与集合能重算、补偿路径标 failed 而不是把参与算进去。
//
// **跑完不清理**：lottery_draws / lottery_win_events 有 append-only 触发器，DELETE 会直接
// 抛；lottery_wins 又被 lottery_draws 以 ON DELETE RESTRICT 引用着。这是那几张表的性质，
// 不是漏删——要把它们清掉只能整库重建。夹具 id 每次都是新的（随机门店、随机活动短名），
// 所以残留既不会影响下一次运行，也不会让断言互相串台。
//
// **这一份不验扣卡。** 福卡余额在 account-service，扣与冲正在那里发生；这里验的是
// 「本地三段事务在扣卡成功 / 失败 / 没结论这三种情况下各自留下什么」。真实的跨服务那一段
// 要走起来才看得见，那是方案验证第 4 条的事。
func lotteryIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LOTTERY_DATABASE_URL")
	if url == "" {
		url = os.Getenv("TEST_DATABASE_URL")
	}
	if url == "" {
		t.Skip("set LOTTERY_DATABASE_URL or TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("lottery test database is unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("lottery test database is unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newCampaignCode 造一个符合 ^[A-Z0-9]{1,16}$ 的活动短名。
//
// 随机而不是固定值：短名是全局唯一的（它是期次号的前缀），固定值会让第二次运行时
// 开通直接撞 lottery_campaigns_code_unique。
func newCampaignCode() string {
	return "LT" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
}

// lotteryFixture 是一家开通过抽奖的门店，连带它的默认活动与第一期。
//
// 用 Activate 而不是「建活动再开期」：开通那条路本身就是「三行记录同一个事务」，
// 用它做夹具顺带把那条路径跑通了。
type lotteryFixture struct {
	repo       *PostgresRepository
	pool       *pgxpool.Pool
	activation *model.Activation
	campaign   *model.Campaign
	round      *model.Round
}

// newLotteryFixture 开一家新门店，门槛 target 次、奖品带 prizeQuantity 个名额。
//
// 这里**没有任何时间参数**：活动与期次都没有窗口了（2026-09-15 去掉），一期收满门槛才
// 开奖，不收满就一直开着。
//
// prizeQuantity 仍然是个参数，虽然 service 层恒填 1（见 campaignParams 里那一行）——**故意
// 留着的**：让 winner_count 冻结成一个大等于 1 的数，才能验出「它确实是从奖池求和来的」。
// 全填 1 的话，「冻结」写成一个写死的 1 也照样绿。仓储这一层本来也还认这个字段。
func newLotteryFixture(t *testing.T, target, prizeQuantity int32) *lotteryFixture {
	t.Helper()
	pool := lotteryIntegrationPool(t)
	repo := NewPostgresRepository(pool, nil)
	ctx := testContext(t)

	created, err := repo.Activate(ctx, ActivateParams{
		// 只给门店 id：名字不落库（见 migrations/lottery），显示时由 service 层向商户域
		// 现解。所以这一层的夹具拿到的门店是一个商户域里并不存在的随机 UUID——这对仓储没
		// 影响（它只写 id），但**它正是那个「幽灵门店」的形状**，存在性校验在服务层。
		LocationID: uuid.NewString(),
		Remark:     "integration test",
		Campaign: DefaultCampaign{
			Code:              newCampaignCode(),
			Name:              "集成测试活动",
			Description:       "integration test",
			ParticipantTarget: target,
			Prize: DefaultPrize{
				Name:     "10 元咖啡兑换券",
				Quantity: prizeQuantity,
			},
		},
	})
	if err != nil {
		t.Fatalf("activate a test location: %v", err)
	}
	if created.Activation == nil || created.Campaign == nil || created.Round == nil {
		t.Fatal("开通应当一次建出开通记录、默认活动与第一期三样东西")
	}
	return &lotteryFixture{
		repo: repo, pool: pool,
		activation: created.Activation, campaign: created.Campaign, round: created.Round,
	}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// participate 走一遍「记下意图 → 确认」两段本地事务。
func (f *lotteryFixture) participate(t *testing.T, ctx context.Context, roundID, userID, key string) *model.Participation {
	t.Helper()
	begun, err := f.repo.Begin(ctx, BeginParams{
		RoundID: roundID, UserID: userID, IdempotencyKey: key, Cost: 1, Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("begin a participation: %v", err)
	}
	if _, _, err := f.repo.Confirm(ctx, ConfirmParams{
		ParticipationID: begun.Participation.ID, FortuneEntryID: uuid.NewString(),
	}); err != nil {
		t.Fatalf("confirm a participation: %v", err)
	}
	return begun.Participation
}

// seedParticipations 造 n 个不同用户的已确认参与，返回他们的参与 id。
func (f *lotteryFixture) seedParticipations(t *testing.T, ctx context.Context, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		p := f.participate(t, ctx, f.round.ID, uuid.NewString(),
			fmt.Sprintf("test:%s:%d", f.round.ID, i))
		ids = append(ids, p.ID)
	}
	return ids
}

// ——— 断言用的小工具 ———

// roundOf 读回一期。断言一律重新读库，不用内存里那份：这一份测试要验的正是「写下去之后
// 库里到底是什么」，拿着调用点返回的结构体断言等于在验返回值拼得对不对。
func roundOf(t *testing.T, ctx context.Context, f *lotteryFixture, id string) *model.Round {
	t.Helper()
	round, err := f.repo.GetRound(ctx, id)
	if err != nil {
		t.Fatalf("read round %s: %v", id, err)
	}
	return round
}

// confirmedIDs 按期次读已确认的参与 id，**与开奖用的那段查询逐字一致**（ORDER BY id）。
//
// 逐字一致是这份重算的全部意义：排序不同就等于换了一份输入，那样即使名单对得上也证明不了
// 第三方能复核。
func confirmedIDs(t *testing.T, ctx context.Context, f *lotteryFixture, roundID string) []string {
	t.Helper()
	rows, err := f.pool.Query(ctx, `SELECT id::text FROM lottery_participations
		WHERE round_id=$1 AND status='confirmed' ORDER BY id`, roundID)
	if err != nil {
		t.Fatalf("read confirmed participations: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan a confirmed participation: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read confirmed participations: %v", err)
	}
	return ids
}

// assertCountInvariant 验那条把滚动计数与参与记录绑在一起的不变式。
//
// 它就是本库的 participant_count 版本的「SUM(amount) = balance」：种子与开奖记录里的
// 参与数取的都是这个数，一旦它与参与记录的实际条数不一致，算出来的种子不属于任何一份
// 可复核的名单，而那种错误事后查不出来。所以每一处断言计数的地方都同时验它。
func assertCountInvariant(t *testing.T, ctx context.Context, f *lotteryFixture, roundID string) *model.Round {
	t.Helper()
	round := roundOf(t, ctx, f, roundID)
	var actual int32
	if err := f.pool.QueryRow(ctx, `SELECT COUNT(*)::int FROM lottery_participations
		WHERE round_id=$1 AND status='confirmed'`, roundID).Scan(&actual); err != nil {
		t.Fatalf("count confirmed participations: %v", err)
	}
	if round.ParticipantCount != actual {
		t.Fatalf("期次 %s 的 participant_count 是 %d，参与记录里有 %d 条 confirmed",
			roundID, round.ParticipantCount, actual)
	}
	return round
}

func countRows(t *testing.T, ctx context.Context, f *lotteryFixture, sql string, args ...any) int {
	t.Helper()
	var count int
	if err := f.pool.QueryRow(ctx, sql, args...).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return count
}

// cancelRoundDirectly 绕过 CancelRound 直接把一期置成 cancelled。
//
// **CancelRound 现在会补开下一期**（见 TestCancelRoundOpensTheNextRound），所以它造不出
// 「活动 enabled、却没有在跑的期次」这个状态，而那个状态在真实系统里是存在的——暂停期间
// 开奖不开新期，恢复时才由 EnsureLiveRound 补。这一条给它用。
func cancelRoundDirectly(t *testing.T, ctx context.Context, f *lotteryFixture, roundID string) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, `UPDATE lottery_rounds
		SET status='cancelled', cancelled_at=NOW(), cancel_reason='fixture'
		WHERE id=$1`, roundID); err != nil {
		t.Fatalf("cancel round %s directly: %v", roundID, err)
	}
}

// ——— 用例 ———

// TestConcurrentConfirmsDoNotLoseUpdatesAndCloseTheRoundAtTarget 是那个并发序列化点的
// 直接验证：24 个人同时挤一扇只放 8 个人的门。
//
// 要验的是两件事，它们是同一个 UPDATE 的两面：
//   - 计数不丢更新（`participant_count = participant_count + 1` 在行锁下重读最新版本再算）；
//   - 达标那一次把期次置 closed，从此不再收人。
//
// 三种结局都要能解释：确认成功、在入口就被关上的门挡掉（Begin 看到 closed）、以及
// 「扣卡飞行途中门关了」——第三种要走补偿，参与记录标 failed 而不计入期次。
func TestConcurrentConfirmsDoNotLoseUpdatesAndCloseTheRoundAtTarget(t *testing.T) {
	const target, attempts = 8, 24
	f := newLotteryFixture(t, target, 3)
	ctx := testContext(t)

	const (
		confirmed = iota
		refusedAtTheDoor
		compensated
	)
	results := make([]int, attempts)
	failures := make([]string, attempts)

	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			userID := uuid.NewString()
			key := fmt.Sprintf("test:%s:%d", f.round.ID, i)

			begun, err := f.repo.Begin(ctx, BeginParams{
				RoundID: f.round.ID, UserID: userID, IdempotencyKey: key,
				Cost: 1, Now: time.Now(),
			})
			if errors.Is(err, ErrRoundClosed) {
				results[i] = refusedAtTheDoor
				return
			}
			if err != nil {
				failures[i] = fmt.Sprintf("begin: %v", err)
				return
			}

			if _, _, err := f.repo.Confirm(ctx, ConfirmParams{
				ParticipationID: begun.Participation.ID, FortuneEntryID: uuid.NewString(),
			}); err != nil {
				if !errors.Is(err, ErrRoundClosed) {
					failures[i] = fmt.Sprintf("confirm: %v", err)
					return
				}
				// 门在扣卡飞行途中关上了：卡要原路退回去，这一笔不算参与。
				reverse := uuid.NewString()
				if err := f.repo.Fail(ctx, FailParams{
					ParticipationID: begun.Participation.ID,
					FailureCode:     model.FailureRoundClosed,
					ReverseEntryID:  &reverse,
				}); err != nil {
					failures[i] = fmt.Sprintf("compensate: %v", err)
					return
				}
				results[i] = compensated
				return
			}
			results[i] = confirmed
		}(i)
	}
	wg.Wait()

	for i, message := range failures {
		if message != "" {
			t.Errorf("第 %d 个参与者：%s", i, message)
		}
	}
	counts := map[int]int{}
	for _, outcome := range results {
		counts[outcome]++
	}
	if counts[confirmed] != target {
		t.Fatalf("确认成功 %d 笔，期望正好 %d 笔（门槛）", counts[confirmed], target)
	}
	if total := counts[confirmed] + counts[refusedAtTheDoor] + counts[compensated]; total != attempts {
		t.Fatalf("%d 次尝试里有 %d 次没有落下任何结局", attempts, attempts-total)
	}

	// 计数不丢更新。
	round := assertCountInvariant(t, ctx, f, f.round.ID)
	if round.ParticipantCount != target {
		t.Fatalf("participant_count 是 %d，期望 %d", round.ParticipantCount, target)
	}
	// 达标即 closed。
	if round.Status != model.RoundClosed {
		t.Fatalf("达标后期次状态是 %q，期望 %q", round.Status, model.RoundClosed)
	}
	// 补偿过的那几笔标 failed 而不是 confirmed——把补偿写成 confirmed 会让这一期事后
	// 多出几个「参与过但没进开奖池」的人。
	if got := countRows(t, ctx, f, `SELECT COUNT(*) FROM lottery_participations
		WHERE round_id=$1 AND status='failed' AND failure_code=$2`,
		f.round.ID, model.FailureRoundClosed); got != counts[compensated] {
		t.Fatalf("标成 round_closed 的失败参与有 %d 条，期望 %d 条", got, counts[compensated])
	}
	// 入口被挡掉的那些**一条记录都不该留下**：一次扣卡都没发生，留一行 failed 只会让
	// 「这一期有多少人参与过」这个数变得没法回答。
	total := countRows(t, ctx, f, `SELECT COUNT(*) FROM lottery_participations WHERE round_id=$1`, f.round.ID)
	if total != counts[confirmed]+counts[compensated] {
		t.Fatalf("期次下落了 %d 条参与记录，期望 %d 条（入口被挡的不留记录）",
			total, counts[confirmed]+counts[compensated])
	}
}

// TestConcurrentDrawsProduceExactlyOneDrawRecord 验的是「一期只能开一次」在多副本下的
// 形态：六个 worker 同时来开同一期。
//
// 全部依据是期次那一把 FOR UPDATE 行锁加 lottery_draws 的 UNIQUE(round_id)，没有选主、
// 没有租约。后到的那些看到 status 已经是 drawn 就退出——**它们不是失败**，是正常的
// 多副本噪声，所以这里断言的是「恰好一个拿到名单、其余都拿到 ErrRoundAlreadyDrawn」。
func TestConcurrentDrawsProduceExactlyOneDrawRecord(t *testing.T) {
	f := newLotteryFixture(t, 50, 2)
	ctx := testContext(t)
	f.seedParticipations(t, ctx, 5)

	actor := uuid.NewString()
	const workers = 6
	outcomes := make([]*DrawOutcome, workers)
	errs := make([]error, workers)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcomes[i], errs[i] = f.repo.DrawRound(ctx, DrawParams{
				RoundID: f.round.ID, Mode: model.DrawModeManual,
				DrawnBy: &actor, Reason: "并发开奖测试", Now: time.Now(),
			})
		}(i)
	}
	wg.Wait()

	won, refused := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
			if outcomes[i] == nil || outcomes[i].Draw == nil {
				t.Errorf("第 %d 个 worker 没报错但也没开出奖", i)
			}
		case errors.Is(err, ErrRoundAlreadyDrawn):
			refused++
		default:
			t.Errorf("第 %d 个 worker 拿到了一个说得不清的错：%v", i, err)
		}
	}
	if won != 1 {
		t.Fatalf("%d 个 worker 里 %d 个开出了奖，期望正好 1 个", workers, won)
	}
	if refused != workers-1 {
		t.Fatalf("被挡掉的是 %d 个，期望 %d 个", refused, workers-1)
	}

	// 兜底那一条索引也要真的存在——上面能过是因为锁与状态判定，这里单独确认记录只有一条。
	if got := countRows(t, ctx, f, `SELECT COUNT(*) FROM lottery_draws WHERE round_id=$1`, f.round.ID); got != 1 {
		t.Fatalf("期次 %s 有 %d 条开奖记录，期望 1 条", f.round.ID, got)
	}
	round := assertCountInvariant(t, ctx, f, f.round.ID)
	if round.Status != model.RoundDrawn || round.DrawnAt == nil {
		t.Fatalf("开奖后期次应当是 drawn 且 drawn_at 非空，实际是 %q / %v", round.Status, round.DrawnAt)
	}
}

// TestDrawIsReproducibleFromTheStoredSeedAndParticipationSet 是 lottery_draws 这张只增表
// 存在的全部意义：拿记录的种子 + 记录的参与集合，第三方能把名单完整重算一遍。
//
// 它同时钉住三件事：种子确实落库、参与集合是自己的大小（不是别人给的数）、名单与输入
// 顺序无关。**它不是公平性证明**——种子是派生值而不是先承诺后揭示的随机数（见
// internal/draw 的包注释）。
func TestDrawIsReproducibleFromTheStoredSeedAndParticipationSet(t *testing.T) {
	f := newLotteryFixture(t, 50, 2)
	ctx := testContext(t)
	f.seedParticipations(t, ctx, 6)

	actor := uuid.NewString()
	outcome, err := f.repo.DrawRound(ctx, DrawParams{
		RoundID: f.round.ID, Mode: model.DrawModeManual,
		DrawnBy: &actor, Reason: "可复现性测试", Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("draw: %v", err)
	}

	record := outcome.Draw
	if record.Algorithm != draw.SeedAlgorithmFor() {
		t.Fatalf("开奖记录里的算法是 %q，期望 %q", record.Algorithm, draw.SeedAlgorithmFor())
	}
	if record.Seed == "" {
		t.Fatal("开奖记录没有落种子——没有它这份记录就复核不了")
	}

	ids := confirmedIDs(t, ctx, f, f.round.ID)
	if int32(len(ids)) != record.ParticipantCount {
		t.Fatalf("开奖记录写下的参与数是 %d，参与集合实际有 %d 条", record.ParticipantCount, len(ids))
	}

	prizes, err := f.repo.ListPrizes(ctx, f.campaign.ID)
	if err != nil {
		t.Fatalf("read the prize pool: %v", err)
	}
	prizeInputs := make([]draw.Prize, 0, len(prizes))
	for _, prize := range prizes {
		prizeInputs = append(prizeInputs, draw.Prize{ID: prize.ID, Quantity: int(prize.Quantity)})
	}

	again := draw.SelectWinners(record.Seed, ids, prizeInputs)
	if len(again) != len(outcome.Winners) {
		t.Fatalf("重算得到 %d 个中奖者，库里写着 %d 个", len(again), len(outcome.Winners))
	}
	for i, winner := range again {
		got := outcome.Winners[i]
		if got.ParticipationID != winner.ParticipationID {
			t.Fatalf("第 %d 名重算是 %s，库里是 %s", i+1, winner.ParticipationID, got.ParticipationID)
		}
		if got.PrizeID != prizeInputs[winner.PrizeIndex].ID {
			t.Fatalf("第 %d 名重算落在第 %d 档，库里写的是另一个奖", i+1, winner.PrizeIndex)
		}
		if got.Status != model.WinPending {
			t.Fatalf("本轮中奖只该停在 pending，实际是 %q", got.Status)
		}
		if got.ClaimNo == "" {
			t.Fatal("中奖记录没有领取凭证号")
		}
	}
}

// TestPendingParticipationIsDiscoverableAndConfirmIsOnceOnly 覆盖「B 成功、C 之前崩」留下的
// 那一行。
//
// 三段事务的第二段是跨服务扣卡，进程可以死在它前后。留下的是一个**悬而未决**的 pending
// ——不是失败。这一条验的是它在这里的三种性质：修复 worker 找得到它、重放不会二次计数、
// 以及幂等键回放会拿回同一行而不是新建一条。
//
// 「余额只扣一次」在 account 那边，靠的是 request_id 就是参与记录 id（没有第二次扣减的
// 余地）；这里能验的是它在本库的这一半：同一条参与不会被算进期次两次。
func TestPendingParticipationIsDiscoverableAndConfirmIsOnceOnly(t *testing.T) {
	f := newLotteryFixture(t, 50, 2)
	ctx := testContext(t)

	userID := uuid.NewString()
	key := "order:" + uuid.NewString()
	begun, err := f.repo.Begin(ctx, BeginParams{
		RoundID: f.round.ID, UserID: userID, IdempotencyKey: key, Cost: 1, Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if !begun.Created {
		t.Fatal("第一次参与应当是新建的一行")
	}
	if begun.Participation.Status != model.ParticipationPending {
		t.Fatalf("刚记下意图的参与应当是 pending，实际是 %q", begun.Participation.Status)
	}

	// 同一个幂等键再来一次（用户连点两下、或者客户端重试）拿回的是**同一行**，
	// 而不是第二条参与。这就是「一笔订单只能参与一次」的执行方式。
	replay, err := f.repo.Begin(ctx, BeginParams{
		RoundID: f.round.ID, UserID: userID, IdempotencyKey: key, Cost: 1, Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("replay with the same idempotency key: %v", err)
	}
	if replay.Created {
		t.Fatal("同一个幂等键第二次调用又新建了一行")
	}
	if replay.Participation.ID != begun.Participation.ID {
		t.Fatalf("幂等回放拿回了另一条参与：%s ≠ %s", replay.Participation.ID, begun.Participation.ID)
	}

	// 修复 worker 的取件。limit 给得很大：这一张表不清空（见文件头），所以库里还留着
	// 之前几次运行留下的 pending 行，只能断言「它在里面」而不是「只有它」。
	pending, err := f.repo.PendingParticipations(ctx, 0, 1000)
	if err != nil {
		t.Fatalf("list pending participations: %v", err)
	}
	if !containsString(pending, begun.Participation.ID) {
		t.Fatalf("修复 worker 找不到卡在 pending 的那条参与 %s", begun.Participation.ID)
	}

	// NoteAttempt 只记观测，不改状态：「扣减没得出结论」不等于失败。
	if err := f.repo.NoteAttempt(ctx, begun.Participation.ID, errors.New("unavailable")); err != nil {
		t.Fatalf("note a failed attempt: %v", err)
	}
	noted, err := f.repo.GetParticipation(ctx, begun.Participation.ID)
	if err != nil {
		t.Fatalf("read the pending participation: %v", err)
	}
	if noted.Status != model.ParticipationPending {
		t.Fatalf("记过一次失败尝试之后状态变成了 %q，期望仍然是 pending", noted.Status)
	}
	if noted.Attempts != 1 {
		t.Fatalf("attempts 是 %d，期望 1", noted.Attempts)
	}
	if noted.LastError == nil || *noted.LastError != "unavailable" {
		t.Fatalf("last_error 没有记下原因：%v", noted.LastError)
	}

	// 重跑第三段。
	if _, _, err := f.repo.Confirm(ctx, ConfirmParams{
		ParticipationID: begun.Participation.ID, FortuneEntryID: uuid.NewString(),
	}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	round := assertCountInvariant(t, ctx, f, f.round.ID)
	if round.ParticipantCount != 1 {
		t.Fatalf("confirm 之后 participant_count 是 %d，期望 1", round.ParticipantCount)
	}

	// 「C 已经提交、但返回前进程死了」之后的重跑：这一步必须被挡下，否则期次会凭空
	// 多一个人——而这正是修复 worker 会做的事（它不知道上一次走到哪一步）。
	if _, _, err := f.repo.Confirm(ctx, ConfirmParams{
		ParticipationID: begun.Participation.ID, FortuneEntryID: uuid.NewString(),
	}); !errors.Is(err, ErrParticipationNotPending) {
		t.Fatalf("对一条已经 confirmed 的参与再确认，得到 %v，期望 ErrParticipationNotPending", err)
	}
	if round := assertCountInvariant(t, ctx, f, f.round.ID); round.ParticipantCount != 1 {
		t.Fatalf("重跑确认把计数加到了 %d", round.ParticipantCount)
	}
}

// TestConfirmOnARoundClosedMidFlightIsCompensatedNotCounted 是补偿路径的仓储侧。
//
// 用户 A 的卡已经在借记路上了，回到本地时期次被别人填满并关上了门。这时能做的只有一件事：
// 把卡退回去、把这一笔标成 failed/round_closed。**不能把参与算进去**——那会让这一期
// 事后多出一个「参与过但没进开奖池」的人，而开奖名单是从 confirmed 里取的。
func TestConfirmOnARoundClosedMidFlightIsCompensatedNotCounted(t *testing.T) {
	f := newLotteryFixture(t, 2, 1)
	ctx := testContext(t)

	// A 在期次还开着的时候记下意图。
	begun, err := f.repo.Begin(ctx, BeginParams{
		RoundID: f.round.ID, UserID: uuid.NewString(),
		IdempotencyKey: "test:" + uuid.NewString(), Cost: 1, Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	// 门在 A 的扣卡往返期间被填满了。
	f.participate(t, ctx, f.round.ID, uuid.NewString(), "test:"+uuid.NewString())
	f.participate(t, ctx, f.round.ID, uuid.NewString(), "test:"+uuid.NewString())
	if round := roundOf(t, ctx, f, f.round.ID); round.Status != model.RoundClosed {
		t.Fatalf("填满之后期次应当是 closed，实际是 %q", round.Status)
	}

	// A 回来确认：影响 0 行。
	if _, _, err := f.repo.Confirm(ctx, ConfirmParams{
		ParticipationID: begun.Participation.ID, FortuneEntryID: uuid.NewString(),
	}); !errors.Is(err, ErrRoundClosed) {
		t.Fatalf("对一期已经关上的期次确认，得到 %v，期望 ErrRoundClosed", err)
	}
	// 关键：那一句 UPDATE 的改动**跟着事务回滚了**，所以补偿路径看到的仍然是一条 pending
	// 的行，而不是一条被标成 confirmed 又没被计数的行。
	if p, err := f.repo.GetParticipation(ctx, begun.Participation.ID); err != nil {
		t.Fatalf("read the participation: %v", err)
	} else if p.Status != model.ParticipationPending {
		t.Fatalf("被拒的确认留下了 %q 的行，期望它仍然是 pending", p.Status)
	}

	// 补偿：退卡 + 标 failed。reverse_entry_id 是 account 那边回的冲正流水。
	reverse := uuid.NewString()
	if err := f.repo.Fail(ctx, FailParams{
		ParticipationID: begun.Participation.ID,
		FailureCode:     model.FailureRoundClosed,
		ReverseEntryID:  &reverse,
	}); err != nil {
		t.Fatalf("compensate: %v", err)
	}

	compensated, err := f.repo.GetParticipation(ctx, begun.Participation.ID)
	if err != nil {
		t.Fatalf("read the compensated participation: %v", err)
	}
	if compensated.Status != model.ParticipationFailed {
		t.Fatalf("补偿之后状态是 %q，期望 %q", compensated.Status, model.ParticipationFailed)
	}
	if compensated.FailureCode != model.FailureRoundClosed {
		t.Fatalf("补偿之后 failure_code 是 %q，期望 %q", compensated.FailureCode, model.FailureRoundClosed)
	}
	if compensated.ReverseEntryID == nil || *compensated.ReverseEntryID != reverse {
		t.Fatalf("冲正流水号没有落库：%v", compensated.ReverseEntryID)
	}
	// 这一次扣卡从头到尾没有成功过，所以不该有 fortune_entry_id——有的话说明补偿路径
	// 把「扣成功又退回」和「压根没扣成」混成了一件。
	if compensated.FortuneEntryID != nil {
		t.Fatalf("没扣成的参与却记下了扣减流水 %s", *compensated.FortuneEntryID)
	}

	// 期次没有被这次失败的确认弄脏。
	round := assertCountInvariant(t, ctx, f, f.round.ID)
	if round.ParticipantCount != 2 {
		t.Fatalf("期次计数是 %d，期望 2（补偿的那一笔不计入）", round.ParticipantCount)
	}
	if ids := confirmedIDs(t, ctx, f, f.round.ID); containsString(ids, begun.Participation.ID) {
		t.Fatal("被补偿的参与出现在开奖名单的候选集合里")
	}

	// 补偿会被重跑（修复 worker 不知道上一次走到没走到最后一步），所以它必须是幂等的。
	if err := f.repo.Fail(ctx, FailParams{
		ParticipationID: begun.Participation.ID,
		FailureCode:     model.FailureRoundClosed,
		ReverseEntryID:  &reverse,
	}); err != nil {
		t.Fatalf("重跑补偿不该报错：%v", err)
	}
}

// TestAutoDrawAtThresholdChainsTheNextRound 验的是**收满门槛**那条路（自动开奖今天唯一的
// 一条路），以及开奖的同事务连开下一期——原型里 LAKE-202608-12 已经到第 12 期，所以
// 一期一活动是不成立的。
//
// 门槛设成 4，然后正好参与 4 次：Confirm 里那一句 CASE 把期次置成 closed，于是它进了
// worker 的取件。**这里曾经验的是「到点必开」**，2026-09-15 那条路删掉了——现在没有任何
// 办法让一个没收满的期次被扫到，下面 TestOpenRoundsAreNeverSwept 专门钉这一点。
func TestAutoDrawAtThresholdChainsTheNextRound(t *testing.T) {
	f := newLotteryFixture(t, 4, 2)
	ctx := testContext(t)
	f.seedParticipations(t, ctx, 4)

	now := time.Now()
	due, err := f.repo.RoundsAwaitingDraw(ctx, 200)
	if err != nil {
		t.Fatalf("list rounds awaiting a draw: %v", err)
	}
	if !containsString(due, f.round.ID) {
		t.Fatal("收满门槛的期次没有被开奖 worker 的取件查询扫到")
	}

	outcome, err := f.repo.DrawRound(ctx, DrawParams{
		RoundID: f.round.ID, Mode: model.DrawModeAuto, Now: now,
	})
	if err != nil {
		t.Fatalf("auto draw: %v", err)
	}
	if outcome.Draw == nil {
		t.Fatal("有人参与的期次收满门槛应当开出奖，而不是作废")
	}
	if outcome.Draw.Trigger != model.TriggerThreshold {
		t.Fatalf("trigger 是 %q，期望 %q", outcome.Draw.Trigger, model.TriggerThreshold)
	}
	if outcome.Draw.Mode != model.DrawModeAuto {
		t.Fatalf("mode 是 %q，期望 %q", outcome.Draw.Mode, model.DrawModeAuto)
	}
	if outcome.Draw.DrawnBy != nil {
		t.Fatal("自动开奖不该有操作人——数据库的 CHECK 也挡这一条")
	}
	if outcome.NextRound == nil {
		t.Fatal("活动还是 enabled，开奖的同事务里应当接着开出下一期")
	}
	if outcome.NextRound.Seq != 2 {
		t.Fatalf("下一期的 seq 是 %d，期望 2", outcome.NextRound.Seq)
	}
	if want := model.RoundNo(f.campaign.Code, 2); outcome.NextRound.RoundNo != want {
		t.Fatalf("下一期的期次号是 %q，期望 %q", outcome.NextRound.RoundNo, want)
	}
	if outcome.NextRound.Status != model.RoundOpen {
		t.Fatalf("下一期的状态是 %q，期望 %q", outcome.NextRound.Status, model.RoundOpen)
	}
	// 下一期的名额从当期奖池冻结——它是新的一期，计数从零开始。
	if outcome.NextRound.ParticipantCount != 0 {
		t.Fatalf("新一期的计数是 %d，期望 0", outcome.NextRound.ParticipantCount)
	}
	if outcome.NextRound.WinnerCount != 2 {
		t.Fatalf("新一期的名额是 %d，期望 2（奖池两个名额）", outcome.NextRound.WinnerCount)
	}

	// 抽奖中心靠 LiveRound 显示「当前正在跑的一期」，所以连开这件事必须在这里看得见。
	live, err := f.repo.LiveRound(ctx, f.campaign.ID)
	if err != nil {
		t.Fatalf("read the live round: %v", err)
	}
	if live == nil || live.ID != outcome.NextRound.ID {
		t.Fatalf("开奖之后 LiveRound 读到的不是新开的那一期：%+v", live)
	}
	// 已开奖的那一期不再出现在取件里。
	if due, err := f.repo.RoundsAwaitingDraw(ctx, 200); err != nil {
		t.Fatalf("list rounds awaiting a draw: %v", err)
	} else if containsString(due, f.round.ID) {
		t.Fatal("已经开过奖的期次还在开奖 worker 的取件里")
	}
}

// TestEditingACampaignKeepsThePrizeRowInPlace 钉住奖品整份替换的前提：**提交上来的奖品要带着
// 它自己的 id**。
//
// replacePrize 的形状是「删掉不是这一行的行 + 更新它」，而 lottery_wins.prize_id 是
// ON DELETE RESTRICT——id 丢了，那句 DELETE 就会把唯一一行删掉，被外键拒绝，整次保存 500。
// 后台表单曾经正是这样（campaigns/index.tsx 的 initialValues 映射奖品时没带 id），于是
// **任何一个已经开过奖的活动都改不动**。这是那条路径的回归点。
//
// 后半段故意再跑一次不带 id 的保存：它必须失败。没有这一段，这条测试就只是「改活动能成功」，
// 而「能成功」的原因可能是别的（比如外键哪天松了），钉不住 id 这件事本身。
func TestEditingACampaignKeepsThePrizeRowInPlace(t *testing.T) {
	f := newLotteryFixture(t, 2, 1)
	ctx := testContext(t)
	f.seedParticipations(t, ctx, 2)

	outcome, err := f.repo.DrawRound(ctx, DrawParams{
		RoundID: f.round.ID, Mode: model.DrawModeAuto, Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("开奖：%v", err)
	}
	if outcome.Draw == nil {
		t.Fatal("收满门槛的期次应当开出奖——没有中奖记录，这条测试就没验到东西")
	}

	prizes, err := f.repo.ListPrizes(ctx, f.campaign.ID)
	if err != nil {
		t.Fatalf("读奖池：%v", err)
	}
	if len(prizes) != 1 {
		t.Fatalf("奖池有 %d 行，要的是 1 行", len(prizes))
	}
	prizeID := prizes[0].ID

	update := func(p PrizeInput) error {
		_, err := f.repo.UpdateCampaign(ctx, f.campaign.ID, CampaignParams{
			Code:              f.campaign.Code,
			Name:              "改过名字的活动",
			Description:       "integration test",
			ParticipantTarget: 2,
			Status:            model.CampaignEnabled,
			Prize:             p,
		})
		return err
	}

	// 带上 id：原地 UPDATE 应当成功。
	want := PrizeInput{
		ID:                prizeID,
		Name:              "改过的奖品",
		CoverImage:        "https://example.test/new-cover.png",
		PosterImage:       "https://example.test/new-poster.png",
		ClaimInstructions: "到店出示中奖记录",
		Quantity:          1,
	}
	if err := update(want); err != nil {
		t.Fatalf("改一个已经开过奖的活动：%v——奖品行被删了重插的话这里就是外键拒绝", err)
	}

	after, err := f.repo.ListPrizes(ctx, f.campaign.ID)
	if err != nil {
		t.Fatalf("改完再读奖池：%v", err)
	}
	if len(after) != 1 {
		t.Fatalf("改完之后奖池有 %d 行，要的是 1 行——原地 UPDATE 不该多出一行", len(after))
	}
	if after[0].ID != prizeID {
		t.Errorf("奖品 id 从 %s 变成了 %s：这是删了重插，中奖记录上的外键正是要挡这件事", prizeID, after[0].ID)
	}
	if after[0].Name != "改过的奖品" || after[0].CoverImage != want.CoverImage || after[0].PosterImage != want.PosterImage {
		t.Errorf("改完的奖品是 %+v，要的是名字 / 两张图都换过来", after[0])
	}

	// 中奖记录仍指得通，而且**名字快照没被改写**：历史记录说的是当时发的是什么，
	// current_prize_name 留给将来换奖那条路（见 model.Win 上的注释），不跟着活动改。
	wins, total, err := f.repo.ListWins(ctx, dto.WinQuery{CampaignID: f.campaign.ID, Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("读中奖记录：%v", err)
	}
	if total != 1 {
		t.Fatalf("中奖记录有 %d 条，要的是 1 条", total)
	}
	if wins[0].PrizeID != prizeID {
		t.Errorf("中奖记录的 prize_id = %s，要的还是原来那一行 %s", wins[0].PrizeID, prizeID)
	}
	if wins[0].OriginalPrizeName != "10 元咖啡兑换券" {
		t.Errorf("原奖品名 = %q，改活动不该改写历史中奖记录", wins[0].OriginalPrizeName)
	}

	// 不带 id：必须失败。这一句就是「表单漏带 id」在仓储层的样子。
	if err := update(PrizeInput{Name: "不带 id 的奖品", CoverImage: want.CoverImage, Quantity: 1}); err == nil {
		t.Error("不带奖品 id 的保存竟然成功了——中奖记录没有被外键挡住，那它指的可能是一行已经不存在的奖品")
	}
}

// TestOpenRoundsAreNeverSwept 钉住「没收满就一直等着」这条新规则最容易被改回去的一点。
//
// 取件查询曾经有一条 `OR (status='open' AND ends_at <= now)`。窗口删掉之后那条路就没有
// 判据了，但如果有人把它写回成「open 且开得够久了就算到点」，**用户会看到说好收满 10 次
// 才开的奖在参与 3 次时就开了**——这是这次改动要根除的那个结果。
//
// 期次的开期时刻故意做得足够老（一小时前）：即便有人按「年龄」来扫也扫不出来。
func TestOpenRoundsAreNeverSwept(t *testing.T) {
	f := newLotteryFixture(t, 10, 2)
	ctx := testContext(t)
	f.seedParticipations(t, ctx, 3)

	if _, err := f.pool.Exec(ctx, `UPDATE lottery_rounds
		SET created_at = NOW() - INTERVAL '1 hour' WHERE id=$1`, f.round.ID); err != nil {
		t.Fatalf("age the round: %v", err)
	}

	due, err := f.repo.RoundsAwaitingDraw(ctx, 200)
	if err != nil {
		t.Fatalf("list rounds awaiting a draw: %v", err)
	}
	if containsString(due, f.round.ID) {
		t.Fatal("一个没收满的 open 期次被开奖取件扫到了——收不满就等着，这是有意的")
	}

	// 扫不到还不算完：真拿它去开也应当被判成「现在不该开」。
	if _, err := f.repo.DrawRound(ctx, DrawParams{
		RoundID: f.round.ID, Mode: model.DrawModeAuto, Now: time.Now(),
	}); !errors.Is(err, ErrRoundNotAwaitingDraw) {
		t.Fatalf("对一个没收满的期次自动开奖，得到 %v，期望 ErrRoundNotAwaitingDraw", err)
	}
}

// TestManualDrawWithNoParticipantsCancelsWithoutADrawRecord 是「零人参与」那条分支。
//
// 不流局（流局要把 N 张卡沿 N 次跨服务冲正还回去，而那条路没有截止时间），但零人参与连
// 一张卡都没扣过——开一次没有名单的奖只会在后台留下一条谁也看不懂的记录。
//
// **这条路今天只有人工开奖走得到**：自动开奖只扫 status='closed' 的期次，而转 closed 的
// 条件是参与数达到 target（CHECK 保证 target > 0），所以自动开奖永远碰不到期次。原先它是
// 「到点必开」扫出来的，那条路 2026-09-15 删掉了。
func TestManualDrawWithNoParticipantsCancelsWithoutADrawRecord(t *testing.T) {
	f := newLotteryFixture(t, 50, 2)
	ctx := testContext(t)

	actor := uuid.NewString()
	outcome, err := f.repo.DrawRound(ctx, DrawParams{
		RoundID: f.round.ID, Mode: model.DrawModeManual, Now: time.Now(),
		DrawnBy: &actor, Reason: "没人来，收摊",
	})
	if err != nil {
		t.Fatalf("manual draw: %v", err)
	}
	if outcome.Draw != nil {
		t.Fatal("零人参与的期次不该写开奖记录")
	}
	if outcome.Round.Status != model.RoundCancelled {
		t.Fatalf("零人参与的期次应当作废，实际状态是 %q", outcome.Round.Status)
	}
	if outcome.Round.CancelReason == "" {
		t.Fatal("作废理由没有落库——第二天没人说得清这一期为什么没有开")
	}
	if outcome.Round.CancelledAt == nil {
		t.Fatal("作废时间没有落库")
	}
	if record, err := f.repo.FindDrawByRound(ctx, f.round.ID); err != nil {
		t.Fatalf("find the draw of a cancelled round: %v", err)
	} else if record != nil {
		t.Fatal("作废的期次不该有开奖记录")
	}
	// 作废照样把期次滚动推下去，否则扫码的用户会看到「暂无进行中的活动」。
	if outcome.NextRound == nil {
		t.Fatal("零人参与的期次作废之后应当接着开出下一期")
	}
}

// TestCancelRoundRefusesWhenThereAreParticipants 把「作废」收窄在零人参与的期次上。
//
// 有参与者的作废需要一个 N 次跨服务冲正循环（每张卡都要沿 account 退回去），属下一轮。
// 今天让它在这里失败，而不是写下一个「作废了但卡没退」的状态——那正是用户第二天来投诉的
// 东西。
func TestCancelRoundRefusesWhenThereAreParticipants(t *testing.T) {
	f := newLotteryFixture(t, 50, 2)
	ctx := testContext(t)
	f.seedParticipations(t, ctx, 1)

	actor := uuid.NewString()
	if _, err := f.repo.CancelRound(ctx, f.round.ID, "运营误建", &actor); !errors.Is(err, ErrRoundHasParticipations) {
		t.Fatalf("作废一期有参与者的期次，得到 %v，期望 ErrRoundHasParticipations", err)
	}
	if round := roundOf(t, ctx, f, f.round.ID); round.Status != model.RoundOpen {
		t.Fatalf("被拒的作废改动了期次状态：%q", round.Status)
	}

	// 另开一家门店，它第一期是零人参与的，那才是可以作废的那种。
	fresh := newLotteryFixture(t, 50, 2)
	cancelled, err := fresh.repo.CancelRound(ctx, fresh.round.ID, "运营误建", &actor)
	if err != nil {
		t.Fatalf("作废一期零人参与的期次：%v", err)
	}
	if cancelled.Status != model.RoundCancelled || cancelled.CancelReason != "运营误建" {
		t.Fatalf("作废结果不对：%q / %q", cancelled.Status, cancelled.CancelReason)
	}
	if cancelled.CancelledBy == nil || *cancelled.CancelledBy != actor {
		t.Fatalf("作废人没有落库：%v", cancelled.CancelledBy)
	}
	// 再作废一次是「这一期根本不作数」，与「你来晚了一步」（已开奖）分开报。
	if _, err := fresh.repo.CancelRound(ctx, fresh.round.ID, "再来一次", &actor); !errors.Is(err, ErrRoundCancelled) {
		t.Fatalf("重复作废得到 %v，期望 ErrRoundCancelled", err)
	}
}

// TestCancelRoundOpensTheNextRound 钉住作废后的那个空档。
//
// 后台的作废确认框一直写着「作废后会立刻开出下一期」，而仓储在此之前只把期次置成
// cancelled——**说的和做的不一样**。拿掉到点必开之后它更要命：零人参与的期次以前能等到点
// 自动收场，现在会永远开着，作废是唯一的出口，而作废之后没有任何东西会去开一期（自动开期
// 那一句只在开奖时跑），活动会一直是「启用中、却没有进行中的期次」。
func TestCancelRoundOpensTheNextRound(t *testing.T) {
	f := newLotteryFixture(t, 50, 2)
	ctx := testContext(t)

	actor := uuid.NewString()
	if _, err := f.repo.CancelRound(ctx, f.round.ID, "运营误建", &actor); err != nil {
		t.Fatalf("cancel the first round: %v", err)
	}

	live, err := f.repo.LiveRound(ctx, f.campaign.ID)
	if err != nil {
		t.Fatalf("read the live round: %v", err)
	}
	if live == nil {
		t.Fatal("作废之后没有在跑的期次——后台那句「作废后会立刻开出下一期」没有兑现")
	}
	if live.Seq != 2 {
		t.Fatalf("补开的期次 seq 是 %d，期望 2（作废过的号不回收）", live.Seq)
	}
	if live.Status != model.RoundOpen {
		t.Fatalf("补开的期次状态是 %q，期望 %q", live.Status, model.RoundOpen)
	}
	if live.ParticipantCount != 0 {
		t.Fatalf("补开的期次计数是 %d，期望 0", live.ParticipantCount)
	}
	if live.WinnerCount != 2 {
		t.Fatalf("补开的期次名额是 %d，期望 2（从奖池冻结）", live.WinnerCount)
	}
}

// TestCancelRoundDoesNotOpenTheNextRoundWhenTheCampaignIsNotEnabled 是上一条的反面。
//
// 暂停 / 结束的活动作废一期之后不该凭空开出一期：那是「暂停停的是下一期」这条规矩的
// 一部分（见 model.CampaignPaused）。
func TestCancelRoundDoesNotOpenTheNextRoundWhenTheCampaignIsNotEnabled(t *testing.T) {
	f := newLotteryFixture(t, 50, 2)
	ctx := testContext(t)

	actor := uuid.NewString()
	if _, err := f.repo.UpdateCampaignStatus(ctx, f.campaign.ID, model.CampaignPaused, &actor); err != nil {
		t.Fatalf("pause the campaign: %v", err)
	}
	if _, err := f.repo.CancelRound(ctx, f.round.ID, "运营误建", &actor); err != nil {
		t.Fatalf("cancel the first round: %v", err)
	}
	if live, err := f.repo.LiveRound(ctx, f.campaign.ID); err != nil {
		t.Fatalf("read the live round: %v", err)
	} else if live != nil {
		t.Fatalf("暂停中的活动不该在作废后补开一期，却开出了 %s", live.ID)
	}
}

// TestEnsureLiveRoundOpensOneOnlyWhenThereIsNone 验恢复暂停活动时的那一步。
//
// 暂停期间开奖不开新期，所以一个暂停后恢复的活动会处于「enabled 但没有在跑的期次」。
// 没有这一步，恢复就成了一次什么也不做的点击，抽奖中心会一直显示「暂无进行中的活动」。
func TestEnsureLiveRoundOpensOneOnlyWhenThereIsNone(t *testing.T) {
	f := newLotteryFixture(t, 50, 2)
	ctx := testContext(t)

	// 已经有第一期在跑：不新开。这正是「管理员连点两下恢复」的那个场景。
	got, err := f.repo.EnsureLiveRound(ctx, f.campaign.ID)
	if err != nil {
		t.Fatalf("ensure a live round: %v", err)
	}
	if got != nil {
		t.Fatalf("已经有在跑的一期时不该再开一期，却开出了 %s", got.ID)
	}

	// 制造出「enabled 但没有在跑的期次」。直接改库而不是调 CancelRound：那个方法现在会在
	// 同一事务里补开下一期（见 TestCancelRoundOpensTheNextRound），造不出这个状态。
	cancelRoundDirectly(t, ctx, f, f.round.ID)
	got, err = f.repo.EnsureLiveRound(ctx, f.campaign.ID)
	if err != nil {
		t.Fatalf("ensure a live round: %v", err)
	}
	if got == nil {
		t.Fatal("没有在跑的期次时应当开一期")
	}
	// 序号接着 MAX(seq) 走，不是 COUNT+1：作废过的期次号不回收，否则客服会对着两条
	// 不同的记录念同一个号。
	if got.Seq != 2 {
		t.Fatalf("补开的期次 seq 是 %d，期望 2", got.Seq)
	}

	// 再调一次不再重复开。
	if again, err := f.repo.EnsureLiveRound(ctx, f.campaign.ID); err != nil {
		t.Fatalf("ensure a live round again: %v", err)
	} else if again != nil {
		t.Fatalf("第二次调用又开了一期：%s", again.ID)
	}
}

// TestMachineScopedCampaignRefusesAnotherMachinesSource 验设备级活动的那一条本地校验。
//
// 它挡的是「从 A 店的码扫进 B 店的活动」这种真实错配，**挡不住**一个直接构造请求的客户端
// （客户端的自述本来就不可信，见 Begin 的注释）。
func TestMachineScopedCampaignRefusesAnotherMachinesSource(t *testing.T) {
	f := newLotteryFixture(t, 50, 2)
	ctx := testContext(t)

	machineID := uuid.NewString()
	campaign, err := f.repo.CreateCampaign(ctx, CampaignParams{
		ActivationID:      f.activation.ID,
		MachineID:         &machineID,
		Code:              newCampaignCode(),
		Name:              "三号机专用活动",
		ParticipantTarget: 50,
		Status:            model.CampaignEnabled,
		Prize:             PrizeInput{Name: "免费拿铁", CoverImage: "https://example.test/latte.png", Quantity: 1},
	})
	if err != nil {
		t.Fatalf("create a machine-scoped campaign: %v", err)
	}
	round, err := f.repo.EnsureLiveRound(ctx, campaign.ID)
	if err != nil || round == nil {
		t.Fatalf("open the first round of a machine-scoped campaign: %v / %+v", err, round)
	}

	// 报的是另一台设备 —— 拒。
	other := uuid.NewString()
	if _, err := f.repo.Begin(ctx, BeginParams{
		RoundID: round.ID, UserID: uuid.NewString(), IdempotencyKey: "test:" + uuid.NewString(),
		SourceMachineID: &other, Cost: 1, Now: time.Now(),
	}); !errors.Is(err, ErrMachineMismatch) {
		t.Fatalf("从别的设备参与一台设备专属的活动，得到 %v，期望 ErrMachineMismatch", err)
	}
	// 什么都不报也算错配：设备级活动要求参与时就说清是哪台机器。
	if _, err := f.repo.Begin(ctx, BeginParams{
		RoundID: round.ID, UserID: uuid.NewString(), IdempotencyKey: "test:" + uuid.NewString(),
		Cost: 1, Now: time.Now(),
	}); !errors.Is(err, ErrMachineMismatch) {
		t.Fatalf("没报来源设备时得到 %v，期望 ErrMachineMismatch", err)
	}
	// 报对了就放行。
	if _, err := f.repo.Begin(ctx, BeginParams{
		RoundID: round.ID, UserID: uuid.NewString(), IdempotencyKey: "test:" + uuid.NewString(),
		SourceMachineID: &machineID, Cost: 1, Now: time.Now(),
	}); err != nil {
		t.Fatalf("报对设备时应当放行：%v", err)
	}
}

// TestIdempotencyKeyBelongingToAnotherUserIsRefused 挡的是「同一个键换了人」。
//
// 幂等键由服务端从订单号派生，所以正常路上撞不出来。撞上说明调用方把号发重了——这不是
// 幂等命中，是一次真的事故，必须让人看见，而不是把别人的参与记录回放给这个人。
func TestIdempotencyKeyBelongingToAnotherUserIsRefused(t *testing.T) {
	f := newLotteryFixture(t, 50, 2)
	ctx := testContext(t)

	key := "order:" + uuid.NewString()
	if _, err := f.repo.Begin(ctx, BeginParams{
		RoundID: f.round.ID, UserID: uuid.NewString(), IdempotencyKey: key, Cost: 1, Now: time.Now(),
	}); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := f.repo.Begin(ctx, BeginParams{
		RoundID: f.round.ID, UserID: uuid.NewString(), IdempotencyKey: key, Cost: 1, Now: time.Now(),
	}); !errors.Is(err, ErrIdempotencyKeyConflict) {
		t.Fatalf("同一个键换个人用，得到 %v，期望 ErrIdempotencyKeyConflict", err)
	}
}

// TestActivatingTheSameLocationTwiceIsRefused 是开通那条幂等结论。
//
// 「重复点击不是两笔业务」这件事落在 lottery_activations_location_unique 上，由这一条
// 集成测试确认那个约束真的存在、且被翻成了说得清楚的错误（而不是一个 500）。
func TestActivatingTheSameLocationTwiceIsRefused(t *testing.T) {
	f := newLotteryFixture(t, 50, 2)
	ctx := testContext(t)

	_, err := f.repo.Activate(ctx, ActivateParams{
		LocationID: f.activation.LocationID,
		Campaign: DefaultCampaign{
			Code: newCampaignCode(), Name: "重复开通", ParticipantTarget: 10,
			Prize: DefaultPrize{Name: "重复开通的奖品", Quantity: 1},
		},
	})
	if !errors.Is(err, ErrLocationAlreadyActivated) {
		t.Fatalf("重复开通同一家门店得到 %v，期望 ErrLocationAlreadyActivated", err)
	}
	// 那次失败的事务不该留下任何东西——开通是「三行记录同一件事」，半途失败的门店会让
	// 抽奖中心对着一个空活动列表报错。
	if got := countRows(t, ctx, f, `SELECT COUNT(*) FROM lottery_campaigns WHERE activation_id=$1`,
		f.activation.ID); got != 1 {
		t.Fatalf("这家门店的活动数是 %d，期望 1（那一次失败的开通留下了半行）", got)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
