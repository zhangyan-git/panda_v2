package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

// AdminOperationLogHandler 后台的操作日志查询。只读：这张表是审计证据，
// 没有删除、没有清空，页面能做的只有筛选和翻页。
type AdminOperationLogHandler struct {
	svc *service.AdminOperationLogService
}

func NewAdminOperationLogHandler(svc *service.AdminOperationLogService) *AdminOperationLogHandler {
	return &AdminOperationLogHandler{svc: svc}
}

// adminOperationLogResponse 是日志列表的一行，也是详情抽屉能拿到的全部内容。
//
// 列表与详情用同一个结构、也只开一个接口：一条日志的字段总共就这么些，快照又已经
// 在写入时脱敏过，再按 id 分出一个详情接口，只会多一次往返和一份重复的结构体。
//
// beforeData / afterData 保持原始 JSON（没有快照时是 null），不在这里摊平成文本：
// 摊平意味着后端要认识每一种模块的字段名，而它本来的样子是「哪个模块写了什么」，
// 让界面去渲染更诚实。
type adminOperationLogResponse struct {
	ID            string          `json:"id"`
	AdminUserID   string          `json:"adminUserId"`
	AdminUsername string          `json:"adminUsername"`
	AdminName     string          `json:"adminName"`
	Module        string          `json:"module"`
	Action        string          `json:"action"`
	Operation     string          `json:"operation"`
	TargetType    string          `json:"targetType"`
	TargetID      string          `json:"targetId"`
	TargetName    string          `json:"targetName"`
	MerchantID    string          `json:"merchantId"`
	Result        string          `json:"result"`
	ErrorCode     string          `json:"errorCode"`
	ErrorMessage  string          `json:"errorMessage"`
	BeforeData    json.RawMessage `json:"beforeData"`
	AfterData     json.RawMessage `json:"afterData"`
	OccurredAt    string          `json:"occurredAt"`
}

func toAdminOperationLogResponse(l *model.AdminOperationLog) adminOperationLogResponse {
	return adminOperationLogResponse{
		ID:            l.ID,
		AdminUserID:   l.AdminUserID,
		AdminUsername: l.AdminUsername,
		AdminName:     l.AdminName,
		Module:        l.Module,
		Action:        l.Action,
		Operation:     l.Operation,
		TargetType:    l.TargetType,
		TargetID:      l.TargetID,
		TargetName:    l.TargetName,
		MerchantID:    l.MerchantID,
		Result:        l.Result,
		ErrorCode:     l.ErrorCode,
		ErrorMessage:  l.ErrorMessage,
		BeforeData:    l.BeforeData,
		AfterData:     l.AfterData,
		OccurredAt:    l.OccurredAt.Format(time.RFC3339),
	}
}

// operationLogQueryFrom 把查询参数收成一个查询条件。参数名与前端一一对应，
// 不在这里做取值校验——那是 service 的事，这里只管搬运。
func operationLogQueryFrom(r *http.Request) service.OperationLogQuery {
	q := r.URL.Query()
	return service.OperationLogQuery{
		Module:     q.Get("module"),
		Action:     q.Get("action"),
		Result:     q.Get("result"),
		Operator:   q.Get("operator"),
		Keyword:    q.Get("keyword"),
		TargetType: q.Get("targetType"),
		TargetID:   q.Get("targetId"),
		StartTime:  q.Get("startTime"),
		EndTime:    q.Get("endTime"),
	}
}

// writeOperationLogQueryError 回筛选条件相关的 400。每个错误值对应一种写错的参数，
// 各自的提示不同，合并成一个「参数错误」等于让调用方去猜是哪一项。
func writeOperationLogQueryError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, service.ErrOperationLogResultInvalid),
		errors.Is(err, service.ErrOperationLogTimeInvalid),
		errors.Is(err, service.ErrOperationLogTimeRangeInvalid),
		errors.Is(err, service.ErrOperationLogTargetIDInvalid),
		errors.Is(err, service.ErrOperationLogFilterTooLong):
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return true
	default:
		return false
	}
}

// List godoc
//
//	@Summary     获取后台操作日志列表（服务端分页）
//	@Description 按事件落库时间倒序。occurredAt 是审计事件被写入这张表的时间，不是操作发生的时刻；relay 积压补投时它整体晚于实际操作时间
//	@Tags        admin-operation-logs
//	@Produce     json
//	@Security    BearerAuth
//	@Param       page       query int    false "页码，从 1 开始，默认 1"
//	@Param       pageSize   query int    false "每页条数，1..200，默认 20"
//	@Param       module     query string false "模块精确筛选，取值见 /facets"
//	@Param       action     query string false "动作精确筛选，取值见 /facets"
//	@Param       result     query string false "结果筛选：success/failure"
//	@Param       operator   query string false "操作人用户名或姓名的前缀"
//	@Param       keyword    query string false "关键词：目标名称或操作描述的子串"
//	@Param       targetType query string false "目标类型精确筛选，如 device / merchant / role"
//	@Param       targetId   query string false "目标对象 id（UUID）。设备详情页用它只看这一台设备的日志"
//	@Param       startTime  query string false "起始时间（含），RFC3339"
//	@Param       endTime    query string false "结束时间（含），RFC3339"
//	@Success     200 {object} api.Response{data=api.PageResponse{items=[]adminOperationLogResponse}}
//	@Failure     400 {object} api.Response
//	@Router      /v1/admin/operation-logs [get]
func (h *AdminOperationLogHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, pageSize, ok, msg := api.ParsePage(q.Get("page"), q.Get("pageSize"), api.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, msg)
		return
	}
	logs, total, err := h.svc.List(r.Context(), operationLogQueryFrom(r), page, pageSize)
	switch {
	case writeOperationLogQueryError(w, err):
		return
	case err != nil:
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	items := make([]adminOperationLogResponse, len(logs))
	for i, l := range logs {
		items[i] = toAdminOperationLogResponse(l)
	}
	api.Success(w, api.PageResponse{Items: items, Total: total, Page: page, PageSize: pageSize})
}

type operationLogFacetsResponse struct {
	Modules []string `json:"modules"`
	Actions []string `json:"actions"`
}

// Facets godoc
//
//	@Summary     获取操作日志的模块与动作候选值
//	@Description 取自库中实际出现过的取值，供筛选下拉使用；没有日志时两个数组都为空
//	@Tags        admin-operation-logs
//	@Produce     json
//	@Security    BearerAuth
//	@Success     200 {object} api.Response{data=operationLogFacetsResponse}
//	@Router      /v1/admin/operation-logs/facets [get]
func (h *AdminOperationLogHandler) Facets(w http.ResponseWriter, r *http.Request) {
	facets, err := h.svc.Facets(r.Context())
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	api.Success(w, operationLogFacetsResponse{Modules: facets.Modules, Actions: facets.Actions})
}
