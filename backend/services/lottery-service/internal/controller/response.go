package controller

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/service"
)

// traceID 是一次请求的追踪号，进 outbox 事件的信封。
//
// 用平台那一个而不是自己从请求头里读：trace 中间件已经把它放进 context 了，两处各读一遍
// 迟早会读到两个不同的值。
func traceID(r *http.Request) string { return audit.TraceIDFromContext(r.Context()) }

// —— 映射 ——
//
// service 回模型与仓储行，这里映射成 dto。**映射只做形状转换，不做判断**：凡是
// 「如果 X 就填 Y」的写法都要停一下——那多半是业务规则，它该在 service 里，而且该被测试。

func activationResponse(row *repository.ActivationListRow) dto.ActivationResponse {
	activation := row.Activation
	return dto.ActivationResponse{
		ID:                  activation.ID,
		LocationID:          activation.LocationID,
		LocationName:        activation.LocationName,
		Status:              activation.Status,
		Remark:              activation.Remark,
		DefaultCampaignID:   row.DefaultCampaignID,
		DefaultCampaignName: row.DefaultCampaignName,
		CampaignCount:       row.CampaignCount,
		ActivatedBy:         derefString(activation.ActivatedBy),
		ActivatedAt:         activation.ActivatedAt,
		DeactivatedAt:       activation.DeactivatedAt,
		CreatedAt:           activation.CreatedAt,
		UpdatedAt:           activation.UpdatedAt,
		LiveRoundID:         row.LiveRoundID,
		LiveRoundNo:         row.LiveRoundNo,
		LiveRoundSize:       row.LiveRoundSize,
		LiveRoundDone:       row.LiveRoundDone,
	}
}

func activationResponses(rows []*repository.ActivationListRow) []dto.ActivationResponse {
	responses := make([]dto.ActivationResponse, 0, len(rows))
	for _, row := range rows {
		responses = append(responses, activationResponse(row))
	}
	return responses
}

func campaignResponse(detail *service.CampaignDetail) dto.CampaignResponse {
	row := detail.View
	campaign := row.Campaign
	response := dto.CampaignResponse{
		ID:                 campaign.ID,
		ActivationID:       campaign.ActivationID,
		LocationID:         row.LocationID,
		LocationName:       row.LocationName,
		MachineID:          campaign.MachineID,
		Code:               campaign.Code,
		Name:               campaign.Name,
		ParticipantTarget:  campaign.ParticipantTarget,
		Description:        campaign.Description,
		IsDefault:          campaign.IsDefault,
		StartAt:            campaign.StartAt,
		EndAt:              campaign.EndAt,
		Status:             campaign.Status,
		Prizes:             prizeResponses(detail.Prizes),
		PrizeTotalQuantity: row.PrizeTotalQuantity,
		LiveRoundID:        row.LiveRoundID,
		LiveRoundNo:        row.LiveRoundNo,
		LiveRoundSize:      row.LiveRoundSize,
		LiveRoundDone:      row.LiveRoundDone,
		RoundCount:         row.RoundCount,
		CreatedAt:          campaign.CreatedAt,
		UpdatedAt:          campaign.UpdatedAt,
	}
	return response
}

// campaignSummaryResponse 是列表页的活动形状：与详情同一个结构，只把奖池留空。
//
// 用同一个结构体而不是另开一个 Summary 类型：前端那一张表同时服务两处，两个类型意味着
// 两套 dataIndex，而它们的差别只有「有没有 prizes」——那种差别用「空数组」表达就够了。
func campaignSummaryResponse(row *repository.CampaignListRow) dto.CampaignResponse {
	return campaignResponse(&service.CampaignDetail{View: row})
}

func campaignSummaryResponses(rows []*repository.CampaignListRow) []dto.CampaignResponse {
	responses := make([]dto.CampaignResponse, 0, len(rows))
	for _, row := range rows {
		responses = append(responses, campaignSummaryResponse(row))
	}
	return responses
}

func prizeResponses(prizes []*model.CampaignPrize) []dto.CampaignPrizeResponse {
	responses := make([]dto.CampaignPrizeResponse, 0, len(prizes))
	for _, prize := range prizes {
		responses = append(responses, dto.CampaignPrizeResponse{
			ID:                prize.ID,
			SortOrder:         prize.SortOrder,
			PrizeKind:         prize.PrizeKind,
			Name:              prize.Name,
			CouponTemplateID:  prize.CouponTemplateID,
			ImageURL:          prize.ImageURL,
			ClaimInstructions: prize.ClaimInstructions,
			Quantity:          prize.Quantity,
		})
	}
	return responses
}

