package controller

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/service"
)

// AdminLotteryController 是后台的抽奖接口。
//
// 四棵树：activations（开通）、campaigns（活动与奖池）、rounds（期次、开奖、作废）、
// wins（中奖记录）。权限码由装配处挂在每一条路由上——**能看 ≠ 能改 ≠ 能开奖**，
// 三个码分开（见 migrations/identity/022_lottery_admin.sql）。
type AdminLotteryController struct{ lottery *service.LotteryService }

const (
	adminActivationPath    = "/v1/admin/lottery/activations"
	adminCampaignPath      = "/v1/admin/lottery/campaigns"
	adminRoundPath         = "/v1/admin/lottery/rounds"
	adminDrawPath          = "/v1/admin/lottery/draws"
	adminWinPath           = "/v1/admin/lottery/wins"
	adminParticipationPath = "/v1/admin/lottery/participations"
)

func NewAdminLotteryController(lottery *service.LotteryService) *AdminLotteryController {
	return &AdminLotteryController{lottery: lottery}
}

// Activations 分发 /v1/admin/lottery/activations 这一棵树。
func (c *AdminLotteryController) Activations(w http.ResponseWriter, r *http.Request) {
	actor, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, adminActivationPath), "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		c.listActivations(w, r)
	case rest == "" && r.Method == http.MethodPost:
		c.activate(w, r, actor)
	case strings.HasSuffix(rest, "/status") && r.Method == http.MethodPost:
		c.updateActivationStatus(w, r, actor, strings.TrimSuffix(rest, "/status"))
	case r.Method == http.MethodGet:
		c.getActivation(w, r, rest)
	default:
		http.NotFound(w, r)
	}
}

// Campaigns 分发 /v1/admin/lottery/campaigns 这一棵树。
//
// 注册顺序从长到短（见 routes/admin.go）：/{id}/status 与 /{id}/prizes 必须能被识别成
// 动作，而不是被当成 id 是 "status" 的活动。
func (c *AdminLotteryController) Campaigns(w http.ResponseWriter, r *http.Request) {
	actor, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, adminCampaignPath), "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		c.listCampaigns(w, r)
	case rest == "" && r.Method == http.MethodPost:
		c.createCampaign(w, r, actor)
	case strings.HasSuffix(rest, "/status") && r.Method == http.MethodPost:
		c.setCampaignStatus(w, r, actor, strings.TrimSuffix(rest, "/status"))
	case strings.HasSuffix(rest, "/prizes") && r.Method == http.MethodGet:
		c.listPrizes(w, r, strings.TrimSuffix(rest, "/prizes"))
	case r.Method == http.MethodGet:
		c.getCampaign(w, r, rest)
	case r.Method == http.MethodPut:
		c.updateCampaign(w, r, actor, rest)
	default:
		http.NotFound(w, r)
	}
}

// Rounds 分发 /v1/admin/lottery/rounds 这一棵树。
func (c *AdminLotteryController) Rounds(w http.ResponseWriter, r *http.Request) {
	actor, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, adminRoundPath), "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		c.listRounds(w, r)
	case strings.HasSuffix(rest, "/draw") && r.Method == http.MethodPost:
		c.draw(w, r, actor, strings.TrimSuffix(rest, "/draw"))
	case strings.HasSuffix(rest, "/cancel") && r.Method == http.MethodPost:
		c.cancelRound(w, r, actor, strings.TrimSuffix(rest, "/cancel"))
	case r.Method == http.MethodGet:
		c.getRound(w, r, rest)
	default:
		http.NotFound(w, r)
	}
}

