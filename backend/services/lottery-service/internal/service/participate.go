package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/repository"
)

// 扣卡流水上给用户看的那两行文案，写死在这里而不是让调用方传。
//
// 它们会出现在用户的福卡明细里（「参与抽奖 -1」），是**对用户的账**而不是对调用方的账。
// 让请求体决定这一段，等于允许小程序写一句用户看不懂甚至误导的话进去。
const (
	deductTitle   = "参与抽奖"
	reverseTitle  = "参与抽奖退回"
	deductRefType = "draw"
)

// reverseRemark 是补偿冲正时的备注。它会出现在账户域的流水上，所以写清楚是谁退的、为什么。
const reverseRemark = "期次已结束，本次参与未生效，福卡原路退回"

// ParticipateResult 是一次参与的结果。
type ParticipateResult struct {
	// Participation 是落库的那一条（可能是这次新建的，也可能是幂等键命中的那一条）。
	Participation *model.Participation
	// Round 是锁内读到的那一期，用来算进度条还差几次参与。
	Round *model.Round
	// Replayed 为 true 表示这次没有扣卡：幂等键之前就成交过（同一张订单点了两下，或者上一次
	// 的响应丢了客户端重试）。卡只扣了一张，期次的计数也只加了一次。
	Replayed bool
}

// Remaining 是这一期还差几次参与到门槛，已达标时是 0。
//
// **数的是次数**：同一个人可以在同一期参与多次，每次各记一笔。
func (r *ParticipateResult) Remaining() int32 {
	if r.Round == nil {
		return 0
	}
	remaining := r.Round.ParticipantTarget - r.Round.ParticipantCount
	if remaining < 0 {
		return 0
	}
	return remaining
}

