package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/repository"
)

// 修复 worker 的入口打的是「一批记录各自往哪走」，所以用例必须能按记录分辨结论——一个
// 写死的返回只验得了「全都成」与「全都不成」两种，而这一段代码真正要防的恰恰是中间那种：
// 一部分收尾了、另一部分还悬着，而运维要能一眼看出还剩几条。

const (
	sweepSettles      = "aaaa1111-1111-4111-8111-111111111111"
	sweepStillPending = "bbbb2222-2222-4222-8222-222222222222"
	sweepConcludes    = "cccc3333-3333-4333-8333-333333333333"
)

// pendingByID 造一批 id 各不相同的 pending 参与记录。
func pendingByID(ids ...string) map[string]*model.Participation {
	out := make(map[string]*model.Participation, len(ids))
	for _, id := range ids {
		record := pendingParticipation()
		record.ID = id
		out[id] = record
	}
	return out
}

// newSweepFixture 把一份待修复清单摆进假仓储：取件返回 ids，逐条按 id 读回各自的记录。
//
// 清单里没有的 id 读回 ErrParticipationNotFound——那就是「扫描之后、处理之前这一条被删了」
// 或者「另一个副本先收尾了」的真实形状。
func newSweepFixture(ids []string, records map[string]*model.Participation) (*fakeRepository, *fakeCards) {
	repo := &fakeRepository{pendingIDs: ids, confirmRound: openRound(1)}
	repo.getParticipation = func(_ context.Context, id string) (*model.Participation, error) {
		record, ok := records[id]
		if !ok {
			return nil, repository.ErrParticipationNotFound
		}
		return record, nil
	}
	return repo, &fakeCards{}
}

// TestSweepCountsUnresolvedParticipationsSeparately 是这一层存在的理由：一轮下来还剩几条
// 没落地，而不是「处理了几条」。
//
// 三个数合成一个会让「一直在重试、一条也没成」看起来与「都收尾了」一模一样，而前者是
// 账户域在出问题、需要有人去看的唯一信号。
func TestSweepCountsUnresolvedParticipationsSeparately(t *testing.T) {
	ids := []string{sweepSettles, sweepStillPending, sweepConcludes}
	repo, cards := newSweepFixture(ids, pendingByID(ids...))
	cards.deductFor = func(req client.DeductRequest) (client.DeductResult, error) {
		switch req.RequestID {
		case sweepStillPending:
			// 账户域不可达：**真的不知道扣没扣**，这一条这一轮得不出结论。
			return client.DeductResult{}, transportFailure()
		case sweepConcludes:
			// 余额不足：有确定依据的结论，重试没有价值。
			return client.DeductResult{}, client.ErrInsufficientFortuneCards
		default:
			return client.DeductResult{EntryID: testEntryID}, nil
		}
	}

	result, err := newTestService(repo, cards).SweepParticipations(t.Context(), 0)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Scanned != 3 {
		t.Fatalf("扫描到 %d 条，期望 3", result.Scanned)
	}
	if result.Settled != 2 {
		t.Fatalf("收尾了 %d 条，期望 2（成交一条、余额不足一条）", result.Settled)
	}
	if result.StillPending != 1 {
		t.Fatalf("还剩 %d 条未决，期望 1——这个数不为零是账户域出问题的唯一信号", result.StillPending)
	}

	// 成交那条：确认了一次，卡只扣了一张。
	if repo.confirmCalls != 1 {
		t.Fatalf("确认了 %d 次，期望 1", repo.confirmCalls)
	}
	// 未决那条：状态一个字没改，只记了一次尝试。
	if len(repo.notedAttempts) != 1 || repo.notedAttempts[0] != sweepStillPending {
		t.Fatalf("未决那条的尝试记录不对：%v", repo.notedAttempts)
	}
	// 有结论那条：标 failed，理由是被账户域拒的那个。
	if len(repo.failParams) != 1 {
		t.Fatalf("标了 %d 次失败，期望 1", len(repo.failParams))
	}
	if repo.failParams[0].ParticipationID != sweepConcludes {
		t.Fatalf("标失败的是 %s，期望 %s", repo.failParams[0].ParticipationID, sweepConcludes)
	}
	if repo.failParams[0].FailureCode != model.FailureInsufficientFortuneCards {
		t.Fatalf("failure_code 是 %q，期望 %q",
			repo.failParams[0].FailureCode, model.FailureInsufficientFortuneCards)
	}
}

// TestSweepRechecksTheStatusBeforeActing 确认每一条都重新读一遍当前状态，而不是信扫描
// 那一刻的快照。
//
// 一轮里前面几条的处理可能耗掉几秒，而这中间可能有别的路径已经把某一条收尾了——用户自己
// 重试、或者另一个副本的 worker。照着旧快照再扣一次卡，就是一次凭空的第二次参与。
func TestSweepRechecksTheStatusBeforeActing(t *testing.T) {
	ids := []string{sweepSettles}
	record := pendingParticipation()
	record.ID = ids[0]
	record.Status = model.ParticipationConfirmed

	repo, cards := newSweepFixture(ids, map[string]*model.Participation{ids[0]: record})
	result, err := newTestService(repo, cards).SweepParticipations(t.Context(), 0)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Scanned != 1 {
		t.Fatalf("扫描到 %d 条，期望 1", result.Scanned)
	}
	// 它既不是「收尾了」（这一轮什么也没做）也不是「未决」（已经到终态了）。
	if result.Settled != 0 || result.StillPending != 0 {
		t.Fatalf("已经不在 pending 的记录不该计入任何一边：settled=%d stillPending=%d",
			result.Settled, result.StillPending)
	}
	if cards.deductCalls != 0 {
		t.Fatalf("对一条已经成交的参与又扣了 %d 次卡", cards.deductCalls)
	}
	if repo.confirmCalls != 0 {
		t.Fatalf("对一条已经成交的参与又确认了 %d 次", repo.confirmCalls)
	}
}

