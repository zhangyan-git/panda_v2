package controller

import (
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
)

// 店铺码会员活动的后台接口。单独一个文件，因为它是**另一棵树**：路径前缀与会员那棵树不同
// （/v1/admin/membership-campaigns），权限码也不同——看用 membership:read，增改与启停用
// membership:manage（见 routes/admin.go 的注册块）。
const adminCampaignsPath = "/v1/admin/membership-campaigns"

// Campaigns 分发 /v1/admin/membership-campaigns 这一棵树。
//
//	GET    ""             列表
//	POST   ""             新建（一律 draft）
//	GET    "{id}"         一场活动
//	PUT    "{id}"         改一场活动（**不改状态**）
//	POST   "{id}/status"  启停
//	GET    "{id}/claims"  领取记录（今天一定是空的）
//
// **没有 POST {id}/qrcode**：生成小程序码要走微信的 wxacode.getUnlimited，而 appid/secret 与
// token 缓存都在 user-service，本服务没有任何微信配置；码的唯一用途又是被扫，扫码入口在小程序端。
// 见 service/campaign.go 的开头。
//
// `status` 与 `claims` 必须先于 `{id}` 判：它们是固定字面量，掉进 `{id}` 那一支会拿 "claims"
// 去当 UUID 解析，结果是一个「这场活动不存在」的 404——一个能跑但说不通的答案。
func (c *AdminMembershipController) Campaigns(w http.ResponseWriter, r *http.Request) {
	rest := restOf(r.URL.Path, adminCampaignsPath)
	id, sub, _ := strings.Cut(rest, "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		c.listCampaigns(w, r)
	case rest == "" && r.Method == http.MethodPost:
		c.createCampaign(w, r)
	case id != "" && sub == "status" && r.Method == http.MethodPost:
		c.setCampaignStatus(w, r, id)
	case id != "" && sub == "claims" && r.Method == http.MethodGet:
		c.listCampaignClaims(w, r, id)
	case id != "" && sub == "" && r.Method == http.MethodGet:
		c.getCampaign(w, r, id)
	case id != "" && sub == "" && r.Method == http.MethodPut:
		c.updateCampaign(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (c *AdminMembershipController) listCampaigns(w http.ResponseWriter, r *http.Request) {
	page, pageSize, ok := pageParams(w, r, dto.MaxPageSize)
	if !ok {
		return
	}
	query := r.URL.Query()
	campaigns, total, err := c.membership.ListCampaigns(r.Context(), dto.CampaignQuery{
		Status:   strings.TrimSpace(query.Get("status")),
		Keyword:  strings.TrimSpace(query.Get("keyword")),
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		writeMembershipError(w, r, err, "failed to list campaigns")
		return
	}
	api.Success(w, api.PageResponse{
		Items:    campaigns,
		Total:    int64(total),
		Page:     page,
		PageSize: pageSize,
	})
}

func (c *AdminMembershipController) getCampaign(w http.ResponseWriter, r *http.Request, id string) {
	campaign, err := c.membership.GetCampaign(r.Context(), id)
	if err != nil {
		writeMembershipError(w, r, err, "failed to get campaign")
		return
	}
	api.Success(w, campaign)
}

// createCampaign 新建一场活动。
//
// 读操作不读身份，三个写操作都读：审计要记是谁配的、谁改的（见 repository.CreateCampaign
// 那一族的 recorder.Record）。
func (c *AdminMembershipController) createCampaign(w http.ResponseWriter, r *http.Request) {
	actor, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	var body dto.CampaignRequest
	if !decodeBody(w, r, &body) {
		return
	}
	campaign, err := c.membership.CreateCampaign(r.Context(), body, actor)
	if err != nil {
		writeMembershipError(w, r, err, "failed to create campaign")
		return
	}
	api.Success(w, campaign)
}

func (c *AdminMembershipController) updateCampaign(w http.ResponseWriter, r *http.Request, id string) {
	actor, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	var body dto.CampaignRequest
	if !decodeBody(w, r, &body) {
		return
	}
	campaign, err := c.membership.UpdateCampaign(r.Context(), id, body, actor)
	if err != nil {
		writeMembershipError(w, r, err, "failed to update campaign")
		return
	}
	api.Success(w, campaign)
}

func (c *AdminMembershipController) setCampaignStatus(w http.ResponseWriter, r *http.Request, id string) {
	actor, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	var body dto.CampaignStatusRequest
	if !decodeBody(w, r, &body) {
		return
	}
	campaign, err := c.membership.SetCampaignStatus(r.Context(), id, body.Status, actor)
	if err != nil {
		writeMembershipError(w, r, err, "failed to set campaign status")
		return
	}
	api.Success(w, campaign)
}

// listCampaignClaims 是一场活动的领取记录。
//
// **今天它一定返回空列表**：领取动作要用户扫门店里那张小程序码，而扫码入口在小程序端、那一端
// 这一轮不写。空态不是故障，页面上要这么说（见 service/campaign.go 的开头）。
func (c *AdminMembershipController) listCampaignClaims(w http.ResponseWriter, r *http.Request, id string) {
	page, pageSize, ok := pageParams(w, r, dto.MaxPageSize)
	if !ok {
		return
	}
	claims, total, err := c.membership.ListCampaignClaims(r.Context(), id, dto.CampaignClaimQuery{
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		writeMembershipError(w, r, err, "failed to list campaign claims")
		return
	}
	api.Success(w, api.PageResponse{
		Items:    claims,
		Total:    int64(total),
		Page:     page,
		PageSize: pageSize,
	})
}
