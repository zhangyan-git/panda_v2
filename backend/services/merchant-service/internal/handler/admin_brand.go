package handler

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/service"
)

// operatorOf 取当前登录管理员 ID，作为创建人/审核人留痕
func operatorOf(r *http.Request) string {
	identity, ok := auth.IdentityFromRequest(r)
	if !ok {
		return ""
	}
	return identity.UserID
}

// writeBrandError 品牌/门店业务错误 → HTTP 状态码
func writeBrandError(w http.ResponseWriter, err error, internalMsg string) {
	switch {
	case errors.Is(err, repository.ErrUnavailable):
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "服务暂不可用")
	case errors.Is(err, service.ErrBrandNameRequired),
		errors.Is(err, service.ErrStoreNameRequired),
		errors.Is(err, service.ErrBrandHasStores),
		errors.Is(err, service.ErrStoreBrandMismatch),
		errors.Is(err, service.ErrStoreMerchantImmutable),
		errors.Is(err, repository.ErrAuditNotPending):
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
	// 客户编码 / DMS 编码撞车是 409 而不是 400：请求本身是合法且完整的，是**当前数据**
	// 让它做不成——换一个编码就过了。400 会被读成「表单填错了」，而用户会去挨个字段找错。
	// 品牌重名同理：换个名字就过了。
	case errors.Is(err, repository.ErrCustomerCodeTaken),
		errors.Is(err, repository.ErrDMSCodeTaken),
		errors.Is(err, repository.ErrBrandNameTaken):
		api.Error(w, http.StatusConflict, api.CodeConflict, err.Error())
	case errors.Is(err, pgx.ErrNoRows):
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "数据不存在")
	default:
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, internalMsg)
	}
}

// ---------------------------------------------------------------------------
// 品牌
// ---------------------------------------------------------------------------

// AdminBrandHandler 平台侧品牌管理接口：CRUD + 状态 + 审核
type AdminBrandHandler struct {
	svc *service.AdminBrandService
}

func NewAdminBrandHandler(svc *service.AdminBrandService) *AdminBrandHandler {
	return &AdminBrandHandler{svc: svc}
}

type brandResponse struct {
	ID           string `json:"id"`
	MerchantID   string `json:"merchantId"`
	MerchantName string `json:"merchantName"`
	Name         string `json:"name"`
	Logo         string `json:"logo"`
	Banner       string `json:"banner"`
	Description  string `json:"description"`
	Status       string `json:"status"`
	AuditStatus  string `json:"auditStatus"`
	AuditRemark  string `json:"auditRemark"`
	AuditAt      string `json:"auditAt"`
	AuditBy      string `json:"auditBy"`
	Remark       string `json:"remark"`
	Visible      bool   `json:"visible"`
	Sort         int    `json:"sort"`
	CreatedAt    string `json:"createdAt"`
}

func toBrandResponse(b *model.Brand) brandResponse {
	auditAt := ""
	if b.AuditAt != nil {
		auditAt = b.AuditAt.Format("2006-01-02T15:04:05Z")
	}
	return brandResponse{
		ID:           b.ID,
		MerchantID:   b.MerchantID,
		MerchantName: b.MerchantName,
		Name:         b.Name,
		Logo:         b.Logo,
		Banner:       b.Banner,
		Description:  b.Description,
		Status:       b.Status,
		AuditStatus:  b.AuditStatus,
		AuditRemark:  b.AuditRemark,
		AuditAt:      auditAt,
		AuditBy:      b.AuditBy,
		Remark:       b.Remark,
		Visible:      b.Visible,
		Sort:         b.Sort,
		CreatedAt:    b.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}
}

// List godoc
//
//	@Summary     获取品牌列表（服务端分页；商户/名称模糊/状态/审核状态过滤，均可选）
//	@Tags        admin-brands
//	@Produce     json
//	@Security    BearerAuth
//	@Param       merchantId  query string false "商户ID过滤"
//	@Param       name        query string false "品牌名称模糊过滤"
//	@Param       status      query string false "状态过滤 active/disabled"
//	@Param       auditStatus query string false "审核状态过滤 pending/approved/rejected"
//	@Param       page        query int    false "页码，从 1 开始，默认 1"
//	@Param       pageSize    query int    false "每页条数，1..200，默认 20"
//	@Success     200 {object} api.Response{data=api.PageResponse{items=[]brandResponse}}
//	@Failure     400 {object} api.Response
//	@Router      /v1/admin/brands [get]
func (h *AdminBrandHandler) List(w http.ResponseWriter, r *http.Request) {
	f := repository.BrandFilter{
		MerchantID:  r.URL.Query().Get("merchantId"),
		Name:        r.URL.Query().Get("name"),
		Status:      r.URL.Query().Get("status"),
		AuditStatus: r.URL.Query().Get("auditStatus"),
	}
	page, pageSize, ok, msg := api.ParsePage(r.URL.Query().Get("page"), r.URL.Query().Get("pageSize"), api.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, msg)
		return
	}
	brands, total, err := h.svc.List(r.Context(), f, page, pageSize)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	resp := make([]brandResponse, len(brands))
	for i, b := range brands {
		resp[i] = toBrandResponse(b)
	}
	api.Success(w, api.PageResponse{Items: resp, Total: total, Page: page, PageSize: pageSize})
}

