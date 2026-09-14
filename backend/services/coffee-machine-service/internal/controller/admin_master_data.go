// Package controller 是 coffee-machine-service 的 HTTP 适配层。
package controller

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/service"
)

// AdminMasterDataController 提供后台的设备域主数据读接口。
type AdminMasterDataController struct{ master *service.MasterDataService }

func NewAdminMasterDataController(master *service.MasterDataService) *AdminMasterDataController {
	return &AdminMasterDataController{master: master}
}

// Manufacturers 处理 GET /v1/admin/coffee-machines/manufacturers。
// 不分页：厂商是手动维护的个位数基础数据，前端所有下拉都要全集。
func (c *AdminMasterDataController) Manufacturers(w http.ResponseWriter, r *http.Request) {
	manufacturers, err := c.master.ListManufacturers(r.Context())
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "failed to list manufacturers")
		return
	}
	items := make([]dto.ManufacturerSummary, 0, len(manufacturers))
	for _, m := range manufacturers {
		items = append(items, dto.ManufacturerSummary{
			ID:           m.ID,
			Code:         m.Code,
			Name:         m.Name,
			ContactName:  m.ContactName,
			ContactPhone: m.ContactPhone,
			Status:       m.Status,
		})
	}
	api.Success(w, items)
}

// Drinks 处理 GET /v1/admin/coffee-machines/drinks。
//
// deviceId 是饮品管理页的「所属设备」筛选。校验格式的理由与 manufacturers 那一条
// 相同但更要紧：device_id 是 uuid 列，畸形值会让 PostgreSQL 在参数解析阶段报 22P02，
// 而这里本该回一句「这个 id 写错了」。
func (c *AdminMasterDataController) Drinks(w http.ResponseWriter, r *http.Request) {
	manufacturerID := strings.TrimSpace(r.URL.Query().Get("manufacturerId"))
	if manufacturerID != "" && !validUUID(manufacturerID) {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "manufacturerId must be a UUID")
		return
	}
	deviceID := strings.TrimSpace(r.URL.Query().Get("deviceId"))
	if deviceID != "" && !validUUID(deviceID) {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "deviceId must be a UUID")
		return
	}
	page, size, ok, message := api.ParsePage(r.URL.Query().Get("page"), r.URL.Query().Get("pageSize"), service.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, message)
		return
	}
	drinks, total, err := c.master.ListDrinks(r.Context(), dto.DrinkQuery{
		Page:           page,
		PageSize:       size,
		DeviceID:       deviceID,
		ManufacturerID: manufacturerID,
		Status:         strings.TrimSpace(r.URL.Query().Get("status")),
	})
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "failed to list drinks")
		return
	}
	items := make([]dto.DrinkSummary, 0, len(drinks))
	for _, d := range drinks {
		items = append(items, drinkSummary(d))
	}
	api.Success(w, api.PageResponse{Items: items, Total: total, Page: page, PageSize: size})
}

func drinkSummary(d *model.Drink) dto.DrinkSummary {
	return dto.DrinkSummary{
		ID:              d.ID,
		DeviceID:        d.DeviceID,
		ManufacturerID:  d.ManufacturerID,
		OriginID:        d.OriginID,
		ProductNum:      d.ProductNum,
		ProductName:     d.ProductName,
		EnName:          d.EnName,
		DrinkType:       d.DrinkType,
		ProductImg:      d.ProductImg,
		ProductDesc:     d.ProductDesc,
		Price:           d.Price,
		VipPrice:        d.VipPrice,
		PickupCodePrice: d.PickupCodePrice,
		Status:          d.Status,
		Sort:            d.Sort,
	}
}

// ListDevices 处理 GET /v1/admin/coffee-machines/devices。
//
// storeIds 是可重复参数（?storeIds=a&storeIds=b），不是逗号拼接的一串：门店 id 里
// 没有逗号，但拼串的方案一旦定下，将来任何带分隔符的值都得先想一遍转义。
func (c *AdminMasterDataController) ListDevices(w http.ResponseWriter, r *http.Request) {
	manufacturerID := strings.TrimSpace(r.URL.Query().Get("manufacturerId"))
	if manufacturerID != "" && !validUUID(manufacturerID) {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "manufacturerId must be a UUID")
		return
	}
	// 逐个校验再下传。SQL 那边用的是 store_id::text = ANY($2::text[])，本来就不会
	// 因为一个畸形值整条报错，校验是为了让「传错一个 id」当场变成 400，而不是安安静静
	// 筛出一个空列表——空列表看起来和「这个门店确实没设备」一模一样。
	storeIDs := r.URL.Query()["storeIds"]
	for i, raw := range storeIDs {
		storeIDs[i] = strings.TrimSpace(raw)
		if !validUUID(storeIDs[i]) {
			api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "storeIds must be UUIDs")
			return
		}
	}
	page, size, ok, message := api.ParsePage(r.URL.Query().Get("page"), r.URL.Query().Get("pageSize"), service.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, message)
		return
	}
	devices, total, err := c.master.ListDevices(r.Context(), dto.DeviceQuery{
		Page:           page,
		PageSize:       size,
		ManufacturerID: manufacturerID,
		StoreIDs:       storeIDs,
		Status:         strings.TrimSpace(r.URL.Query().Get("status")),
		Keyword:        strings.TrimSpace(r.URL.Query().Get("keyword")),
	})
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "failed to list devices")
		return
	}
	items := make([]dto.DeviceSummary, 0, len(devices))
	for _, d := range devices {
		items = append(items, deviceSummary(d))
	}
	api.Success(w, api.PageResponse{Items: items, Total: total, Page: page, PageSize: size})
}

