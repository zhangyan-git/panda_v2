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
		errors.Is(err, service.ErrAuditNotPending):
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
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
//	@Summary     获取品牌列表（商户/名称模糊/状态/审核状态过滤，均可选）
//	@Tags        admin-brands
//	@Produce     json
//	@Security    BearerAuth
//	@Param       merchantId  query string false "商户ID过滤"
//	@Param       name        query string false "品牌名称模糊过滤"
//	@Param       status      query string false "状态过滤 active/disabled"
//	@Param       auditStatus query string false "审核状态过滤 pending/approved/rejected"
//	@Success     200 {object} api.Response{data=[]brandResponse}
//	@Router      /v1/admin/brands [get]
func (h *AdminBrandHandler) List(w http.ResponseWriter, r *http.Request) {
	f := repository.BrandFilter{
		MerchantID:  r.URL.Query().Get("merchantId"),
		Name:        r.URL.Query().Get("name"),
		Status:      r.URL.Query().Get("status"),
		AuditStatus: r.URL.Query().Get("auditStatus"),
	}
	brands, err := h.svc.List(r.Context(), f)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	resp := make([]brandResponse, len(brands))
	for i, b := range brands {
		resp[i] = toBrandResponse(b)
	}
	api.Success(w, resp)
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
type AdminStoreHandler struct {
	svc *service.AdminStoreService
}

func NewAdminStoreHandler(svc *service.AdminStoreService) *AdminStoreHandler {
	return &AdminStoreHandler{svc: svc}
}

type storeResponse struct {
	ID            string   `json:"id"`
	MerchantID    string   `json:"merchantId"`
	MerchantName  string   `json:"merchantName"`
	BrandID       string   `json:"brandId"`
	BrandName     string   `json:"brandName"`
	Name          string   `json:"name"`
	Logo          string   `json:"logo"`
	Photos        []string `json:"photos"`
	Province      string   `json:"province"`
	City          string   `json:"city"`
	District      string   `json:"district"`
	ProvinceCode  string   `json:"provinceCode"`
	CityCode      string   `json:"cityCode"`
	DistrictCode  string   `json:"districtCode"`
	Address       string   `json:"address"`
	Longitude     *float64 `json:"longitude"`
	Latitude      *float64 `json:"latitude"`
	Phone         string   `json:"phone"`
	ContactName   string   `json:"contactName"`
	ContactPhone  string   `json:"contactPhone"`
	Detail        string   `json:"detail"`
	BusinessHours string   `json:"businessHours"`
	Status        string   `json:"status"`
	AuditStatus   string   `json:"auditStatus"`
	AuditRemark   string   `json:"auditRemark"`
	AuditAt       string   `json:"auditAt"`
	AuditBy       string   `json:"auditBy"`
	Remark        string   `json:"remark"`
	Visible       bool     `json:"visible"`
	CreatedAt     string   `json:"createdAt"`
}

func toStoreResponse(s *model.Store) storeResponse {
	photos := s.Photos
	if photos == nil {
		photos = []string{}
	}
	auditAt := ""
	if s.AuditAt != nil {
		auditAt = s.AuditAt.Format("2006-01-02T15:04:05Z")
	}
	return storeResponse{
		ID:            s.ID,
		MerchantID:    s.MerchantID,
		MerchantName:  s.MerchantName,
		BrandID:       s.BrandID,
		BrandName:     s.BrandName,
		Name:          s.Name,
		Logo:          s.Logo,
		Photos:        photos,
		Province:      s.Province,
		City:          s.City,
		District:      s.District,
		ProvinceCode:  s.ProvinceCode,
		CityCode:      s.CityCode,
		DistrictCode:  s.DistrictCode,
		Address:       s.Address,
		Longitude:     s.Longitude,
		Latitude:      s.Latitude,
		Phone:         s.Phone,
		ContactName:   s.ContactName,
		ContactPhone:  s.ContactPhone,
		Detail:        s.Detail,
		BusinessHours: s.BusinessHours,
		Status:        s.Status,
		AuditStatus:   s.AuditStatus,
		AuditRemark:   s.AuditRemark,
		AuditAt:       auditAt,
		AuditBy:       s.AuditBy,
		Remark:        s.Remark,
		Visible:       s.Visible,
		CreatedAt:     s.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}
}

// List godoc
//
//	@Summary     获取门店列表（商户/品牌/名称模糊/状态/审核状态过滤，均可选）
//	@Tags        admin-stores
//	@Produce     json
//	@Security    BearerAuth
//	@Param       merchantId  query string false "商户ID过滤"
//	@Param       brandId     query string false "品牌ID过滤"
//	@Param       name        query string false "门店名称模糊过滤"
//	@Param       status      query string false "状态过滤 active/disabled"
//	@Param       auditStatus query string false "审核状态过滤 pending/approved/rejected"
//	@Success     200 {object} api.Response{data=[]storeResponse}
//	@Router      /v1/admin/stores [get]
func (h *AdminStoreHandler) List(w http.ResponseWriter, r *http.Request) {
	f := repository.StoreFilter{
		MerchantID:  r.URL.Query().Get("merchantId"),
		BrandID:     r.URL.Query().Get("brandId"),
		Name:        r.URL.Query().Get("name"),
		Status:      r.URL.Query().Get("status"),
		AuditStatus: r.URL.Query().Get("auditStatus"),
	}
	stores, err := h.svc.List(r.Context(), f)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	resp := make([]storeResponse, len(stores))
	for i, s := range stores {
		resp[i] = toStoreResponse(s)
	}
	api.Success(w, resp)
}

// Get godoc
//
//	@Summary     获取单个门店
//	@Tags        admin-stores
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id path string true "门店ID"
//	@Success     200 {object} api.Response{data=storeResponse}
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/stores/{id} [get]
func (h *AdminStoreHandler) Get(w http.ResponseWriter, r *http.Request) {
	s, err := h.svc.GetByID(r.Context(), pathVar(r, "id"))
	if err != nil {
		writeBrandError(w, err, "服务内部错误")
		return
	}
	api.Success(w, toStoreResponse(s))
}

// storeRequest 创建/编辑门店；status 与审核字段不在 body 中，分别走 PATCH 接口
type storeRequest struct {
	MerchantID string   `json:"merchantId"`
	BrandID    string   `json:"brandId"`
	Name       string   `json:"name"`
	Logo       string   `json:"logo"`
	Photos     []string `json:"photos"`
	Province   string   `json:"province"`
	City       string   `json:"city"`
	District   string   `json:"district"`
	// 三个编码与三个名称一起提交：级联选择器同时给出，服务端不做名字到编码的推算
	// （那需要一份区划主数据，本轮刻意不建）。历史自由文本回填不上时留空。
	ProvinceCode  string   `json:"provinceCode"`
	CityCode      string   `json:"cityCode"`
	DistrictCode  string   `json:"districtCode"`
	Address       string   `json:"address"`
	Longitude     *float64 `json:"longitude"`
	Latitude      *float64 `json:"latitude"`
	Phone         string   `json:"phone"`
	ContactName   string   `json:"contactName"`
	ContactPhone  string   `json:"contactPhone"`
	Detail        string   `json:"detail"`
	BusinessHours string   `json:"businessHours"`
	Remark        string   `json:"remark"`
	Visible       bool     `json:"visible"`
}

func (q storeRequest) toInput() service.StoreInput {
	return service.StoreInput{
		MerchantID:    q.MerchantID,
		BrandID:       q.BrandID,
		Name:          q.Name,
		Logo:          q.Logo,
		Photos:        q.Photos,
		Province:      q.Province,
		City:          q.City,
		District:      q.District,
		ProvinceCode:  q.ProvinceCode,
		CityCode:      q.CityCode,
		DistrictCode:  q.DistrictCode,
		Address:       q.Address,
		Longitude:     q.Longitude,
		Latitude:      q.Latitude,
		Phone:         q.Phone,
		ContactName:   q.ContactName,
		ContactPhone:  q.ContactPhone,
		Detail:        q.Detail,
		BusinessHours: q.BusinessHours,
		Remark:        q.Remark,
		Visible:       q.Visible,
	}
}

// Create godoc
//
//	@Summary     创建门店（状态 active，审核状态 pending 并生成待审核记录；品牌必须属于同一商户）
//	@Tags        admin-stores
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       body body storeRequest true "门店信息"
//	@Success     200 {object} api.Response{data=storeResponse}
//	@Failure     400 {object} api.Response
//	@Router      /v1/admin/stores [post]
func (h *AdminStoreHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req storeRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if req.MerchantID == "" || req.BrandID == "" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "所属商户与品牌不能为空")
		return
	}
	s, err := h.svc.Create(r.Context(), req.toInput(), operatorOf(r))
	if err != nil {
		writeBrandError(w, err, "创建失败")
		return
	}
	api.Success(w, toStoreResponse(s))
}

