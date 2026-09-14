package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/service"
)

type AdminCouponController struct{ coupons *service.CouponService }

// maxCouponPageSize 是优惠券列表接口的 pageSize 上限。
//
// 比 api.MaxPageSize（200）紧一档，因为这里的上限本来就是 100、前端从未要过更大的页；
// 后台那几个「要全集」的下拉/Transfer 只出现在身份与商户模块，与优惠券无关。
// 收紧而不是放宽，是为了不改动任何已有调用方的可用范围。
const maxCouponPageSize = 100

func writeCouponMutationError(w http.ResponseWriter, err error, message string) {
	if errors.Is(err, service.ErrInvalidRequestID) || errors.Is(err, service.ErrInvalidCouponID) || errors.Is(err, service.ErrInvalidActorID) || errors.Is(err, service.ErrInvalidReason) {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	if errors.Is(err, service.ErrIdempotencyConflict) {
		api.Error(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", err.Error())
		return
	}
	if errors.Is(err, service.ErrInvalidCouponType) {
		api.Error(w, http.StatusConflict, "CONFLICT", err.Error())
		return
	}
	// 状态机拒绝（重复核销、过期券、非法撤销）是业务冲突，不是服务端故障：
	// 映射成 409，否则调用方只能看到一个无信息量的 500。
	if errors.Is(err, repository.ErrCouponNotRedeemable) {
		api.Error(w, http.StatusConflict, "CONFLICT", err.Error())
		return
	}
	if errors.Is(err, repository.ErrIdempotencyInProgress) {
		api.Error(w, http.StatusConflict, "IDEMPOTENCY_IN_PROGRESS", err.Error())
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "user coupon not found")
		return
	}
	api.Error(w, http.StatusInternalServerError, "INTERNAL_ERROR", message)
}

func (c *AdminCouponController) Batches(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/admin/coupons/batches")
	if path == "" || path == "/" {
		q := dto.CouponBatchQuery{TemplateID: strings.TrimSpace(r.URL.Query().Get("templateId")), Status: strings.TrimSpace(r.URL.Query().Get("status")), Source: strings.TrimSpace(r.URL.Query().Get("source")), BatchNo: strings.TrimSpace(r.URL.Query().Get("batchNo"))}
		page, size, ok, msg := api.ParsePage(r.URL.Query().Get("page"), r.URL.Query().Get("pageSize"), maxCouponPageSize)
		if !ok {
			api.Error(w, 400, "INVALID_ARGUMENT", msg)
			return
		}
		q.Page, q.PageSize = page, size
		if q.TemplateID != "" {
			if _, err := uuid.Parse(q.TemplateID); err != nil {
				api.Error(w, 400, "INVALID_ARGUMENT", "templateId must be a UUID")
				return
			}
		}
		items, total, err := c.coupons.ListBatches(r.Context(), q)
		if err != nil {
			api.Error(w, 500, "INTERNAL_ERROR", "failed to list coupon batches")
			return
		}
		api.Success(w, api.PageResponse{Items: batchResponses(items), Total: total, Page: q.Page, PageSize: q.PageSize})
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 1 && !(len(parts) == 2 && parts[1] == "stats") {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	if len(parts) == 2 && parts[1] == "stats" {
		if _, err := uuid.Parse(id); err != nil {
			api.Error(w, 400, "INVALID_ARGUMENT", "batch id must be a UUID")
			return
		}
		result, err := c.coupons.BatchStats(r.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			api.Error(w, 404, api.CodeNotFound, "coupon batch not found")
			return
		}
		if err != nil {
			api.Error(w, 500, "INTERNAL_ERROR", "failed to get coupon batch stats")
			return
		}
		api.Success(w, result)
		return
	}
	result, err := c.coupons.GetBatch(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		api.Error(w, 404, api.CodeNotFound, "coupon batch not found")
		return
	}
	if err != nil {
		api.Error(w, 500, "INTERNAL_ERROR", "failed to get coupon batch")
		return
	}
	api.Success(w, batchResponse(result))
}

func (c *AdminCouponController) UserCoupons(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/admin/coupons")
	p := strings.Split(strings.Trim(path, "/"), "/")
	// 路径按 "/" 切段（前缀已剥掉）：列表 1 段（user-coupons）、详情 2 段
	// （user-coupons/{id}）、统计与核销/撤销 3 段。判断分支必须按段数分别写，
	// 详情曾被误写成 3 段，导致详情恒定 404。
	if len(p) < 1 || p[0] != "user-coupons" {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet && len(p) == 1 {
		q := dto.UserCouponQuery{UserID: strings.TrimSpace(r.URL.Query().Get("userId")), Status: strings.TrimSpace(r.URL.Query().Get("status")), BatchID: strings.TrimSpace(r.URL.Query().Get("batchId")), TemplateID: strings.TrimSpace(r.URL.Query().Get("templateId"))}
		page, size, ok, msg := api.ParsePage(r.URL.Query().Get("page"), r.URL.Query().Get("pageSize"), maxCouponPageSize)
		if !ok {
			api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msg)
			return
		}
		q.Page, q.PageSize = page, size
		for name, value := range map[string]string{"userId": q.UserID, "batchId": q.BatchID, "templateId": q.TemplateID} {
			if value != "" {
				if _, err := uuid.Parse(value); err != nil {
					api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", name+" must be a UUID")
					return
				}
			}
		}
		if q.Status != "" {
			switch q.Status {
			case "claimed", "held", "redeemed", "expired", "refunded", "invalidated":
			default:
				api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "status is invalid")
				return
			}
		}
		items, total, e := c.coupons.ListUserCoupons(r.Context(), q)
		if e != nil {
			api.Error(w, 500, "INTERNAL_ERROR", "failed to list user coupons")
			return
		}
		api.Success(w, api.PageResponse{Items: userCouponResponses(items), Total: total, Page: q.Page, PageSize: q.PageSize})
		return
	}
	if r.Method == http.MethodGet && len(p) == 3 && p[1] == "stats" && p[0] == "user-coupons" {
		userID := strings.TrimSpace(p[2])
		if _, err := uuid.Parse(userID); err != nil {
			api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "user id must be a UUID")
			return
		}
		x, e := c.coupons.UserCouponStats(r.Context(), userID)
		if e != nil {
			api.Error(w, 500, "INTERNAL_ERROR", "failed to get user coupon stats")
			return
		}
		api.Success(w, x)
		return
	}
	// 详情是两段：user-coupons/{id}。上面 stats 分支已把三段路径摘走。
	if r.Method == http.MethodGet && len(p) == 2 {
		id := strings.TrimSpace(p[1])
		if _, err := uuid.Parse(id); err != nil {
			api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "user coupon id must be a UUID")
			return
		}
		x, e := c.coupons.GetUserCoupon(r.Context(), id)
		if errors.Is(e, pgx.ErrNoRows) {
			api.Error(w, http.StatusNotFound, api.CodeNotFound, "user coupon not found")
			return
		}
		if e != nil {
			api.Error(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to get user coupon")
			return
		}
		api.Success(w, userCouponResponse(x))
		return
	}
	if r.Method == http.MethodPost && len(p) == 3 && p[2] == "redeem" {
		id := strings.TrimSpace(p[1])
		if _, err := uuid.Parse(id); err != nil {
			api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "user coupon id must be a UUID")
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Idempotency-Key is required")
			return
		}
		identity, ok := auth.IdentityFromRequest(r)
		if !ok || strings.TrimSpace(identity.Subject) == "" {
			api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "unauthorized")
			return
		}
		x, e := c.coupons.Redeem(r.Context(), id, key, identity.Subject)
		if e != nil {
			writeCouponMutationError(w, e, "failed to redeem coupon")
			return
		}
		api.Success(w, userCouponResponse(x))
		return
	}
	if r.Method == http.MethodPost && len(p) == 3 && p[2] == "revoke" {
		var req dto.RevokeCouponRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid request body")
			return
		}
		id := strings.TrimSpace(p[1])
		if _, err := uuid.Parse(id); err != nil {
			api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "user coupon id must be a UUID")
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Idempotency-Key is required")
			return
		}
		identity, ok := auth.IdentityFromRequest(r)
		if !ok || strings.TrimSpace(identity.Subject) == "" {
			api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "unauthorized")
			return
		}
		x, e := c.coupons.Revoke(r.Context(), id, key, identity.Subject, req.Reason)
		if e != nil {
			writeCouponMutationError(w, e, "failed to revoke coupon")
			return
		}
		api.Success(w, userCouponResponse(x))
		return
	}
	http.NotFound(w, r)
}
func NewAdminCouponController(coupons *service.CouponService) *AdminCouponController {
	return &AdminCouponController{coupons: coupons}
}

