package service

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/platform/audit"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/repository"
)

// DrawManually 是人工开奖。
//
// 它与自动开奖走的是**同一个仓储函数、同一套算法、同一个种子派生**，差别只有三个字段：
// mode='manual'、drawn_by 是操作员、reason 必填。这不是巧合——「人工开奖」这个动作的全部
// 意义就是「在算法认为不该开的时刻开」，而不是「用另一套规则决定谁中奖」。两套算法意味着
// 同一期的结果取决于谁点的按钮，那样抽奖就不再是抽奖了。
//
// # 为什么要 expected 校验
//
// 管理员拿着一个页面点了开奖，而那个页面可能是三十秒前加载的：这期间期次可能已经达标关闭、
// 已经被自动开奖、甚至已经被作废。对不上就回 409（ErrRoundChanged / ErrRoundAlreadyDrawn），
// 而不是替一个已经变了的局面决定谁中奖。
//
// 与 account-service 人工调整余额那条路（identity/021）同一条思路：全系统少数几个能决定
// 资产归属的动作，必须先证明自己看的是当前状态。**本轮不做审批流**（方案 §18.3 要求 RBAC +
// 审批 + 不可篡改审计，审批这一项由「只绑 super_admin + 强制 reason + expect 校验 + 只增的
// 开奖记录 + 审计日志」替代，见 migrations/identity/022_lottery_admin.sql 的收窄说明）。
func (s *LotteryService) DrawManually(ctx context.Context, roundID string, req dto.DrawRequest, actor *string) (*repository.DrawOutcome, error) {
	roundID = strings.TrimSpace(roundID)
	if roundID == "" {
		return nil, ErrRoundIDRequired
	}
	if _, err := uuid.Parse(roundID); err != nil {
		return nil, ErrRoundIDInvalid
	}
	reason, err := validateReason(req.Reason)
	if err != nil {
		return nil, err
	}
	expectedStatus, err := normaliseRoundStatus(req.ExpectedRoundStatus)
	if err != nil {
		return nil, err
	}
	if req.ExpectedParticipantCount == nil {
		// expect 校验的另一半。只校验状态不校验人数，会漏掉「状态没变、人又进来了几个」
		// 那一类变化——而开奖的名单正是按人数取的，人数变了名单就变了。
		return nil, ErrExpectedCountRequired
	}
	if actor == nil || strings.TrimSpace(*actor) == "" {
		// drawn_by 是 NOT NULL 的语义（CHECK 也挡）：人工开奖必须留下是谁开的。
		return nil, ErrActorRequired
	}

	outcome, err := s.repository.DrawRound(ctx, repository.DrawParams{
		RoundID:                  roundID,
		Mode:                     model.DrawModeManual,
		DrawnBy:                  actor,
		Reason:                   reason,
		Now:                      s.now(),
		TraceID:                  audit.TraceIDFromContext(ctx),
		ExpectedStatus:           expectedStatus,
		ExpectedParticipantCount: req.ExpectedParticipantCount,
	})
	if err != nil {
		return nil, err
	}
	return outcome, nil
}

// DrawAutomatically 是自动开奖 worker 的入口：**收满门槛**的期次，把它开掉。
//
// 这是自动开奖唯一的一条路（2026-09-15 起）。期次等级上没有截止时间，一个没收满的期次
// 不会被 worker 碰——它的出路是管理员人工开奖或作废。
//
// **返回 (nil, nil) 表示「不用开」，而且这是正常的**。worker 扫到的 id 是几十毫秒前那一
// 眼的状态，锁内重新判定之后可能发现：
//   - 另一个副本已经开过了（ErrRoundAlreadyDrawn）；
//   - 已经被人工开奖抢先（同上）；
//   - 已经被作废（ErrRoundCancelled）；
//   - 状态在扫描之后又变了，不再满足开奖条件（ErrRoundNotAwaitingDraw）——扫描只看
//     status='closed'，这四种的共同点都是「它已经不是那个收满待开的期次了」。
//
// 这四种都不是故障，是「多副本 + 无选主」这套设计的**正常噪声**——它们能被区分出来，正是
// 因为真正的开奖判定在行锁内做，而不是在扫描时做（见 repository.DrawRound）。
// worker 把 (nil, nil) 记成一次跳过，不该告警；否则每多一个副本，告警就多一份。
//
// 零人参与那一条也走这里：结果里的 Draw 为 nil、Round.Status 是 cancelled，那**不是**
// 一次失败的开奖，是「这一期没人来，直接开下一期」。
func (s *LotteryService) DrawAutomatically(ctx context.Context, roundID string) (*repository.DrawOutcome, error) {
	if _, err := uuid.Parse(strings.TrimSpace(roundID)); err != nil {
		return nil, ErrRoundIDInvalid
	}
	outcome, err := s.repository.DrawRound(ctx, repository.DrawParams{
		RoundID: roundID,
		Mode:    model.DrawModeAuto,
		Now:     s.now(),
		TraceID: audit.TraceIDFromContext(ctx),
	})
	if err != nil {
		if isDrawRace(err) {
			return nil, nil
		}
		return nil, err
	}
	return outcome, nil
}

