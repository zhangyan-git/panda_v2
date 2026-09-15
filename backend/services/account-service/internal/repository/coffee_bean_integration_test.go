package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
)

// 这一份验的是**与 SQL 有关**的东西：行锁下的余额判定、唯一索引上的幂等、冲正按订单反查
// 与它的上限。形状校验那一层（UUID、正数、必填的号）在 service 的单测里验，不重复。
//
// 夹具只用一个调用方给的库 URL：不建库、不删库、不跑迁移，用户 id 一律随机。
// **跑完不清理**：coffee_bean_entries 有 append-only 触发器（DELETE 会直接抛），
// coffee_bean_accounts 又被流水以 ON DELETE RESTRICT 引用着。这是那两张表的性质，不是漏删
// ——夹具 id 每次都是新的，残留不影响下一次运行，也不会让断言互相串台（每一处断言都按
// 本次的 userID 过滤）。与 fortune_card_integration_test.go 同一条约定。
func beanIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return fortuneCardIntegrationPool(t)
}

// balanceOfBean 读余额，并顺手验那条不变式：SUM(amount) 必须等于 balance。
//
// 这是账户域唯一一条真正的正确性判据——余额与流水在同一个事务里改，两边一旦不一致，
// 后面所有「他还有多少豆」的回答都不可信。所以每一处断言余额的地方都同时验它。
func balanceOfBean(t *testing.T, pool *pgxpool.Pool, userID string) int64 {
	t.Helper()
	var balance, ledgerSum int64
	if err := pool.QueryRow(context.Background(),
		`SELECT balance FROM coffee_bean_accounts WHERE user_id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("read bean balance: %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount),0) FROM coffee_bean_entries WHERE user_id = $1`, userID).Scan(&ledgerSum); err != nil {
		t.Fatalf("sum bean entries: %v", err)
	}
	if balance != ledgerSum {
		t.Fatalf("balance %d does not equal the ledger sum %d", balance, ledgerSum)
	}
	return balance
}