// Draws 读一次开奖记录。只用 GET。
//
// 与 GET /rounds/{id} 的区别是它多带了那次开奖抽出的中奖名单：期次详情回答「这一期开到
// 哪了」，开奖记录回答「这一次开出了谁」。
//
// 期次要单独读一次是因为 GetDraw 只回 draw 与中奖记录，而这几行的种子、算法、参与人数都在
// draw 上，期次号得从 round 拿。多一次按主键的点查换掉「在 controller 里拼一个假 round」，
// 后者会让 dto 里多出一个只有这里为真的空壳。
func (c *AdminLotteryController) Draws(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, adminDrawPath), "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	draw, winners, err := c.lottery.GetDraw(r.Context(), id)
	if err != nil {
		writeLotteryError(w, r, err, "failed to get the lottery draw")
		return
	}
	round, err := c.lottery.GetRound(r.Context(), draw.RoundID)
	if err != nil {
		writeLotteryError(w, r, err, "failed to get the lottery round of a draw")
		return
	}
	api.Success(w, drawResponse(&repository.DrawOutcome{
		Draw:    draw,
		Round:   round.Round,
		Winners: winners,
	}))
}

// Wins 分发 /v1/admin/lottery/wins 这一棵树。
func (c *AdminLotteryController) Wins(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, adminWinPath), "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		c.listWins(w, r)
	case r.Method == http.MethodGet:
		c.getWin(w, r, rest)
	default:
		http.NotFound(w, r)
	}
}

// Participations 读参与记录（只读，后台没有「替用户参与」这回事）。
func (c *AdminLotteryController) Participations(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	c.listParticipations(w, r, "")
}

// —— 开通 ——

func (c *AdminLotteryController) listActivations(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page, pageSize, ok, message := api.ParsePage(query.Get("page"), query.Get("pageSize"), dto.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return
	}
	rows, total, err := c.lottery.ListActivations(r.Context(), dto.ActivationQuery{
		LocationID: strings.TrimSpace(query.Get("locationId")),
		Status:     strings.TrimSpace(query.Get("status")),
		Page:       page,
		PageSize:   pageSize,
	})
	if err != nil {
		writeLotteryError(w, r, err, "failed to list lottery activations")
		return
	}
	api.Success(w, api.PageResponse{
		Items:    activationResponses(rows),
		Total:    int64(total),
		Page:     page,
		PageSize: pageSize,
	})
}

func (c *AdminLotteryController) activate(w http.ResponseWriter, r *http.Request, actor string) {
	var body dto.ActivateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidBody)
		return
	}
	row, err := c.lottery.Activate(r.Context(), body, &actor)
	if err != nil {
		writeLotteryError(w, r, err, "failed to activate lottery for the location")
		return
	}
	api.Created(w, activationResponse(row))
}

func (c *AdminLotteryController) getActivation(w http.ResponseWriter, r *http.Request, id string) {
	row, err := c.lottery.GetActivation(r.Context(), id)
	if err != nil {
		writeLotteryError(w, r, err, "failed to get lottery activation")
		return
	}
	api.Success(w, activationResponse(row))
}

func (c *AdminLotteryController) updateActivationStatus(w http.ResponseWriter, r *http.Request, actor, id string) {
	var body dto.UpdateActivationRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidBody)
		return
	}
	row, err := c.lottery.UpdateActivationStatus(r.Context(), id, body, &actor)
	if err != nil {
		writeLotteryError(w, r, err, "failed to update lottery activation status")
		return
	}
	api.Success(w, activationResponse(row))
}

// —— 活动与奖池 ——