// isDrawRace 判断一个开奖错误是不是「被别人抢先了」。
//
// 这三种错误对 worker 的含义是「这次不用做了」，对人工开奖的含义是「你手上的页面过期了」
// ——同一个错误在两处的处置不同，所以判定放在这里而不是塞进 worker：worker 只该知道
// 「跳过还是告警」。
func isDrawRace(err error) bool {
	return errors.Is(err, ErrRoundAlreadyDrawn) ||
		errors.Is(err, ErrRoundNotAwaitingDraw) ||
		errors.Is(err, ErrRoundCancelled)
}

// CancelRound 作废一期。
//
// **仅允许零人参与的期次**（仓储用 ErrRoundHasParticipations 挡）：有参与者的作废意味着
// 要把 N 张福卡沿着 N 次跨服务冲正还回去，那条路没有截止时间、没有重试上限，属下一轮。
// 真需要作废一期已经收了人的期次时，正确的做法是让它照常开奖，然后对结果做撤销——那是
// 另一件有据可查的事。
func (s *LotteryService) CancelRound(ctx context.Context, roundID string, req dto.CancelRoundRequest, actor *string) (*model.Round, error) {
	roundID = strings.TrimSpace(roundID)
	if roundID == "" {
		return nil, ErrRoundIDRequired
	}
	if _, err := uuid.Parse(roundID); err != nil {
		return nil, ErrRoundIDInvalid
	}
	reason, err := validateReason(req.Reason)
	if err != nil {
		return nil, err
	}
	return s.repository.CancelRound(ctx, roundID, reason, actor)
}

// RoundsAwaitingDraw 取应当开奖的期次 id，给开奖 worker 用。
//
// 它只是**扫描**：真正的判定在 DrawRound 的锁内重做一遍。这两处必须都在，而且不能把扫描
// 的结论当成结论——扫描到调用之间隔着几十毫秒，其间期次可能已经达标、已经被人开掉。
func (s *LotteryService) RoundsAwaitingDraw(ctx context.Context, limit int) ([]string, error) {
	return s.repository.RoundsAwaitingDraw(ctx, limit)
}

// GetRound 读一期。
func (s *LotteryService) GetRound(ctx context.Context, id string) (*repository.RoundListRow, error) {
	if _, err := uuid.Parse(strings.TrimSpace(id)); err != nil {
		return nil, ErrRoundIDInvalid
	}
	return s.repository.GetRoundView(ctx, id)
}

// ListRounds 分页读期次。
func (s *LotteryService) ListRounds(ctx context.Context, q dto.RoundQuery) ([]*repository.RoundListRow, int, error) {
	if q.CampaignID != "" {
		if _, err := uuid.Parse(strings.TrimSpace(q.CampaignID)); err != nil {
			return nil, 0, ErrCampaignIDInvalid
		}
	}
	if q.Status != "" {
		if _, err := normaliseRoundStatus(q.Status); err != nil {
			return nil, 0, err
		}
	}
	return s.repository.ListRounds(ctx, q)
}