// beanEntriesOf 读一个用户的全部豆流水，最近的在前。夹具的用户都是全新的，所以这里
// 「全部」就是本次用例造出来的那几行，条数本身也是断言的一部分。
func beanEntriesOf(t *testing.T, pool *pgxpool.Pool, userID string) []*model.CoffeeBeanEntry {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT `+beanEntryColumns+` FROM coffee_bean_entries
		 WHERE user_id = $1 ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		t.Fatalf("read bean entries: %v", err)
	}
	defer rows.Close()
	var entries []*model.CoffeeBeanEntry
	for rows.Next() {
		entry, err := scanBeanEntry(rows)
		if err != nil {
			t.Fatalf("scan bean entry: %v", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate bean entries: %v", err)
	}
	return entries
}

// adjust 走后台那条写路径。审计传 Noop：这一份要验的是余额与流水，审计链路在别处验
// （它会写 message_outbox 并由 relay 投出去，那是另一个进程的事）。
func adjust(repo *PostgresRepository, userID string, amount int64, requestID string) (EntryResult, error) {
	return NewBeanAdminRepository(repo, audit.Noop{}).AdjustBeans(context.Background(), BeanAdjustParams{
		UserID:     userID,
		Amount:     amount,
		RequestID:  requestID,
		Remark:     "集成测试",
		Operator:   uuid.NewString(),
		OccurredAt: time.Now().UTC(),
	})
}

func consume(repo *PostgresRepository, userID, orderID string, amount int64) (EntryResult, error) {
	return repo.ConsumeBeans(context.Background(), BeanConsumeParams{
		UserID:     userID,
		OrderID:    orderID,
		OrderNo:    "CO-" + orderID[:8],
		Amount:     amount,
		OccurredAt: time.Now().UTC(),
	})
}

func reverse(repo *PostgresRepository, orderID, afterSaleNo string, amount int64) (bool, error) {
	return repo.ReverseBeans(context.Background(), BeanReverseParams{
		OrderID:     orderID,
		AfterSaleID: uuid.NewString(),
		AfterSaleNo: afterSaleNo,
		Amount:      amount,
		OccurredAt:  time.Now().UTC(),
	})
}

func TestAdjustBeansIsIdempotentPerRequestID(t *testing.T) {
	pool := beanIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID := uuid.NewString()
	requestID := "req-" + uuid.NewString()[:8]

	first, err := adjust(repo, userID, 5000, requestID)
	if err != nil {
		t.Fatalf("adjust: %v", err)
	}
	if first.Replayed {
		t.Fatal("a first adjustment must not look like a replay")
	}
	if first.BalanceAfter != 5000 {
		t.Fatalf("want balance_after 5000, got %d", first.BalanceAfter)
	}
	if got := balanceOfBean(t, pool, userID); got != 5000 {
		t.Fatalf("want balance 5000, got %d", got)
	}

	entries := beanEntriesOf(t, pool, userID)
	if len(entries) != 1 {
		t.Fatalf("want one entry, got %d", len(entries))
	}
	if entries[0].EntryType != model.BeanEntryTypeAdjust || entries[0].Amount != 5000 {
		t.Fatalf("unexpected entry: %+v", entries[0])
	}
	// 调整没有「起因对象」的 ID：后台填的那张单子不在这个系统里，reference_no 因此是幂等号，
	// 后台明细按它检索。
	if entries[0].EntryKey != model.BeanAdjustKey(requestID) ||
		entries[0].ReferenceNo != requestID ||
		entries[0].ReferenceType != model.BeanReferenceTypeManual {
		t.Fatalf("adjustment must be keyed and referenced by the request id: %+v", entries[0])
	}
	if entries[0].OperatorID == "" {
		t.Fatal("operator id must be recorded: it is the second trace besides the audit row")
	}

	// 同一个 requestId 又来一次。**这条路径不回放**：底层那个 ON CONFLICT 确实把键认出来了
	// （Replayed=true），但 AdjustBeans 把它翻成 ErrBeanDuplicateRequest 并回滚整个事务——
	// 与扣减那条路刻意相反。理由是调用方不同：扣减的重发来自 payment-service 的超时重试，
	// 它要的就是「那笔扣过的结果」；调整是**人点的**，回放原样会让界面显示成功，而管理员
	// 多半会再点一次，于是充两次。这里断言的是这个岔口本身（见 ErrBeanDuplicateRequest）。
	second, err := adjust(repo, userID, 5000, requestID)
	if !errors.Is(err, ErrBeanDuplicateRequest) {
		t.Fatalf("want ErrBeanDuplicateRequest on a repeated request id, got %v (result %+v)", err, second)
	}
	if got := balanceOfBean(t, pool, userID); got != 5000 {
		t.Fatalf("a duplicate request must not move the balance, got %d", got)
	}
	if got := len(beanEntriesOf(t, pool, userID)); got != 1 {
		t.Fatalf("a duplicate request must not add a row, got %d", got)
	}

	// 换一枚新的 requestId 就是另一次调整，正常入账。这条与上一条一起说明：「幂等」在这里
	// 是**由调用方给的号**划定的，不是「同一金额只能充一次」——充两次同样金额是完全正常的
	// 事（分两次活动各送 5000）。
	third, err := adjust(repo, userID, 5000, "req-"+uuid.NewString()[:8])
	if err != nil {
		t.Fatalf("a new request id must be accepted: %v", err)
	}
	if third.Replayed || third.BalanceAfter != 10000 {
		t.Fatalf("a new request id must write a new entry: %+v", third)
	}
	if got := balanceOfBean(t, pool, userID); got != 10000 {
		t.Fatalf("want balance 10000, got %d", got)
	}
}

func TestAdjustBeansAllowsNegativeButNotBelowZero(t *testing.T) {
	pool := beanIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID := uuid.NewString()

	if _, err := adjust(repo, userID, 5000, "req-a-"+uuid.NewString()[:8]); err != nil {
		t.Fatalf("adjust: %v", err)
	}

	// 把充错的豆调回来是这条路径存在的理由之一：负数必须被允许。
	if _, err := adjust(repo, userID, -2000, "req-b-"+uuid.NewString()[:8]); err != nil {
		t.Fatalf("a negative adjustment within the balance must be allowed: %v", err)
	}
	if got := balanceOfBean(t, pool, userID); got != 3000 {
		t.Fatalf("want balance 3000, got %d", got)
	}

	// 但余额不能被调成负数。这条判定在行锁里、写之前，所以失败之后一行流水都不该多。
	if _, err := adjust(repo, userID, -6000, "req-c-"+uuid.NewString()[:8]); !errors.Is(err, ErrInsufficientCoffeeBeans) {
		t.Fatalf("want ErrInsufficientCoffeeBeans, got %v", err)
	}
	if got := balanceOfBean(t, pool, userID); got != 3000 {
		t.Fatalf("a rejected adjustment must not move the balance, got %d", got)
	}
	// 两条：那次充值 +5000 与那次纠错 -2000。被拒的那一次一行都不能留。
	if got := len(beanEntriesOf(t, pool, userID)); got != 2 {
		t.Fatalf("a rejected adjustment must not leave an entry, got %d rows", got)
	}
}

func TestConsumeBeansReplaysAndChecksBalance(t *testing.T) {
	pool := beanIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID := uuid.NewString()
	orderID := uuid.NewString()

	if _, err := adjust(repo, userID, 5000, "req-"+uuid.NewString()[:8]); err != nil {
		t.Fatalf("adjust: %v", err)
	}

	first, err := consume(repo, userID, orderID, 3400)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if first.BalanceAfter != 1600 {
		t.Fatalf("want balance_after 1600, got %d", first.BalanceAfter)
	}
	if got := balanceOfBean(t, pool, userID); got != 1600 {
		t.Fatalf("want balance 1600, got %d", got)
	}

	entries := beanEntriesOf(t, pool, userID)
	if len(entries) != 2 {
		t.Fatalf("want two entries, got %d", len(entries))
	}
	// 流水上扣减记负数：调用方给的是「这单用掉多少豆」，符号由仓储定。
	var consumed *model.CoffeeBeanEntry
	for _, entry := range entries {
		if entry.EntryType == model.BeanEntryTypeConsume {
			consumed = entry
		}
	}
	if consumed == nil {
		t.Fatal("the deduction entry is missing")
	}
	if consumed.Amount != -3400 {
		t.Fatalf("a deduction must be recorded as a negative amount, got %d", consumed.Amount)
	}
	if consumed.EntryKey != model.BeanConsumeKey(orderID) {
		t.Fatalf("the deduction must be keyed by the order id, got %q", consumed.EntryKey)
	}

	// 发起支付时的超时重发：同一张订单再扣一次。必须回放原样，而不是得到「余额不足」
	// ——那会让调用方把一次**已经生效**的扣减读成失败，然后按失败去补偿。
	replayed, err := consume(repo, userID, orderID, 3400)
	if err != nil {
		t.Fatalf("replayed consume: %v", err)
	}
	if !replayed.Replayed || replayed.EntryID != first.EntryID {
		t.Fatalf("a replay must return the original entry: first=%+v replayed=%+v", first, replayed)
	}
	if got := balanceOfBean(t, pool, userID); got != 1600 {
		t.Fatalf("a replay must not deduct twice, got %d", got)
	}

	// 另一张订单要 2000，余额只有 1600：这是 ErrInsufficientCoffeeBeans——一个**业务结果**，
	// payment-service 把它翻成一次发起即失败的支付结果，不是当作故障重试。
	if _, err := consume(repo, userID, uuid.NewString(), 2000); !errors.Is(err, ErrInsufficientCoffeeBeans) {
		t.Fatalf("want ErrInsufficientCoffeeBeans, got %v", err)
	}
	if got := balanceOfBean(t, pool, userID); got != 1600 {
		t.Fatalf("a rejected deduction must not move the balance, got %d", got)
	}
	if got := len(beanEntriesOf(t, pool, userID)); got != 2 {
		t.Fatalf("a rejected deduction must not leave an entry, got %d rows", got)
	}
}

func TestReverseBeansIsANoopWithoutADeduction(t *testing.T) {
	pool := beanIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID := uuid.NewString()

	// 渠道支付的订单走到这里：这张订单没有豆扣减，没什么可退的。**不是错误**——绝大多数
	// 订单都是渠道支付的，这条路径天天会被走到，报错会把一条正常的事件推进重试链。
	reversed, err := reverse(repo, uuid.NewString(), "AS-"+uuid.NewString()[:8], 900)
	if err != nil {
		t.Fatalf("a reverse without a deduction must not be an error: %v", err)
	}
	if reversed {
		t.Fatal("a reverse without a deduction must report false")
	}
	if got := len(beanEntriesOf(t, pool, userID)); got != 0 {
		t.Fatalf("a no-op reverse must not write anything, got %d rows", got)
	}
}

// TestReverseBeansWaitsForTheDeductionRow 钉住冲正的**第一把锁**：它必须在算出「还剩多少
// 可冲」之前先按住那笔扣减。
//
// 为什么值得单独一条：上限判定只有在读到的是最新已冲回额时才成立，而「最新」是靠锁换来的。
// 少了这把锁，两条针对同一张订单的冲正各自读到同一个旧值，各自都觉得自己没超——合起来退掉
// 超过扣减额的钱。余额只增不减，没有任何约束拦得住，也不会有报错：这是静默的。
//
// 断言方式是「会不会等」，不是「算得对不对」：另一条连接先按住那行，冲正就必须停在那里。
// 单副本串行消费时这条路径碰不上，所以它是一条只在并发下才成立的用例。
//
// 为什么不写成「两条并发冲正只成一条」那种更像在验结果的用例：**写出来也抓不住**。少这把
// 锁时要真的多退一笔，得让两条冲正都在对方插入之前读完已冲回额，而 PostgreSQL 自己的可见性
// 规则会挡掉大半——后读的那条一旦扫到对方尚未提交的冲正行，就会等它提交、然后读到新值。
// 于是「并发跑 N 条、看是不是只成一条」这种用例在缺锁的版本上也照样通过（实测 8 条并发
// 3 轮全过）。它验的是运气，这条验的是锁。
//
// 按住那行用的是 FOR KEY SHARE 而**不是 FOR UPDATE**：那把锁正是并发的另一条冲正在插入
// 自己的冲正行时会持有的锁（reverses_entry_id 的外键会把被引用的扣减行加上 FOR KEY SHARE，
// 一直拿到提交）。用 FOR UPDATE 的话这条用例证明不了任何事——它与那条外键锁也冲突，于是
// 少了本用例要钉的那把锁、冲正照样会停在外键检查上，测试照过（这里是踩过一遍才写下的）。
func TestReverseBeansWaitsForTheDeductionRow(t *testing.T) {
	pool := beanIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID := uuid.NewString()
	orderID := uuid.NewString()
	ctx := context.Background()

	if _, err := adjust(repo, userID, 5000, "req-"+uuid.NewString()[:8]); err != nil {
		t.Fatalf("adjust: %v", err)
	}
	if _, err := consume(repo, userID, orderID, 3400); err != nil {
		t.Fatalf("consume: %v", err)
	}

	// 另一条连接按住那笔扣减，扮演「另一条同订单的冲正在飞」。它不解锁，冲正就动不了。
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	var lockedID string
	if err := blocker.QueryRow(ctx, `SELECT id::text FROM coffee_bean_entries
		WHERE entry_key = $1 FOR KEY SHARE`, model.BeanConsumeKey(orderID)).Scan(&lockedID); err != nil {
		t.Fatalf("hold the deduction: %v", err)
	}

	type outcome struct {
		reversed bool
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		reversed, err := reverse(repo, orderID, "AS-"+uuid.NewString()[:8], 900)
		done <- outcome{reversed, err}
	}()

	select {
	case got := <-done:
		t.Fatalf("a reversal must wait for the row lock on the deduction before it decides how much is left, got %+v", got)
	case <-time.After(500 * time.Millisecond):
	}

	// 「会等」不等于「会卡死」：锁一让开，这次冲正必须照常做完。
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("release the deduction: %v", err)
	}
	select {
	case got := <-done:
		if got.err != nil || !got.reversed {
			t.Fatalf("the reversal must go through once the lock is free: %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the reversal never finished after the row lock was released")
	}
	if got := balanceOfBean(t, pool, userID); got != 2500 {
		t.Fatalf("want balance 2500 after a 900 refund, got %d", got)
	}
}

func TestReverseBeansPartiallyThenFully(t *testing.T) {
	pool := beanIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID := uuid.NewString()
	orderID := uuid.NewString()

	if _, err := adjust(repo, userID, 5000, "req-"+uuid.NewString()[:8]); err != nil {
		t.Fatalf("adjust: %v", err)
	}
	consumed, err := consume(repo, userID, orderID, 3400)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if got := balanceOfBean(t, pool, userID); got != 1600 {
		t.Fatalf("want balance 1600, got %d", got)
	}

	// 先退加购行（900）：部分退款是常态，所以冲正额由调用方给，不整单照抄。
	firstSale := "AS-" + uuid.NewString()[:8]
	reversed, err := reverse(repo, orderID, firstSale, 900)
	if err != nil {
		t.Fatalf("partial reverse: %v", err)
	}
	if !reversed {
		t.Fatal("a partial reverse must report true")
	}
	if got := balanceOfBean(t, pool, userID); got != 2500 {
		t.Fatalf("want balance 2500 after a 900 refund, got %d", got)
	}

	var reversedEntry *model.CoffeeBeanEntry
	for _, entry := range beanEntriesOf(t, pool, userID) {
		if entry.EntryType == model.BeanEntryTypeReverse {
			reversedEntry = entry
		}
	}
	if reversedEntry == nil {
		t.Fatal("the reversal entry is missing")
	}
	if reversedEntry.Amount != 900 {
		t.Fatalf("a reversal must be the positive mirror of the deduction, got %d", reversedEntry.Amount)
	}
	// 冲正指向被冲的那笔扣减：客服判断「这 900 分是退哪一笔的」看的就是它。
	if reversedEntry.ReversesEntryID == nil || *reversedEntry.ReversesEntryID != consumed.EntryID {
		t.Fatalf("the reversal must point at the deduction: %+v", reversedEntry)
	}
	if reversedEntry.EntryKey != model.BeanReverseKey(firstSale) ||
		reversedEntry.ReferenceNo != firstSale ||
		reversedEntry.ReferenceType != model.BeanReferenceTypeAfterSale {
		t.Fatalf("a reversal must be keyed and referenced by the after sale no: %+v", reversedEntry)
	}

	// 重投同一条售后事件：一张售后只能冲一次。回放走唯一索引，余额一格不动。
	twice, err := reverse(repo, orderID, firstSale, 900)
	if err != nil {
		t.Fatalf("replayed reverse: %v", err)
	}
	if twice {
		t.Fatal("a replayed after sale must report false: nothing was reversed the second time")
	}
	if got := balanceOfBean(t, pool, userID); got != 2500 {
		t.Fatalf("a replayed reversal must not move the balance, got %d", got)
	}

	// 再退整单的余额部分：**上限是「这笔扣减还没冲回的部分」**，也就是 3400-900=2500。
	// 减掉已冲回额这一步不能省，否则第二次部分退会把上一次退过的部分又退一遍。
	finalSale := "AS-" + uuid.NewString()[:8]
	if _, err := reverse(repo, orderID, finalSale, 2500); err != nil {
		t.Fatalf("reverse the rest: %v", err)
	}
	if got := balanceOfBean(t, pool, userID); got != 5000 {
		t.Fatalf("reversing the whole order must restore the original balance, got %d", got)
	}

	// 全冲回之后再重投**那一条**（这次是最后那条、把剩余冲干净的）售后：一样是 no-op，
	// 一样不报错。这一条单独验，是因为「冲干净了」会让上限判定算出剩余 0——若幂等查排在
	// 上限判定之后，这次重投就会撞成 ErrReverseUncovered，把一条正常的事件推进重试链。
	if _, err := reverse(repo, orderID, finalSale, 2500); err != nil {
		t.Fatalf("a replay of a fully reversed after sale must not be an error: %v", err)
	}
	if got := balanceOfBean(t, pool, userID); got != 5000 {
		t.Fatalf("a replay must not move the balance, got %d", got)
	}

	// 再退 1 分就是超退。订单域已经按「实付 - 已退 - 在途」钳过一次，理论上撞不到；真撞到
	// 说明有一处算错了，这里报错而不是静默钳制——欠退比错退更容易被忽略。
	if _, err := reverse(repo, orderID, "AS-"+uuid.NewString()[:8], 1); !errors.Is(err, ErrReverseUncovered) {
		t.Fatalf("want ErrReverseUncovered, got %v", err)
	}
	if got := balanceOfBean(t, pool, userID); got != 5000 {
		t.Fatalf("a rejected reversal must not move the balance, got %d", got)
	}
	// 三次冲正各一行（900、2500，以及被拒的那次一行都没有）。
	reverseCount := 0
	for _, entry := range beanEntriesOf(t, pool, userID) {
		if entry.EntryType == model.BeanEntryTypeReverse {
			reverseCount++
		}
	}
	if reverseCount != 2 {
		t.Fatalf("want two reversal rows, got %d", reverseCount)
	}
}