// Participate 是参与抽奖：三段事务，跨服务调用夹在中间。
//
// # 为什么是三段而不是一个大事务
//
// 扣卡要调 account-service。**跨服务调用在任何 PG 事务之外**（payment-service 的先例：
// 一次跨服务往返不该让数据库事务一直开着行锁），所以顺序只能是「先落本地记录 → 再扣卡 →
// 再确认」。中间任何一步崩掉都会留下一条 pending 记录，修复 worker 拿同一个 request_id
// 重跑——扣减本身幂等，所以重跑不会扣第二张卡。
//
// # 每一段各自负责什么
//
//   - Begin 锁住期次（与开奖互斥）并落下一条 pending。期次已经不再收人就是 ErrRoundClosed，
//     这一次参与**根本没有开始**，没有任何东西需要补偿。
//   - 扣卡。三个结论分别处理：余额不足 → 参与标 failed（账户域没动过，没有卡需要退）；
//     参数非法 → 参与标 failed 并大声记日志（本服务的 bug）；其余（超时、不可达、内部错）
//     → **保持 pending**，返回 ErrParticipationPending。第三种是唯一一个「还不知道结果」的
//     返回，不许下结论：说失败可能吞掉用户一张卡，说成功可能让他白拿一次参与。
//   - Confirm 加计数、把参与转 confirmed。**影响 0 行 = 期次在扣卡飞行途中关了或开了奖**，
//     用户什么也没得到，走补偿把卡退回去。
//
// idempotencyKey 是 Idempotency-Key 请求头。请求体带 sourceOrderId 时它被**忽略**：键改由
// 订单号派生（见 model.ParticipationKeyFromOrder），否则同一个订单换个头就能参与两次，
// 「一笔订单只能参与一次」那条不变量就没了。
func (s *LotteryService) Participate(ctx context.Context, roundID, userID string, req dto.ParticipateRequest, idempotencyKey string) (*ParticipateResult, error) {
	if s.cards == nil {
		// 没接账户域的部署不允许参与：没有任何一笔参与能被扣卡，记成 confirmed 就是撒谎。
		return nil, ErrFortuneCardsUnavailable
	}
	roundID = strings.TrimSpace(roundID)
	if roundID == "" {
		return nil, ErrRoundIDRequired
	}
	if _, err := uuid.Parse(roundID); err != nil {
		return nil, ErrRoundIDInvalid
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		// 调用方（controller）从令牌里取的用户，取不到就该在更早的地方被拦住；走到这里
		// 说明授权链配错了，而不是用户填错了什么。
		return nil, ErrUserIDRequired
	}

	orderID := strings.TrimSpace(req.SourceOrderID)
	key := strings.TrimSpace(idempotencyKey)
	if orderID != "" {
		if _, err := uuid.Parse(orderID); err != nil {
			return nil, ErrOrderIDInvalid
		}
		key = model.ParticipationKeyFromOrder(orderID)
	}
	if key == "" {
		return nil, ErrIdempotencyNeeded
	}
	// 三个「可空 uuid」快照一起转。空串收成 nil 而不是原样往下传：那几列是可空 uuid，
	// 空串进 uuid 列会报 invalid input syntax（不是「没有值」）。
	orderRef, err := optionalUUID(stringPtrOrNil(orderID), ErrOrderIDInvalid)
	if err != nil {
		return nil, err
	}
	machineID, err := optionalUUID(stringPtrOrNil(req.SourceMachineID), ErrMachineIDInvalid)
	if err != nil {
		return nil, err
	}
	locationID, err := optionalUUID(stringPtrOrNil(req.SourceLocationID), ErrLocationIDInvalid)
	if err != nil {
		return nil, err
	}

	// —— 第一段：锁期次、落一条 pending ——
	begun, err := s.repository.Begin(ctx, repository.BeginParams{
		RoundID:          roundID,
		UserID:           userID,
		IdempotencyKey:   key,
		SourceOrderID:    orderRef,
		SourceMachineID:  machineID,
		SourceLocationID: locationID,
		Cost:             int32(ParticipationCost),
		Now:              s.now(),
	})
	if err != nil {
		return nil, err
	}
	participation := begun.Participation

	if !begun.Created {
		// 幂等键命中。三种已有状态各走各的：
		switch participation.Status {
		case model.ParticipationConfirmed:
			// 已经成交过。**不扣卡**，把上一次的结果原样回放——这正是幂等键存在的理由。
			return &ParticipateResult{Participation: participation, Round: begun.Round, Replayed: true}, nil
		case model.ParticipationPending:
			// 上一次卡在第二段中间（进程死了，或者扣减调用挂住了）。接着往下走：扣减本身
			// 幂等，所以重跑第二段要么拿到上次那笔流水（replayed=true），要么这次真扣一笔。
		default:
			// failed / reversed：这一笔已经有过结论，不能装作它是新的。
			//
			// **已知的边界**：一条 failed 的行会一直占着这个幂等键，所以「余额不足」之后
			// 用户充值再拿同一张订单重试，仍然会读到这条失败记录。它没有被静默吞掉——用户
			// 拿到的是当初那条失败原因，而不是一个假的成功。真正修它需要「把一条 failed 的
			// 参与复活成 pending 再扣一次」，而那要确认账户域在失败的那条路上有没有留下
			// request_id 的去重记录；在那之前，这里不猜。
			return nil, failedParticipationError(participation)
		}
	}

	round, err := s.settle(ctx, participation)
	if err != nil {
		if errors.Is(err, ErrParticipationPending) {
			// **未决同时带着结果与错误**，这是本服务唯一一处这么写的返回。
			//
			// 理由：一条 202 必须说得出是哪一条参与在「处理中」，否则用户去「我的参与」里
			// 只能看见一堆不知道自己刚才点的那笔是哪个。而 ErrParticipationPending 是一个
			// 裸错误，装不下那个 id。调用方拿到它时的规矩是：**看错误决定状态码，看结果填
			// 响应体**，不要因为有错误就把结果丢掉。
			//
			// Round 用 Begin 锁内读到的那一份：这一次参与还没有被记进计数，所以它不是
			// 「当前进度」，但它带着期次号，够页面说清「在处理的是哪一期」。
			return &ParticipateResult{Participation: participation, Round: begun.Round}, err
		}
		return nil, err
	}
	// 回读一次而不是拿 settle 手里的那一条：确认这一步改了 status 与 confirmed_at，
	// 而返回给用户的记录必须是改完之后的那一份。
	confirmed, err := s.repository.GetParticipation(ctx, participation.ID)
	if err != nil {
		return nil, err
	}
	return &ParticipateResult{Participation: confirmed, Round: round}, nil
}