func roundResponse(row *repository.RoundListRow) dto.RoundResponse {
	round := row.Round
	return dto.RoundResponse{
		ID:                round.ID,
		CampaignID:        round.CampaignID,
		CampaignCode:      row.CampaignCode,
		CampaignName:      row.CampaignName,
		Seq:               round.Seq,
		RoundNo:           round.RoundNo,
		Status:            round.Status,
		ParticipantTarget: round.ParticipantTarget,
		ParticipantCount:  round.ParticipantCount,
		WinnerCount:       round.WinnerCount,
		StartsAt:          round.StartsAt,
		EndsAt:            round.EndsAt,
		DrawnAt:           round.DrawnAt,
		CancelledAt:       round.CancelledAt,
		CancelReason:      round.CancelReason,
		DrawID:            row.DrawID,
		DrawMode:          row.DrawMode,
		DrawTrigger:       row.DrawTrigger,
		ActualWinnerCount: row.ActualWinnerCount,
		CreatedAt:         round.CreatedAt,
	}
}

func roundResponses(rows []*repository.RoundListRow) []dto.RoundResponse {
	responses := make([]dto.RoundResponse, 0, len(rows))
	for _, row := range rows {
		responses = append(responses, roundResponse(row))
	}
	return responses
}

func participationResponse(row *repository.ParticipationListRow) dto.ParticipationResponse {
	p := row.Participation
	return dto.ParticipationResponse{
		ID:               p.ID,
		RoundID:          p.RoundID,
		RoundNo:          p.RoundNo,
		CampaignID:       p.CampaignID,
		CampaignName:     p.CampaignName,
		UserID:           p.UserID,
		SourceOrderID:    derefString(p.SourceOrderID),
		SourceOrderNo:    p.SourceOrderNo,
		SourceMachineID:  derefString(p.SourceMachineID),
		SourceLocationID: derefString(p.SourceLocationID),
		Cost:             p.Cost,
		Status:           p.Status,
		FailureCode:      p.FailureCode,
		FortuneEntryID:   derefString(p.FortuneEntryID),
		CreatedAt:        p.CreatedAt,
		ConfirmedAt:      p.ConfirmedAt,
		WinID:            row.WinID,
		WinClaimNo:       row.WinClaimNo,
		PrizeName:        row.PrizeName,
	}
}

func participationResponses(rows []*repository.ParticipationListRow) []dto.ParticipationResponse {
	responses := make([]dto.ParticipationResponse, 0, len(rows))
	for _, row := range rows {
		responses = append(responses, participationResponse(row))
	}
	return responses
}

func winResponse(win *model.Win) dto.WinResponse {
	return dto.WinResponse{
		ID:                 win.ID,
		DrawID:             win.DrawID,
		RoundID:            win.RoundID,
		RoundNo:            win.RoundNo,
		CampaignID:         win.CampaignID,
		CampaignName:       win.CampaignName,
		ParticipationID:    win.ParticipationID,
		UserID:             win.UserID,
		PrizeID:            win.PrizeID,
		PrizeKind:          win.PrizeKind,
		OriginalPrizeName:  win.OriginalPrizeName,
		CurrentPrizeName:   win.CurrentPrizeName,
		ClaimNo:            win.ClaimNo,
		Status:             win.Status,
		Testimonial:        win.Testimonial,
		TestimonialImages:  decodeStringList(win.TestimonialImages),
		SourceOrderID:      derefString(win.SourceOrderID),
		SourceOrderNo:      win.SourceOrderNo,
		SourceMachineID:    derefString(win.SourceMachineID),
		SourceLocationID:   derefString(win.SourceLocationID),
		ExpiresAt:          win.ExpiresAt,
		ClaimedAt:          win.ClaimedAt,
		RedeemedAt:         win.RedeemedAt,
		RedeemedBy:         derefString(win.RedeemedBy),
		RedeemLocationID:   derefString(win.RedeemLocationID),
		RedeemLocationName: win.RedeemLocationName,
		CreatedAt:          win.CreatedAt,
	}
}

func winResponses(wins []*model.Win) []dto.WinResponse {
	responses := make([]dto.WinResponse, 0, len(wins))
	for _, win := range wins {
		responses = append(responses, winResponse(win))
	}
	return responses
}

