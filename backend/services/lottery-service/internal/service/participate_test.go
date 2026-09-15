package service

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/repository"
)

// 一段传输层故障：账户域不可达。**它不能被当成一次拒绝**——那会把一条其实已经扣了卡的
// 参与标成 failed，用户白丢一张卡。
func transportFailure() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
}

// ——— 入口校验 ———

// TestParticipateRefusesRequestsItCannotTrust 把入口那几道校验一次列全。
//
// 它们全部发生在第一段事务之前：**一条也不该扣卡，一条也不该落库**。这正是不合法请求与
// 业务失败的分别——后者已经有一行记录可以交代，前者连开始都没开始。
func TestParticipateRefusesRequestsItCannotTrust(t *testing.T) {
	cases := []struct {
		name    string
		roundID string
		userID  string
		request func() (dto.ParticipateRequest, string)
		want    error
	}{
		{
			name: "期次 id 为空", roundID: "", userID: testUserID,
			request: func() (dto.ParticipateRequest, string) { return directRequest(), "key" },
			want:    ErrRoundIDRequired,
		},
		{
			name: "期次 id 不是 uuid", roundID: "not-a-uuid", userID: testUserID,
			request: func() (dto.ParticipateRequest, string) { return directRequest(), "key" },
			want:    ErrRoundIDInvalid,
		},
		{
			name: "用户 id 为空", roundID: testRoundID, userID: "   ",
			request: func() (dto.ParticipateRequest, string) { return directRequest(), "key" },
			want:    ErrUserIDRequired,
		},
		{
			name: "既没有来源订单也没有幂等键", roundID: testRoundID, userID: testUserID,
			request: func() (dto.ParticipateRequest, string) { return directRequest(), "  " },
			want:    ErrIdempotencyNeeded,
		},
		{
			name: "来源订单不是 uuid", roundID: testRoundID, userID: testUserID,
			request: func() (dto.ParticipateRequest, string) {
				return dto.ParticipateRequest{SourceOrderID: "oops"}, "key"
			},
			want: ErrOrderIDInvalid,
		},
		{
			name: "来源设备不是 uuid", roundID: testRoundID, userID: testUserID,
			request: func() (dto.ParticipateRequest, string) {
				return dto.ParticipateRequest{SourceMachineID: "oops"}, "key"
			},
			want: ErrMachineIDInvalid,
		},
		{
			name: "来源门店不是 uuid", roundID: testRoundID, userID: testUserID,
			request: func() (dto.ParticipateRequest, string) {
				return dto.ParticipateRequest{SourceLocationID: "oops"}, "key"
			},
			want: ErrLocationIDInvalid,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, cards := &fakeRepository{}, &fakeCards{}
			request, key := tc.request()

			result, err := newTestService(repo, cards).Participate(
				t.Context(), tc.roundID, tc.userID, request, key)
			if !errors.Is(err, tc.want) {
				t.Fatalf("得到 %v，期望 %v", err, tc.want)
			}
			if result != nil {
				t.Fatalf("被拒的请求不该带回结果：%+v", result)
			}
			// 一次校验都没通过就跑了事务，是这一条最要紧的断言。
			if repo.beginCalls != 0 {
				t.Fatalf("入口校验失败却调了 %d 次 Begin", repo.beginCalls)
			}
			if cards.deductCalls != 0 {
				t.Fatalf("入口校验失败却扣了 %d 次卡", cards.deductCalls)
			}
		})
	}
}

