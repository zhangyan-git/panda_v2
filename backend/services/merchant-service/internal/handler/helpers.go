package handler

import (
	"encoding/json"
	"errors"
	"io"

	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/service"

	"github.com/gorilla/mux"
)

// pathVar 读取路径变量。kratos 的 Server.HandleFunc 走 gorilla/mux 路由，
// 路径变量存在 mux.Vars 中，Go 1.22 ServeMux 的 r.PathValue 取不到值。
func pathVar(r *http.Request, key string) string {
	return mux.Vars(r)[key]
}

// decodeJSON 将请求体反序列化为 v，body 过大或格式错误时返回 error
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20)) // 限制 1 MiB
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("请求体不能为空")
		}
		return err
	}
	return nil
}

func writeMerchantError(w http.ResponseWriter, err error, internalMsg string) {
	switch {
	case errors.Is(err, service.ErrMerchantNameRequired), errors.Is(err, service.ErrMerchantStatusTransition),
		errors.Is(err, service.ErrMerchantHasUsers), errors.Is(err, service.ErrMerchantHasBrandsOrStores):
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
	case errors.Is(err, pgx.ErrNoRows):
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "商户不存在")
	default:
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, internalMsg)
	}
}
