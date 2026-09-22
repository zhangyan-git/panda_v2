package repository

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
)

// 这一份验的是 Begin 的两条入口之间的关系：**幂等回放**与**新建**。
//
// 夹具与「跑完不清理」的说明都在 lottery_integration_test.go 的文件头，这一份直接复用
// 那一套（同一个 package）。用例 id 每次都是新的，所以留下的残留不会让断言串台。

// TestBeginReplaysAnExistingKeyAfterTheRoundClosed 是「卡已经扣了，期次却关了」这一条的
// 直接验证。
//
// 顺序反过来（先判期次再判幂等）的后果是一个真实场景：用户参与的那一下恰好把这一期收满，
// 或者响应丢了他又点了一次，而这时期次已经 closed/drawn/cancelled。他拿到的是一个 409，
// 而卡已经在账户域扣掉了——**参与查不到、卡也退不回来**。幂等键的全部意义就是「同一个键
// 永远拿回同一个答案」，期次校验挡的是新的参与，不是一条已经存在的参与的回放。
func TestBeginReplaysAnExistingKeyAfterTheRoundClosed(t *testing.T) {
	// 门槛 1：一次确认就把这一期收满并转 closed，不必手工改期次状态。
	f := newLotteryFixture(t, 1, 1)
	ctx := testContext(t)

	userID := uuid.NewString()
	key := "order:" + uuid.NewString()
	first := f.participate(t, ctx, f.round.ID, userID, key)

	if round := roundOf(t, ctx, f, f.round.ID); round.Status != model.RoundClosed {
		t.Fatalf("门槛 1 的期次在第一次确认之后应当是 %q，实际是 %q",
			model.RoundClosed, round.Status)
	}

	replay, err := f.repo.Begin(ctx, BeginParams{
		RoundID: f.round.ID, UserID: userID, IdempotencyKey: key, Cost: 1, Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("期次已关闭时用同一个幂等键重发，得到 %v，期望回放：这个键卡已经被扣过了", err)
	}
	if replay.Created {
		t.Fatal("同一个幂等键重发又新建了一行")
	}
	if replay.Participation.ID != first.ID {
		t.Fatalf("回放拿回了另一条参与：%s ≠ %s", replay.Participation.ID, first.ID)
	}
	if replay.Participation.Status != model.ParticipationConfirmed {
		t.Fatalf("回放的那条参与状态是 %q，期望 %q",
			replay.Participation.Status, model.ParticipationConfirmed)
	}
	// Round 是从这一行自己的 round_id 读的，回放要说的是「你当初参与的是哪一期」。
	if replay.Round == nil || replay.Round.ID != f.round.ID {
		t.Fatalf("回放带回来的期次是 %+v，期望 %s", replay.Round, f.round.ID)
	}

	// 只成交过一次：这个键在库里仍然只有一行。
	if got := countRows(t, ctx, f, `SELECT COUNT(*) FROM lottery_participations
		WHERE idempotency_key=$1`, key); got != 1 {
		t.Fatalf("同一个幂等键留下了 %d 行参与，期望 1 行", got)
	}
}

// TestBeginReplaysAnExistingKeyIntoTheRoundItWasRecordedIn 验的是回放读的是**行里的**
// round_id，不是调用方这次请求里带的期次。
//
// 客户端拿着旧链接重发、或者服务端把期次号传错，都不该让回放的答案变成另一期：那个键对应
// 的参与是确确实实发生在原来那一期里的。
func TestBeginReplaysAnExistingKeyIntoTheRoundItWasRecordedIn(t *testing.T) {
	ctx := testContext(t)
	first := newLotteryFixture(t, 10, 1)
	other := newLotteryFixture(t, 10, 1)

	userID := uuid.NewString()
	key := "order:" + uuid.NewString()
	participation := first.participate(t, ctx, first.round.ID, userID, key)

	replay, err := other.repo.Begin(ctx, BeginParams{
		RoundID: other.round.ID, UserID: userID, IdempotencyKey: key, Cost: 1, Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("用同一个键、换一个期次号重发，得到 %v，期望回放", err)
	}
	if replay.Created {
		t.Fatal("这不是一条新的参与，回放不该新建行")
	}
	if replay.Participation.ID != participation.ID {
		t.Fatalf("回放拿回了另一条参与：%s ≠ %s", replay.Participation.ID, participation.ID)
	}
	if replay.Round == nil || replay.Round.ID != first.round.ID {
		t.Fatalf("回放带回来的期次是 %+v，期望它属于原来那一期 %s", replay.Round, first.round.ID)
	}
}

// TestBeginRefusesANewKeyOnAClosedRound 是上一条的反面：**新的**参与仍然必须过期次那一关。
//
// 把幂等判定提前不能顺手把这道门拆掉——不然期次关了之后随便换个 Idempotency-Key 就能继续
// 挤进来，而这一期已经进了开奖名单。
func TestBeginRefusesANewKeyOnAClosedRound(t *testing.T) {
	f := newLotteryFixture(t, 1, 1)
	ctx := testContext(t)

	// 先把这一期收满关上。
	f.participate(t, ctx, f.round.ID, uuid.NewString(), "order:"+uuid.NewString())
	if round := roundOf(t, ctx, f, f.round.ID); round.Status != model.RoundClosed {
		t.Fatalf("门槛 1 的期次在第一次确认之后应当是 %q，实际是 %q",
			model.RoundClosed, round.Status)
	}

	_, err := f.repo.Begin(ctx, BeginParams{
		RoundID: f.round.ID, UserID: uuid.NewString(),
		IdempotencyKey: "order:" + uuid.NewString(), Cost: 1, Now: time.Now(),
	})
	if err == nil || !errors.Is(err, ErrRoundClosed) {
		t.Fatalf("用一个新的幂等键参与一期已关闭的期次，得到 %v，期望 ErrRoundClosed", err)
	}
}

// TestBeginRefusesANewKeyOnACancelledRound 覆盖另一条期次出路（作废）。作废是终态，同样
// 不收新人；同样，它也不该影响一条已经存在的参与的回放——那一条由上面两个用例管。
func TestBeginRefusesANewKeyOnACancelledRound(t *testing.T) {
	f := newLotteryFixture(t, 50, 1)
	ctx := testContext(t)
	cancelRoundDirectly(t, ctx, f, f.round.ID)

	_, err := f.repo.Begin(ctx, BeginParams{
		RoundID: f.round.ID, UserID: uuid.NewString(),
		IdempotencyKey: "order:" + uuid.NewString(), Cost: 1, Now: time.Now(),
	})
	if err == nil || !errors.Is(err, ErrRoundClosed) {
		t.Fatalf("用一个新的幂等键参与一期已作废的期次，得到 %v，期望 ErrRoundClosed", err)
	}
}

// TestConcurrentBeginsWithTheSameKeyOnlyCreateOneRow 把「同一个键并发进来只成功一次」钉住。
//
// 幂等回放的判定被提到了期次校验之前，而它是**一次不加锁的读**，所以两个并发请求会一起
// 走到 miss、一起插。这一次仍然只有一个能成——依据是唯一索引加 ON CONFLICT DO NOTHING
// （下面那条回读分支就是为它留的）。这条用例是那个依据的现场验证：少了它，把顺序改回来的
// 人不会看到任何红灯。
func TestConcurrentBeginsWithTheSameKeyOnlyCreateOneRow(t *testing.T) {
	const racers = 8
	f := newLotteryFixture(t, 100, 1)
	ctx := testContext(t)

	userID := uuid.NewString()
	key := "order:" + uuid.NewString()

	created := make([]bool, racers)
	ids := make([]string, racers)
	failures := make([]string, racers)

	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			begun, err := f.repo.Begin(ctx, BeginParams{
				RoundID: f.round.ID, UserID: userID, IdempotencyKey: key, Cost: 1, Now: time.Now(),
			})
			if err != nil {
				failures[i] = err.Error()
				return
			}
			created[i] = begun.Created
			ids[i] = begun.Participation.ID
		}(i)
	}
	wg.Wait()

	for i, message := range failures {
		if message != "" {
			t.Errorf("第 %d 个并发请求：%s", i, message)
		}
	}
	createdCount := 0
	want := ""
	for i, isCreated := range created {
		if !isCreated {
			continue
		}
		createdCount++
		want = ids[i]
	}
	if createdCount != 1 {
		t.Fatalf("%d 个并发请求里有 %d 个说自己新建了行，期望正好 1 个", racers, createdCount)
	}
	if want == "" {
		t.Fatal("新建的那一行没有带回参与 id")
	}
	for i, id := range ids {
		if created[i] {
			continue
		}
		if id != want {
			t.Fatalf("第 %d 个请求拿回了另一条参与 %s，期望 %s", i, id, want)
		}
	}
	if got := countRows(t, ctx, f, `SELECT COUNT(*) FROM lottery_participations
		WHERE idempotency_key=$1`, key); got != 1 {
		t.Fatalf("同一个幂等键在并发下留下了 %d 行参与，期望 1 行", got)
	}
}