// GetDevice 处理 GET /v1/admin/coffee-machines/devices/{id}。
func (c *AdminMasterDataController) GetDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	device, err := c.master.GetDevice(r.Context(), id)
	if errors.Is(err, repository.ErrDeviceNotFound) {
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "device not found")
		return
	}
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "failed to load device")
		return
	}
	api.Success(w, deviceDetail(device))
}

// ListDeviceDrinks 处理 GET /v1/admin/coffee-machines/devices/{id}/drinks。
//
// 设备不存在回 404 而不是空数组：详情页是一个带 id 的地址，那个 id 打错了要和
// 「这台设备确实一款饮品都没有」分得开——前者重试无用，后者是还没配饮品。
//
// 返回的形状与 GET /drinks 完全一致（同一行饮品，只是换了个入口）：设备详情页那一屏
// 要的就是这个列表，两处各写一份摘要结构只会让字段各自漂移。
func (c *AdminMasterDataController) ListDeviceDrinks(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := pathID(w, r)
	if !ok {
		return
	}
	drinks, err := c.master.ListDeviceDrinks(r.Context(), deviceID)
	if errors.Is(err, repository.ErrDeviceNotFound) {
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "device not found")
		return
	}
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "failed to list device drinks")
		return
	}
	items := make([]dto.DrinkSummary, 0, len(drinks))
	for _, d := range drinks {
		items = append(items, drinkSummary(d))
	}
	api.Success(w, items)
}

// ListDeviceBalanceEntries 处理 GET /v1/admin/coffee-machines/devices/{id}/balance。
//
// 设备不存在回 404 而不是空列表，理由同 ListDeviceDrinks：详情页上「这台设备不在了」
// 和「这台设备还没动过余额」要说的话不一样。
func (c *AdminMasterDataController) ListDeviceBalanceEntries(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := pathID(w, r)
	if !ok {
		return
	}
	page, size, ok, message := api.ParsePage(r.URL.Query().Get("page"), r.URL.Query().Get("pageSize"), service.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, message)
		return
	}
	entries, total, err := c.master.ListDeviceBalanceEntries(r.Context(), deviceID, page, size)
	if errors.Is(err, repository.ErrDeviceNotFound) {
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "device not found")
		return
	}
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "failed to list device balance entries")
		return
	}
	items := make([]dto.DeviceBalanceEntrySummary, 0, len(entries))
	for _, e := range entries {
		items = append(items, balanceEntrySummary(e))
	}
	api.Success(w, api.PageResponse{Items: items, Total: total, Page: page, PageSize: size})
}

func balanceEntrySummary(e *model.DeviceBalanceEntry) dto.DeviceBalanceEntrySummary {
	return dto.DeviceBalanceEntrySummary{
		ID:              e.ID,
		DeviceID:        e.DeviceID,
		Type:            e.Type,
		Amount:          e.Amount,
		BalanceBefore:   e.BalanceAfter - e.Amount,
		BalanceAfter:    e.BalanceAfter,
		ReversesEntryID: e.ReversesEntryID,
		ReferenceType:   e.ReferenceType,
		ReferenceID:     e.ReferenceID,
		RequestID:       e.RequestID,
		Remark:          e.Remark,
		OperatorID:      e.OperatorID,
		OperatorName:    e.OperatorName,
		CreatedAt:       e.CreatedAt,
	}
}

func deviceSummary(d *model.Device) dto.DeviceSummary {
	return dto.DeviceSummary{
		ID:               d.ID,
		SerialUnique:     d.SerialUnique,
		DeviceName:       d.DeviceName,
		ManufacturerID:   d.ManufacturerID,
		StoreID:          d.StoreID,
		Status:           d.Status,
		VendorOnline:     d.VendorOnline,
		LastSyncedAt:     d.LastSyncedAt,
		LastFaultCode:    d.LastFaultCode,
		LastFaultMessage: d.LastFaultMessage,
		LastActiveAt:     d.LastActiveAt,
		CreatedAt:        d.CreatedAt,
		UpdatedAt:        d.UpdatedAt,
	}
}

func deviceDetail(d *model.Device) dto.DeviceDetail {
	return dto.DeviceDetail{
		ID:                         d.ID,
		SerialUnique:               d.SerialUnique,
		DeviceName:                 d.DeviceName,
		ManufacturerID:             d.ManufacturerID,
		StoreID:                    d.StoreID,
		Status:                     d.Status,
		VendorOnline:               d.VendorOnline,
		LastSyncedAt:               d.LastSyncedAt,
		VersionNumber:              d.VersionNumber,
		AndroidVersion:             d.AndroidVersion,
		MainBoardVersion:           d.MainBoardVersion,
		LastFaultCode:              d.LastFaultCode,
		LastFaultMessage:           d.LastFaultMessage,
		LastFaultAt:                d.LastFaultAt,
		LastActiveAt:               d.LastActiveAt,
		PickupPassword:             d.PickupPassword,
		CoffeeBalance:              d.CoffeeBalance,
		ShowVip:                    d.ShowVip,
		EnableCouponVerification:   d.EnableCouponVerification,
		WarrantyEndAt:              d.WarrantyEndAt,
		QrcodeType:                 d.QrcodeType,
		RegularQrcodePaymentMethod: d.RegularQrcodePaymentMethod,
		CreatedAt:                  d.CreatedAt,
		UpdatedAt:                  d.UpdatedAt,
	}
}

func validUUID(value string) bool {
	_, err := uuid.Parse(value)
	return err == nil
}