// TestBothValidationAndInfrastructureFailuresAreRefused 确认「这一层认识的错误」在
// ValidationErrors 里的归属是对的。
//
// 这个表被 controller 用来决定状态码（校验错 → 400），漏掉一个的表现是那条校验变成 500。
func TestBothValidationAndInfrastructureFailuresAreRefused(t *testing.T) {
	for _, err := range []error{
		ErrRoundIDRequired, ErrRoundIDInvalid, ErrIdempotencyNeeded,
		ErrOrderIDInvalid, ErrMachineIDInvalid, ErrLocationIDInvalid,
	} {
		if !IsValidationError(err) {
			t.Fatalf("%v 是一次请求校验失败，却不在 ValidationErrors 里", err)
		}
	}

	// —— 反过来：这些**不是**请求不合法，controller 不该把它们映成 400 ——
	for _, err := range []error{
		// 业务结论：用户没填错什么，是卡不够 / 期次关了。
		ErrInsufficientFortuneCards, ErrRoundClosed, ErrParticipationPending,
		ErrParticipationFailed, ErrInvalidDeductRequest,
		// 身份缺失单列：它**故意**不进这张表。
		//
		// userId / 操作人从来不在请求体里，它们来自令牌。走到这里说明装配处漏挂了认证
		// ——是部署错了，不是用户填错了，所以 controller 给它 401 而不是 400（见
		// response.go 里与 ErrActorRequired 合并的那一条）。把它放进 ValidationErrors
		// 会让一次装配事故对用户显示成「你的请求参数不对」，而真正该改代码的人看不到。
		ErrUserIDRequired, ErrActorRequired,
	} {
		if IsValidationError(err) {
			t.Fatalf("%v 不该被当成请求校验失败——它会以 400 回给用户", err)
		}
	}
}

// ——— 幂等 ———

// TestTheIdempotencyKeyComesFromTheOrderWhenThereIsOne 是「一笔订单只能参与一次」的
// 执行方式：键由**服务端**从订单号派生，请求头被忽略。
//
// 反过来（让请求头优先）的那条路看起来很合理，但它有一个洞：同一个订单换一个
// Idempotency-Key 就能再参与一次，于是「订单送出的福卡只能花一次」这条不变量没了。
func TestTheIdempotencyKeyComesFromTheOrderWhenThereIsOne(t *testing.T) {
	repo := &fakeRepository{beginResult: begunPending()}
	cards := &fakeCards{deductResult: client.DeductResult{EntryID: testEntryID}}
	repo.participation = pendingParticipation()
	repo.round = openRound(4)

	_, err := newTestService(repo, cards).Participate(t.Context(), testRoundID, testUserID,
		dto.ParticipateRequest{SourceOrderID: testOrderID}, "a-header-value")
	if err != nil {
		t.Fatalf("participate: %v", err)
	}
	want := model.ParticipationKeyFromOrder(testOrderID)
	if repo.beginParams.IdempotencyKey != want {
		t.Fatalf("落库用的幂等键是 %q，期望 %q（请求头应当被忽略）",
			repo.beginParams.IdempotencyKey, want)
	}
	// 请求体里的订单同时要落进那一列快照，供中奖记录的「来源订单」用。
	if repo.beginParams.SourceOrderID == nil || *repo.beginParams.SourceOrderID != testOrderID {
		t.Fatalf("来源订单没有跟着下去：%v", repo.beginParams.SourceOrderID)
	}
	// 一次参与扣几张由服务端定死，不看请求体。
	if repo.beginParams.Cost != int32(ParticipationCost) {
		t.Fatalf("记录的扣减张数是 %d，期望 %d", repo.beginParams.Cost, ParticipationCost)
	}
}

// TestAParticipationThatAlreadySucceededIsReplayedWithoutChargingAgain 覆盖幂等键命中
// confirmed 那一条：用户拿同一张订单点了第二下，或者上一次的响应丢了客户端重试。
//
// **一次扣卡都不该发生。** 这条与「修复 worker 重跑」不同——那个走的是 settle，这个连
// settle 都不进。
func TestAParticipationThatAlreadySucceededIsReplayedWithoutChargingAgain(t *testing.T) {
	confirmed := pendingParticipation()
	confirmed.Status = model.ParticipationConfirmed
	repo := &fakeRepository{beginResult: &repository.BeginResult{
		Round: openRound(4), Participation: confirmed, Created: false,
	}}
	cards := &fakeCards{}

	result, err := newTestService(repo, cards).Participate(
		t.Context(), testRoundID, testUserID, directRequest(), "key")
	if err != nil {
		t.Fatalf("participate: %v", err)
	}
	if !result.Replayed {
		t.Fatal("幂等命中 confirmed 的参与应当标记为回放")
	}
	if result.Participation.ID != confirmed.ID {
		t.Fatalf("回放的是另一条参与：%s ≠ %s", result.Participation.ID, confirmed.ID)
	}
	if cards.deductCalls != 0 {
		t.Fatalf("回放不该扣卡，却扣了 %d 次", cards.deductCalls)
	}
	if repo.confirmCalls != 0 {
		t.Fatalf("回放不该再确认一次，却确认了 %d 次", repo.confirmCalls)
	}
}