func winEventResponses(events []*model.WinEvent) []dto.WinEventResponse {
	responses := make([]dto.WinEventResponse, 0, len(events))
	for _, event := range events {
		responses = append(responses, dto.WinEventResponse{
			ID:         event.ID,
			EventType:  event.EventType,
			FromStatus: event.FromStatus,
			ToStatus:   event.ToStatus,
			ActorType:  event.ActorType,
			ActorID:    derefString(event.ActorID),
			ActorName:  event.ActorName,
			Reason:     event.Reason,
			Metadata:   decodeObject(event.Metadata),
			CreatedAt:  event.CreatedAt,
		})
	}
	return responses
}

// drawResponse 映射一次开奖的结果。调用方必须先确认 outcome.Draw 不为 nil。
//
// 期次号取自 Round 而不是回查：outcome 里那一份就是开奖那一刻的期次，与中奖记录、
// 种子出自同一个事务。
func drawResponse(outcome *repository.DrawOutcome) dto.DrawResponse {
	draw := outcome.Draw
	response := dto.DrawResponse{
		DrawID:           draw.ID,
		RoundID:          draw.RoundID,
		Mode:             draw.Mode,
		Trigger:          draw.Trigger,
		Seed:             draw.Seed,
		Algorithm:        draw.Algorithm,
		ParticipantCount: draw.ParticipantCount,
		WinnerCount:      draw.WinnerCount,
		CreatedAt:        draw.CreatedAt,
		Winners:          winResponses(outcome.Winners),
		CampaignEnded:    outcome.CampaignEnded,
	}
	if outcome.Round != nil {
		response.RoundNo = outcome.Round.RoundNo
	}
	if outcome.NextRound != nil {
		response.NextRoundID = outcome.NextRound.ID
		response.NextRoundNo = outcome.NextRound.RoundNo
	}
	return response
}

// —— 用户身份 ——

// requireConsumer 是小程序端入口的闸门，返回调用者的用户 ID；失败时已经写好了响应。
//
// 判定必须落在 realm 上，不能只看 subject == user_id：平台管理员和商户账号的令牌同样满足
// 那两条，只查它们等于把抽奖接口对管理端敞开——那不只是越权，还会让参与挂在一个根本不是
// 消费者的 user_id 上。缺失和域不对分成 401 与 403：前者是没登录，后者是拿错了身份的
// 令牌，客户端要做的处理不同。
//
// 这一段与 order-service / user-service 的 requireConsumer 是同一套判定，三处必须保持
// 一致，否则同一种令牌在两个服务上会得到不同的回答。
func requireConsumer(w http.ResponseWriter, r *http.Request) (string, bool) {
	identity, ok := auth.IdentityFromRequest(r)
	if !ok || strings.TrimSpace(identity.UserID) == "" || identity.Subject != identity.UserID {
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "未登录")
		return "", false
	}
	if identity.Realm != auth.RealmConsumer {
		api.Error(w, http.StatusForbidden, api.CodeForbidden, "非小程序用户")
		return "", false
	}
	return identity.UserID, true
}

// requireAdmin 读后台调用者的身份。认证与权限码已经由路由上的中间件验过，这里只取「是谁」。
//
// 取不到就回 401 而不是继续：人工开奖要往 lottery_draws.drawn_by 里写一个操作人，
// 写不进去的那次开奖会是一条查不到是谁干的记录（数据库的 CHECK 也会拒，但那是一条 23514）。
func requireAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	identity, ok := auth.IdentityFromRequest(r)
	if !ok || strings.TrimSpace(identity.UserID) == "" {
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "unauthorized")
		return "", false
	}
	return identity.UserID, true
}

// —— 错误 ——

