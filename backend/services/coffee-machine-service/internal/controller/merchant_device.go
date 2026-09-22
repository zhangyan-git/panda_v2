package controller

import (
	"errors"
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/service"
)

// MerchantDeviceController 是商户端的设备只读面。
type MerchantDeviceController struct {
	devices *service.MerchantDeviceService
}

func NewMerchantDeviceController(devices *service.MerchantDeviceService) *MerchantDeviceController {
	return &MerchantDeviceController{devices: devices}
}

// List 处理 GET /v1/merchant/devices。
//
// 查询串里只认 manufacturerId / status / keyword：门店那一项由中间件解析出来的数据
// 范围决定，请求里带了也不看。边界不是过滤条件，是这一屏存在的前提。
func (c *MerchantDeviceController) List(w http.ResponseWriter, r *http.Request) {
	scope, ok := auth.StoreScopeFromRequest(r)
	if !ok {
		// 没有边界就什么都不给：这一条是 fail-closed 的落点，漏挂中间件的路由必须
		// 返回 503，而不是把「没有范围」读成「全部」。
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "商户鉴权暂不可用")
		return
	}
	manufacturerID := strings.TrimSpace(r.URL.Query().Get("manufacturerId"))
	if manufacturerID != "" && !validUUID(manufacturerID) {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "manufacturerId must be a UUID")
		return
	}
	page, size, ok, message := api.ParsePage(r.URL.Query().Get("page"), r.URL.Query().Get("pageSize"), service.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, message)
		return
	}
	devices, total, err := c.devices.List(r.Context(), scope, service.MerchantDeviceQuery{
		ManufacturerID: manufacturerID,
		Status:         strings.TrimSpace(r.URL.Query().Get("status")),
		Keyword:        strings.TrimSpace(r.URL.Query().Get("keyword")),
		Page:           page,
		PageSize:       size,
	})
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "设备列表加载失败")
		return
	}
	items := make([]dto.DeviceSummary, 0, len(devices))
	for _, d := range devices {
		items = append(items, deviceSummary(d))
	}
	api.Success(w, api.PageResponse{Items: items, Total: total, Page: page, PageSize: size})
}

// Get 处理 GET /v1/merchant/devices/{id}。
//
// 越界与不存在都回 404，同一句话：范围外的那台设备对调用方而言就是不存在。
func (c *MerchantDeviceController) Get(w http.ResponseWriter, r *http.Request) {
	scope, ok := auth.StoreScopeFromRequest(r)
	if !ok {
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "商户鉴权暂不可用")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	device, err := c.devices.Get(r.Context(), scope, id)
	if errors.Is(err, service.ErrDeviceOutOfScope) || errors.Is(err, repository.ErrDeviceNotFound) {
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "设备不存在")
		return
	}
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "设备详情加载失败")
		return
	}
	// 详情形状与后台同一个 DTO：同一台设备在两个端上要显示的是同一组事实，两处各写
	// 一份只会让字段各自漂移。
	api.Success(w, deviceDetail(device))
}