// Get godoc
//
//	@Summary     获取单个品牌
//	@Tags        admin-brands
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id path string true "品牌ID"
//	@Success     200 {object} api.Response{data=brandResponse}
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/brands/{id} [get]
func (h *AdminBrandHandler) Get(w http.ResponseWriter, r *http.Request) {
	b, err := h.svc.GetByID(r.Context(), pathVar(r, "id"))
	if err != nil {
		writeBrandError(w, err, "服务内部错误")
		return
	}
	api.Success(w, toBrandResponse(b))
}

// brandRequest 创建/编辑品牌；status 与审核字段不在 body 中，分别走 PATCH 接口
type brandRequest struct {
	MerchantID  string `json:"merchantId"`
	Name        string `json:"name"`
	Logo        string `json:"logo"`
	Banner      string `json:"banner"`
	Description string `json:"description"`
	Remark      string `json:"remark"`
	Visible     bool   `json:"visible"`
	Sort        int    `json:"sort"`
}

func (q brandRequest) toInput() service.BrandInput {
	return service.BrandInput{
		MerchantID:  q.MerchantID,
		Name:        q.Name,
		Logo:        q.Logo,
		Banner:      q.Banner,
		Description: q.Description,
		Remark:      q.Remark,
		Visible:     q.Visible,
		Sort:        q.Sort,
	}
}

// Create godoc
//
//	@Summary     创建品牌（状态 active，审核状态 pending 并生成待审核记录）
//	@Tags        admin-brands
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       body body brandRequest true "品牌信息"
//	@Success     200 {object} api.Response{data=brandResponse}
//	@Failure     400 {object} api.Response
//	@Router      /v1/admin/brands [post]
func (h *AdminBrandHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req brandRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if req.MerchantID == "" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "所属商户不能为空")
		return
	}
	b, err := h.svc.Create(r.Context(), req.toInput(), operatorOf(r))
	if err != nil {
		writeBrandError(w, err, "创建失败")
		return
	}
	api.Success(w, toBrandResponse(b))
}

// Update godoc
//
//	@Summary     编辑品牌（平台直接生效，同时留一条已通过的修改记录）
//	@Tags        admin-brands
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id   path string      true "品牌ID"
//	@Param       body body brandRequest true "品牌信息"
//	@Success     200 {object} api.Response{data=brandResponse}
//	@Failure     400 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/brands/{id} [put]
func (h *AdminBrandHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req brandRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	b, err := h.svc.Update(r.Context(), pathVar(r, "id"), req.toInput(), operatorOf(r))
	if err != nil {
		writeBrandError(w, err, "更新失败")
		return
	}
	api.Success(w, toBrandResponse(b))
}

// updateBrandStatusRequest 品牌状态（updateStatusRequest 已被 handler/user.go 占用）
type updateBrandStatusRequest struct {
	Status string `json:"status"` // active | disabled
}

// UpdateStatus godoc
//
//	@Summary     启用/禁用品牌
//	@Tags        admin-brands
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id   path string                      true "品牌ID"
//	@Param       body body updateBrandStatusRequest true "状态"
//	@Success     200 {object} api.Response
//	@Failure     400 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/brands/{id}/status [patch]
func (h *AdminBrandHandler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	var req updateBrandStatusRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if req.Status != "active" && req.Status != "disabled" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "status 只能为 active 或 disabled")
		return
	}
	if err := h.svc.UpdateStatus(r.Context(), pathVar(r, "id"), req.Status); err != nil {
		writeBrandError(w, err, "操作失败")
		return
	}
	api.Success(w, nil)
}

// auditRequest 审核请求（品牌/门店通用）
type auditRequest struct {
	Approve bool   `json:"approve"`
	Remark  string `json:"remark"`
}

// Audit godoc
//
//	@Summary     审核品牌（仅 pending 可通过或拒绝，同步落章最近一条待审核记录）
//	@Tags        admin-brands
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id   path string    true "品牌ID"
//	@Param       body body auditRequest true "审核结果"
//	@Success     200 {object} api.Response
//	@Failure     400 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/brands/{id}/audit [patch]
func (h *AdminBrandHandler) Audit(w http.ResponseWriter, r *http.Request) {
	var req auditRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if err := h.svc.Audit(r.Context(), pathVar(r, "id"), req.Approve, req.Remark, operatorOf(r)); err != nil {
		writeBrandError(w, err, "操作失败")
		return
	}
	api.Success(w, nil)
}

// Delete godoc
//
//	@Summary     删除品牌（名下存在门店时拒绝；指向它的账号范围回收为商户级）
//	@Tags        admin-brands
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id path string true "品牌ID"
//	@Success     200 {object} api.Response
//	@Failure     400 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Failure     503 {object} api.Response "账号范围回收或实时授权服务不可用，删除被阻止"
//	@Router      /v1/admin/brands/{id} [delete]
func (h *AdminBrandHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Delete(r.Context(), pathVar(r, "id")); err != nil {
		writeBrandError(w, err, "删除失败")
		return
	}
	api.Success(w, nil)
}

// ---------------------------------------------------------------------------
// 门店
// ---------------------------------------------------------------------------

// AdminStoreHandler 平台侧门店管理接口：CRUD + 状态 + 审核
