package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/service"
)

// MiniAppLotteryController 是小程序端的抽奖接口。
//
// # 归属
//
// 这一棵树上**每一个** user_id 都来自令牌，没有任何一个来自查询串或请求体。四棵树都是
// 这样，包括「我的参与」「我的中奖」的列表：查询串里的 userId 一律不看，因为看它就等于
// 让任何人改一个参数去读别人的中奖记录。
//
// 单条记录（中奖详情）多一道比较：拿到的记录不是自己的就当它不存在（404 而不是 403）。
// 回 403 等于告诉对方「这条记录是存在的，只是不归你」，而凭证号这种东西的存在性本身就是
// 信息。
type MiniAppLotteryController struct{ lottery *service.LotteryService }

const (
	miniappCampaignPath      = "/v1/miniapp/lottery/campaigns"
	miniappRoundPath         = "/v1/miniapp/lottery/rounds"
	miniappParticipationPath = "/v1/miniapp/lottery/participations"
	miniappWinPath           = "/v1/miniapp/lottery/wins"
)

func NewMiniAppLotteryController(lottery *service.LotteryService) *MiniAppLotteryController {
	return &MiniAppLotteryController{lottery: lottery}
}

// Campaigns 分发 /v1/miniapp/lottery/campaigns 这一棵树。
func (c *MiniAppLotteryController) Campaigns(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireConsumer(w, r)
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, miniappCampaignPath), "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		c.listCampaigns(w, r)
	case r.Method == http.MethodGet:
		c.center(w, r, rest, userID)
	default:
		http.NotFound(w, r)
	}
}

// Rounds 分发 /v1/miniapp/lottery/rounds 这一棵树。
//
// 只有一条：`POST /{id}/participations`。期次本身不给小程序单独读接口——用户看的进度在
// 抽奖中心的响应里，单独开一个 GET 只会让前端多一次往返去问同一件事。
func (c *MiniAppLotteryController) Rounds(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireConsumer(w, r)
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, miniappRoundPath), "/")
	if strings.HasSuffix(rest, "/participations") && r.Method == http.MethodPost {
		c.participate(w, r, userID, strings.TrimSuffix(rest, "/participations"))
		return
	}
	http.NotFound(w, r)
}

// Participations 读「我的参与」。
//
// 只有列表没有详情：参与记录的全部内容列表里都有，而用户点进去想看的是「我中了什么」，
// 那是中奖详情的事。两个入口指向同一堆数据会让前端不知道该刷新哪一个。
func (c *MiniAppLotteryController) Participations(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireConsumer(w, r)
	if !ok {
		return
	}
	if strings.Trim(strings.TrimPrefix(r.URL.Path, miniappParticipationPath), "/") != "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	c.listMyParticipations(w, r, userID)
}

// Wins 分发 /v1/miniapp/lottery/wins 这一棵树。
func (c *MiniAppLotteryController) Wins(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireConsumer(w, r)
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, miniappWinPath), "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		c.listMyWins(w, r, userID)
	case r.Method == http.MethodGet:
		c.getMyWin(w, r, rest, userID)
	default:
		http.NotFound(w, r)
	}
}

// —— 抽奖中心 ——

// listCampaigns 是抽奖中心的活动列表：这个门店（或这台设备）现在能参与哪些活动。
//
// **状态被强制成 enabled**，不接受查询串覆盖：draft 是运营还没写完的，paused 是被人停掉的，
// ended 是已经结束的——它们出现在小程序上只会让用户点进一个转不动的页面。后台那条路
// （admin 的 listCampaigns）不强制，因为它就是要把各种状态都列出来给运营看。
func (c *MiniAppLotteryController) listCampaigns(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page, pageSize, ok, message := api.ParsePage(query.Get("page"), query.Get("pageSize"), dto.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return
	}
	rows, total, err := c.lottery.ListCampaigns(r.Context(), dto.CampaignQuery{
		LocationID: strings.TrimSpace(query.Get("locationId")),
		MachineID:  strings.TrimSpace(query.Get("machineId")),
		Status:     model.CampaignEnabled,
		Page:       page,
		PageSize:   pageSize,
	})
	if err != nil {
		writeLotteryError(w, r, err, "failed to list lottery campaigns")
		return
	}
	api.Success(w, api.PageResponse{
		Items:    campaignSummaryResponses(rows),
		Total:    int64(total),
		Page:     page,
		PageSize: pageSize,
	})
}

// center 是抽奖中心那一页：活动 + 进行中的期次 + 我的进度 + 我的福卡余额。
func (c *MiniAppLotteryController) center(w http.ResponseWriter, r *http.Request, campaignID, userID string) {
	center, err := c.lottery.CampaignCenter(r.Context(), campaignID, userID)
	if err != nil {
		writeLotteryError(w, r, err, "failed to get the lottery campaign center")
		return
	}
	response := dto.CampaignCenterResponse{Campaign: campaignResponse(center.Detail)}
	if center.Round != nil {
		response.Round = &dto.RoundProgress{
			RoundID:              center.Round.ID,
			RoundNo:              center.Round.RoundNo,
			Status:               center.Round.Status,
			ParticipantCount:     center.Round.ParticipantCount,
			ParticipantTarget:    center.Round.ParticipantTarget,
			EndsAt:               center.Round.EndsAt,
			MyParticipationCount: center.MyParticipationCount,
			MyFortuneCardBalance: center.Balance,
			BalanceUnavailable:   center.BalanceUnavailable,
		}
	}
	api.Success(w, response)
}

// —— 参与 ——