// TestAParticipationThatAlreadyFailedIsReplayedWithItsOwnReason 覆盖幂等键命中的另一半：
// 上一次已经有了结论，这一笔不能装作它是新的。
func TestAParticipationThatAlreadyFailedIsReplayedWithItsOwnReason(t *testing.T) {
	for _, tc := range []struct {
		failureCode string
		want        error
	}{
		{model.FailureInsufficientFortuneCards, ErrInsufficientFortuneCards},
		{model.FailureRoundClosed, ErrRoundClosed},
		{model.FailureInvalidRequest, ErrInvalidDeductRequest},
		// 将来新增的 failure_code 落到兜底那一条，至少不会静默变成「成功」。
		{"something_new", ErrParticipationFailed},
	} {
		t.Run(tc.failureCode, func(t *testing.T) {
			failed := pendingParticipation()
			failed.Status = model.ParticipationFailed
			failed.FailureCode = tc.failureCode
			repo := &fakeRepository{beginResult: &repository.BeginResult{
				Round: openRound(4), Participation: failed, Created: false,
			}}
			cards := &fakeCards{}

			_, err := newTestService(repo, cards).Participate(
				t.Context(), testRoundID, testUserID, directRequest(), "key")
			if !errors.Is(err, tc.want) {
				t.Fatalf("得到 %v，期望 %v", err, tc.want)
			}
			if cards.deductCalls != 0 {
				t.Fatalf("回放一条已失败的参与不该扣卡，却扣了 %d 次", cards.deductCalls)
			}
		})
	}
}

// TestAParticipationLeftPendingIsResumed 覆盖幂等键命中 pending：上一次卡在第二段中间
// （进程死了，或者扣减调用挂住了），这一次接着往下走。
//
// 重跑安全，因为扣减请求里的 request_id 就是这条参与记录的 id。
func TestAParticipationLeftPendingIsResumed(t *testing.T) {
	repo := &fakeRepository{beginResult: &repository.BeginResult{
		Round: openRound(4), Participation: pendingParticipation(), Created: false,
	}}
	cards := &fakeCards{deductResult: client.DeductResult{EntryID: testEntryID}}
	repo.round = openRound(5)
	repo.participation = pendingParticipation()

	result, err := newTestService(repo, cards).Participate(
		t.Context(), testRoundID, testUserID, directRequest(), "key")
	if err != nil {
		t.Fatalf("participate: %v", err)
	}
	if cards.deductCalls != 1 {
		t.Fatalf("一条 pending 的参与应当接着扣卡，实际扣了 %d 次", cards.deductCalls)
	}
	if repo.confirmCalls != 1 {
		t.Fatalf("扣卡成功后应当确认一次，实际确认了 %d 次", repo.confirmCalls)
	}
	if result.Replayed {
		t.Fatal("这一笔是这次才成交的，不该标记为回放")
	}
}

// ——— 扣卡的三个结论 ———

