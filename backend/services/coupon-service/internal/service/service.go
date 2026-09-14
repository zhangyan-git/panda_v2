package service

import (
	"context"
	"errors"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/repository"
)

// CouponService coordinates coupon operations while retaining the legacy
// reserve and redeem entry points used by other callers.
type CouponService struct {
	batches     repository.BatchRepository
	coupons     repository.UserCouponRepository
	idempotency repository.IdempotencyRepository
	types       repository.CouponTypeRepository
	templates   repository.CouponTemplateRepository
}

func New(b repository.BatchRepository, c repository.UserCouponRepository, i repository.IdempotencyRepository) *CouponService {
	types, _ := b.(repository.CouponTypeRepository)
	templates, _ := b.(repository.CouponTemplateRepository)
	return &CouponService{batches: b, coupons: c, idempotency: i, types: types, templates: templates}
}

var (
	ErrInvalidCouponType     = errors.New("invalid coupon type")
	ErrCouponTypeUnavailable = errors.New("coupon type repository is not configured")
	ErrInvalidRequestID      = errors.New("request id is required")
	ErrInvalidCouponID       = errors.New("coupon id must be a UUID")
	ErrInvalidActorID        = errors.New("actor id must be a UUID")
	ErrInvalidReason         = errors.New("reason is required")
)

func (s *CouponService) ListCouponTypes(ctx context.Context) ([]*model.CouponType, error) {
	if s.types == nil {
		return nil, ErrCouponTypeUnavailable
	}
	return s.types.ListCouponTypes(ctx)
}

func (s *CouponService) CreateCouponType(ctx context.Context, req dto.CouponTypeRequest) (*model.CouponType, error) {
	if s.types == nil {
		return nil, ErrCouponTypeUnavailable
	}
	req.Code = strings.TrimSpace(req.Code)
	req.Name = strings.TrimSpace(req.Name)
	if req.Code == "" || len([]rune(req.Code)) > 64 || req.Name == "" || len([]rune(req.Name)) > 100 || len([]rune(req.Description)) > 500 || (req.Status != "active" && req.Status != "disabled") {
		return nil, ErrInvalidCouponType
	}
	return s.types.CreateCouponType(ctx, &model.CouponType{Code: req.Code, Name: req.Name, Description: req.Description, Status: req.Status})
}

func (s *CouponService) UpdateCouponType(ctx context.Context, id string, req dto.CouponTypeRequest) (*model.CouponType, error) {
	if s.types == nil {
		return nil, ErrCouponTypeUnavailable
	}
	id = strings.TrimSpace(id)
	req.Name = strings.TrimSpace(req.Name)
	if id == "" || req.Name == "" || len([]rune(req.Name)) > 100 || len([]rune(req.Description)) > 500 || (req.Status != "active" && req.Status != "disabled") {
		return nil, ErrInvalidCouponType
	}
	return s.types.UpdateCouponType(ctx, &model.CouponType{ID: id, Name: req.Name, Description: req.Description, Status: req.Status})
}
func (s *CouponService) ReserveInventory(ctx context.Context, batchID string, quantity int64) error {
	return s.batches.ReserveInventory(ctx, batchID, quantity)
}
func (s *CouponService) ListUserCoupons(ctx context.Context, q dto.UserCouponQuery) ([]*model.UserCoupon, int64, error) {
	r, ok := s.coupons.(interface {
		ListUserCoupons(context.Context, dto.UserCouponQuery) ([]*model.UserCoupon, int64, error)
	})
	if !ok {
		return nil, 0, errors.New("user coupon repository is not configured")
	}
	return r.ListUserCoupons(ctx, q)
}
func (s *CouponService) GetUserCoupon(ctx context.Context, id string) (*model.UserCoupon, error) {
	r, ok := s.coupons.(interface {
		GetUserCoupon(context.Context, string) (*model.UserCoupon, error)
	})
	if !ok {
		return nil, errors.New("user coupon repository is not configured")
	}
	return r.GetUserCoupon(ctx, id)
}
func (s *CouponService) UserCouponStats(ctx context.Context, userID string) (*dto.UserCouponStats, error) {
	r, ok := s.coupons.(interface {
		UserCouponStats(context.Context, string) (*dto.UserCouponStats, error)
	})
	if !ok {
		return nil, errors.New("user coupon repository is not configured")
	}
	return r.UserCouponStats(ctx, userID)
}
func (s *CouponService) Revoke(ctx context.Context, id, requestID, actorID, reason string) (*model.UserCoupon, error) {
	id = strings.TrimSpace(id)
	requestID = strings.TrimSpace(requestID)
	actorID = strings.TrimSpace(actorID)
	reason = strings.TrimSpace(reason)
	if id == "" {
		return nil, ErrInvalidCouponID
	}
	if requestID == "" {
		return nil, ErrInvalidRequestID
	}
	if actorID == "" {
		return nil, ErrInvalidActorID
	}
	if reason == "" {
		return nil, ErrInvalidReason
	}
	r, ok := s.coupons.(interface {
		Revoke(context.Context, string, string, string, string) (*model.UserCoupon, error)
	})
	if !ok {
		return nil, errors.New("user coupon repository is not configured")
	}
	return r.Revoke(ctx, id, requestID, actorID, reason)
}
func (s *CouponService) Redeem(ctx context.Context, couponID, requestID string, actorID ...string) (*model.UserCoupon, error) {
	couponID = strings.TrimSpace(couponID)
	requestID = strings.TrimSpace(requestID)
	if couponID == "" {
		return nil, ErrInvalidCouponID
	}
	if requestID == "" {
		return nil, ErrInvalidRequestID
	}
	if len(actorID) > 1 {
		return nil, ErrInvalidActorID
	}
	if len(actorID) == 1 {
		if strings.TrimSpace(actorID[0]) == "" {
			return nil, ErrInvalidActorID
		}
	}
	return s.coupons.Redeem(ctx, couponID, requestID)
}

func (s *CouponService) ListBatches(ctx context.Context, q dto.CouponBatchQuery) ([]*model.CouponBatch, int64, error) {
	r, ok := s.batches.(repository.BatchRepositoryQuery)
	if !ok {
		return nil, 0, errors.New("batch query repository is not configured")
	}
	return r.ListBatches(ctx, q)
}
func (s *CouponService) GetBatch(ctx context.Context, id string) (*model.CouponBatch, error) {
	r, ok := s.batches.(repository.BatchRepositoryQuery)
	if !ok {
		return nil, errors.New("batch query repository is not configured")
	}
	return r.GetBatch(ctx, id)
}
func (s *CouponService) BatchStats(ctx context.Context, id string) (*dto.CouponBatchStats, error) {
	r, ok := s.batches.(repository.BatchRepositoryQuery)
	if !ok {
		return nil, errors.New("batch query repository is not configured")
	}
	return r.BatchStats(ctx, id)
}