// settle 是第二段 + 第三段：扣卡，然后确认。
//
// 它**只对一条已经落库的 pending 记录**做事，所以调用方有两个：Participate 的第一次尝试，
// 以及修复 worker 重跑卡住的记录。两条路走同一段代码不是顺手复用——「重跑」与「第一次」
// 必须逐字相同，否则修复出来的结果与正常路径的结果会出现两种形状，而其中一种没人测过。
//
// 之所以能被重跑，全部依据是**扣减请求里的 request_id 就是 participation.ID**：账户域拿它
// 派生幂等键，重跑拿回的是同一笔流水（Replayed=true），而不是扣第二张卡。
//
// 返回 ErrParticipationPending 表示「还不知道结果」——调用方把它当成一次未决，不是失败。
func (s *LotteryService) settle(ctx context.Context, participation *model.Participation) (*model.Round, error) {
	if s.cards == nil {
		return nil, ErrFortuneCardsUnavailable
	}

	// —— 第二段：扣卡，事务外，硬超时 ——
	deductCtx, cancel := context.WithTimeout(ctx, s.deductTimeout)
	defer cancel()
	deducted, err := s.cards.Deduct(deductCtx, client.DeductRequest{
		UserID:        participation.UserID,
		Amount:        ParticipationCost,
		RequestID:     participation.ID,
		Title:         deductTitle,
		ReferenceType: deductRefType,
		ReferenceID:   participation.ID,
		ReferenceNo:   participation.RoundNo,
		Remark:        participation.CampaignName,
	})
	switch {
	case err == nil:
		// 落库继续。deducted.Replayed 真假都算成功：卡只扣了一张，这是全部依据。
	case errors.Is(err, client.ErrInsufficientFortuneCards):
		return nil, s.failParticipation(ctx, participation, model.FailureInsufficientFortuneCards, err)
	case errors.Is(err, client.ErrInvalidDeductRequest):
		// 本服务的 bug（负数张数、空 user_id）。重试一万次也是同样的结果，所以标 failed；
		// 但它是**我们自己的错**，要大声到有人来看。
		slog.ErrorContext(ctx, "lottery participation rejected by account service as invalid",
			"participationId", participation.ID, "userId", participation.UserID, "error", err)
		return nil, s.failParticipation(ctx, participation, model.FailureInvalidRequest, err)
	default:
		// 超时 / 不可达 / 账户域内部错。**不知道卡扣没扣**，所以不下任何结论：记一次尝试，
		// 保持 pending，让修复 worker 拿同一个 request_id 重跑。
		if noteErr := s.repository.NoteAttempt(ctx, participation.ID, err); noteErr != nil {
			slog.ErrorContext(ctx, "recording lottery participation attempt failed",
				"participationId", participation.ID, "error", noteErr)
		}
		slog.WarnContext(ctx, "lottery participation is awaiting confirmation",
			"participationId", participation.ID, "roundNo", participation.RoundNo, "error", err)
		return nil, ErrParticipationPending
	}

	// —— 第三段：确认 + 计数 ——
	round, _, err := s.repository.Confirm(ctx, repository.ConfirmParams{
		ParticipationID: participation.ID,
		FortuneEntryID:  deducted.EntryID,
	})
	if err != nil {
		if errors.Is(err, ErrRoundClosed) {
			// 期次在扣卡飞行途中关了或开了奖。用户什么也没得到，卡要退回去。
			return nil, s.compensate(ctx, participation, deducted.EntryID)
		}
		if errors.Is(err, repository.ErrParticipationNotPending) {
			// 这条记录在扫码的这一刻已经不是 pending 了，两个来源各走各的：
			//
			//   - confirmed：**上一次尝试其实已经提交成功**，只是响应丢了或进程死在返回前，
			//     所以重跑才会撞上它。不补偿（卡已经正确地扣了、计数也已经加了），把当期
			//     原样回给调用方。**绝不能冲正**——那会把一张已经成交的参与的卡退掉，而参与
			//     还留着，用户白拿一次机会。
			//   - failed / reversed：上一次已经有了结论（余额不足，或者补偿退卡之后）。
			//     回放那条结论，而不是编一个新的。
			//
			// 两种都不是故障，是「重跑安全」那条保证在起作用。
			current, readErr := s.repository.GetParticipation(ctx, participation.ID)
			if readErr != nil {
				return nil, readErr
			}
			if current.Status == model.ParticipationConfirmed {
				return s.repository.GetRound(ctx, current.RoundID)
			}
			return nil, failedParticipationError(current)
		}
		return nil, err
	}
	return round, nil
}

