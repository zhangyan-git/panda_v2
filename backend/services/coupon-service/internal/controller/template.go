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

// templateWriteMethod 报告这个 HTTP 方法是不是「改数据」的方法。
//
// 白名单而不是「只要不是 GET」：HEAD 是 GET 的孪生（同一个 handler、Go 也允许它带
// body），DELETE / OPTIONS 更没人用来改状态。把允许的动词写死，就不用每加一个方法
// 再想一遍「还有哪个口子没堵上」。后台前端用的就是 PATCH
// （admin-web/src/services/coupon.ts 的 auditCouponTemplate / updateCouponTemplateStatus）。
func templateWriteMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	default:
		return false
	}
}

// templateMethodNotAllowed 用本仓的错误信封回 405，并在 Allow 头里列出能用的动词，
// 调用方不用去翻代码就知道该换成哪个方法。
func templateMethodNotAllowed(w http.ResponseWriter, allow ...string) {
	w.Header().Set("Allow", strings.Join(allow, ", "))
	api.Error(w, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed, "method not allowed")
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
		// 四个筛选与批次/用户券两个列表同形（空串 = 不筛）。在这之前 name/status/auditStatus
		// 一直没被读，界面上摆着却按了没反应：参数确实发出去了，只是服务端从头到尾只看
		// page/size。couponTypeCode 是后加的，给「只挑某一类券的模板」用（会员套餐表单挑
		// 会员价体验券），它让调用方不必先查一次要 coupon:type:manage 的类型列表。
		q := dto.CouponTemplateQuery{
			Page:           page,
			PageSize:       size,
			Name:           strings.TrimSpace(r.URL.Query().Get("name")),
			Status:         strings.TrimSpace(r.URL.Query().Get("status")),
			AuditStatus:    strings.TrimSpace(r.URL.Query().Get("auditStatus")),
			CouponTypeCode: strings.TrimSpace(r.URL.Query().Get("couponTypeCode")),
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
	// audit 与 status 都改数据：一个改审核结论、一个改模板状态。它们必须只走写方法，
	// GET 打过来要 405——**方法判断放在这里，不进 decodeBody**。
	//
	// 为什么不在 routes/admin.go 的 templateDispatch 里拦：dispatch 是按方法挑权限码的
	// （GET → coupon:read，其余 → coupon:template:manage），它并不区分 /status 是不是写动作；
	// 「哪几个子路径会写」这件事只在这里的 switch 里。守卫放在真正要写的那一层，上层怎么
	// 重挂路由都拦得住，也不必把子路径清单在 dispatch 和 controller 里各写一份、日后漂移。
	//
	// 修的是这个洞：dispatch 见 GET 就发 coupon:read，而 case "status" 原来不看方法，
	// 于是**只有只读权限的账号**发一个带 body 的
	// `GET /v1/admin/coupons/templates/{id}/status {"status":"disabled"}` 就能停用模板。
	// 同一件事 audit 也一样（那边落在 coupon:template:audit 上，同样不该由 GET 触发）。
	switch parts[1] {
	case "audit":
		if !templateWriteMethod(r.Method) {
			templateMethodNotAllowed(w, http.MethodPost, http.MethodPut, http.MethodPatch)
			return
		}
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
		if !templateWriteMethod(r.Method) {
			templateMethodNotAllowed(w, http.MethodPost, http.MethodPut, http.MethodPatch)
			return
		}
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
