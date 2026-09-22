package controller

import (
	"context"
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
		LocationName:        row.LocationName,
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
		ID:                campaign.ID,
		ActivationID:      campaign.ActivationID,
		LocationID:        row.LocationID,
		LocationName:      row.LocationName,
		MachineID:         campaign.MachineID,
		Code:              campaign.Code,
		Name:              campaign.Name,
		ParticipantTarget: campaign.ParticipantTarget,
		Description:       campaign.Description,
		IsDefault:         campaign.IsDefault,
		Status:            campaign.Status,
		Prize:             prizeResponse(detail.Prize),
		LiveRoundID:       row.LiveRoundID,
		LiveRoundNo:       row.LiveRoundNo,
		LiveRoundSize:     row.LiveRoundSize,
		LiveRoundDone:     row.LiveRoundDone,
		RoundCount:        row.RoundCount,
		CreatedAt:         campaign.CreatedAt,
		UpdatedAt:         campaign.UpdatedAt,
	}
	return response
}

// campaignSummaryResponse 是列表页的活动形状：与详情同一个结构，只把奖品留空（nil）。
//
// 用同一个结构体而不是另开一个 Summary 类型：前端那一张表同时服务两处，两个类型意味着
// 两套 dataIndex，而它们的差别只有「有没有 prize」——那个差别用「空值」表达就够了。
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

// prizeResponse 翻一个奖品。nil（活动没有奖品行）原样回 nil——前端那张详情页按「还没有
// 奖品」渲染，而不是一个名字为空的奖品。
func prizeResponse(prize *model.CampaignPrize) *dto.CampaignPrizeResponse {
	if prize == nil {
		return nil
	}
	return &dto.CampaignPrizeResponse{
		ID:                prize.ID,
		Name:              prize.Name,
		CoverImage:        prize.CoverImage,
		PosterImage:       prize.PosterImage,
		ClaimInstructions: prize.ClaimInstructions,
	}
}

// prizeListResponse 翻 GET /campaigns/{id}/prizes 那条路由的数组。
//
// 那条路由没有调用方（详情接口已经带上 prize 了），留着是因为删一条公开接口不在这次改动
// 范围里。奖池只剩一行，它回的就是一个长度 0 或 1 的数组。
func prizeListResponse(prizes []*model.CampaignPrize) []dto.CampaignPrizeResponse {
	responses := make([]dto.CampaignPrizeResponse, 0, len(prizes))
	for _, prize := range prizes {
		if mapped := prizeResponse(prize); mapped != nil {
			responses = append(responses, *mapped)
		}
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
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, msgUnauthorized)
		return "", false
	}
	return identity.UserID, true
}

// —— 错误 ——

// 几个在请求解析阶段就要回给用户的中文句子。
//
// 抽成常量而不是在十来处各写一遍字面量：这些调用点本来就有同一个含义，而写十遍的结果一定
// 是改的时候漏掉两三处——后台是中文界面，漏掉的那几处就会弹英文（2026-09-15 之前它们就是
// 字面量英文 "invalid request body" / "status is invalid"）。
//
// 它们不进 userMessages：那张表按**错误值**索引，而这几句产生在错误值出现之前
// （query 里的 status 还没变成 ErrStatusInvalid 就被挡下了）。
const (
	msgInvalidBody   = "请求体格式不正确"
	msgStatusInvalid = "状态取值不合法"
	msgUnauthorized  = "未登录"
)

