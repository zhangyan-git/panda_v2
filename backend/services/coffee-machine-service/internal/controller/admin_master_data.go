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
func (c *AdminMasterDataController) Drinks(w http.ResponseWriter, r *http.Request) {
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
	drinks, total, err := c.master.ListDrinks(r.Context(), dto.DrinkQuery{
		Page:           page,
		PageSize:       size,
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
func (c *AdminMasterDataController) ListDevices(w http.ResponseWriter, r *http.Request) {
	manufacturerID := strings.TrimSpace(r.URL.Query().Get("manufacturerId"))
	storeID := strings.TrimSpace(r.URL.Query().Get("storeId"))
	for name, value := range map[string]string{"manufacturerId": manufacturerID, "storeId": storeID} {
		if value != "" && !validUUID(value) {
			api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, name+" must be a UUID")
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
		StoreID:        storeID,
		Status:         strings.TrimSpace(r.URL.Query().Get("status")),
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
func (c *AdminMasterDataController) ListDeviceDrinks(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := pathID(w, r)
	if !ok {
		return
	}
	relations, err := c.master.ListDeviceDrinks(r.Context(), deviceID)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "failed to list device drinks")
		return
	}
	items := make([]dto.DeviceDrinkSummary, 0, len(relations))
	for _, rel := range relations {
		items = append(items, dto.DeviceDrinkSummary{
			DeviceID:        rel.DeviceID,
			DrinkID:         rel.DrinkID,
			Enabled:         rel.Enabled,
			SortOrder:       rel.SortOrder,
			Price:           rel.Price,
			VipPrice:        rel.VipPrice,
			PickupCodePrice: rel.PickupCodePrice,
		})
	}
	api.Success(w, items)
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