// GetDraw 读一次开奖记录（含它开出的中奖名单）。
//
// 中奖名单按 **round_id** 反查，不按 draw_id：lottery_draws.round_id 是唯一索引，一期
// 至多一次开奖，所以「这一次开奖开出的名单」与「这一期的中奖记录」是同一批。按 round_id
// 查还顺带回答了那个更要紧的问题——**这一期一共发出去几张中奖记录**，而按 draw_id 查只会
// 回答「记录上挂着哪几张」。
func (s *LotteryService) GetDraw(ctx context.Context, id string) (*model.Draw, []*model.Win, error) {
	if _, err := uuid.Parse(strings.TrimSpace(id)); err != nil {
		return nil, nil, ErrDrawNotFound
	}
	draw, err := s.repository.GetDraw(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	winners, _, err := s.repository.ListWins(ctx, dto.WinQuery{
		RoundID: draw.RoundID,
		// Page 必须显式给 1：这里不是分页读，是「这一期的全部中奖记录」。WinQuery 的零值是
		// 0，而 repository 把 page 原样交给 api.PageOffset 算 (page-1)*pageSize——0 会算成
		// 负偏移，PostgreSQL 直接拒掉整条查询（SQLSTATE 2201X），于是「看开奖」弹窗永远打不开。
		Page:     1,
		PageSize: dto.MaxPageSize,
	})
	if err != nil {
		return nil, nil, err
	}
	return draw, winners, nil
}

// FindDrawByRound 读某一期的开奖记录。
func (s *LotteryService) FindDrawByRound(ctx context.Context, roundID string) (*model.Draw, error) {
	if _, err := uuid.Parse(strings.TrimSpace(roundID)); err != nil {
		return nil, ErrRoundIDInvalid
	}
	return s.repository.FindDrawByRound(ctx, roundID)
}

// ListWins 分页读中奖记录。
func (s *LotteryService) ListWins(ctx context.Context, q dto.WinQuery) ([]*model.Win, int, error) {
	for _, id := range []string{q.RoundID, q.CampaignID, q.UserID} {
		if id == "" {
			continue
		}
		if _, err := uuid.Parse(id); err != nil {
			return nil, 0, ErrRoundIDInvalid
		}
	}
	return s.repository.ListWins(ctx, q)
}

// GetWin 读一条中奖记录（含它的流水）。
func (s *LotteryService) GetWin(ctx context.Context, id string) (*model.Win, []*model.WinEvent, error) {
	if _, err := uuid.Parse(strings.TrimSpace(id)); err != nil {
		return nil, nil, ErrWinNotFound
	}
	win, err := s.repository.GetWin(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	events, err := s.repository.ListWinEvents(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return win, events, nil
}

// ListWinEvents 读一条中奖记录的流水。
func (s *LotteryService) ListWinEvents(ctx context.Context, winID string) ([]*model.WinEvent, error) {
	if _, err := uuid.Parse(strings.TrimSpace(winID)); err != nil {
		return nil, ErrWinNotFound
	}
	return s.repository.ListWinEvents(ctx, winID)
}

// CountWinsByUser 数一个用户中过几次、其中几次还没领。
//
// 抽奖中心首页的「我的奖品」角标用它。它读的是本库自己的表，不走账户域。
func (s *LotteryService) CountWinsByUser(ctx context.Context, userID string) (int, int, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return 0, 0, ErrUserIDRequired
	}
	return s.repository.CountWinsByUser(ctx, userID)
}

// validateReason 校验并归一化人工开奖 / 作废的理由。
//
// 长度按**字符**算（不是字节）：中文一个字符三字节，按字节限 200 会让运营在写了一百来个字的
// 时候被莫名其妙地拒绝。归一化之后交给仓储，落进 lottery_draws.reason。
func validateReason(raw string) (string, error) {
	reason := strings.TrimSpace(raw)
	if reason == "" {
		return "", ErrReasonRequired
	}
	if utf8.RuneCountInString(reason) > MaxReasonLength {
		return "", ErrReasonTooLong
	}
	return reason, nil
}

// normaliseRoundStatus 校验并归一化期次状态。
//
// 期次状态**只能读、不能写**：它由 Begin / Confirm / DrawRound / CancelRound 四条路各自
// 改成它该有的样子，没有一个「直接把期次改成某个状态」的接口。这个函数只服务于筛选。
func normaliseRoundStatus(raw string) (string, error) {
	status := strings.ToLower(strings.TrimSpace(raw))
	switch status {
	case "":
		return "", ErrStatusRequired
	case model.RoundOpen, model.RoundClosed, model.RoundDrawn, model.RoundCancelled:
		return status, nil
	default:
		return "", ErrStatusInvalid
	}
}
