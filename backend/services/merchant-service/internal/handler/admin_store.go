package handler

import (
	"net/http"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/service"
)

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
	// 订货系统 xlsx「客户」页三列（migrations/merchant）；列表与详情都回，出库单的门店下拉要用编码
	CustomerCode string `json:"customerCode"`
	DMSCode      string `json:"dmsCode"`
	CustomerType string `json:"customerType"`
	Status       string `json:"status"`
	AuditStatus  string `json:"auditStatus"`
	AuditRemark  string `json:"auditRemark"`
	AuditAt      string `json:"auditAt"`
	AuditBy      string `json:"auditBy"`
	Remark       string `json:"remark"`
	Visible      bool   `json:"visible"`
	CreatedAt    string `json:"createdAt"`
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
		CustomerCode:  s.CustomerCode,
		DMSCode:       s.DMSCode,
		CustomerType:  s.CustomerType,
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
//	@Summary     获取门店列表（服务端分页；商户/品牌/名称模糊/状态/审核状态过滤，均可选）
//	@Tags        admin-stores
//	@Produce     json
//	@Security    BearerAuth
//	@Param       merchantId  query string false "商户ID过滤"
//	@Param       brandId     query string false "品牌ID过滤"
//	@Param       name        query string false "门店名称模糊过滤"
//	@Param       status      query string false "状态过滤 active/disabled"
//	@Param       auditStatus query string false "审核状态过滤 pending/approved/rejected"
//	@Param       page        query int    false "页码，从 1 开始，默认 1"
//	@Param       pageSize    query int    false "每页条数，1..200，默认 20"
//	@Success     200 {object} api.Response{data=api.PageResponse{items=[]storeResponse}}
//	@Failure     400 {object} api.Response
//	@Router      /v1/admin/stores [get]
func (h *AdminStoreHandler) List(w http.ResponseWriter, r *http.Request) {
	f := repository.StoreFilter{
		MerchantID:  r.URL.Query().Get("merchantId"),
		BrandID:     r.URL.Query().Get("brandId"),
		Name:        r.URL.Query().Get("name"),
		Status:      r.URL.Query().Get("status"),
		AuditStatus: r.URL.Query().Get("auditStatus"),
	}
	page, pageSize, ok, msg := api.ParsePage(r.URL.Query().Get("page"), r.URL.Query().Get("pageSize"), api.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, msg)
		return
	}
	stores, total, err := h.svc.List(r.Context(), f, page, pageSize)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	resp := make([]storeResponse, len(stores))
	for i, s := range stores {
		resp[i] = toStoreResponse(s)
	}
	api.Success(w, api.PageResponse{Items: resp, Total: total, Page: page, PageSize: pageSize})
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
	// 客户三列。编码为空串表示「还没编码」，**不是**「清空」——库上两个部分唯一索引
	// 都是 WHERE 编码 <> ''，所以空串可以并存，而重复的非空编码会被唯一索引拦下。
	CustomerCode string `json:"customerCode"`
	DMSCode      string `json:"dmsCode"`
	CustomerType string `json:"customerType"`
	Remark       string `json:"remark"`
	Visible      bool   `json:"visible"`
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
		CustomerCode:  q.CustomerCode,
		DMSCode:       q.DMSCode,
		CustomerType:  q.CustomerType,
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