// TestTheDeductRequestUsesTheParticipationIDAsItsRequestID 钉住那条让重跑安全的值。
//
// 账户域拿 request_id 派生幂等键（`draw:{requestId}`），所以「同一条参与重试多少次都只扣
// 一张卡」挂在这个 id 的唯一性上，而不是靠调用方自觉。改用它（比如改成订单号）会让重跑
// 变成第二次扣卡，而症状是「用户参与一次掉了两张卡」。
func TestTheDeductRequestUsesTheParticipationIDAsItsRequestID(t *testing.T) {
	participation := pendingParticipation()
	repo := &fakeRepository{beginResult: &repository.BeginResult{
		Round: openRound(0), Participation: participation, Created: true,
	}}
	cards := &fakeCards{deductResult: client.DeductResult{EntryID: testEntryID}}
	repo.round = openRound(1)
	repo.participation = participation

	if _, err := newTestService(repo, cards).Participate(
		t.Context(), testRoundID, testUserID, directRequest(), "key"); err != nil {
		t.Fatalf("participate: %v", err)
	}
	req := cards.deductRequests[0]
	if req.RequestID != participation.ID {
		t.Fatalf("扣减请求的 request_id 是 %q，期望参与记录的 id %q", req.RequestID, participation.ID)
	}
	if req.Amount != ParticipationCost {
		t.Fatalf("扣减张数是 %d，期望 %d", req.Amount, ParticipationCost)
	}
	// 期次号进 remark / reference_no：两侧人工对账时靠它互相对上号。
	if req.ReferenceNo != participation.RoundNo {
		t.Fatalf("扣减请求带的是期次号 %q，期望 %q", req.ReferenceNo, participation.RoundNo)
	}
}

// TestAnUnreachableAccountServiceLeavesTheParticipationPending 是本服务最要紧的一条返回：
// **未决同时带着结果与错误**。
//
// 账户域超时的时候我们真的不知道卡扣没扣。说失败可能吞掉用户一张卡，说成功可能让他白拿
// 一次参与，所以只有一条路——记一次尝试、保持 pending、回 ErrParticipationPending，让
// controller 出 202。而那条 202 必须说得出是哪一条参与在处理中，所以结果不能丢。
func TestAnUnreachableAccountServiceLeavesTheParticipationPending(t *testing.T) {
	participation := pendingParticipation()
	repo := &fakeRepository{beginResult: &repository.BeginResult{
		Round: openRound(0), Participation: participation, Created: true,
	}}
	cards := &fakeCards{deductErr: transportFailure()}

	result, err := newTestService(repo, cards).Participate(
		t.Context(), testRoundID, testUserID, directRequest(), "key")
	if !errors.Is(err, ErrParticipationPending) {
		t.Fatalf("得到 %v，期望 ErrParticipationPending", err)
	}
	if result == nil {
		t.Fatal("未决也必须带回结果——否则那条 202 说不出是哪一条参与在处理中")
	}
	if result.Participation.ID != participation.ID {
		t.Fatalf("带回来的是另一条参与：%s", result.Participation.ID)
	}
	if result.Round == nil || result.Round.ID != testRoundID {
		t.Fatal("未决的结果里要带上一期，页面才说得出「在处理的是哪一期」")
	}
	// 状态一个字都不许改：既没标 failed，也没标 confirmed。
	if len(repo.failParams) != 0 {
		t.Fatalf("未决却标了 %d 次失败", len(repo.failParams))
	}
	if repo.confirmCalls != 0 {
		t.Fatalf("未决却确认了 %d 次", repo.confirmCalls)
	}
	// 只记观测：attempts / last_error 是给人看的（超过一小时仍未决就该告警）。
	if len(repo.notedAttempts) != 1 || repo.notedAttempts[0] != participation.ID {
		t.Fatalf("没有记下这一次尝试：%v", repo.notedAttempts)
	}
}

