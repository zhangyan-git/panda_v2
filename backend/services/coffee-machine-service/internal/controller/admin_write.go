package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/service"
)

// AdminWriteController 提供后台的设备域主数据写接口。
type AdminWriteController struct{ admin *service.AdminService }

func NewAdminWriteController(admin *service.AdminService) *AdminWriteController {
	return &AdminWriteController{admin: admin}
}

// ============================================================
// 厂商
// ============================================================

// CreateManufacturer 处理 POST /v1/admin/coffee-machines/manufacturers。
func (c *AdminWriteController) CreateManufacturer(w http.ResponseWriter, r *http.Request) {
	var in dto.ManufacturerInput
	if err := decodeJSON(r, &in); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}
	manufacturer, err := c.admin.CreateManufacturer(r.Context(), in)
	if err != nil {
		writeAdminError(w, err, "创建厂商失败")
		return
	}
	api.Created(w, dto.ManufacturerSummary{
		ID:           manufacturer.ID,
		Code:         manufacturer.Code,
		Name:         manufacturer.Name,
		ContactName:  manufacturer.ContactName,
		ContactPhone: manufacturer.ContactPhone,
		Status:       manufacturer.Status,
	})
}

// UpdateManufacturer 处理 PUT /v1/admin/coffee-machines/manufacturers/{id}。
//
// 入参里没有 code：编码是稳定业务标识，建好之后不可修改。少了这个字段，前端也就
// 不会做出一个点了会报错的输入框。
func (c *AdminWriteController) UpdateManufacturer(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in dto.ManufacturerInput
	if err := decodeJSON(r, &in); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}
	if _, err := c.admin.UpdateManufacturer(r.Context(), id, in); err != nil {
		writeAdminError(w, err, "修改厂商失败")
		return
	}
	api.NoContent(w)
}

func (c *AdminWriteController) SetManufacturerStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	in, ok := decodeStatus(w, r)
	if !ok {
		return
	}
	if err := c.admin.SetManufacturerStatus(r.Context(), id, in.Status); err != nil {
		writeAdminError(w, err, "修改厂商状态失败")
		return
	}
	api.NoContent(w)
}

// ============================================================
// 设备
// ============================================================

// CreateDevice 处理 POST /v1/admin/coffee-machines/devices。
func (c *AdminWriteController) CreateDevice(w http.ResponseWriter, r *http.Request) {
	var in dto.DeviceInput
	if err := decodeJSON(r, &in); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}
	device, err := c.admin.CreateDevice(r.Context(), in)
	if err != nil {
		writeAdminError(w, err, "创建设备失败")
		return
	}
	api.Created(w, deviceDetail(device))
}

// UpdateDevice 处理 PUT /v1/admin/coffee-machines/devices/{id}。
//
// 改不到余额、状态，也改不到厂商同步写入的那些列——它们不在入参结构里。
func (c *AdminWriteController) UpdateDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in dto.DeviceInput
	if err := decodeJSON(r, &in); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}
	if _, err := c.admin.UpdateDevice(r.Context(), id, in); err != nil {
		writeAdminError(w, err, "修改设备失败")
		return
	}
	api.NoContent(w)
}

func (c *AdminWriteController) SetDeviceStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	in, ok := decodeStatus(w, r)
	if !ok {
		return
	}
	if err := c.admin.SetDeviceStatus(r.Context(), id, in.Status); err != nil {
		writeAdminError(w, err, "修改设备状态失败")
		return
	}
	api.NoContent(w)
}

// AdjustDeviceBalance 处理 POST /v1/admin/coffee-machines/devices/{id}/balance。
//
// 方案 11.6 L893 必审清单上的操作：余额、流水、审计事件在同一个事务里落。成功回
// 调整后的余额，前端不必再查一次。
func (c *AdminWriteController) AdjustDeviceBalance(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in dto.BalanceAdjustInput
	if err := decodeJSON(r, &in); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}
	balance, err := c.admin.AdjustBalance(r.Context(), id, in, operatorOf(r))
	if err != nil {
		writeAdminError(w, err, "调整余额失败")
		return
	}
	api.Success(w, dto.BalanceResult{CoffeeBalance: balance})
}

// ============================================================
// 饮品
// ============================================================