// participate 是「用福卡参与抽奖」。
//
// # 幂等键
//
// 请求体带 sourceOrderId 时服务端从订单号派生键，请求头被忽略（同一张订单换个头就能参与
// 两次的话，「一笔订单只能参与一次」那条不变量就没了）。不带的必须给 `Idempotency-Key`，
// 服务端不替客户端生成——一个服务端生成的键在响应丢失之后没有任何用处，客户端重试会拿到
// 一个新的键，于是扣第二张卡。
//
// # 三个状态码
//
//   - 200：成交，或幂等键命中一次已经成交的参与（Replayed=true，**没有扣卡**）。
//   - 202：扣卡结果未知（超时 / 账户域不可达）。**不是失败**——用户的卡可能已经扣了，
//     也可能没有。修复 worker 会拿同一个 request_id 重跑，重跑安全（扣减幂等）。
//     body 里带着那条 pending 记录，用户可以据此在「我的参与」里找到它。
//   - 400：余额不足（业务码 INSUFFICIENT_FORTUNE_CARDS）或参数不合法。
func (c *MiniAppLotteryController) participate(w http.ResponseWriter, r *http.Request, userID, roundID string) {
	var body dto.ParticipateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid request body")
		return
	}
	result, err := c.lottery.Participate(r.Context(), roundID, userID, body,
		r.Header.Get("Idempotency-Key"))
	if err != nil {
		if errors.Is(err, service.ErrParticipationPending) && result != nil {
			api.Accepted(w, dto.ParticipateResponse{
				Participation: participationResponse(&repository.ParticipationListRow{
					Participation: result.Participation,
				}),
				Remaining: result.Remaining(),
				Replayed:  false,
			})
			return
		}
		writeLotteryError(w, r, err, "failed to participate in the lottery")
		return
	}
	api.Success(w, dto.ParticipateResponse{
		Participation: participationResponse(&repository.ParticipationListRow{
			Participation: result.Participation,
		}),
		Remaining: result.Remaining(),
		Replayed:  result.Replayed,
	})
}

// —— 我的参与 / 我的中奖 ——

func (c *MiniAppLotteryController) listMyParticipations(w http.ResponseWriter, r *http.Request, userID string) {
	query := r.URL.Query()
	page, pageSize, ok, message := api.ParsePage(query.Get("page"), query.Get("pageSize"), dto.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return
	}
	status := strings.TrimSpace(query.Get("status"))
	if status != "" && !isKnownParticipationStatus(status) {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "status is invalid")
		return
	}
	rows, total, err := c.lottery.ListParticipations(r.Context(), dto.ParticipationQuery{
		// UserID 来自令牌。查询串里的 userId 连读都不读——读它就是给自己留一个
		// 「以后有人顺手把它接上去」的口子。
		UserID:     userID,
		RoundID:    strings.TrimSpace(query.Get("roundId")),
		CampaignID: strings.TrimSpace(query.Get("campaignId")),
		Status:     status,
		Page:       page,
		PageSize:   pageSize,
	})
	if err != nil {
		writeLotteryError(w, r, err, "failed to list my lottery participations")
		return
	}
	api.Success(w, api.PageResponse{
		Items:    participationResponses(rows),
		Total:    int64(total),
		Page:     page,
		PageSize: pageSize,
	})
}

// listMyWins 是「我的中奖」，附带顶部那两个数（总数、待领取）。
//
// 两个数单独一次 COUNT 而不是让前端数当前页：列表是一页 20 条，而顶部要显示的是**总共**
// 中了几次——用当页条数去显示那个数字，第 2 页开始就错了。
func (c *MiniAppLotteryController) listMyWins(w http.ResponseWriter, r *http.Request, userID string) {
	query := r.URL.Query()
	page, pageSize, ok, message := api.ParsePage(query.Get("page"), query.Get("pageSize"), dto.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return
	}
	status := strings.TrimSpace(query.Get("status"))
	if status != "" && !isKnownWinStatus(status) {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "status is invalid")
		return
	}
	wins, total, err := c.lottery.ListWins(r.Context(), dto.WinQuery{
		UserID:   userID,
		RoundID:  strings.TrimSpace(query.Get("roundId")),
		Status:   status,
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		writeLotteryError(w, r, err, "failed to list my lottery wins")
		return
	}
	totalWins, pendingWins, err := c.lottery.CountWinsByUser(r.Context(), userID)
	if err != nil {
		writeLotteryError(w, r, err, "failed to count my lottery wins")
		return
	}
	// 内嵌 api.PageResponse 而不是再包一层：这一页的 items/total/page/pageSize 与全仓
	// 每一个列表接口逐字相同，前端的分页组件靠这个形状工作。summary 是这一页多出来的
	// 那件事，所以它在顶层多一个键，而不是把分页那四个数挪进一个子对象。
	api.Success(w, struct {
		api.PageResponse
		Summary dto.MyWinSummary `json:"summary"`
	}{
		PageResponse: api.PageResponse{
			Items:    winResponses(wins),
			Total:    int64(total),
			Page:     page,
			PageSize: pageSize,
		},
		Summary: dto.MyWinSummary{Total: totalWins, Pending: pendingWins},
	})
}

// getMyWin 是中奖详情：记录本身 + 它的流水。
func (c *MiniAppLotteryController) getMyWin(w http.ResponseWriter, r *http.Request, id, userID string) {
	win, events, err := c.lottery.GetWin(r.Context(), id)
	if err != nil {
		writeLotteryError(w, r, err, "failed to get my lottery win")
		return
	}
	if win.UserID != userID {
		// 不是自己的，就当它不存在。回 403 会告诉对方这条记录是存在的。
		writeLotteryError(w, r, service.ErrWinNotFound, "failed to get my lottery win")
		return
	}
	api.Success(w, dto.WinDetailResponse{
		Win:    winResponse(win),
		Events: winEventResponses(events),
	})
}
