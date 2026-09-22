package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/service"
)

// 这一组守的是「只读权限也能改模板状态」那个洞。
//
// 起因：/v1/admin/coupons/templates/{id}/status 挂在 templateDispatch 上，dispatch
// 见 GET 就把请求发给 coupon:read，而 controller 的 case "status" 原来不看方法、
// 解码 body 直接写库。于是只有只读权限的账号发一个带 JSON body 的 GET 就能停用模板。
//
// 修法：方法白名单落在 controller（理由见 template.go 里那段注释）。这里因此能从
// controller 这一层把整件事验完——不需要路由、不需要数据库，被验的就是「GET 到不了
// 写路径、写方法到得了」这一条。
//
// 断言用具名 mock 而不是真实的 postgres：用例要证明的是「repo 有没有被调用」，
// 调用了就是洞还在，至于写成功与否是 repository 集成测试的事。

// templateMethodRepo 顶替整个 repository：service.New 会把同一个仓库按
// CouponTemplateRepository / CouponTypeRepository / BatchRepository /
// UserCouponRepository / IdempotencyRepository 逐个做类型断言，所以这几个接口的
// 方法都得在（用不上的给一个能编译的桩即可）。
type templateMethodRepo struct {
	statusCalls []string
	auditCalls  []string
}

func (r *templateMethodRepo) SetTemplateStatus(_ context.Context, id, status string) (*model.CouponTemplate, error) {
	r.statusCalls = append(r.statusCalls, status)
	return &model.CouponTemplate{ID: id, Status: status}, nil
}

func (r *templateMethodRepo) AuditTemplate(_ context.Context, id, status, _, _ string) (*model.CouponTemplate, error) {
	r.auditCalls = append(r.auditCalls, status)
	return &model.CouponTemplate{ID: id, AuditStatus: status}, nil
}

func (r *templateMethodRepo) ReserveInventory(context.Context, string, int64) error { return nil }
func (r *templateMethodRepo) Redeem(context.Context, string, string, string) (*model.UserCoupon, error) {
	return nil, errors.New("unused")
}
func (r *templateMethodRepo) Create(context.Context, *model.IdempotencyKey) (bool, error) {
	return false, errors.New("unused")
}
func (r *templateMethodRepo) Find(context.Context, string, string) (*model.IdempotencyKey, error) {
	return nil, errors.New("unused")
}
func (r *templateMethodRepo) ListCouponTypes(context.Context) ([]*model.CouponType, error) {
	return nil, errors.New("unused")
}
func (r *templateMethodRepo) CreateCouponType(context.Context, *model.CouponType) (*model.CouponType, error) {
	return nil, errors.New("unused")
}
func (r *templateMethodRepo) UpdateCouponType(context.Context, *model.CouponType) (*model.CouponType, error) {
	return nil, errors.New("unused")
}
func (r *templateMethodRepo) ListTemplates(context.Context, dto.CouponTemplateQuery) ([]*model.CouponTemplate, int64, error) {
	return nil, 0, errors.New("unused")
}
func (r *templateMethodRepo) GetTemplate(context.Context, string) (*model.CouponTemplate, error) {
	return nil, errors.New("unused")
}
func (r *templateMethodRepo) CreateTemplate(context.Context, *model.CouponTemplate) (*model.CouponTemplate, error) {
	return nil, errors.New("unused")
}
func (r *templateMethodRepo) UpdateTemplate(context.Context, *model.CouponTemplate) (*model.CouponTemplate, error) {
	return nil, errors.New("unused")
}
func (r *templateMethodRepo) DeleteTemplate(context.Context, string) error {
	return errors.New("unused")
}
func (r *templateMethodRepo) TemplateStats(context.Context, string) (*repository.TemplateStats, error) {
	return nil, errors.New("unused")
}

func newTemplateMethodController(repo *templateMethodRepo) *AdminCouponController {
	return &AdminCouponController{coupons: service.New(repo, repo, repo)}
}

