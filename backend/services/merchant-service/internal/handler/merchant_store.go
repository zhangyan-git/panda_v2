package handler

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/service"
)

// MerchantStoreHandler 是商户域的门店只读面。它只回商户自己的数据范围里那一部分，
// 而范围来自中间件放在 context 上的 StoreScope，不来自任何请求字段。
type MerchantStoreHandler struct {
	svc *service.MerchantStoreService
}

func NewMerchantStoreHandler(svc *service.MerchantStoreService) *MerchantStoreHandler {
	return &MerchantStoreHandler{svc: svc}
}

// writeMerchantStoreError 把越界与不存在并为同一个 404。
//
// 分成 403 与 404 就等于承认了「这个 id 存在，只是不属于你」——拿别人的 id 逐个
// 试一遍，响应码会把对方有哪些门店说出来。
func writeMerchantStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrStoreOutOfScope), errors.Is(err, pgx.ErrNoRows):
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "门店不存在")
	default:
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
	}
}

// List godoc
//
//	@Summary     商户端门店列表（只返回本账号数据范围内的门店）
//	@Tags        merchant-stores
//	@Produce     json
//	@Security    BearerAuth
//	@Param       name     query string false "门店名称模糊过滤"
//	@Param       status   query string false "状态过滤 active/disabled"
//	@Param       page     query int    false "页码，从 1 开始，默认 1"
//	@Param       pageSize query int    false "每页条数，1..200，默认 20"
//	@Success     200 {object} api.Response{data=api.PageResponse{items=[]storeResponse}}
//	@Failure     400 {object} api.Response
//	@Failure     503 {object} api.Response "实时授权服务不可用"
//	@Router      /v1/merchant/stores [get]
func (h *MerchantStoreHandler) List(w http.ResponseWriter, r *http.Request) {
	scope, ok := auth.StoreScopeFromRequest(r)
	if !ok {
		// 走到这里说明这条路由没挂商户鉴权中间件。没有范围不等于没有边界。
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "商户鉴权暂不可用")
		return
	}
	q := r.URL.Query()
	// merchantId 不在这里读：查询串能指定商户就等于把数据范围交给了调用方。
	// 其余两项是普通的展示性过滤，跟范围叠加，取交集。
	f := repository.StoreFilter{
		Name:   q.Get("name"),
		Status: q.Get("status"),
	}
	page, pageSize, ok, msg := api.ParsePage(q.Get("page"), q.Get("pageSize"), api.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, msg)
		return
	}
	stores, total, err := h.svc.List(r.Context(), scope, f, page, pageSize)
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
//	@Summary     商户端门店详情（范围外与不存在都返回 404）
//	@Tags        merchant-stores
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id path string true "门店ID"
//	@Success     200 {object} api.Response{data=storeResponse}
//	@Failure     404 {object} api.Response
//	@Failure     503 {object} api.Response "实时授权服务不可用"
//	@Router      /v1/merchant/stores/{id} [get]
func (h *MerchantStoreHandler) Get(w http.ResponseWriter, r *http.Request) {
	scope, ok := auth.StoreScopeFromRequest(r)
	if !ok {
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "商户鉴权暂不可用")
		return
	}
	s, err := h.svc.Get(r.Context(), scope, pathVar(r, "id"))
	if err != nil {
		writeMerchantStoreError(w, err)
		return
	}
	api.Success(w, toStoreResponse(s))
}