// userMessages 是「服务层的错误值 → 给用户看的那句话」。
//
// **为什么要有这张表**：服务层的错误串一律是英文（`errors.New("this location already has
// lottery enabled")`），这是 Go 的惯例，也让 grep 日志的语义保持清楚。但 admin-web 的
// requestErrorMessage 会**优先用后端返回的 errorMessage**，于是这些英文原样弹到了运营脸上
// ——2026-09-15 开通弹窗上就是一句「this location already has lottery enabled」。
//
// 所以人话在这一层决定：controller 是 HTTP 边界，也是唯一同时知道「这是哪一条错误」和
// 「这句话是给谁看的」的地方。原来的写法是 `api.Error(..., err.Error())`，等于把日志文案
// 直接当成产品文案。
//
// **这张表必须是全集**：漏一条不会报错，只会静默退回兜底那句话（日志里留一条 warn）。
// userMessages_test.go 拿 service 的几组错误逐个钉住，新增错误值忘了加表会红。
//
// 措辞对着「运营看到这句话之后该做什么」写，不是对着英文直译：
// 「这家门店已经开通了抽奖」比「location already activated」有用，
// 「这一期的状态已经变了，请刷新后重试」比「round changed」有用。
var userMessages = []struct {
	err  error
	text string
}{
	// —— 请求不合法（400）——
	{service.ErrLocationIDRequired, "请选择门店"},
	{service.ErrLocationIDInvalid, "门店 ID 不是合法的 UUID"},
	{service.ErrStatusRequired, "缺少状态"},
	{service.ErrStatusInvalid, "状态取值不合法"},
	{service.ErrCampaignIDRequired, "缺少活动 ID"},
	{service.ErrCampaignIDInvalid, "活动 ID 不是合法的 UUID"},
	{service.ErrMachineIDInvalid, "咖啡机 ID 不是合法的 UUID"},
	{service.ErrCampaignCodeRequired, "缺少活动短名"},
	{service.ErrCampaignCodeInvalid, "活动短名只能是 1-16 位大写字母或数字"},
	{service.ErrCampaignNameRequired, "缺少活动名称"},
	{service.ErrTargetNotPositive, "参与门槛必须大于 0"},
	{service.ErrPrizeIDInvalid, "奖品 ID 不是合法的 UUID"},
	{service.ErrPrizeNameRequired, "缺少奖品名称"},
	{service.ErrPrizeCoverRequired, "请上传奖品封面图"},
	{service.ErrOrderIDInvalid, "订单 ID 不是合法的 UUID"},
	{service.ErrRoundIDRequired, "缺少期次 ID"},
	{service.ErrRoundIDInvalid, "期次 ID 不是合法的 UUID"},
	{service.ErrReasonRequired, "请填写理由"},
	{service.ErrReasonTooLong, "理由不能超过 200 个字"},
	{service.ErrIdempotencyNeeded, "缺少订单号，也没有带 Idempotency-Key 请求头"},

	// —— 找不到（404）——
	{service.ErrActivationNotFound, "这家门店没有开通抽奖"},
	{service.ErrCampaignNotFound, "活动不存在"},
	{service.ErrRoundNotFound, "期次不存在"},
	{service.ErrParticipationNotFound, "参与记录不存在"},
	{service.ErrWinNotFound, "中奖记录不存在"},
	{service.ErrDrawNotFound, "开奖记录不存在"},
	// 门店不存在是「你选的那家店在门店库里查不到」——运营看到它该做的是刷新门店下拉，
	// 所以这句里要带上「重选」的动作，而不是干说一句「不存在」。
	{service.ErrStoreNotFound, "这家门店在门店库里不存在，请刷新后重新选择"},

	// —— 状态冲突（409）：请求本身没问题，是现在这个局面不接受它 ——
	{service.ErrLocationAlreadyActivated, "这家门店已经开通了抽奖"},
	{service.ErrCampaignCodeTaken, "活动短名已被别的活动占用"},
	{service.ErrDefaultCampaignExists, "这条开通记录已经有默认活动了"},
	{service.ErrRoundAlreadyLive, "这个活动已经有一期在收了"},
	{service.ErrRoundClosed, "这一期已经停止收人了"},
	{service.ErrRoundChanged, "这一期的状态已经变了，请刷新后重试"},
	{service.ErrRoundAlreadyDrawn, "这一期已经开过奖了"},
	{service.ErrRoundNotAwaitingDraw, "这一期当前不该开奖，请刷新后重试"},
	{service.ErrRoundCancelled, "这一期已经被作废"},
	{service.ErrRoundHasParticipations, "已经有参与者的一期不能作废"},
	{service.ErrIdempotencyKeyConflict, "这次参与和已有的一条参与记录撞了，请勿重复提交"},
	{service.ErrMachineMismatch, "这是设备级活动，参与时要指到那台咖啡机"},
	{service.ErrCampaignEnded, "这个活动已经结束了"},
	{service.ErrCampaignStatusTransition, "当前状态不允许这样切换"},
	{service.ErrParticipationFailed, "这次参与已经失败过了"},

	// —— 身份缺失（401）：userId / 操作人不在请求体里，它来自令牌 ——
	{service.ErrUserIDRequired, "未登录"},
	{service.ErrActorRequired, "未登录"},
}

// userMessage 取这条错误给用户看的那句话；第二个返回值是「表里有它」。
//
// 用 errors.Is 逐个比而不是 map[error]string：service 与 repository 会把错误包一层上下文
// 再往上返，包装之后两个值不再相等，map 查不到。
func userMessage(err error) (string, bool) {
	for _, entry := range userMessages {
		if errors.Is(err, entry.err) {
			return entry.text, true
		}
	}
	return "", false
}

// genericErrorMessage 是表里没有这条错误时的兜底。
//
// 它**故意不是 err.Error()**：兜底存在的意义就是守住「不把英文和内部报错弹给用户」这条
// 规矩。真走到这里说明表漏了一条，日志里有原文（下面记 warn）。
const genericErrorMessage = "操作失败，请稍后重试"

// userFacingMessage 取人话，取不到就兜底并记一条 warn。
//
// 记 warn 而不是 error：漏一条表不该让一次请求以 500 收场，那是两件事。但它在日志里必须
// 找得到——不然后台只会说「操作失败」，谁也看不出漏的是哪一条。
func userFacingMessage(ctx context.Context, err error) string {
	if text, ok := userMessage(err); ok {
		return text
	}
	slog.WarnContext(ctx, "lottery error has no user-facing message", "error", err)
	return genericErrorMessage
}