func (c *AdminCouponController) Types(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		result, err := c.coupons.ListCouponTypes(r.Context())
		if err != nil {
			api.Error(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list coupon types")
			return
		}
		api.Success(w, couponTypeResponses(result))
		return
	}
	var req dto.CouponTypeRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid request body")
		return
	}
	var result *model.CouponType
	var err error
	if r.Method == http.MethodPost {
		result, err = c.coupons.CreateCouponType(r.Context(), req)
	} else if r.Method == http.MethodPut {
		result, err = c.coupons.UpdateCouponType(r.Context(), strings.TrimPrefix(r.URL.Path, "/v1/admin/coupons/types/"), req)
	} else {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		if errors.Is(err, service.ErrInvalidCouponType) {
			api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		} else if errors.Is(err, pgx.ErrNoRows) {
			api.Error(w, http.StatusNotFound, api.CodeNotFound, "coupon type not found")
		} else if strings.Contains(strings.ToLower(err.Error()), "duplicate key") {
			api.Error(w, http.StatusConflict, "ALREADY_EXISTS", "coupon type code already exists")
		} else {
			api.Error(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to save coupon type")
		}
		return
	}
	api.Success(w, couponTypeResponse(result))
}
func (c *AdminCouponController) Issue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Idempotency-Key is required")
		return
	}
	var req dto.IssueCouponsRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid request body")
		return
	}
	identity, ok := auth.IdentityFromRequest(r)
	if !ok || strings.TrimSpace(identity.Subject) == "" {
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "unauthorized")
		return
	}
	result, err := c.coupons.IssueCoupons(r.Context(), key, identity.Subject, req)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidIssueRequest):
			api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		case errors.Is(err, service.ErrIdempotencyConflict):
			api.Error(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", err.Error())
		default:
			api.Error(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to issue coupons")
		}
		return
	}
	api.Success(w, result)
}