// compensate 是「卡扣了、参与没成」那条路上唯一的出口：退卡 + 把参与标 failed。
//
// 冲正由账户域按 `reverse:{entryId}` 幂等，所以这一整段可以安全重跑。
//
// 三种结果，处置不同：
//   - 退成功 → 参与标 failed / round_closed，记下 reverse_entry_id。
//   - 账户域**拒绝**退（ErrReverseRefused：退回去会把余额扣成负数，也就是那张卡已经被别处
//     花掉了）→ 仍然标 failed（用户确实什么也没得到，这是诚实的记录），但这是一条**必须
//     响的警情**：用户被扣了卡、没参与上、还退不回来，只能人工补。
//   - 退的时候超时 / 不可达 → 卡退没退**不知道**。这时**保持 pending**，交给修复 worker
//     重跑：它会重新扣一次（replayed=true，不会扣第二张）再撞回同一条补偿路径，直到退成。
//     标 failed 会让这条记录再也没人管，而用户可能已经丢了一张卡。
func (s *LotteryService) compensate(ctx context.Context, participation *model.Participation, entryID string) error {
	if entryID == "" {
		// 扣减调用成功却没有 entry_id：契约说它非空。没有 entry_id 就退不了款，把它当成
		// 「不知道结果」处理（保持 pending、交给修复 worker），而不是标 failed 把卡丢掉。
		err := errors.New("account service returned an empty entry id")
		slog.ErrorContext(ctx, "lottery participation compensation has no entry to reverse",
			"participationId", participation.ID, "error", err)
		if noteErr := s.repository.NoteAttempt(ctx, participation.ID, err); noteErr != nil {
			slog.ErrorContext(ctx, "recording lottery participation attempt failed",
				"participationId", participation.ID, "error", noteErr)
		}
		return ErrParticipationPending
	}

	reverse, err := s.cards.Reverse(ctx, entryID, reverseTitle, reverseRemark)
	if err != nil {
		if errors.Is(err, client.ErrReverseRefused) {
			slog.ErrorContext(ctx, "LOTTERY COMPENSATION REFUSED: a user was charged and got nothing",
				"participationId", participation.ID, "userId", participation.UserID,
				"roundNo", participation.RoundNo, "fortuneEntryId", entryID, "error", err)
			return s.failParticipation(ctx, participation, model.FailureRoundClosed, err)
		}
		if noteErr := s.repository.NoteAttempt(ctx, participation.ID, err); noteErr != nil {
			slog.ErrorContext(ctx, "recording lottery participation attempt failed",
				"participationId", participation.ID, "error", noteErr)
		}
		slog.WarnContext(ctx, "lottery participation compensation could not be confirmed",
			"participationId", participation.ID, "fortuneEntryId", entryID, "error", err)
		return ErrParticipationPending
	}

	// 冲正成功。把**冲正流水**的 id 记下来（不是被冲正的那一笔）：将来查「这张卡怎么回来
	// 的」要能按它反查到那一笔账变。
	slog.InfoContext(ctx, "lottery participation compensated",
		"participationId", participation.ID, "roundNo", participation.RoundNo,
		"fortuneEntryId", entryID, "reverseEntryId", reverse.EntryID)
	if err := s.repository.Fail(ctx, repository.FailParams{
		ParticipationID: participation.ID,
		FailureCode:     model.FailureRoundClosed,
		ReverseEntryID:  &reverse.EntryID,
	}); err != nil {
		return err
	}
	// 面向用户的结论：这一期已经结束，你这张卡退回来了。它不是故障，所以不返回 5xx。
	return ErrRoundClosed
}

// failParticipation 把一条参与标成失败并返回给用户的结论。
//
// cause 是**原因**（账户域那条、或者冲正被拒那条），它进 last_error 是为了让人事后能看见
// 到底发生了什么；返回给用户的是结论（余额不足 / 期次已结束），不是那一串内部描述。
func (s *LotteryService) failParticipation(ctx context.Context, participation *model.Participation, failureCode string, cause error) error {
	if err := s.repository.Fail(ctx, repository.FailParams{
		ParticipationID: participation.ID,
		FailureCode:     failureCode,
	}); err != nil {
		return err
	}
	if cause != nil {
		slog.InfoContext(ctx, "lottery participation failed",
			"participationId", participation.ID, "roundNo", participation.RoundNo,
			"failureCode", failureCode, "error", cause)
	}
	return failedParticipationError(&model.Participation{
		ID: participation.ID, RoundNo: participation.RoundNo, FailureCode: failureCode,
	})
}

// failedParticipationError 把一条已失败的参与记录翻成一个返回给调用方的错误。
//
// 翻回错误而不是把这条记录当结果回下去：调用方（controller）只需要一条路——错误里带着
// 结论，它照着映射状态码。留两条路（一条错误、一条「成功的响应里 status 是 failed」）迟早
// 会有一条被漏掉，而漏掉的那条会把一次失败渲染成成功。
func failedParticipationError(participation *model.Participation) error {
	switch participation.FailureCode {
	case model.FailureInsufficientFortuneCards:
		return ErrInsufficientFortuneCards
	case model.FailureRoundClosed, model.FailureRoundNotOpen:
		return ErrRoundClosed
	case model.FailureInvalidRequest:
		return ErrInvalidDeductRequest
	default:
		// 将来新增的 failure_code 落到这里：至少不会静默变成「成功」。
		return ErrParticipationFailed
	}
}