// TestParticipationSourceSnapshotIsPersistedAndReadBack 覆盖来源快照那四个字段的读写列位。
//
// 它们（订单、订单号、设备、点位）在插入语句与两条 SELECT 的列清单里各写了一遍，而这几处
// **都是手写的字符串**：列名写错、顺序写反，编译器一句话也不会说，表现是后台列表里的
// 「来源订单号」永远空着。所以这里从两条读路径各断言一次：详情用的 GetParticipation，
// 与后台列表用的 ListParticipations（两条路各有一份列的清单）。
func TestParticipationSourceSnapshotIsPersistedAndReadBack(t *testing.T) {
	f := newLotteryFixture(t, 50, 1)
	ctx := testContext(t)

	userID := uuid.NewString()
	orderID := uuid.NewString()
	orderNo := "SO" + uuid.NewString()[:8]
	machineID := uuid.NewString()
	locationID := uuid.NewString()

	begun, err := f.repo.Begin(ctx, BeginParams{
		RoundID: f.round.ID, UserID: userID, IdempotencyKey: "order:" + orderID,
		SourceOrderID: &orderID, SourceOrderNo: orderNo,
		SourceMachineID: &machineID, SourceLocationID: &locationID,
		Cost: 1, Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	assertSnapshot := func(where string, got *model.Participation) {
		t.Helper()
		if got.SourceOrderID == nil || *got.SourceOrderID != orderID {
			t.Fatalf("%s 的来源订单 id 是 %v，期望 %s", where, got.SourceOrderID, orderID)
		}
		if got.SourceOrderNo != orderNo {
			t.Fatalf("%s 的来源订单号是 %q，期望 %q", where, got.SourceOrderNo, orderNo)
		}
		if got.SourceMachineID == nil || *got.SourceMachineID != machineID {
			t.Fatalf("%s 的来源设备 id 是 %v，期望 %s", where, got.SourceMachineID, machineID)
		}
		if got.SourceLocationID == nil || *got.SourceLocationID != locationID {
			t.Fatalf("%s 的来源点位 id 是 %v，期望 %s", where, got.SourceLocationID, locationID)
		}
	}

	detail, err := f.repo.GetParticipation(ctx, begun.Participation.ID)
	if err != nil {
		t.Fatalf("read the participation back: %v", err)
	}
	assertSnapshot("参与详情", detail)

	// Page 从 1 起：HTTP 那一层由 api.ParsePage 保证，仓储这一层不做兜底（Page=0 算出来的
	// OFFSET 是负的）。
	rows, _, err := f.repo.ListParticipations(ctx, dto.ParticipationQuery{UserID: userID, Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("list participations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("按这个用户查到 %d 条参与，期望 1 条", len(rows))
	}
	assertSnapshot("后台参与列表", rows[0].Participation)
}
