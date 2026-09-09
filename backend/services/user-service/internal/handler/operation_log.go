package handler

import (
	"net/http"
	"strconv"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/operationlog"
)

type OperationLogHandler struct{ service *operationlog.Service }

func NewOperationLogHandler(service *operationlog.Service) *OperationLogHandler {
	return &OperationLogHandler{service: service}
}

// List returns platform administrator business operation logs.
func (h *OperationLogHandler) List(w http.ResponseWriter, r *http.Request) {
	q := operationlog.Query{
		AdminUserID: r.URL.Query().Get("adminUserId"), Module: r.URL.Query().Get("module"),
		Action: r.URL.Query().Get("action"), TargetType: r.URL.Query().Get("targetType"),
		Result: r.URL.Query().Get("result"), MerchantID: r.URL.Query().Get("merchantId"),
	}
	q.Page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	q.PageSize, _ = strconv.Atoi(r.URL.Query().Get("pageSize"))
	items, total, err := h.service.List(r.Context(), q)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "查询操作日志失败")
		return
	}
	api.SuccessPage(w, items, int64(total))
}