// TestInsufficientCardsFailTheParticipationWithoutRetrying 是「余额不足」那条路。
//
// 它是一次说得清楚的业务结论，不是故障：账户域没动过，所以没有卡需要退，也没有重试的
// 价值——用户该做的是去下一单。
func TestInsufficientCardsFailTheParticipationWithoutRetrying(t *testing.T) {
	participation := pendingParticipation()
	repo := &fakeRepository{beginResult: &repository.BeginResult{
		Round: openRound(0), Participation: participation, Created: true,
	}}
	cards := &fakeCards{deductErr: client.ErrInsufficientFortuneCards}

	_, err := newTestService(repo, cards).Participate(
		t.Context(), testRoundID, testUserID, directRequest(), "key")
	if !errors.Is(err, ErrInsufficientFortuneCards) {
		t.Fatalf("得到 %v，期望 ErrInsufficientFortuneCards", err)
	}
	if len(repo.failParams) != 1 {
		t.Fatalf("应当标一次失败，实际 %d 次", len(repo.failParams))
	}
	if got := repo.failParams[0].FailureCode; got != model.FailureInsufficientFortuneCards {
		t.Fatalf("failure_code 是 %q，期望 %q", got, model.FailureInsufficientFortuneCards)
	}
	// 没扣成，所以没有卡要退——补偿路径不该被碰到。
	if repo.failParams[0].ReverseEntryID != nil {
		t.Fatal("一次没发生的扣卡却记了冲正流水")
	}
	if cards.reverseCalls != 0 {
		t.Fatalf("余额不足不该触发冲正，实际冲了 %d 次", cards.reverseCalls)
	}
	if repo.confirmCalls != 0 {
		t.Fatalf("没扣成却确认了参与 %d 次", repo.confirmCalls)
	}
}

// TestAnInvalidDeductRequestIsOurBugAndFailsLoudly 「参数非法」与「余额不足」都是结论，
// 但责任方不同：那是本服务自己的 bug（负数张数、空 user_id），重试一万次也是同样的结果。
//
// 这里验的是它**不与余额不足混为一谈**——混了的表现是小程序对一次自己的 bug 提示
// 「福卡不够了」，而真正的原因要看服务端日志才找得到。
func TestAnInvalidDeductRequestIsOurBugAndFailsLoudly(t *testing.T) {
	participation := pendingParticipation()
	repo := &fakeRepository{beginResult: &repository.BeginResult{
		Round: openRound(0), Participation: participation, Created: true,
	}}
	cards := &fakeCards{deductErr: client.ErrInvalidDeductRequest}

	_, err := newTestService(repo, cards).Participate(
		t.Context(), testRoundID, testUserID, directRequest(), "key")
	if !errors.Is(err, ErrInvalidDeductRequest) {
		t.Fatalf("得到 %v，期望 ErrInvalidDeductRequest", err)
	}
	if got := repo.failParams[0].FailureCode; got != model.FailureInvalidRequest {
		t.Fatalf("failure_code 是 %q，期望 %q", got, model.FailureInvalidRequest)
	}
	if cards.reverseCalls != 0 {
		t.Fatal("一次没发生的扣卡不该触发冲正")
	}
}

// ——— 补偿路径的四个出口 ———

// TestARoundClosedMidFlightCompensatesTheCharge 是补偿的正路：卡扣了，回到本地时期次已经
// 关了或开了奖，用户什么也没得到。
//
// 唯一能做的事是把卡退回去、把这一笔标成 failed/round_closed。落库的是**冲正流水**的 id
// 而不是被冲正那一笔的：将来查「这张卡怎么回来的」要按它反查到那一笔账变。
func TestARoundClosedMidFlightCompensatesTheCharge(t *testing.T) {
	participation := pendingParticipation()
	repo := &fakeRepository{
		beginResult: &repository.BeginResult{Round: openRound(0), Participation: participation, Created: true},
		confirmErr:  ErrRoundClosed,
		failParams:  nil,
	}
	cards := &fakeCards{
		deductResult:  client.DeductResult{EntryID: testEntryID},
		reverseResult: client.ReverseResult{EntryID: testReverseID},
	}

	_, err := newTestService(repo, cards).Participate(
		t.Context(), testRoundID, testUserID, directRequest(), "key")
	if !errors.Is(err, ErrRoundClosed) {
		t.Fatalf("得到 %v，期望 ErrRoundClosed", err)
	}
	if cards.reverseCalls != 1 {
		t.Fatalf("应当冲正一次，实际 %d 次", cards.reverseCalls)
	}
	// 冲的是**扣减那一笔**的流水，不是别的。
	if cards.reverseEntry != testEntryID {
		t.Fatalf("冲正的是 %q，期望扣减流水 %q", cards.reverseEntry, testEntryID)
	}
	if len(repo.failParams) != 1 {
		t.Fatalf("应当标一次失败，实际 %d 次", len(repo.failParams))
	}
	failed := repo.failParams[0]
	if failed.FailureCode != model.FailureRoundClosed {
		t.Fatalf("failure_code 是 %q，期望 %q", failed.FailureCode, model.FailureRoundClosed)
	}
	if failed.ReverseEntryID == nil || *failed.ReverseEntryID != testReverseID {
		t.Fatalf("落库的冲正流水是 %v，期望 %q（应当是冲正那一笔的 id）",
			failed.ReverseEntryID, testReverseID)
	}
}