// 分组：writeLotteryError 按组映射 HTTP 状态，userMessages_test.go 按组核对人话表。
//
// 分成组而不是把 errors.Is 一条条写进 switch，是为了让「新增一条 404 错误」变成一处改动，
// 而不是两处（switch 一处、测试一处）——两处的写法漏过一次之后，测试就不再可信了。
var (
	// notFoundErrors：都是「你要的那个东西找不到」。
	//
	// 门店不存在与「开通记录不存在」同组：对调用方来说它们是同一件事。它与下面
	// storesUnavailableErrors 成对——把问不到说成不存在，会让运营反复重试一个合法的开通。
	notFoundErrors = []error{
		service.ErrActivationNotFound,
		service.ErrCampaignNotFound,
		service.ErrRoundNotFound,
		service.ErrParticipationNotFound,
		service.ErrWinNotFound,
		service.ErrDrawNotFound,
		service.ErrStoreNotFound,
	}

	// conflictErrors：请求本身没问题，是现在这个局面不接受它。
	conflictErrors = []error{
		service.ErrLocationAlreadyActivated,
		service.ErrCampaignCodeTaken,
		service.ErrDefaultCampaignExists,
		service.ErrRoundAlreadyLive,
		service.ErrRoundClosed,
		service.ErrRoundChanged,
		service.ErrRoundAlreadyDrawn,
		service.ErrRoundNotAwaitingDraw,
		service.ErrRoundCancelled,
		service.ErrRoundHasParticipations,
		service.ErrIdempotencyKeyConflict,
		service.ErrMachineMismatch,
		service.ErrCampaignEnded,
		service.ErrCampaignStatusTransition,
		service.ErrParticipationFailed,
	}

	// unauthorizedErrors：userId / 操作人不在请求体里，它来自令牌。走到这里说明装配处漏挂了
	// 认证中间件，不是用户填错了什么。
	unauthorizedErrors = []error{service.ErrUserIDRequired, service.ErrActorRequired}
)

// matches 判断 err 是不是这一组里的某一个。
func matches(err error, group []error) bool {
	for _, candidate := range group {
		if errors.Is(err, candidate) {
			return true
		}
	}
	return false
}

// writeLotteryError 把业务层的错误映射成 HTTP 响应。
//
// 顺序有讲究：**先认业务结论，再认请求不合法**。两边的错误值不重叠，所以顺序本身不影响
// 结果，但它决定了新增一个错误值时最先要问的问题——「这是一次说得清楚的结果，还是调用方
// 传错了？」。
//
// 回给用户的一律是中文（见 userMessages）。`fallback` 是**给日志的**：它是「这是哪一次
// 请求」的英文描述，配合 slog 里的 error 才能定位，它不进响应体。
func writeLotteryError(w http.ResponseWriter, r *http.Request, err error, fallback string) {
	switch {
	case service.IsValidationError(err):
		// 错误码用字面量 "INVALID_ARGUMENT"，与 order-service / coupon-service 一致
		// （platform/api 的常量表里没有它，那个表是给跨服务的通用码用的）。
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", userFacingMessage(r.Context(), err))

	// —— 找不到（404）——
	case matches(err, notFoundErrors):
		api.Error(w, http.StatusNotFound, api.CodeNotFound, userFacingMessage(r.Context(), err))

	// —— 状态冲突（409）——
	case matches(err, conflictErrors):
		api.Error(w, http.StatusConflict, api.CodeConflict, userFacingMessage(r.Context(), err))

	// —— 福卡不够：400 加一个**业务码** ——
	//
	// 它不是「参数写错了」（那是 INVALID_ARGUMENT），也不是冲突：这是用户看一眼就懂的
	// 结论，小程序据此弹「福卡不够了，去完成订单领卡」而不是「系统繁忙」。
	case errors.Is(err, service.ErrInsufficientFortuneCards):
		api.Error(w, http.StatusBadRequest, "INSUFFICIENT_FORTUNE_CARDS", "福卡不足")

	// —— 身份缺失（401）——
	case matches(err, unauthorizedErrors):
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "未登录")

	// —— 账户域不可用：503 而不是 500 ——
	//
	// 这是**依赖没接上**，重试（等它起来）有意义。本轮只有测试会走到。
	case errors.Is(err, service.ErrFortuneCardsUnavailable):
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "福卡账户不可用")

	// —— 商户域不可用：503 ——
	//
	// 开通前那次存在性检查没问出结果。**这一条必须是 503 而不是 404**：我们并不知道这家店
	// 存不存在，说「不存在」是在替商户域下一个我们证不出来的结论。
	case errors.Is(err, service.ErrStoresUnavailable):
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "门店信息不可用")

	// —— 本服务自己的 bug：500 ——
	//
	// 参数被账户域判非法意味着我们传错了东西。**大声记日志**：这类 bug 不会自己消失，
	// 而用户那边只该看到一句兜底话——`fallback` 只进日志，不进响应体。
	case errors.Is(err, service.ErrInvalidDeductRequest):
		slog.ErrorContext(r.Context(), "lottery service sent an invalid deduction request",
			"error", err, "op", fallback)
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, genericErrorMessage)

	default:
		slog.ErrorContext(r.Context(), "lottery request failed", "error", err, "op", fallback)
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, genericErrorMessage)
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
