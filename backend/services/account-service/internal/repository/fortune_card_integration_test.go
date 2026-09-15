package repository

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
)

// 这些用例只用一个调用方给的库 URL：不建库、不删库、不跑迁移，夹具一律用随机 UUID。
//
// **跑完不清理**：fortune_card_entries 有 append-only 触发器，DELETE 会直接抛；
// fortune_card_accounts 又被流水以 ON DELETE RESTRICT 引用着。这是那两张表的性质，
// 不是漏删——要把它们清掉只能整库重建。夹具 id 每次都是新的，所以残留不会影响下一次运行，
// 也不会让断言互相串台。
func fortuneCardIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("ACCOUNT_DATABASE_URL")
	if url == "" {
		url = os.Getenv("TEST_DATABASE_URL")
	}
	if url == "" {
		t.Skip("set ACCOUNT_DATABASE_URL or TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("account test database is unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("account test database is unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newAccountIDs 造一组互不相干的夹具 id。
func newAccountIDs() (userID, orderID, orderNo string) {
	orderID = uuid.NewString()
	return uuid.NewString(), orderID, "CO-TEST-" + orderID[:8]
}

// entryByKey 按幂等键读一行流水。按键读而不是按 id 读：幂等键是这几条用例真正在讨论的
// 东西，读到它才能确认「第二次调用没有新增一行，而是命中了同一行」。
func entryByKey(t *testing.T, pool *pgxpool.Pool, key string) *model.FortuneCardEntry {
	t.Helper()
	entry, err := scanEntry(pool.QueryRow(context.Background(),
		`SELECT `+entryColumns+` FROM fortune_card_entries WHERE entry_key = $1`, key))
	if err != nil {
		t.Fatalf("read entry %q: %v", key, err)
	}
	return entry
}

func countEntries(t *testing.T, pool *pgxpool.Pool, userID string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM fortune_card_entries WHERE user_id = $1`, userID).Scan(&count); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	return count
}

// balanceOf 读余额，并顺手验那条不变式：SUM(amount) 必须等于 balance。
//
// 这是本服务唯一一条真正的正确性判据——余额与流水在同一个事务里改，两边一旦不一致，
// 后面所有「他还有几张」的回答都不可信。所以每一处断言余额的地方都同时验它。
func balanceOf(t *testing.T, pool *pgxpool.Pool, userID string) int64 {
	t.Helper()
	var balance, ledgerSum int64
	if err := pool.QueryRow(context.Background(),
		`SELECT balance FROM fortune_card_accounts WHERE user_id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount),0) FROM fortune_card_entries WHERE user_id = $1`, userID).Scan(&ledgerSum); err != nil {
		t.Fatalf("sum entries: %v", err)
	}
	if balance != ledgerSum {
		t.Fatalf("balance %d does not equal the ledger sum %d", balance, ledgerSum)
	}
	return balance
}

func grant(repo *PostgresRepository, userID, orderID, orderNo string, lines ...GrantLine) ([]EntryResult, error) {
	return repo.GrantOrderFortune(context.Background(), GrantParams{
		UserID:     userID,
		OrderID:    orderID,
		OrderNo:    orderNo,
		OccurredAt: time.Now().UTC(),
		Lines:      lines,
	})
}

func TestGrantOrderFortuneWritesBothLinesOnce(t *testing.T) {
	pool := fortuneCardIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID, orderID, orderNo := newAccountIDs()
	baseKey := model.BaseGrantKey(orderID)
	bonusKey := model.BonusGrantKey(orderID, "campaign-1")

	results, err := grant(repo, userID, orderID, orderNo,
		GrantLine{Title: "订单完成赠送", Amount: 1, EntryKey: baseKey},
		GrantLine{Title: "订单完成赠送（幸运杯套）", Amount: 1, EntryKey: bonusKey},
	)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want two results, got %d", len(results))
	}
	// 两条在同一个事务里按顺序落：第一笔之后的余额是 1，第二笔之后是 2。顺序反了
	// （比如按 map 遍历），明细页上的「变动后余额」就会前后颠倒。
	if results[0].BalanceAfter != 1 || results[1].BalanceAfter != 2 {
		t.Fatalf("balance_after must follow the line order: %+v", results)
	}
	if results[0].Replayed || results[1].Replayed {
		t.Fatalf("a first grant must not look like a replay: %+v", results)
	}
	if got := balanceOf(t, pool, userID); got != 2 {
		t.Fatalf("want balance 2, got %d", got)
	}

	// 重投同一条事件：一行都不能多，余额一格都不能动。这就是「重放 order.completed 不会
	// 发两次福卡」的全部依据，靠的是 entry_key 的唯一索引，不是调用方的自觉。
	replayed, err := grant(repo, userID, orderID, orderNo,
		GrantLine{Title: "订单完成赠送", Amount: 1, EntryKey: baseKey},
		GrantLine{Title: "订单完成赠送（幸运杯套）", Amount: 1, EntryKey: bonusKey},
	)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replayed[0].Replayed || !replayed[1].Replayed {
		t.Fatalf("a replay must be reported as such: %+v", replayed)
	}
	if replayed[0].EntryID != results[0].EntryID || replayed[1].EntryID != results[1].EntryID {
		t.Fatalf("a replay must return the same entries: %+v vs %+v", replayed, results)
	}
	// 回放答的是**当时那笔**的余额，不是此刻的余额：调用方重试时要的就是当时那个回答。
	if replayed[0].BalanceAfter != 1 || replayed[1].BalanceAfter != 2 {
		t.Fatalf("a replay must answer the stored balance_after: %+v", replayed)
	}
	if got := countEntries(t, pool, userID); got != 2 {
		t.Fatalf("a replay must not add rows, got %d", got)
	}
	if got := balanceOf(t, pool, userID); got != 2 {
		t.Fatalf("a replay must not change the balance, got %d", got)
	}

	// 流水的可读字段：订单详情那个页签按订单号取全，列表按时间倒序，两者都靠这几列。
	entry := entryByKey(t, pool, baseKey)
	if entry.EntryType != model.EntryTypeGrant || entry.Amount != 1 || entry.BalanceAfter != 1 {
		t.Fatalf("base entry is wrong: %+v", entry)
	}
	if entry.ReferenceNo != orderNo || entry.ReferenceID != orderID || entry.ReferenceType != model.ReferenceTypeOrder {
		t.Fatalf("base entry lost the order reference: %+v", entry)
	}
}

func TestGrantRejectsAnEntryKeyOwnedByAnotherUser(t *testing.T) {
	pool := fortuneCardIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	firstUser, firstOrder, firstOrderNo := newAccountIDs()
	secondUser, _, secondOrderNo := newAccountIDs()
	key := model.BaseGrantKey(firstOrder)

	if _, err := grant(repo, firstUser, firstOrder, firstOrderNo,
		GrantLine{Title: "订单完成赠送", Amount: 1, EntryKey: key}); err != nil {
		t.Fatalf("first grant: %v", err)
	}

	// 幂等号撞到别人头上。回放那一笔是错的——调用方会拿到另一个人的 entry_id 与余额，
	// 所以这里必须报错，而不是「看起来成功了」。
	_, err := grant(repo, secondUser, firstOrder, secondOrderNo,
		GrantLine{Title: "订单完成赠送", Amount: 1, EntryKey: key})
	if !errors.Is(err, ErrEntryKeyConflict) {
		t.Fatalf("want ErrEntryKeyConflict, got %v", err)
	}
	// 整单回滚：第二个用户连账户行都不该被创建，更不该凭空多出 1 张。
	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM fortune_card_accounts WHERE user_id = $1)`, secondUser).Scan(&exists); err != nil {
		t.Fatalf("check second account: %v", err)
	}
	if exists {
		t.Fatal("a rolled back grant must not leave an account row behind")
	}
	if got := countEntries(t, pool, secondUser); got != 0 {
		t.Fatalf("a rolled back grant must not leave entries, got %d", got)
	}
}