// TestACompensationThatCannotBeConfirmedStaysPending 是补偿的第二个出口：退的时候超时 /
// 不可达，卡退没退**不知道**。
//
// 这时标 failed 会让这条记录再也没人管，而用户可能已经丢了一张卡。所以保持 pending，
// 交给修复 worker：它会重新扣一次（replayed=true，不会扣第二张）再撞回同一条补偿路径，
// 直到退成。
func TestACompensationThatCannotBeConfirmedStaysPending(t *testing.T) {
	participation := pendingParticipation()
	repo := &fakeRepository{
		beginResult: &repository.BeginResult{Round: openRound(0), Participation: participation, Created: true},
		confirmErr:  ErrRoundClosed,
	}
	cards := &fakeCards{
		deductResult: client.DeductResult{EntryID: testEntryID},
		reverseErr:   transportFailure(),
	}

	result, err := newTestService(repo, cards).Participate(
		t.Context(), testRoundID, testUserID, directRequest(), "key")
	if !errors.Is(err, ErrParticipationPending) {
		t.Fatalf("得到 %v，期望 ErrParticipationPending", err)
	}
	if result == nil {
		t.Fatal("未决也要带回结果")
	}
	// 一个字都没落下去：既没标 failed（那会让它没人管），也没标 confirmed。
	if len(repo.failParams) != 0 {
		t.Fatalf("退卡没结论却标了 %d 次失败", len(repo.failParams))
	}
	if len(repo.notedAttempts) != 1 {
		t.Fatalf("应当记下这一次没结论的尝试，实际 %d 次", len(repo.notedAttempts))
	}
}

// TestARefusedReversalStillFailsTheParticipationButIsLoud 是补偿的第三个出口：账户域
// **拒绝**冲正（退回去会把余额扣成负数，也就是那张卡已经被别处花掉了）。
//
// 参与仍然标 failed——用户确实什么也没得到，这是诚实的记录——但这是一条必须响的警情：
// 用户被扣了卡、没参与上、还退不回来，只能人工补。所以它与「退成功」的唯一分别只在于
// reverse_entry_id 是空的。
func TestARefusedReversalStillFailsTheParticipationButIsLoud(t *testing.T) {
	participation := pendingParticipation()
	repo := &fakeRepository{
		beginResult: &repository.BeginResult{Round: openRound(0), Participation: participation, Created: true},
		confirmErr:  ErrRoundClosed,
	}
	cards := &fakeCards{
		deductResult: client.DeductResult{EntryID: testEntryID},
		reverseErr:   client.ErrReverseRefused,
	}

	_, err := newTestService(repo, cards).Participate(
		t.Context(), testRoundID, testUserID, directRequest(), "key")
	if !errors.Is(err, ErrRoundClosed) {
		t.Fatalf("得到 %v，期望 ErrRoundClosed", err)
	}
	if len(repo.failParams) != 1 {
		t.Fatalf("退不回来也应当留下一条记录，实际 %d 条", len(repo.failParams))
	}
	if repo.failParams[0].ReverseEntryID != nil {
		t.Fatal("退卡被拒却记了一个冲正流水号")
	}
	if repo.failParams[0].FailureCode != model.FailureRoundClosed {
		t.Fatalf("failure_code 是 %q，期望 %q",
			repo.failParams[0].FailureCode, model.FailureRoundClosed)
	}
}