// GET 打 status：405，且**一次都没碰到仓库**。断言「没碰到仓库」才是这个用例的重点——
// 只断言 405 的话，把守卫挪到解码之后照样能过。
func TestTemplateStatusRejectsGETWithBody(t *testing.T) {
	r := &templateMethodRepo{}
	ctrl := newTemplateMethodController(r)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/coupons/templates/tpl-1/status", strings.NewReader(`{"status":"disabled"}`))
	ctrl.Templates(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405；body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "METHOD_NOT_ALLOWED") {
		t.Errorf("响应里没有 METHOD_NOT_ALLOWED：%s", rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Allow"), http.MethodPatch) {
		t.Errorf("Allow = %q, 应当告诉调用方可以用 PATCH", rec.Header().Get("Allow"))
	}
	if len(r.statusCalls) != 0 {
		t.Fatalf("GET 落到了写路径：SetTemplateStatus 被调了 %v", r.statusCalls)
	}
}

// HEAD 是 GET 的孪生，同一张嘴：它也必须被拒。
func TestTemplateStatusRejectsHEAD(t *testing.T) {
	r := &templateMethodRepo{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/v1/admin/coupons/templates/tpl-1/status", strings.NewReader(`{"status":"disabled"}`))
	newTemplateMethodController(r).Templates(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD status = %d, want 405", rec.Code)
	}
	if len(r.statusCalls) != 0 {
		t.Fatalf("HEAD 落到了写路径：SetTemplateStatus 被调了 %v", r.statusCalls)
	}
}

// 同一批：audit 也是写动作，GET 一样到不了。
func TestTemplateAuditRejectsGETWithBody(t *testing.T) {
	r := &templateMethodRepo{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/coupons/templates/tpl-1/audit", strings.NewReader(`{"status":"approved"}`))
	newTemplateMethodController(r).Templates(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET audit = %d, want 405；body = %s", rec.Code, rec.Body.String())
	}
	if len(r.auditCalls) != 0 {
		t.Fatalf("GET 落到了写路径：AuditTemplate 被调了 %v", r.auditCalls)
	}
}

// 正确的方法仍然能改：POST / PUT / PATCH 三个写动词都放行，状态真的传到了仓库，
// 回包里是改完的状态。
func TestTemplateStatusAcceptsWriteMethods(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			r := &templateMethodRepo{}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/v1/admin/coupons/templates/tpl-1/status", strings.NewReader(`{"status":"disabled"}`))
			newTemplateMethodController(r).Templates(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("%s status = %d, want 200；body = %s", method, rec.Code, rec.Body.String())
			}
			if len(r.statusCalls) != 1 || r.statusCalls[0] != "disabled" {
				t.Fatalf("%s 传到仓库的状态 = %v, want [disabled]", method, r.statusCalls)
			}
			if !strings.Contains(rec.Body.String(), `"status":"disabled"`) {
				t.Errorf("%s 的回包里没有改完的状态：%s", method, rec.Body.String())
			}
		})
	}
}

// 写方法但 body 不合法：仍然按原来的约定回 400，而不是被方法守卫顺手拦成 405。
// 这条钉住「守卫只认方法，不揽解码的活」。
func TestTemplateStatusWriteMethodStillValidatesBody(t *testing.T) {
	r := &templateMethodRepo{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/admin/coupons/templates/tpl-1/status", strings.NewReader(`{"status":`))
	newTemplateMethodController(r).Templates(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH + 坏 body = %d, want 400；body = %s", rec.Code, rec.Body.String())
	}
	if len(r.statusCalls) != 0 {
		t.Fatalf("坏 body 落到了写路径：%v", r.statusCalls)
	}
}

// GET 详情（同一个 handler 的读路径）不能被子路径守卫误伤——守卫是按 parts[1] 写的，
// 这里钉住它只关掉 audit/status 两个写口。
func TestTemplateDetailStillReadableWithGET(t *testing.T) {
	r := &templateMethodRepo{}
	rec := httptest.NewRecorder()
	// 详情走的是 len(parts)==1 那条分支，仓库返回 unused 错误 → 500；这里只关心
	// 「没有被 405 拦下」，404/500 都说明方法守卫没有误伤读路径。
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/coupons/templates/tpl-1", nil)
	newTemplateMethodController(r).Templates(rec, req)

	if rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("GET 详情被方法守卫误伤了：%s", rec.Body.String())
	}
}
