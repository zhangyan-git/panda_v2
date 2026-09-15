package service

import (
	"context"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/repository"
)

// 这一份测试打的是**没有数据库**的那一半：请求校验、三段事务在扣卡返回不同结论时各自往哪
// 走、以及补偿路径的四个出口。这些分支用集成测试打不动——它们全都取决于 account-service
// 在第二段回了什么，而那个结论在真跑起来的时候只由传输层决定（余额不足 / 参数非法 / 超时），
// 三条里能自然撞上一条就不错。
//
// 仓储与账户域在这里都是假的（service 的两个依赖本来就是接口，见 service.go 里的理由）。
// **假仓储只摆出用例真正用到的那几个方法**：其余方法由嵌入的那个 nil 接口兜底，调用即
// panic——这正是想要的效果，用例不该悄悄依赖一个它没摆出来的行为。

// fakeRepository 是 Repository 的测试替身。
//
// 每个字段对应一个被回复的方法，nil / 零值有自己的含义（比如 beginResult 为 nil 时 Begin
// 回 nil, nil）。**不做默认行为**：让一个没摆出来的行为静默地返回零值，会让用例通过在一个
// 它其实没验的形状上。
type fakeRepository struct {
	Repository

	beginResult *repository.BeginResult
	beginErr    error
	// beginParams 记下最后一次调用的入参：幂等键派生是这一层的事，而它只在这条边界上看得见。
	beginParams *repository.BeginParams
	beginCalls  int

	confirmRound *model.Round
	confirmErr   error
	confirmCalls int

	failErr    error
	failParams []repository.FailParams

	notedAttempts []string

	round    *model.Round
	roundErr error

	// getParticipation 为空时用 participation / participationErr。
	getParticipation func(ctx context.Context, id string) (*model.Participation, error)
	participation    *model.Participation
	participationErr error

	pendingIDs []string
	pendingErr error
	// pendingLimit / pendingAfter 记下取件时用的参数：批量与下限都是配置，而配置错了的
	// 表现是「修复 worker 一直跑空」或者「它每一轮都在扫刚落下的记录」。
	pendingLimit int
	pendingAfter time.Duration

	activateResult *repository.ActivationCreated
	activateErr    error
	// activateParams 记下这一次开通交给仓储的整份东西。**模板是在这一层拼的**（活动名、
	// 门槛、窗口、奖池），而它们只在这一次调用上看得见；不记下来，用例就只能断言返回的
	// 那个读回值，而那个值在假仓储里是我自己摆的——等于什么都没验。
	activateParams *repository.ActivateParams
	activateCalls  int

	activationView    *repository.ActivationListRow
	activationViewErr error
	viewCalls         int
}

func (f *fakeRepository) Activate(_ context.Context, p repository.ActivateParams) (*repository.ActivationCreated, error) {
	f.activateCalls++
	f.activateParams = &p
	return f.activateResult, f.activateErr
}

func (f *fakeRepository) GetActivationView(_ context.Context, _ string) (*repository.ActivationListRow, error) {
	f.viewCalls++
	return f.activationView, f.activationViewErr
}

func (f *fakeRepository) Begin(_ context.Context, p repository.BeginParams) (*repository.BeginResult, error) {
	f.beginCalls++
	f.beginParams = &p
	return f.beginResult, f.beginErr
}

func (f *fakeRepository) Confirm(_ context.Context, p repository.ConfirmParams) (*model.Round, bool, error) {
	f.confirmCalls++
	return f.confirmRound, false, f.confirmErr
}

func (f *fakeRepository) Fail(_ context.Context, p repository.FailParams) error {
	f.failParams = append(f.failParams, p)
	return f.failErr
}

func (f *fakeRepository) NoteAttempt(_ context.Context, participationID string, _ error) error {
	f.notedAttempts = append(f.notedAttempts, participationID)
	return nil
}

func (f *fakeRepository) GetRound(_ context.Context, _ string) (*model.Round, error) {
	return f.round, f.roundErr
}

func (f *fakeRepository) GetParticipation(ctx context.Context, id string) (*model.Participation, error) {
	if f.getParticipation != nil {
		return f.getParticipation(ctx, id)
	}
	return f.participation, f.participationErr
}

func (f *fakeRepository) PendingParticipations(_ context.Context, repairAfter time.Duration, limit int) ([]string, error) {
	f.pendingAfter, f.pendingLimit = repairAfter, limit
	return f.pendingIDs, f.pendingErr
}