func TestDeductRejectsAnOverdraftWithoutWritingAnything(t *testing.T) {
	pool := fortuneCardIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID, orderID, orderNo := newAccountIDs()

	if _, err := grant(repo, userID, orderID, orderNo,
		GrantLine{Title: "订单完成赠送", Amount: 1, EntryKey: model.BaseGrantKey(orderID)}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// 余额不足是「用户没钱」不是「服务坏了」：既不能扣成负数，也不能留下半笔账变。
	_, err := repo.Deduct(context.Background(), DeductParams{
		UserID:        userID,
		Amount:        2,
		Title:         "参与抽奖",
		ReferenceType: model.ReferenceTypeDraw,
		EntryKey:      model.DrawKey("participation-overdraft"),
		OccurredAt:    time.Now().UTC(),
	})
	if !errors.Is(err, ErrInsufficientFortuneCards) {
		t.Fatalf("want ErrInsufficientFortuneCards, got %v", err)
	}
	if got := countEntries(t, pool, userID); got != 1 {
		t.Fatalf("a rejected deduct must not write a row, got %d entries", got)
	}
	if got := balanceOf(t, pool, userID); got != 1 {
		t.Fatalf("a rejected deduct must not change the balance, got %d", got)
	}
}

func TestDeductIsIdempotentByRequestKey(t *testing.T) {
	pool := fortuneCardIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID, orderID, orderNo := newAccountIDs()

	if _, err := grant(repo, userID, orderID, orderNo,
		GrantLine{Title: "订单完成赠送", Amount: 2, EntryKey: model.BaseGrantKey(orderID)}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// request_id 每次都是新的：抽奖流水用的是 `draw:{participationId}`，写死一个号的话
	// 第二次运行会撞上上一轮留下的那一行（流水表只追加，清不掉），而那一行属于另一个
	// 随机用户——报出来的会是「幂等号发到别人头上了」，与被测的重放语义毫不相干。
	requestID := "participation-" + uuid.NewString()
	params := DeductParams{
		UserID:        userID,
		Amount:        1,
		Title:         "参与抽奖",
		ReferenceType: model.ReferenceTypeDraw,
		ReferenceID:   requestID,
		EntryKey:      model.DrawKey(requestID),
		OccurredAt:    time.Now().UTC(),
	}
	first, err := repo.Deduct(context.Background(), params)
	if err != nil {
		t.Fatalf("deduct: %v", err)
	}
	// 流水上扣减记负数：符号由 repository 定，调用方给的一直是「扣几张」。
	entry := entryByKey(t, pool, params.EntryKey)
	if entry.EntryType != model.EntryTypeDraw || entry.Amount != -1 {
		t.Fatalf("a deduct must be stored as a negative draw: %+v", entry)
	}
	if first.BalanceAfter != 1 {
		t.Fatalf("want balance_after 1, got %d", first.BalanceAfter)
	}

	// 同一个 request_id 重放（调用方超时重试）：拿回同一笔，不能再扣一张。
	second, err := repo.Deduct(context.Background(), params)
	if err != nil {
		t.Fatalf("replay deduct: %v", err)
	}
	if !second.Replayed || second.EntryID != first.EntryID {
		t.Fatalf("a replayed deduct must return the same entry: %+v vs %+v", second, first)
	}
	if got := balanceOf(t, pool, userID); got != 1 {
		t.Fatalf("a replayed deduct must not deduct twice, got balance %d", got)
	}
}

func TestReverseIsIdempotentAndRefusesChainedReversals(t *testing.T) {
	pool := fortuneCardIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID, orderID, orderNo := newAccountIDs()
	key := model.BaseGrantKey(orderID)

	granted, err := grant(repo, userID, orderID, orderNo,
		GrantLine{Title: "订单完成赠送", Amount: 1, EntryKey: key})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	params := ReverseParams{EntryID: granted[0].EntryID, Title: "冲正", OccurredAt: time.Now().UTC()}
	first, err := repo.Reverse(context.Background(), params)
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	entry := entryByKey(t, pool, model.ReverseKey(granted[0].EntryID))
	if entry.EntryType != model.EntryTypeReverse || entry.Amount != -1 {
		t.Fatalf("a reversal of a grant must be a negative reverse: %+v", entry)
	}
	// 冲正挂的是原流水的单据号，所以它和它冲掉的那笔出现在订单详情的同一屏里。
	if entry.ReferenceNo != orderNo {
		t.Fatalf("a reversal must carry the original order number, got %q", entry.ReferenceNo)
	}
	if entry.ReversesEntryID == nil || *entry.ReversesEntryID != granted[0].EntryID {
		t.Fatalf("a reversal must point at what it reverses: %+v", entry)
	}
	if got := balanceOf(t, pool, userID); got != 0 {
		t.Fatalf("want balance 0 after the reversal, got %d", got)
	}

	// 重放同一笔冲正：拿回同一行，余额不再动。
	second, err := repo.Reverse(context.Background(), params)
	if err != nil {
		t.Fatalf("replay reverse: %v", err)
	}
	if !second.Replayed || second.EntryID != first.EntryID {
		t.Fatalf("a replayed reverse must return the same entry: %+v vs %+v", second, first)
	}
	if got := balanceOf(t, pool, userID); got != 0 {
		t.Fatalf("a replayed reverse must not change the balance, got %d", got)
	}

	// 冲正一笔冲正：反向的反向就是原来那笔，那不是冲正的语义，得由人来判断。
	if _, err := repo.Reverse(context.Background(), ReverseParams{
		EntryID: first.EntryID, OccurredAt: time.Now().UTC(),
	}); !errors.Is(err, ErrNotReversible) {
		t.Fatalf("want ErrNotReversible, got %v", err)
	}
}

func TestReverseRefusesWhenTheCardsWereAlreadySpent(t *testing.T) {
	pool := fortuneCardIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID, orderID, orderNo := newAccountIDs()

	granted, err := grant(repo, userID, orderID, orderNo,
		GrantLine{Title: "订单完成赠送", Amount: 1, EntryKey: model.BaseGrantKey(orderID)})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := repo.Deduct(context.Background(), DeductParams{
		UserID:        userID,
		Amount:        1,
		Title:         "参与抽奖",
		ReferenceType: model.ReferenceTypeDraw,
		EntryKey:      model.DrawKey("participation-" + uuid.NewString()),
		OccurredAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("deduct: %v", err)
	}

	// 「已经抽过奖的福卡追不回来」是业务规则，不是故障：明确拒绝，而不是把余额扣成负数。
	_, err = repo.Reverse(context.Background(), ReverseParams{
		EntryID: granted[0].EntryID, Title: "冲正", OccurredAt: time.Now().UTC(),
	})
	if !errors.Is(err, ErrReverseUncovered) {
		t.Fatalf("want ErrReverseUncovered, got %v", err)
	}
	if got := balanceOf(t, pool, userID); got != 0 {
		t.Fatalf("a refused reversal must not change the balance, got %d", got)
	}

	// 不存在的流水：ErrEntryNotFound，同样不能留下一行。
	if _, err := repo.Reverse(context.Background(), ReverseParams{
		EntryID: uuid.NewString(), Title: "冲正", OccurredAt: time.Now().UTC(),
	}); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("want ErrEntryNotFound, got %v", err)
	}
}

func TestListEntriesFiltersAndPaging(t *testing.T) {
	pool := fortuneCardIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID, orderID, orderNo := newAccountIDs()
	otherUser, otherOrder, _ := newAccountIDs()

	occurredAt := time.Now().UTC().Add(-time.Hour)
	for i, key := range []string{model.BaseGrantKey(orderID), model.BonusGrantKey(orderID, "campaign-2")} {
		if _, err := repo.GrantOrderFortune(context.Background(), GrantParams{
			UserID:     userID,
			OrderID:    orderID,
			OrderNo:    orderNo,
			OccurredAt: occurredAt.Add(time.Duration(i) * time.Minute),
			Lines:      []GrantLine{{Title: "订单完成赠送", Amount: 1, EntryKey: key}},
		}); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}
	// 另一个用户的流水：下面每一条查询都必须把它排除在外，否则「谁的单」这件事就漏了。
	if _, err := grant(repo, otherUser, otherOrder, "CO-TEST-OTHER",
		GrantLine{Title: "订单完成赠送", Amount: 5, EntryKey: model.BaseGrantKey(otherOrder)}); err != nil {
		t.Fatalf("grant other user: %v", err)
	}

	entries, total, err := repo.ListEntries(context.Background(), dto.EntryQuery{
		UserID: userID, Page: 1, PageSize: 20,
	})
	if err != nil {
		t.Fatalf("list by user: %v", err)
	}
	if total != 2 || len(entries) != 2 {
		t.Fatalf("want the caller's two entries, got %d of %d", len(entries), total)
	}
	// 倒序：明细页最新的一条在最上面。
	if !entries[0].OccurredAt.After(entries[1].OccurredAt) {
		t.Fatalf("entries must be newest first: %+v", entries)
	}

	// 订单详情那个页签的查法：只给订单号。
	entries, total, err = repo.ListEntries(context.Background(), dto.EntryQuery{
		OrderNo: orderNo, Page: 1, PageSize: 20,
	})
	if err != nil {
		t.Fatalf("list by order number: %v", err)
	}
	if total != 2 || len(entries) != 2 {
		t.Fatalf("want the order's two entries, got %d of %d", len(entries), total)
	}
	for _, entry := range entries {
		if entry.ReferenceNo != orderNo {
			t.Fatalf("a filter by order number returned another order: %+v", entry)
		}
	}

	// 闭开区间 [From, To)：第一笔在 [occurredAt, occurredAt+1m) 里，第二笔不在。
	to := occurredAt.Add(time.Minute)
	entries, total, err = repo.ListEntries(context.Background(), dto.EntryQuery{
		UserID: userID, From: &occurredAt, To: &to, Page: 1, PageSize: 20,
	})
	if err != nil {
		t.Fatalf("list by time window: %v", err)
	}
	if total != 1 || len(entries) != 1 || !entries[0].OccurredAt.Equal(occurredAt) {
		t.Fatalf("want exactly the first entry, got %d of %d: %+v", len(entries), total, entries)
	}

	// 按类型筛：两笔都是发放，一笔抽奖都不该在这里出现。
	entries, total, err = repo.ListEntries(context.Background(), dto.EntryQuery{
		UserID: userID, EntryType: model.EntryTypeDraw, Page: 1, PageSize: 20,
	})
	if err != nil {
		t.Fatalf("list by type: %v", err)
	}
	if total != 0 || len(entries) != 0 {
		t.Fatalf("want no draws for this user, got %d of %d", len(entries), total)
	}

	// 分页：每页一条，两页取全，两页之间不重不漏。
	seen := map[string]bool{}
	for page := 1; page <= 2; page++ {
		entries, total, err := repo.ListEntries(context.Background(), dto.EntryQuery{
			UserID: userID, Page: page, PageSize: 1,
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if total != 2 || len(entries) != 1 {
			t.Fatalf("page %d: want one of two, got %d of %d", page, len(entries), total)
		}
		if seen[entries[0].ID] {
			t.Fatalf("page %d repeated an entry: %s", page, entries[0].ID)
		}
		seen[entries[0].ID] = true
	}
}

// freezeOf 按售后单号读一行冻结。这个号是冻结侧唯一的幂等键，也就是下面几条用例真正在
// 讨论的东西——按它读才能确认「第二次调用没有新增一行，而是命中了同一行」。
func freezeOf(t *testing.T, pool *pgxpool.Pool, afterSaleNo string) *model.FortuneCardFreeze {
	t.Helper()
	freeze, err := scanFreeze(pool.QueryRow(context.Background(),
		`SELECT `+freezeColumns+` FROM fortune_card_freezes WHERE after_sale_no = $1`, afterSaleNo))
	if err != nil {
		t.Fatalf("read freeze %q: %v", afterSaleNo, err)
	}
	return freeze
}

func countFreezes(t *testing.T, pool *pgxpool.Pool, userID string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM fortune_card_freezes WHERE user_id = $1`, userID).Scan(&count); err != nil {
		t.Fatalf("count freezes: %v", err)
	}
	return count
}

// frozenAndAvailable 读冻结张数与可用张数。
//
// 只验 frozen_balance <= balance 那条 CHECK 是不够的：它守的是「不自相矛盾」，而这里要的是
// 「可用」这个导出量的具体数值——扣减的判据就是它，算错了不变量照样成立。
func frozenAndAvailable(t *testing.T, pool *pgxpool.Pool, userID string) (frozen, available int64) {
	t.Helper()
	var balance int64
	if err := pool.QueryRow(context.Background(),
		`SELECT balance, frozen_balance FROM fortune_card_accounts WHERE user_id = $1`,
		userID).Scan(&balance, &frozen); err != nil {
		t.Fatalf("read account: %v", err)
	}
	return frozen, balance - frozen
}

// testFreezeReason 是一句任意的原因文案。原因那句话归 service 管（applied / rejected /
// cancelled 三选一），repository 只负责把它原样存下来，所以这里不抄 service 的措辞——
// 抄一份出来就有了「两处各改一次、只改了一处」的可能。
const testFreezeReason = "测试：申请退款冻结"

func freezeAfterSale(repo *PostgresRepository, userID, orderID, orderNo, afterSaleNo string, keys ...string) (bool, error) {
	return repo.FreezeAfterSale(context.Background(), FreezeParams{
		UserID:      userID,
		AfterSaleNo: afterSaleNo,
		OrderID:     orderID,
		OrderNo:     orderNo,
		EntryKeys:   keys,
		Reason:      testFreezeReason,
		OccurredAt:  time.Now().UTC(),
	})
}

func releaseAfterSale(repo *PostgresRepository, afterSaleNo string) (bool, error) {
	return repo.ReleaseAfterSale(context.Background(), ReleaseParams{
		AfterSaleNo: afterSaleNo,
		Reason:      "测试：解冻",
		OccurredAt:  time.Now().UTC(),
	})
}

func TestFreezeAfterSaleHoldsOnlyTheNamedKeys(t *testing.T) {
	pool := fortuneCardIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID, orderID, orderNo := newAccountIDs()
	baseKey := model.BaseGrantKey(orderID)
	bonusKey := model.BonusGrantKey(orderID, "campaign-1")

	if _, err := grant(repo, userID, orderID, orderNo,
		GrantLine{Title: "订单完成赠送", Amount: 1, EntryKey: baseKey},
		GrantLine{Title: "订单完成赠送（幸运杯套）", Amount: 1, EntryKey: bonusKey},
	); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// 只退加购行：冻的只有 bonus 那一张。这是「按退款范围冻」的全部含义，也是要的粒度——
	// 整单全冻会把用户根本没退的那张饮品卡一起锁上。
	afterSaleNo := "REF-TEST-" + uuid.NewString()
	created, err := freezeAfterSale(repo, userID, orderID, orderNo, afterSaleNo, bonusKey)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if !created {
		t.Fatal("a freeze that holds something must report itself as created")
	}
	freeze := freezeOf(t, pool, afterSaleNo)
	if len(freeze.EntryKeys) != 1 || freeze.EntryKeys[0] != bonusKey {
		t.Fatalf("a freeze must hold exactly the keys it was given: %+v", freeze.EntryKeys)
	}
	if freeze.Amount != 1 || freeze.Status != model.FreezeStatusFrozen {
		t.Fatalf("want one card held in frozen state, got %+v", freeze)
	}
	if freeze.OrderID != orderID || freeze.OrderNo != orderNo {
		t.Fatalf("a freeze must record which order it belongs to: %+v", freeze)
	}
	if freeze.Reason != testFreezeReason {
		t.Fatalf("the reason must be stored as given, got %q", freeze.Reason)
	}
	if freeze.ReleasedAt != nil {
		t.Fatalf("a fresh freeze must not look released: %+v", freeze)
	}

	// 冻结**不是账变**：余额一格没动、一行流水也没多。它改的只有可用。
	if got := balanceOf(t, pool, userID); got != 2 {
		t.Fatalf("a freeze must not change the balance, got %d", got)
	}
	if got := countEntries(t, pool, userID); got != 2 {
		t.Fatalf("a freeze must not write ledger entries, got %d", got)
	}
	frozen, available := frozenAndAvailable(t, pool, userID)
	if frozen != 1 || available != 1 {
		t.Fatalf("want 1 frozen and 1 available, got %d and %d", frozen, available)
	}
}

func TestFreezeAfterSaleIsIdempotentByAfterSaleNo(t *testing.T) {
	pool := fortuneCardIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID, orderID, orderNo := newAccountIDs()
	bonusKey := model.BonusGrantKey(orderID, "campaign-1")

	if _, err := grant(repo, userID, orderID, orderNo,
		GrantLine{Title: "订单完成赠送", Amount: 1, EntryKey: bonusKey}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	afterSaleNo := "REF-TEST-" + uuid.NewString()
	if _, err := freezeAfterSale(repo, userID, orderID, orderNo, afterSaleNo, bonusKey); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	// 同一条 applied 事件重投（消费侧重试、或者 broker 至少一次投递）：冻结行的唯一键挡住
	// 第二次，frozen_balance 不能跟着加第二遍——加两遍的后果是用户的两张卡被永久冻住。
	again, err := freezeAfterSale(repo, userID, orderID, orderNo, afterSaleNo, bonusKey)
	if err != nil {
		t.Fatalf("replay freeze: %v", err)
	}
	if again {
		t.Fatal("a replayed freeze must not report itself as created")
	}
	if got := countFreezes(t, pool, userID); got != 1 {
		t.Fatalf("a replayed freeze must not add rows, got %d", got)
	}
	frozen, available := frozenAndAvailable(t, pool, userID)
	if frozen != 1 || available != 0 {
		t.Fatalf("a replayed freeze must not freeze twice, got %d frozen / %d available", frozen, available)
	}
	if got := freezeOf(t, pool, afterSaleNo).Amount; got != 1 {
		t.Fatalf("a replayed freeze must not change the amount, got %d", got)
	}
}

func TestFreezeBeforeGrantIsBackfilledWhenTheCardsArrive(t *testing.T) {
	pool := fortuneCardIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID, orderID, orderNo := newAccountIDs()
	baseKey := model.BaseGrantKey(orderID)

	// paid 状态就允许申请退款，所以冻结会先于发放到达。这一刻账上一张卡都没有，冻不出东西。
	afterSaleNo := "REF-TEST-" + uuid.NewString()
	if _, err := freezeAfterSale(repo, userID, orderID, orderNo, afterSaleNo, baseKey); err != nil {
		t.Fatalf("freeze before grant: %v", err)
	}
	freeze := freezeOf(t, pool, afterSaleNo)
	// 空壳照建：它就是「这一单申请过退款」这件事在账上的凭据，解冻时靠它放回去。
	if freeze.Amount != 0 || freeze.Status != model.FreezeStatusFrozen {
		t.Fatalf("a freeze that arrived before the grant must be an empty shell: %+v", freeze)
	}
	if frozen, _ := frozenAndAvailable(t, pool, userID); frozen != 0 {
		t.Fatalf("nothing was granted, so nothing can be frozen: %d", frozen)
	}

	// 发放落库：同一事务里把这次的张数补进正盖着这个键的冻结行。少了这一步，卡会在
	// 退款申请之后照常发出去、照常能抽——而那正是这一轮要堵的口子。
	if _, err := grant(repo, userID, orderID, orderNo,
		GrantLine{Title: "订单完成赠送", Amount: 1, EntryKey: baseKey}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if got := freezeOf(t, pool, afterSaleNo).Amount; got != 1 {
		t.Fatalf("the grant must be backfilled into the pending freeze, got amount %d", got)
	}
	frozen, available := frozenAndAvailable(t, pool, userID)
	if frozen != 1 || available != 0 {
		t.Fatalf("want 1 frozen and 0 available, got %d and %d", frozen, available)
	}
}

// TestFreezeBeforeGrantBackfillsEveryKeyOfTheOrder 守住「冻结行盖着多个发放键时，
// 后到的那些键也要把张数补进去」。
//
// 一条售后单冻的是它那一单的**全部**发放键（基础赠送 + 幸运杯套……），所以冻结行上的
// amount 是这些键的总和；而补张数这件事是**逐键**发生的——发放流水一条一条落库，每条
// 只带自己那个键的张数。拿「本次单键的张数」去减「行上多键的总和」，两个口径相减没有
// 意义，结果是后面的键只补进去一个差额，剩下的卡照旧能拿去抽奖。
//
// 少冻是静默的：frozen_balance <= balance 那条不变量照样成立，没有任何一层会报错。
// 所以要断言冻结张数的**具体数值**——只验不变量，这个错验不出来。
func TestFreezeBeforeGrantBackfillsEveryKeyOfTheOrder(t *testing.T) {
	pool := fortuneCardIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID, orderID, orderNo := newAccountIDs()
	baseKey := model.BaseGrantKey(orderID)
	bonusKey := model.BonusGrantKey(orderID, "campaign-1")

	afterSaleNo := "REF-TEST-" + uuid.NewString()
	if _, err := freezeAfterSale(repo, userID, orderID, orderNo, afterSaleNo, baseKey, bonusKey); err != nil {
		t.Fatalf("freeze before grant: %v", err)
	}

	// 基础键 1 张先到并被补进去，幸运杯套 2 张随后到——第二笔才是这条用例要观察的：
	// 修复前它只补 min(2, 2-1)=1 张，最后一张卡在自己的退款申请之后仍然能抽。
	if _, err := grant(repo, userID, orderID, orderNo,
		GrantLine{Title: "订单完成赠送", Amount: 1, EntryKey: baseKey},
		GrantLine{Title: "订单完成赠送（幸运杯套）", Amount: 2, EntryKey: bonusKey}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	if got := freezeOf(t, pool, afterSaleNo).Amount; got != 3 {
		t.Fatalf("这一单发的 3 张都该冻上，freeze amount = %d", got)
	}
	frozen, available := frozenAndAvailable(t, pool, userID)
	if frozen != 3 || available != 0 {
		t.Fatalf("want 3 frozen and 0 available, got %d and %d", frozen, available)
	}
}

func TestReleaseAfterSaleRestoresAvailabilityAndIsANoOpOtherwise(t *testing.T) {
	pool := fortuneCardIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID, orderID, orderNo := newAccountIDs()
	bonusKey := model.BonusGrantKey(orderID, "campaign-1")

	if _, err := grant(repo, userID, orderID, orderNo,
		GrantLine{Title: "订单完成赠送", Amount: 1, EntryKey: bonusKey}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// 从头就没有过的售后单：no-op 而不是错误。两条解冻来路（驳回、撤销）各自独立投递，
	// 把「没什么可解的」做成错误，会让一条正常的重投一路重试到死信。
	released, err := releaseAfterSale(repo, "REF-TEST-"+uuid.NewString())
	if err != nil {
		t.Fatalf("releasing an unknown after-sale number must not error: %v", err)
	}
	if released {
		t.Fatal("releasing an unknown after-sale number must report nothing released")
	}

	afterSaleNo := "REF-TEST-" + uuid.NewString()
	if _, err := freezeAfterSale(repo, userID, orderID, orderNo, afterSaleNo, bonusKey); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	released, err = releaseAfterSale(repo, afterSaleNo)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if !released {
		t.Fatal("releasing a live freeze must report itself as released")
	}
	freeze := freezeOf(t, pool, afterSaleNo)
	if freeze.Status != model.FreezeStatusReleased || freeze.ReleasedAt == nil {
		t.Fatalf("want a released row with a timestamp, got %+v", freeze)
	}
	// amount 不清零：审计要看当初冻了多少，清掉就只剩「有过一张申请」。
	if freeze.Amount != 1 {
		t.Fatalf("releasing must not erase how much was held, got %d", freeze.Amount)
	}
	// 解冻不写流水：余额没变过，变的只有可用。
	if got := balanceOf(t, pool, userID); got != 1 {
		t.Fatalf("a release must not change the balance, got %d", got)
	}
	if got := countEntries(t, pool, userID); got != 1 {
		t.Fatalf("a release must not write ledger entries, got %d", got)
	}
	frozen, available := frozenAndAvailable(t, pool, userID)
	if frozen != 0 || available != 1 {
		t.Fatalf("want 0 frozen and 1 available after release, got %d and %d", frozen, available)
	}

	// 再解一次（另一条来路后到）：no-op，且不能把 frozen_balance 减出个负数来。
	released, err = releaseAfterSale(repo, afterSaleNo)
	if err != nil {
		t.Fatalf("a second release must not error: %v", err)
	}
	if released {
		t.Fatal("a second release must report nothing released")
	}
	if frozen, _ := frozenAndAvailable(t, pool, userID); frozen != 0 {
		t.Fatalf("a second release must not drive the frozen balance below zero: %d", frozen)
	}
}

// waitUntilSomethingIsLocked 等到有别的后端确实卡在某把锁上。只用作同步：本用例自己握着
// 账户行的时候，会卡住的只可能是下面那条解冻。看的是 wait_event_type 而不是 query 文本
// ——后者对不是自己角色的后端会被屏蔽成 <insufficient privilege>，那会让这条用例在换了
// 一个库用户之后变成一条永远等不到的用例。
func waitUntilSomethingIsLocked(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the release never reached the account row")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestReleaseAfterSaleLocksTheAccountBeforeTheFreezeRow 钉住解冻那两句 SQL 的**顺序**。
//
// 解冻要同时动账户行（frozen_balance）与冻结行，两把锁都得拿；发放那条路要拿的也是这两把，
// 顺序是先账户行、再由 bindGrantToFreezes 拿冻结行。顺序相反就成了经典的成环：发放握着
// 账户行等冻结行，解冻握着冻结行等账户行，PostgreSQL 挑一条报死锁——那一条事件落一次谁
// 也说不清的中断。要撞上得有两个消费者同时处理同一个人的「发放」与「解冻」（多副本），
// 单副本串行消费碰不到，所以这条用例直接用一把外部的锁把那个交错的时刻摆出来。
//
// 摆法：另一条连接先握着账户行（扮演正在发放的那个消费者），解冻就必须停在账户行上。
// 这时从第三条连接用 NOWAIT 去要那把冻结行——**必须拿得到**。拿不到就说明解冻在等账户行
// 之前已经把冻结行攥在手里了，也就是那个会成环的顺序。
func TestReleaseAfterSaleLocksTheAccountBeforeTheFreezeRow(t *testing.T) {
	pool := fortuneCardIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID, orderID, orderNo := newAccountIDs()
	baseKey := model.BaseGrantKey(orderID)
	ctx := context.Background()

	if _, err := grant(repo, userID, orderID, orderNo,
		GrantLine{Title: "订单完成赠送", Amount: 2, EntryKey: baseKey}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	afterSaleNo := "REF-LOCK-" + uuid.NewString()
	// 冻结按的是**发放键**，所以这一单的两张都冻上（那正是「申请退款之后不能再抽」）。
	// 解冻之后可用回到 2，余额始终是 2——解冻不动余额，只动可用。
	if _, err := freezeAfterSale(repo, userID, orderID, orderNo, afterSaleNo, baseKey); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if frozen, available := frozenAndAvailable(t, pool, userID); frozen != 2 || available != 0 {
		t.Fatalf("want 2 frozen and 0 available before the release, got %d and %d", frozen, available)
	}

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	var held string
	if err := holder.QueryRow(ctx, `SELECT user_id::text FROM fortune_card_accounts
		WHERE user_id = $1 FOR UPDATE`, userID).Scan(&held); err != nil {
		t.Fatalf("hold the account row: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := releaseAfterSale(repo, afterSaleNo)
		done <- err
	}()
	waitUntilSomethingIsLocked(t, pool)

	probe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin probe: %v", err)
	}
	var probedID string
	err = probe.QueryRow(ctx, `SELECT id::text FROM fortune_card_freezes
		WHERE after_sale_no = $1 FOR UPDATE NOWAIT`, afterSaleNo).Scan(&probedID)
	_ = probe.Rollback(ctx)
	if err != nil {
		t.Fatalf("a release waiting on the account row must not be holding the freeze row: %v", err)
	}

	// 让开账户行：解冻必须接着做完，而不是卡死。
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release the account row: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the release never finished after the account row was free")
	}
	if frozen, available := frozenAndAvailable(t, pool, userID); frozen != 0 || available != 2 {
		t.Fatalf("want 0 frozen and 2 available after the release, got %d and %d", frozen, available)
	}
	if got := balanceOf(t, pool, userID); got != 2 {
		t.Fatalf("a release must not move the balance, got %d", got)
	}
}

func TestDeductSpendsOnlyTheAvailableBalance(t *testing.T) {
	pool := fortuneCardIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID, orderID, orderNo := newAccountIDs()
	baseKey := model.BaseGrantKey(orderID)
	bonusKey := model.BonusGrantKey(orderID, "campaign-1")

	if _, err := grant(repo, userID, orderID, orderNo,
		GrantLine{Title: "订单完成赠送", Amount: 1, EntryKey: baseKey},
		GrantLine{Title: "订单完成赠送（幸运杯套）", Amount: 1, EntryKey: bonusKey},
	); err != nil {
		t.Fatalf("grant: %v", err)
	}
	afterSaleNo := "REF-TEST-" + uuid.NewString()
	if _, err := freezeAfterSale(repo, userID, orderID, orderNo, afterSaleNo, bonusKey); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	// 余额是 2，但其中 1 张正冻在退款申请上。抽奖看的是**可用**，所以这一笔必须被拒——
	// 只看 balance 的老判据会放行，于是钱要退、卡已经抽掉，追不回来。
	_, err := repo.Deduct(context.Background(), DeductParams{
		UserID:        userID,
		Amount:        2,
		Title:         "参与抽奖",
		ReferenceType: model.ReferenceTypeDraw,
		EntryKey:      model.DrawKey("freeze-deduct-" + uuid.NewString()),
		OccurredAt:    time.Now().UTC(),
	})
	if !errors.Is(err, ErrInsufficientFortuneCards) {
		t.Fatalf("want ErrInsufficientFortuneCards, got %v", err)
	}
	if got := countEntries(t, pool, userID); got != 2 {
		t.Fatalf("a rejected deduct must not write a row, got %d entries", got)
	}
	if got := balanceOf(t, pool, userID); got != 2 {
		t.Fatalf("a rejected deduct must not change the balance, got %d", got)
	}

	// 扣得动的那 1 张：余额里扣，冻结那 1 张原地不动，可用归零。
	if _, err := repo.Deduct(context.Background(), DeductParams{
		UserID:        userID,
		Amount:        1,
		Title:         "参与抽奖",
		ReferenceType: model.ReferenceTypeDraw,
		EntryKey:      model.DrawKey("freeze-deduct-" + uuid.NewString()),
		OccurredAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("deduct: %v", err)
	}
	if got := balanceOf(t, pool, userID); got != 1 {
		t.Fatalf("want balance 1 after the draw, got %d", got)
	}
	frozen, available := frozenAndAvailable(t, pool, userID)
	if frozen != 1 || available != 0 {
		t.Fatalf("the held card must stay held, got %d frozen / %d available", frozen, available)
	}
}

func TestFreezeAfterSaleIsClampedToTheAvailableBalance(t *testing.T) {
	pool := fortuneCardIntegrationPool(t)
	repo := NewPostgresRepository(pool)
	userID, orderID, orderNo := newAccountIDs()
	baseKey := model.BaseGrantKey(orderID)

	if _, err := grant(repo, userID, orderID, orderNo,
		GrantLine{Title: "订单完成赠送", Amount: 1, EntryKey: baseKey}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// 用户先拿这张卡抽了奖，然后才来申请退款。冻不满是正常的——那正是审核时「福卡未参与
	// 抽奖」那个人工确认闸门在管的事；冻出个负数可用不是，所以这里要被钳到 0。
	if _, err := repo.Deduct(context.Background(), DeductParams{
		UserID:        userID,
		Amount:        1,
		Title:         "参与抽奖",
		ReferenceType: model.ReferenceTypeDraw,
		EntryKey:      model.DrawKey("freeze-clamp-" + uuid.NewString()),
		OccurredAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("deduct: %v", err)
	}

	afterSaleNo := "REF-TEST-" + uuid.NewString()
	if _, err := freezeAfterSale(repo, userID, orderID, orderNo, afterSaleNo, baseKey); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	freeze := freezeOf(t, pool, afterSaleNo)
	if freeze.Amount != 0 || freeze.Status != model.FreezeStatusFrozen {
		t.Fatalf("want a clamp to zero rather than a negative available, got %+v", freeze)
	}
	if frozen, available := frozenAndAvailable(t, pool, userID); frozen != 0 || available != 0 {
		t.Fatalf("want 0 frozen and 0 available, got %d and %d", frozen, available)
	}
	if got := balanceOf(t, pool, userID); got != 0 {
		t.Fatalf("a clamped freeze must not change the balance, got %d", got)
	}
}
