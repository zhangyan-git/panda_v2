package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/service"
)

func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func templateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidTemplate), errors.Is(err, service.ErrInvalidTemplateAmount), errors.Is(err, service.ErrInvalidAudit), errors.Is(err, service.ErrInvalidTemplateStatus):
		api.Error(w, 400, "INVALID_ARGUMENT", err.Error())
	case errors.Is(err, pgx.ErrNoRows):
		api.Error(w, 404, api.CodeNotFound, "template not found")
	default:
		api.Error(w, 500, "INTERNAL_ERROR", "template operation failed")
	}
}
func (c *AdminCouponController) Templates(w http.ResponseWriter, r *http.Request) {
	// 按路径段判断：mux 只做精确匹配，{id} 路由不会把 /stats 之类的后缀
	// 一起带进来，所以这里自己拆段。
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/admin/coupons/templates"), "/")
	if path == "" {
		if r.Method != "GET" {
			if r.Method != "POST" {
				http.NotFound(w, r)
				return
			}
			var req dto.TemplateRequest
			if decodeBody(w, r, &req) != nil {
				api.Error(w, 400, "INVALID_ARGUMENT", "invalid request body")
				return
			}
			id, _ := auth.IdentityFromRequest(r)
			out, err := c.coupons.CreateTemplate(r.Context(), req, id.Subject)
			if err != nil {
				templateError(w, err)
				return
			}
			api.Created(w, templateResponse(out))
			return
		}
		// 以前这里用 strconv.Atoi 忽略错误，非法值一路传给 service 由它兜底成
		// 默认页——但回显出去的仍是最初的入参，于是响应会声称「pageSize=500」
		// 而实际只返回了 20 条。改成和其他列表接口同一套解析：非法值直接 400，
		// 回显的一定是真正生效的值。
		page, size, ok, msg := api.ParsePage(r.URL.Query().Get("page"), r.URL.Query().Get("pageSize"), dto.MaxPageSize)
		if !ok {
			api.Error(w, 400, "INVALID_ARGUMENT", msg)
			return
		}
		// 三个筛选与批次/用户券两个列表同形（空串 = 不筛）。在这之前它们一直没被读，
		// 界面上摆着却按了没反应：参数确实发出去了，只是服务端从头到尾只看 page/size。
		q := dto.CouponTemplateQuery{
			Page:        page,
			PageSize:    size,
			Name:        strings.TrimSpace(r.URL.Query().Get("name")),
			Status:      strings.TrimSpace(r.URL.Query().Get("status")),
			AuditStatus: strings.TrimSpace(r.URL.Query().Get("auditStatus")),
		}
		items, total, err := c.coupons.ListTemplates(r.Context(), q)
		if err != nil {
			templateError(w, err)
			return
		}
		api.Success(w, api.PageResponse{Items: templateResponses(items), Total: total, Page: page, PageSize: size})
		return
	}
	parts := strings.Split(path, "/")
	id := parts[0]
	if len(parts) == 1 {
		switch r.Method {
		case "GET":
			out, err := c.coupons.GetTemplate(r.Context(), id)
			if err != nil {
				templateError(w, err)
				return
			}
			api.Success(w, templateResponse(out))
		case "PUT":
			var req dto.TemplateRequest
			if decodeBody(w, r, &req) != nil {
				api.Error(w, 400, "INVALID_ARGUMENT", "invalid request body")
				return
			}
			out, err := c.coupons.UpdateTemplate(r.Context(), id, req)
			if err != nil {
				templateError(w, err)
				return
			}
			api.Success(w, templateResponse(out))
		case "DELETE":
			if err := c.coupons.DeleteTemplate(r.Context(), id); err != nil {
				templateError(w, err)
				return
			}
			api.NoContent(w)
		default:
			http.NotFound(w, r)
		}
		return
	}
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	identity, _ := auth.IdentityFromRequest(r)
	switch parts[1] {
	case "audit":
		var req dto.AuditRequest
		if decodeBody(w, r, &req) != nil {
			api.Error(w, 400, "INVALID_ARGUMENT", "invalid request body")
			return
		}
		out, err := c.coupons.AuditTemplate(r.Context(), id, req, identity.Subject)
		if err != nil {
			templateError(w, err)
			return
		}
		api.Success(w, templateResponse(out))
	case "status":
		var req dto.StatusRequest
		if decodeBody(w, r, &req) != nil {
			api.Error(w, 400, "INVALID_ARGUMENT", "invalid request body")
			return
		}
		out, err := c.coupons.SetTemplateStatus(r.Context(), id, req)
		if err != nil {
			templateError(w, err)
			return
		}
		api.Success(w, templateResponse(out))
	case "stats":
		out, err := c.coupons.TemplateStats(r.Context(), id)
		if err != nil {
			templateError(w, err)
			return
		}
		api.Success(w, out)
	default:
		http.NotFound(w, r)
	}
}