func (c *AdminLotteryController) listCampaigns(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page, pageSize, ok, message := api.ParsePage(query.Get("page"), query.Get("pageSize"), dto.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return
	}
	rows, total, err := c.lottery.ListCampaigns(r.Context(), dto.CampaignQuery{
		ActivationID: strings.TrimSpace(query.Get("activationId")),
		LocationID:   strings.TrimSpace(query.Get("locationId")),
		MachineID:    strings.TrimSpace(query.Get("machineId")),
		Status:       strings.TrimSpace(query.Get("status")),
		Name:         strings.TrimSpace(query.Get("name")),
		Page:         page,
		PageSize:     pageSize,
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

func (c *AdminLotteryController) getCampaign(w http.ResponseWriter, r *http.Request, id string) {
	detail, err := c.lottery.GetCampaign(r.Context(), id)
	if err != nil {
		writeLotteryError(w, r, err, "failed to get lottery campaign")
		return
	}
	api.Success(w, campaignResponse(detail))
}

func (c *AdminLotteryController) createCampaign(w http.ResponseWriter, r *http.Request, actor string) {
	var body dto.CampaignRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidBody)
		return
	}
	detail, err := c.lottery.CreateCampaign(r.Context(), body, &actor)
	if err != nil {
		writeLotteryError(w, r, err, "failed to create lottery campaign")
		return
	}
	api.Created(w, campaignResponse(detail))
}

func (c *AdminLotteryController) updateCampaign(w http.ResponseWriter, r *http.Request, actor, id string) {
	var body dto.CampaignRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidBody)
		return
	}
	detail, err := c.lottery.UpdateCampaign(r.Context(), id, body, &actor)
	if err != nil {
		writeLotteryError(w, r, err, "failed to update lottery campaign")
		return
	}
	api.Success(w, campaignResponse(detail))
}

// setCampaignStatus 改活动的生命周期。请求体的形状与 dto.UpdateActivationRequest 一样
// （status + remark），所以直接复用它而不是再定义一个同形状的结构。
func (c *AdminLotteryController) setCampaignStatus(w http.ResponseWriter, r *http.Request, actor, id string) {
	var body dto.UpdateActivationRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidBody)
		return
	}
	detail, err := c.lottery.SetCampaignStatus(r.Context(), id, body.Status, &actor)
	if err != nil {
		writeLotteryError(w, r, err, "failed to update lottery campaign status")
		return
	}
	api.Success(w, campaignResponse(detail))
}

func (c *AdminLotteryController) listPrizes(w http.ResponseWriter, r *http.Request, campaignID string) {
	prizes, err := c.lottery.ListPrizes(r.Context(), campaignID)
	if err != nil {
		writeLotteryError(w, r, err, "failed to list lottery campaign prizes")
		return
	}
	api.Success(w, prizeListResponse(prizes))
}

// —— 期次与开奖 ——

func (c *AdminLotteryController) listRounds(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page, pageSize, ok, message := api.ParsePage(query.Get("page"), query.Get("pageSize"), dto.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return
	}
	rows, total, err := c.lottery.ListRounds(r.Context(), dto.RoundQuery{
		CampaignID: strings.TrimSpace(query.Get("campaignId")),
		Status:     strings.TrimSpace(query.Get("status")),
		Page:       page,
		PageSize:   pageSize,
	})
	if err != nil {
		writeLotteryError(w, r, err, "failed to list lottery rounds")
		return
	}
	api.Success(w, api.PageResponse{
		Items:    roundResponses(rows),
		Total:    int64(total),
		Page:     page,
		PageSize: pageSize,
	})
}

func (c *AdminLotteryController) getRound(w http.ResponseWriter, r *http.Request, id string) {
	row, err := c.lottery.GetRound(r.Context(), id)
	if err != nil {
		writeLotteryError(w, r, err, "failed to get lottery round")
		return
	}
	api.Success(w, roundResponse(row))
}

// draw 是人工开奖。权限码 lottery:draw 由装配处挂在这一条路由上，**只绑 super_admin**。
func (c *AdminLotteryController) draw(w http.ResponseWriter, r *http.Request, actor, roundID string) {
	var body dto.DrawRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidBody)
		return
	}
	outcome, err := c.lottery.DrawManually(r.Context(), roundID, body, &actor)
	if err != nil {
		writeLotteryError(w, r, err, "failed to draw the lottery round")
		return
	}
	if outcome.Draw == nil {
		// 零人参与：这一期按作废处理，没有开奖记录。回一句说清楚的话而不是一个空的开奖
		// 响应——「开奖成功但没有任何记录」会让管理员以为哪里出错了。
		api.Success(w, map[string]any{
			"roundId": roundID,
			"status":  outcome.Round.Status,
			"drawId":  "",
			"message": "本期无人参与，已直接作废并开出下一期",
		})
		return
	}
	api.Success(w, drawResponse(outcome))
}