// writeLotteryError 把业务层的错误映射成 HTTP 响应。
//
// 顺序有讲究：**先认业务结论，再认请求不合法**。两边的错误值不重叠，所以顺序本身不影响
// 结果，但它决定了新增一个错误值时最先要问的问题——「这是一次说得清楚的结果，还是调用方
// 传错了？」。默认落到 500 且不把内部描述回给用户：那些字符串里会有账户域的错误、SQL 的
// 报错，它们是给日志的，不是给小程序弹窗的。
func writeLotteryError(w http.ResponseWriter, r *http.Request, err error, fallback string) {
	switch {
	case service.IsValidationError(err):
		// 错误码用字面量 "INVALID_ARGUMENT"，与 order-service / coupon-service 一致
		// （platform/api 的常量表里没有它，那个表是给跨服务的通用码用的）。
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())

	// —— 找不到（404）——
	case errors.Is(err, service.ErrActivationNotFound),
		errors.Is(err, service.ErrCampaignNotFound),
		errors.Is(err, service.ErrRoundNotFound),
		errors.Is(err, service.ErrParticipationNotFound),
		errors.Is(err, service.ErrWinNotFound),
		errors.Is(err, service.ErrDrawNotFound):
		api.Error(w, http.StatusNotFound, api.CodeNotFound, err.Error())

	// —— 状态冲突（409）：请求本身没问题，是现在这个局面不接受它 ——
	case errors.Is(err, service.ErrLocationAlreadyActivated),
		errors.Is(err, service.ErrCampaignCodeTaken),
		errors.Is(err, service.ErrDefaultCampaignExists),
		errors.Is(err, service.ErrRoundAlreadyLive),
		errors.Is(err, service.ErrRoundClosed),
		errors.Is(err, service.ErrRoundChanged),
		errors.Is(err, service.ErrRoundAlreadyDrawn),
		errors.Is(err, service.ErrRoundNotAwaitingDraw),
		errors.Is(err, service.ErrRoundCancelled),
		errors.Is(err, service.ErrRoundHasParticipations),
		errors.Is(err, service.ErrIdempotencyKeyConflict),
		errors.Is(err, service.ErrMachineMismatch),
		errors.Is(err, service.ErrCampaignEnded),
		errors.Is(err, service.ErrCampaignWindowOver),
		errors.Is(err, service.ErrCampaignStatusTransition),
		errors.Is(err, service.ErrParticipationFailed):
		api.Error(w, http.StatusConflict, api.CodeConflict, err.Error())

	// —— 福卡不够：400 加一个**业务码** ——
	//
	// 它不是「参数写错了」（那是 INVALID_ARGUMENT），也不是冲突：这是用户看一眼就懂的
	// 结论，小程序据此弹「福卡不够了，去完成订单领卡」而不是「系统繁忙」。
	case errors.Is(err, service.ErrInsufficientFortuneCards):
		api.Error(w, http.StatusBadRequest, "INSUFFICIENT_FORTUNE_CARDS", "福卡不足")

	// —— 身份缺失（401）：userId / 操作人不在请求体里，它来自令牌 ——
	//
	// 走到这里说明装配处漏挂了认证，不是用户填错了什么。
	case errors.Is(err, service.ErrUserIDRequired), errors.Is(err, service.ErrActorRequired):
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, err.Error())

	// —— 账户域不可用：503 而不是 500 ——
	//
	// 这是**依赖没接上**，重试（等它起来）有意义。本轮只有测试会走到。
	case errors.Is(err, service.ErrFortuneCardsUnavailable):
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "福卡账户不可用")

	// —— 本服务自己的 bug：500 ——
	//
	// 参数被账户域判非法意味着我们传错了东西。**大声记日志**：这类 bug 不会自己消失，
	// 而用户那边只该看到一句「系统繁忙」。
	case errors.Is(err, service.ErrInvalidDeductRequest):
		slog.ErrorContext(r.Context(), "lottery service sent an invalid deduction request", "error", err)
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, fallback)

	default:
		slog.ErrorContext(r.Context(), "lottery request failed", "error", err, "fallback", fallback)
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, fallback)
	}
}

// derefString 把可空字符串列转成 dto 里的普通字符串。
//
// 空串而不是指针：dto 面向的是 JSON，`null` 与 `""` 在前端那一侧通常会被同一个「或空串」
// 的兜底写法吞掉，多一层指针只会让 services/lottery.ts 里多一堆 `?: string | null`。
func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// decodeStringList 解析 testimonial_images 那一列（JSONB 数组）。
//
// 解析失败回空切片而不是报错：这一列本轮**没有写入方**，恒为 `[]`；将来有了写入方之后，
// 一条读不懂的历史数据不该让整个中奖列表 500。
func decodeStringList(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return []string{}
	}
	var items []string
	if err := json.Unmarshal(raw, &items); err != nil {
		return []string{}
	}
	if items == nil {
		return []string{}
	}
	return items
}

// decodeObject 解析 metadata 那一列。
//
// 解析失败回空对象而不是 nil：前端按 eventType 分支去读它，`{}` 与 `null` 在那里是两种
// 取值方式（后者要先判空），而这一列的形状本来就不由本服务保证。
func decodeObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return map[string]any{}
	}
	if object == nil {
		return map[string]any{}
	}
	return object
}