// fakeCards 是账户域的测试替身，摆出 deduct / reverse / balance 三个动作各自的结论。
type fakeCards struct {
	deductResult client.DeductResult
	deductErr    error
	deductCalls  int
	// deductRequests 记下每一次扣减请求。request_id 必须是参与记录的 id——那是「重跑不会
	// 扣第二张卡」的全部依据，也是这一层唯一能验到它的地方。
	deductRequests []client.DeductRequest
	// deductFor 按请求决定这一次扣卡回什么；为空时用 deductResult / deductErr。
	//
	// 修复 worker 一轮里要处理好几条记录，而每一条的结论可以不同，所以那一层必须能按请求
	// 分辨——一个写死的返回只验得了「全都成」与「全都不成」两种。
	deductFor func(req client.DeductRequest) (client.DeductResult, error)

	reverseResult client.ReverseResult
	reverseErr    error
	reverseCalls  int
	reverseEntry  string

	balance      int64
	balanceErr   error
	balanceCalls int
}

func (c *fakeCards) Deduct(_ context.Context, req client.DeductRequest) (client.DeductResult, error) {
	c.deductCalls++
	c.deductRequests = append(c.deductRequests, req)
	if c.deductFor != nil {
		return c.deductFor(req)
	}
	return c.deductResult, c.deductErr
}

func (c *fakeCards) Reverse(_ context.Context, entryID, _, _ string) (client.ReverseResult, error) {
	c.reverseCalls++
	c.reverseEntry = entryID
	return c.reverseResult, c.reverseErr
}

func (c *fakeCards) Balance(_ context.Context, _ string) (int64, error) {
	c.balanceCalls++
	return c.balance, c.balanceErr
}

// ——— 夹具 ———

// 固定的测试时刻。用字面量而不是 time.Now()：这一层有几处按时间判定（窗口、到点），
// 用例断言的是「此刻这一期该不该收人」，让它随机器时间漂移只会换来偶发红。
var testNow = time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)

const (
	testRoundID    = "11111111-1111-4111-8111-111111111111"
	testCampaignID = "22222222-2222-4222-8222-222222222222"
	testUserID     = "33333333-3333-4333-8333-333333333333"
	testOrderID    = "44444444-4444-4444-8444-444444444444"
	testMachineID  = "55555555-5555-4555-8555-555555555555"
	testLocationID = "66666666-6666-4666-8666-666666666666"
	testEntryID    = "77777777-7777-4777-8777-777777777777"
	testReverseID  = "88888888-8888-4888-8888-888888888888"
)

// newTestService 造一个业务层，仓储与账户域都是假的。
//
// cards 收**接口**而不是 *fakeCards：传字面量 nil 时，参数类型是具体类型的话会变成一个
// 「装着空指针的非空接口」，而 service 的第一道检查（s.cards == nil = 这次部署没接账户域）
// 正好认不出它——那个用例会以一次 nil 解引用结束，而它想验的是「明确失败」。
func newTestService(repo *fakeRepository, cards FortuneCards) *LotteryService {
	return New(repo, cards, Options{Now: func() time.Time { return testNow }})
}

// pendingParticipation 是一条刚落库、还没扣卡的参与记录。
func pendingParticipation() *model.Participation {
	return &model.Participation{
		ID:             "99999999-9999-4999-8999-999999999999",
		RoundID:        testRoundID,
		CampaignID:     testCampaignID,
		CampaignName:   "门店抽奖",
		RoundNo:        "LT0001-0001",
		UserID:         testUserID,
		Cost:           1,
		Status:         model.ParticipationPending,
		IdempotencyKey: "test-key",
		CreatedAt:      testNow,
	}
}

// openRound 是一期正在收人的期次。
func openRound(count int32) *model.Round {
	return &model.Round{
		ID: testRoundID, CampaignID: testCampaignID, Seq: 1, RoundNo: "LT0001-0001",
		Status:            model.RoundOpen,
		ParticipantTarget: 10,
		ParticipantCount:  count,
		WinnerCount:       2,
		StartsAt:          testNow.Add(-time.Hour),
		EndsAt:            testNow.Add(time.Hour),
	}
}

// begunPending 是「刚落下一条 pending、可以往下走」的那一次 Begin 结果。
func begunPending() *repository.BeginResult {
	return &repository.BeginResult{
		Round: openRound(3), Campaign: &model.Campaign{ID: testCampaignID, Status: model.CampaignEnabled},
		Participation: pendingParticipation(), Created: true,
	}
}

// 一次不带来源订单的直接参与。
func directRequest() dto.ParticipateRequest { return dto.ParticipateRequest{} }