// Update godoc
//
//	@Summary     编辑门店（平台直接生效，同时留一条已通过的修改记录）
//	@Tags        admin-stores
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id   path string      true "门店ID"
//	@Param       body body storeRequest true "门店信息"
//	@Success     200 {object} api.Response{data=storeResponse}
//	@Failure     400 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/stores/{id} [put]
func (h *AdminStoreHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req storeRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if req.MerchantID == "" || req.BrandID == "" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "所属商户与品牌不能为空")
		return
	}
	s, err := h.svc.Update(r.Context(), pathVar(r, "id"), req.toInput(), operatorOf(r))
	if err != nil {
		writeBrandError(w, err, "更新失败")
		return
	}
	api.Success(w, toStoreResponse(s))
}

// updateStoreStatusRequest 门店状态
type updateStoreStatusRequest struct {
	Status string `json:"status"` // active | disabled
}

// UpdateStatus godoc
//
//	@Summary     启用/禁用门店
//	@Tags        admin-stores
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id   path string                      true "门店ID"
//	@Param       body body updateStoreStatusRequest true "状态"
//	@Success     200 {object} api.Response
//	@Failure     400 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/stores/{id}/status [patch]
func (h *AdminStoreHandler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	var req updateStoreStatusRequest
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

// Audit godoc
//
//	@Summary     审核门店（仅 pending 可通过或拒绝，同步落章最近一条待审核记录）
//	@Tags        admin-stores
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id   path string    true "门店ID"
//	@Param       body body auditRequest true "审核结果"
//	@Success     200 {object} api.Response
//	@Failure     400 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/stores/{id}/audit [patch]
func (h *AdminStoreHandler) Audit(w http.ResponseWriter, r *http.Request) {
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
//	@Summary     删除门店（指向它的账号范围回收为商户级）
//	@Tags        admin-stores
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id path string true "门店ID"
//	@Success     200 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Failure     503 {object} api.Response "账号范围回收或实时授权服务不可用，删除被阻止"
//	@Router      /v1/admin/stores/{id} [delete]
func (h *AdminStoreHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Delete(r.Context(), pathVar(r, "id")); err != nil {
		writeBrandError(w, err, "删除失败")
		return
	}
	api.Success(w, nil)
}
