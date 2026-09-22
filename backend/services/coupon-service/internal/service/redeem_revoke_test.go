package service

import (
	"context"
	"errors"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/repository"
)

type redeemRevokeMock struct {
	redeemCouponID  string
	redeemRequestID string
	redeemActorID   string
	redeemResult    *model.UserCoupon
	redeemErr       error
	revokeID        string
	revokeRequestID string
	revokeActorID   string
	revokeReason    string
	revokeResult    *model.UserCoupon
	revokeErr       error
}

func (m *redeemRevokeMock) Redeem(_ context.Context, couponID, requestID, actorID string) (*model.UserCoupon, error) {
	m.redeemCouponID = couponID
	m.redeemRequestID = requestID
	m.redeemActorID = actorID
	return m.redeemResult, m.redeemErr
}

func (m *redeemRevokeMock) Revoke(_ context.Context, id, requestID, actorID, reason string) (*model.UserCoupon, error) {
	m.revokeID = id
	m.revokeRequestID = requestID
	m.revokeActorID = actorID
	m.revokeReason = reason
	return m.revokeResult, m.revokeErr
}

func TestRedeemDelegatesCouponRequestAndActorIDs(t *testing.T) {
	want := &model.UserCoupon{ID: "coupon-1", Status: "redeemed"}
	mock := &redeemRevokeMock{redeemResult: want}
	svc := New(&issueBatchMock{}, mock, idemMock{})

	got, err := svc.Redeem(context.Background(), "coupon-1", "request-1", "actor-1")
	if err != nil {
		t.Fatalf("Redeem() error = %v", err)
	}
	if got != want {
		t.Fatalf("Redeem() result = %#v, want same result %#v", got, want)
	}
	if mock.redeemCouponID != "coupon-1" || mock.redeemRequestID != "request-1" {
		t.Fatalf("Redeem() arguments = (%q, %q)", mock.redeemCouponID, mock.redeemRequestID)
	}
	// actor 必须传到 repository：以前这里校验完就丢，流水上的操作人恒为空。
	if mock.redeemActorID != "actor-1" {
		t.Fatalf("Redeem() actor = %q, want %q", mock.redeemActorID, "actor-1")
	}
}

// 没有操作人的调用方（内部回调）走的是不带 actor 的那条路，落库为空串——由
// repository 的 NULLIF 转成 NULL。这不该被当成「参数错」。
func TestRedeemWithoutActorPassesEmptyActor(t *testing.T) {
	mock := &redeemRevokeMock{redeemResult: &model.UserCoupon{ID: "coupon-1"}}
	svc := New(&issueBatchMock{}, mock, idemMock{})

	if _, err := svc.Redeem(context.Background(), "coupon-1", "request-1"); err != nil {
		t.Fatalf("Redeem() error = %v", err)
	}
	if mock.redeemActorID != "" {
		t.Fatalf("Redeem() actor = %q, want empty", mock.redeemActorID)
	}
}

// 传了 actor 但传空串是「调错了」，必须在 service 这一层就被拒——空串的含义是
// 「没有操作人」，两者不能靠调用方随手传空来混淆。
func TestRedeemRejectsBlankActor(t *testing.T) {
	mock := &redeemRevokeMock{redeemResult: &model.UserCoupon{ID: "coupon-1"}}
	svc := New(&issueBatchMock{}, mock, idemMock{})

	if _, err := svc.Redeem(context.Background(), "coupon-1", "request-1", " "); !errors.Is(err, ErrInvalidActorID) {
		t.Fatalf("Redeem() error = %v, want ErrInvalidActorID", err)
	}
	if mock.redeemCouponID != "" {
		t.Fatalf("Redeem() 走到了 repository（couponID=%q）", mock.redeemCouponID)
	}
}

func TestRedeemPropagatesCouponNotRedeemableError(t *testing.T) {
	mock := &redeemRevokeMock{redeemErr: repository.ErrCouponNotRedeemable}
	svc := New(&issueBatchMock{}, mock, idemMock{})

	_, err := svc.Redeem(context.Background(), "coupon-1", "request-1", "actor-1")
	if !errors.Is(err, repository.ErrCouponNotRedeemable) {
		t.Fatalf("Redeem() error = %v, want ErrCouponNotRedeemable", err)
	}
}

func TestRevokeDelegatesCouponIDAndAuditArguments(t *testing.T) {
	want := &model.UserCoupon{ID: "coupon-1", Status: "invalidated"}
	mock := &redeemRevokeMock{revokeResult: want}
	svc := New(&issueBatchMock{}, mock, idemMock{})

	got, err := svc.Revoke(context.Background(), "coupon-1", "request-1", "actor-1", "manual revocation")
	if err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	if got != want {
		t.Fatalf("Revoke() result = %#v, want same result %#v", got, want)
	}
	if mock.revokeID != "coupon-1" || mock.revokeRequestID != "request-1" || mock.revokeActorID != "actor-1" || mock.revokeReason != "manual revocation" {
		t.Fatalf("Revoke() arguments = (%q, %q, %q, %q)", mock.revokeID, mock.revokeRequestID, mock.revokeActorID, mock.revokeReason)
	}
}

func TestRevokePropagatesRepositoryStateError(t *testing.T) {
	mock := &redeemRevokeMock{revokeErr: repository.ErrCouponNotRedeemable}
	svc := New(&issueBatchMock{}, mock, idemMock{})

	_, err := svc.Revoke(context.Background(), "coupon-1", "request-1", "actor-1", "reason")
	if !errors.Is(err, repository.ErrCouponNotRedeemable) {
		t.Fatalf("Revoke() error = %v, want ErrCouponNotRedeemable", err)
	}
}

func TestRevokeReturnsUnavailableErrorWhenRepositoryDoesNotSupportRevoke(t *testing.T) {
	svc := New(&issueBatchMock{}, couponMock{}, idemMock{})

	_, err := svc.Revoke(context.Background(), "coupon-1", "request-1", "actor-1", "reason")
	if err == nil || err.Error() != "user coupon repository is not configured" {
		t.Fatalf("Revoke() error = %v, want repository unavailable error", err)
	}
}