func (c *AdminLotteryController) cancelRound(w http.ResponseWriter, r *http.Request, actor, roundID string) {
	var body dto.CancelRoundRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidBody)
		return
	}
	round, err := c.lottery.CancelRound(r.Context(), roundID, body, &actor)
	if err != nil {
		writeLotteryError(w, r, err, "failed to cancel the lottery round")
		return
	}
	api.Success(w, map[string]any{
		"roundId":      round.ID,
		"roundNo":      round.RoundNo,
		"status":       round.Status,
		"cancelReason": round.CancelReason,
	})
}

// —— 中奖 ——

func (c *AdminLotteryController) listWins(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page, pageSize, ok, message := api.ParsePage(query.Get("page"), query.Get("pageSize"), dto.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return
	}
	status := strings.TrimSpace(query.Get("status"))
	if status != "" && !isKnownWinStatus(status) {
		// 状态是枚举，写错一个字母会静默返回空列表——调用方会以为「这个状态下没有中奖」，
		// 而不是「我筛错了」。
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgStatusInvalid)
		return
	}
	wins, total, err := c.lottery.ListWins(r.Context(), dto.WinQuery{
		RoundID:    strings.TrimSpace(query.Get("roundId")),
		CampaignID: strings.TrimSpace(query.Get("campaignId")),
		UserID:     strings.TrimSpace(query.Get("userId")),
		Status:     status,
		ClaimNo:    strings.TrimSpace(query.Get("claimNo")),
		Page:       page,
		PageSize:   pageSize,
	})
	if err != nil {
		writeLotteryError(w, r, err, "failed to list lottery wins")
		return
	}
	api.Success(w, api.PageResponse{
		Items:    winResponses(wins),
		Total:    int64(total),
		Page:     page,
		PageSize: pageSize,
	})
}

func (c *AdminLotteryController) getWin(w http.ResponseWriter, r *http.Request, id string) {
	win, events, err := c.lottery.GetWin(r.Context(), id)
	if err != nil {
		writeLotteryError(w, r, err, "failed to get the lottery win")
		return
	}
	api.Success(w, dto.WinDetailResponse{
		Win:    winResponse(win),
		Events: winEventResponses(events),
	})
}

// —— 参与（只读） ——

func (c *AdminLotteryController) listParticipations(w http.ResponseWriter, r *http.Request, _ string) {
	query := r.URL.Query()
	page, pageSize, ok, message := api.ParsePage(query.Get("page"), query.Get("pageSize"), dto.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return
	}
	status := strings.TrimSpace(query.Get("status"))
	if status != "" && !isKnownParticipationStatus(status) {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgStatusInvalid)
		return
	}
	rows, total, err := c.lottery.ListParticipations(r.Context(), dto.ParticipationQuery{
		RoundID:    strings.TrimSpace(query.Get("roundId")),
		CampaignID: strings.TrimSpace(query.Get("campaignId")),
		UserID:     strings.TrimSpace(query.Get("userId")),
		Status:     status,
		Page:       page,
		PageSize:   pageSize,
	})
	if err != nil {
		writeLotteryError(w, r, err, "failed to list lottery participations")
		return
	}
	api.Success(w, api.PageResponse{
		Items:    participationResponses(rows),
		Total:    int64(total),
		Page:     page,
		PageSize: pageSize,
	})
}

// —— 枚举 ——

func isKnownWinStatus(status string) bool {
	switch status {
	case model.WinPending, model.WinClaimed, model.WinRedeemed,
		model.WinExpired, model.WinRevoked, model.WinSuperseded:
		return true
	default:
		return false
	}
}

func isKnownParticipationStatus(status string) bool {
	switch status {
	case model.ParticipationPending, model.ParticipationConfirmed,
		model.ParticipationFailed, model.ParticipationReversed:
		return true
	default:
		return false
	}
}