// TestSweepSkipsRecordsThatNoLongerExist 确认读不到记录时这一轮继续往下走。
//
// 剩下的记录还等着收尾，为了其中一条不见了就整轮退出，会让积压一直清不完。
func TestSweepSkipsRecordsThatNoLongerExist(t *testing.T) {
	ids := []string{sweepSettles}
	repo, cards := newSweepFixture(ids, nil)

	result, err := newTestService(repo, cards).SweepParticipations(t.Context(), 0)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Scanned != 1 {
		t.Fatalf("扫描到 %d 条，期望 1", result.Scanned)
	}
	if result.Settled != 0 || result.StillPending != 0 {
		t.Fatalf("读不到的记录不该计入任何一边：settled=%d stillPending=%d",
			result.Settled, result.StillPending)
	}
}

// TestSweepTreatsACompensatedParticipationAsAConclusion 把「退卡 + 标 failed」归到结论
// 那一边，而不是未决。
//
// 它的处置与余额不足不同（要真的冲一次正），但对这一轮的计数来说是一样的：这条不再需要
// 重跑。算进 StillPending 会让运维每天看见一个假的积压。
func TestSweepTreatsACompensatedParticipationAsAConclusion(t *testing.T) {
	ids := []string{sweepSettles}
	repo, cards := newSweepFixture(ids, pendingByID(ids...))
	repo.confirmErr = ErrRoundClosed
	cards.deductResult = client.DeductResult{EntryID: testEntryID}
	cards.reverseResult = client.ReverseResult{EntryID: testReverseID}

	result, err := newTestService(repo, cards).SweepParticipations(t.Context(), 0)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Settled != 1 || result.StillPending != 0 {
		t.Fatalf("补偿过的那条应当算作已收尾：settled=%d stillPending=%d",
			result.Settled, result.StillPending)
	}
	if cards.reverseCalls != 1 {
		t.Fatalf("冲正了 %d 次，期望 1", cards.reverseCalls)
	}
	if len(repo.failParams) != 1 {
		t.Fatalf("标了 %d 次失败，期望 1", len(repo.failParams))
	}
	failed := repo.failParams[0]
	if failed.FailureCode != model.FailureRoundClosed {
		t.Fatalf("failure_code 是 %q，期望 %q", failed.FailureCode, model.FailureRoundClosed)
	}
	if failed.ReverseEntryID == nil || *failed.ReverseEntryID != testReverseID {
		t.Fatalf("冲正流水没有落库：%v", failed.ReverseEntryID)
	}
}

// TestSweepUsesTheConfiguredBatchAndRepairWindow 钉住取件的两个参数。
//
// 两个都是配置，而配置错了的症状不响：批量太小是「一轮只清几条」，下限太小是「每一轮都在
// 扫刚落下的记录」，把一段毫秒级的三段事务当成卡住了。
func TestSweepUsesTheConfiguredBatchAndRepairWindow(t *testing.T) {
	repo, cards := newSweepFixture(nil, nil)
	svc := New(repo, cards, Options{
		Now:         func() time.Time { return testNow },
		RepairAfter: 90 * time.Second,
	})
	if svc.RepairAfter() != 90*time.Second {
		t.Fatalf("RepairAfter 是 %v，期望 90s", svc.RepairAfter())
	}

	// 不传批量时用默认值——worker 那边就是这么调的。
	if _, err := svc.SweepParticipations(t.Context(), 0); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if repo.pendingLimit != DefaultSweepBatch {
		t.Fatalf("取件批量是 %d，期望默认值 %d", repo.pendingLimit, DefaultSweepBatch)
	}
	if repo.pendingAfter != 90*time.Second {
		t.Fatalf("取件下限是 %v，期望配置的 90s", repo.pendingAfter)
	}

	// 显式给的批量原样传下去。
	if _, err := svc.SweepParticipations(t.Context(), 7); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if repo.pendingLimit != 7 {
		t.Fatalf("取件批量是 %d，期望 7", repo.pendingLimit)
	}
}

// TestSweepSurfacesAFailureToReadTheBacklog 确认「读不出待修复的清单」是一次错误，而不是
// 一个空结果。
//
// 空结果在 worker 那边看起来与「没有待修复的记录」一模一样——而后者是常态、前者是数据库
// 在抖。混成一个会让一次故障静默通过。
func TestSweepSurfacesAFailureToReadTheBacklog(t *testing.T) {
	repo := &fakeRepository{pendingErr: errors.New("database is down")}

	result, err := newTestService(repo, &fakeCards{}).SweepParticipations(t.Context(), 0)
	if err == nil {
		t.Fatal("读不出清单却报了成功——worker 会当成「没有待修复的记录」")
	}
	if result != (SweepResult{}) {
		t.Fatalf("失败的那一轮不该带回计数：%+v", result)
	}
}