// CreateDrink 处理 POST /v1/admin/coffee-machines/drinks。
func (c *AdminWriteController) CreateDrink(w http.ResponseWriter, r *http.Request) {
	var in dto.DrinkInput
	if err := decodeJSON(r, &in); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}
	drink, err := c.admin.CreateDrink(r.Context(), in)
	if err != nil {
		writeAdminError(w, err, "创建饮品失败")
		return
	}
	api.Created(w, drinkSummary(drink))
}

// UpdateDrink 处理 PUT /v1/admin/coffee-machines/drinks/{id}。
func (c *AdminWriteController) UpdateDrink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in dto.DrinkUpdateInput
	if err := decodeJSON(r, &in); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	}
	if _, err := c.admin.UpdateDrink(r.Context(), id, in); err != nil {
		writeAdminError(w, err, "修改饮品失败")
		return
	}
	api.NoContent(w)
}

func (c *AdminWriteController) SetDrinkStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	in, ok := decodeStatus(w, r)
	if !ok {
		return
	}
	if err := c.admin.SetDrinkStatus(r.Context(), id, in.Status); err != nil {
		writeAdminError(w, err, "修改饮品状态失败")
		return
	}
	api.NoContent(w)
}

// ============================================================
// 公共辅助
// ============================================================

// pathID 取出并校验路径上的 {id}。非法 UUID 直接回 400——放进 SQL 就是一个
// invalid input syntax，那会以 500 的形式回给前端。路径变量存在 mux.Vars 里，
// Go 1.22 ServeMux 的 r.PathValue 在这套路由下取不到值。
func pathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := strings.TrimSpace(mux.Vars(r)["id"])
	if !validUUID(id) {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "id must be a UUID")
		return "", false
	}
	return id, true
}

func decodeStatus(w http.ResponseWriter, r *http.Request) (dto.StatusInput, bool) {
	var in dto.StatusInput
	if err := decodeJSON(r, &in); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return in, false
	}
	return in, true
}

// decodeJSON 把请求体读进 v，请求体过大或格式不对时返回可读的错误。
// DisallowUnknownFields 是有意的：字段名拼错时静默忽略，会变成一个「保存成功了但
// 什么都没变」的工单。
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20)) // 1 MiB
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("请求体不能为空")
		}
		return err
	}
	return nil
}

// operatorOf 取操作人的用户 ID。
//
// 只取 ID 不取名字：令牌里没有用户名（见 platform/audit 的 Entry 说明）。这个 ID 会
// 落到余额流水的 operator_id 和审计事件的 actor_id 上，展示名由读侧按 id 解析。
func operatorOf(r *http.Request) string {
	identity, ok := auth.IdentityFromRequest(r)
	if !ok {
		return ""
	}
	return identity.UserID
}

// writeAdminError 把服务层的错误翻成 HTTP 状态码。
//
// 默认落在 500 而不是 400：把基础设施故障误报成 400，调用方会以为改一下参数就行，
// 而反过来只是个难看的错误码。所以只有明确认识的输入错误才回 400。
func writeAdminError(w http.ResponseWriter, err error, internalMsg string) {
	switch {
	case errors.Is(err, repository.ErrManufacturerNotFound),
		errors.Is(err, repository.ErrDeviceNotFound),
		errors.Is(err, repository.ErrDrinkNotFound):
		api.Error(w, http.StatusNotFound, api.CodeNotFound, err.Error())
	case errors.Is(err, repository.ErrSerialTaken),
		errors.Is(err, repository.ErrManufacturerCodeTaken),
		errors.Is(err, repository.ErrDrinkOriginTaken):
		// 撞唯一键是「你现在改的东西和已有的冲突」，不是服务出错。
		api.Error(w, http.StatusConflict, api.CodeConflict, err.Error())
	case errors.Is(err, repository.ErrDuplicateRequest):
		// 同一个 requestId 又来了：上一次多半已经成功，前端只是没收到响应。
		// 回 409 而不是当作成功，让调用方自己去查流水，而不是在这里替它下结论。
		api.Error(w, http.StatusConflict, api.CodeConflict, "该 requestId 已经记过账")
	case errors.Is(err, repository.ErrManufacturerMissing),
		errors.Is(err, repository.ErrInsufficientBalance),
		errors.Is(err, repository.ErrBalanceOutOfRange):
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
	case service.IsValidationError(err):
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
	default:
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, internalMsg)
	}
}