// TestAnEmptyEntryIDIsTreatedAsUnknownNotAsAFailure 是补偿的第四个出口。
//
// 扣减调用成功却没有 entry_id：契约说它非空。没有 entry_id 就退不了款，所以只能当成
// 「不知道结果」处理——标 failed 等于把一张扣掉的卡直接丢掉。
func TestAnEmptyEntryIDIsTreatedAsUnknownNotAsAFailure(t *testing.T) {
	participation := pendingParticipation()
	repo := &fakeRepository{
		beginResult: &repository.BeginResult{Round: openRound(0), Participation: participation, Created: true},
		confirmErr:  ErrRoundClosed,
	}
	cards := &fakeCards{deductResult: client.DeductResult{EntryID: ""}}

	result, err := newTestService(repo, cards).Participate(
		t.Context(), testRoundID, testUserID, directRequest(), "key")
	if !errors.Is(err, ErrParticipationPending) {
		t.Fatalf("得到 %v，期望 ErrParticipationPending", err)
	}
	if result == nil {
		t.Fatal("未决也要带回结果")
	}
	if cards.reverseCalls != 0 {
		t.Fatalf("没有流水可冲，却冲了 %d 次", cards.reverseCalls)
	}
	if len(repo.failParams) != 0 {
		t.Fatalf("没有流水可冲却标了 %d 次失败——那张卡就此丢掉", len(repo.failParams))
	}
}

// TestASecondAttemptThatFindsTheRoundAlreadyConfirmedIsNotCompensated 是**最危险的那条
// 分支**：Confirm 回 ErrParticipationNotPending，而回读发现这条参与其实已经 confirmed。
//
// 那说明上一次尝试已经提交成功了，只是响应丢了或进程死在返回前。这时**绝不能冲正**——
// 那会把一张已经成交的参与的卡退掉，而参与还留着，用户白拿一次机会。正确做法是把当期
// 原样回给调用方，让重跑看起来就像第一次成功了。
func TestASecondAttemptThatFindsTheRoundAlreadyConfirmedIsNotCompensated(t *testing.T) {
	participation := pendingParticipation()
	repo := &fakeRepository{
		beginResult: &repository.BeginResult{Round: openRound(0), Participation: participation, Created: true},
		confirmErr:  repository.ErrParticipationNotPending,
		round:       openRound(1),
	}
	cards := &fakeCards{
		deductResult:  client.DeductResult{EntryID: testEntryID, Replayed: true},
		reverseResult: client.ReverseResult{EntryID: testReverseID},
	}
	confirmed := pendingParticipation()
	confirmed.Status = model.ParticipationConfirmed
	repo.getParticipation = func(_ context.Context, _ string) (*model.Participation, error) {
		return confirmed, nil
	}

	result, err := newTestService(repo, cards).Participate(
		t.Context(), testRoundID, testUserID, directRequest(), "key")
	if err != nil {
		t.Fatalf("重跑撞上一条已经成交的参与不该报错：%v", err)
	}
	if cards.reverseCalls != 0 {
		t.Fatalf("**冲正了一次已经成交的参与**，用户白拿一次机会（冲了 %d 次）", cards.reverseCalls)
	}
	if len(repo.failParams) != 0 {
		t.Fatalf("已经成交的参与被标成了失败：%+v", repo.failParams)
	}
	if result.Round == nil || result.Round.ID != testRoundID {
		t.Fatal("应当把当期原样回给调用方")
	}
}