// GetParticipation 读一条参与记录。
func (s *LotteryService) GetParticipation(ctx context.Context, id string) (*model.Participation, error) {
	if _, err := uuid.Parse(strings.TrimSpace(id)); err != nil {
		return nil, ErrParticipationNotFound
	}
	return s.repository.GetParticipation(ctx, id)
}

// ListParticipations 分页读参与记录。
//
// **归属由调用方决定**：后台按任意 roundId / campaignId / userId 查，小程序那条路在
// controller 里被强制成「只能查自己」（requireConsumer）。这一层不区分——它只有一个 userID
// 字段，是「筛谁」而不是「谁在查」。
func (s *LotteryService) ListParticipations(ctx context.Context, q dto.ParticipationQuery) ([]*repository.ParticipationListRow, int, error) {
	for _, id := range []string{q.RoundID, q.CampaignID, q.UserID} {
		if id == "" {
			continue
		}
		if _, err := uuid.Parse(id); err != nil {
			return nil, 0, ErrRoundIDInvalid
		}
	}
	if q.Status != "" {
		switch q.Status {
		case model.ParticipationPending, model.ParticipationConfirmed,
			model.ParticipationFailed, model.ParticipationReversed:
		default:
			return nil, 0, ErrStatusInvalid
		}
	}
	return s.repository.ListParticipations(ctx, q)
}

// CampaignCenter 是抽奖中心页的四处读数凑在一起：活动、这一期、我的进度、我的福卡。
//
// 它拼的是**模型**而不是 dto：响应的形状归 controller（本服务的规矩是「service 回模型与
// 仓储行，controller 映射成 dto」），这里只负责把东西取齐。
type CampaignCenter struct {
	Detail *CampaignDetail
	// Round 为 nil = 这个活动当前没有进行中的期次。
	Round *model.Round
	// MyParticipationCount 是当前这个用户在这一期记了几笔（含 pending）。
	MyParticipationCount int32
	// Balance 是账户域的实时余额，本库没有余额列（方案 §5.7）。
	Balance int64
	// BalanceUnavailable 为 true 表示余额**没读到**（账户域不可达），而不是余额为 0。
	// 页面据它显示「余额暂时读不到」——把这两件事显示成同一句话会让用户在账户域抖动的时候
	// 白跑一趟。
	BalanceUnavailable bool
}

// CampaignCenter 读抽奖中心页要的全部东西。
//
// 没有在跑的期次**不是错误**：一个刚建好还没启用的活动、一个暂停中的活动，抽奖中心都该
// 显示「暂无进行中的期次」而不是一个报错——期次是随时间滚动出来的，不是活动存在的证明。
//
// 余额读失败同样不返回错误：用户已经登录、已经能看见期次，不该因为账户域抖了一下就整页
// 报错，那会连「还差几个人」都不告诉他。
func (s *LotteryService) CampaignCenter(ctx context.Context, campaignID, userID string) (*CampaignCenter, error) {
	detail, err := s.GetCampaign(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	center := &CampaignCenter{Detail: detail}

	// LiveRound 在没有在跑的期次时回的是 (nil, nil) 而不是 ErrRoundNotFound：「这个活动
	// 现在没开期」是一个正常的答案，不是一次查不到。
	round, err := s.repository.LiveRound(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	if round == nil {
		return center, nil
	}
	center.Round = round

	mine, err := s.repository.CountUserParticipations(ctx, round.ID, userID)
	if err != nil {
		return nil, err
	}
	center.MyParticipationCount = mine

	if s.cards == nil {
		// 没接账户域：不是「余额为 0」，是读不到。
		center.BalanceUnavailable = true
		return center, nil
	}
	balance, err := s.cards.Balance(ctx, userID)
	if err != nil {
		slog.WarnContext(ctx, "reading fortune card balance for the lottery center failed",
			"userId", userID, "error", err)
		center.BalanceUnavailable = true
		return center, nil
	}
	center.Balance = balance
	return center, nil
}

// stringPtrOrNil 把「空串等于没值」的请求字段转成指针。
//
// 请求体里这几个字段是纯字符串（省略和空串在 JSON 里长得一样），而库里那几列是可空 uuid：
// 空串进 uuid 列会报 invalid input syntax，所以空串一律收成 nil（= 没有这个快照）。
func stringPtrOrNil(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