// TestAParticipateEndsInAFailureThatIsAlreadyOnRecord 是那条分支的另一半：回读发现这条
// 参与早就失败了（余额不足，或者补偿退卡之后）。回放那条结论，而不是编一个新的。
func TestAParticipateEndsInAFailureThatIsAlreadyOnRecord(t *testing.T) {
	participation := pendingParticipation()
	repo := &fakeRepository{
		beginResult: &repository.BeginResult{Round: openRound(0), Participation: participation, Created: true},
		confirmErr:  repository.ErrParticipationNotPending,
	}
	cards := &fakeCards{deductResult: client.DeductResult{EntryID: testEntryID, Replayed: true}}
	compensated := pendingParticipation()
	compensated.Status = model.ParticipationFailed
	compensated.FailureCode = model.FailureRoundClosed
	repo.getParticipation = func(_ context.Context, _ string) (*model.Participation, error) {
		return compensated, nil
	}

	_, err := newTestService(repo, cards).Participate(
		t.Context(), testRoundID, testUserID, directRequest(), "key")
	if !errors.Is(err, ErrRoundClosed) {
		t.Fatalf("得到 %v，期望回放到那条已记录在案的 ErrRoundClosed", err)
	}
	if cards.reverseCalls != 0 {
		t.Fatalf("回放一条已补偿的参与又冲了一次正：%d 次", cards.reverseCalls)
	}
}

// TestParticipateWithoutAnAccountServiceFails 确认「没接账户域」是一次明确失败。
//
// 没有账户域就没有任何一笔参与能被扣卡。记成一条 confirmed 却没扣卡的记录，用户会白拿
// 一次中奖机会——那比一条说得清楚的错误贵得多。
func TestParticipateWithoutAnAccountServiceFails(t *testing.T) {
	repo := &fakeRepository{beginResult: begunPending()}

	result, err := newTestService(repo, nil).Participate(
		t.Context(), testRoundID, testUserID, directRequest(), "key")
	if !errors.Is(err, ErrFortuneCardsUnavailable) {
		t.Fatalf("得到 %v，期望 ErrFortuneCardsUnavailable", err)
	}
	if result != nil {
		t.Fatalf("没接账户域不该带回结果：%+v", result)
	}
	if repo.beginCalls != 0 {
		t.Fatalf("没接账户域却落了 %d 条本地记录", repo.beginCalls)
	}
}

// TestParticipateSurfacesTheRoundRefusalFromBegin 确认「期次不在收人窗口内」是被原样
// 透传的，不被这一层加工成别的错误——它不是故障，是一次说得清楚的入口拒绝，而且
// **这次参与根本没有开始**，没有任何东西需要补偿。
func TestParticipateSurfacesTheRoundRefusalFromBegin(t *testing.T) {
	repo := &fakeRepository{beginErr: ErrRoundClosed}
	cards := &fakeCards{}

	_, err := newTestService(repo, cards).Participate(
		t.Context(), testRoundID, testUserID, directRequest(), "key")
	if !errors.Is(err, ErrRoundClosed) {
		t.Fatalf("得到 %v，期望 ErrRoundClosed", err)
	}
	if cards.deductCalls != 0 {
		t.Fatalf("入口就被拒了却扣了 %d 次卡", cards.deductCalls)
	}
	if len(repo.failParams) != 0 {
		t.Fatal("入口就被拒的参与没有记录可标失败——它压根没落库")
	}
}

// TestRemainingCountsDownToTheTarget 是抽奖中心进度条那个数。
//
// 三个边界都要对：正常差几个、已达标回 0（不是负数——页面上会显示「还差 -2 人」）、
// 以及没有在跑的期次时回 0。
func TestRemainingCountsDownToTheTarget(t *testing.T) {
	if got := (&ParticipateResult{Round: openRound(7)}).Remaining(); got != 3 {
		t.Fatalf("还差 %d 人，期望 3", got)
	}
	if got := (&ParticipateResult{Round: openRound(10)}).Remaining(); got != 0 {
		t.Fatalf("已达标时还差 %d 人，期望 0", got)
	}
	if got := (&ParticipateResult{Round: openRound(12)}).Remaining(); got != 0 {
		t.Fatalf("超过门槛时还差 %d 人，期望 0（不能是负数）", got)
	}
	if got := (&ParticipateResult{}).Remaining(); got != 0 {
		t.Fatalf("没有在跑的期次时还差 %d 人，期望 0", got)
	}
}
